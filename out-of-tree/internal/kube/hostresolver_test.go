// SPDX-License-Identifier: Apache-2.0

package kube_test

import (
	"testing"
	"time"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"metal3.local/anaconda/internal/core"
	"metal3.local/anaconda/internal/kube"
	"metal3.local/anaconda/internal/testsupport"
)

// Only hints that name a udev link survive into a kickstart. The rest select by
// property, which Ironic can do and kickstart cannot.
func TestRootDeviceSpec(t *testing.T) {
	cases := map[string]struct {
		hints *metal3api.RootDeviceHints
		want  string
	}{
		"no hints at all":     {hints: nil},
		"device name":         {hints: &metal3api.RootDeviceHints{DeviceName: "/dev/" + testsupport.Disk}, want: testsupport.Disk},
		"by-path device name": {hints: &metal3api.RootDeviceHints{DeviceName: "/dev/disk/by-path/pci-0000:01:00.0"}, want: "disk/by-path/pci-0000:01:00.0"},
		"wwn gains its prefix": {
			hints: &metal3api.RootDeviceHints{WWN: "5000c500a1b2c3d4"},
			want:  "disk/by-id/wwn-0x5000c500a1b2c3d4",
		},
		"wwn keeps the prefix it has": {
			hints: &metal3api.RootDeviceHints{WWN: "0x5000c500a1b2c3d4"},
			want:  "disk/by-id/wwn-0x5000c500a1b2c3d4",
		},
		// The extension is part of the same link, so the longer hint has to win or
		// the spec names a device that does not exist.
		"extension beats bare wwn": {
			hints: &metal3api.RootDeviceHints{WWN: "0x5000c500", WWNWithExtension: "0x5000c500a1b2"},
			want:  "disk/by-id/wwn-0x5000c500a1b2",
		},
		// The transport prefix is not in the hint, so the serial anchors the tail.
		"serial globs the transport prefix": {
			hints: &metal3api.RootDeviceHints{SerialNumber: "S3Z1NB0K"},
			want:  "disk/by-id/*S3Z1NB0K",
		},
		"device name beats everything": {
			hints: &metal3api.RootDeviceHints{DeviceName: "/dev/sda", WWN: "0x5000c500", SerialNumber: "S3Z1NB0K"},
			want:  "sda",
		},
		// A glob built from an empty serial would be disk/by-id/*, which matches
		// every disk on the machine and hands clearpart the whole array.
		"property hints are not expressible": {
			hints: &metal3api.RootDeviceHints{Model: "SAMSUNG", Vendor: "ATA", MinSizeGigabytes: 400, HCTL: "0:0:0:0"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := kube.RootDeviceSpec(tc.hints); got != tc.want {
				t.Errorf("RootDeviceSpec() = %q, want %q", got, tc.want)
			}
		})
	}
}

// newCallbackResolver builds a HostResolver backed by a fake client seeded with objs.
func newCallbackResolver(t *testing.T, objs ...client.Object) *kube.HostResolver {
	t.Helper()

	// The fixtures put every object in "ns", so the operator namespace has to
	// match or the scoped lists look at somewhere else entirely.
	t.Setenv("POD_NAMESPACE", "ns")

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}

	if err := metal3api.AddToScheme(scheme); err != nil {
		t.Fatalf("add metal3api to scheme: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	return &kube.HostResolver{
		Client:    c,
		APIReader: c,
	}
}

// hostWithBMC returns a BareMetalHost referencing the named BMC credentials Secret.
func hostWithBMC(namespace, name, credsName string) *metal3api.BareMetalHost {
	return &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: testsupport.UID},
		Spec:       metal3api.BareMetalHostSpec{BMC: metal3api.BMCDetails{CredentialsName: credsName}},
	}
}

// hostWithMACs builds a host carrying a boot MAC and inspected NIC MACs.
func hostWithMACs(name, bootMAC string, nicMACs ...string) *metal3api.BareMetalHost {
	host := hostWithBMC("ns", name, "bmc-creds")
	host.UID = types.UID("uid-" + name)
	host.Spec.BootMACAddress = bootMAC

	if len(nicMACs) > 0 {
		nics := make([]metal3api.NIC, 0, len(nicMACs))
		for _, m := range nicMACs {
			nics = append(nics, metal3api.NIC{MAC: m})
		}

		host.Status.HardwareDetails = &metal3api.HardwareDetails{NIC: nics}
	}

	return host
}

func TestInstallReportRoundTrip(t *testing.T) {
	ctx := t.Context()
	r := newCallbackResolver(t, hostWithBMC("ns", testsupport.Host, "bmc-creds"))

	if got, err := r.ReadInstallReport(ctx, "ns", testsupport.Host); err != nil || got != nil {
		t.Fatalf("ReadInstallReport before any = (%v, %v), want (nil, nil)", got, err)
	}

	if err := r.WriteInstallReport(ctx, "ns", testsupport.Host, core.InstallReport{Message: testsupport.Failure}); err != nil {
		t.Fatalf("WriteInstallReport: %v", err)
	}

	host := &metal3api.BareMetalHost{}
	key := types.NamespacedName{Namespace: "ns", Name: testsupport.Host}

	if err := r.Client.Get(ctx, key, host); err != nil {
		t.Fatalf("get host: %v", err)
	}

	// Annotations rather than a Secret, so nothing is created and BMO's own
	// permissions are enough.
	if host.Annotations[kube.InstallResultAnnotation] != kube.InstallResultFailed {
		t.Errorf("result annotation = %q, want %q", host.Annotations[kube.InstallResultAnnotation], kube.InstallResultFailed)
	}

	if host.Annotations[kube.InstallMessageAnnotation] != testsupport.Failure {
		t.Errorf("message annotation = %q", host.Annotations[kube.InstallMessageAnnotation])
	}

	got, err := r.ReadInstallReport(ctx, "ns", testsupport.Host)
	if err != nil || got == nil || got.Succeeded || got.Message != testsupport.Failure {
		t.Fatalf("ReadInstallReport = (%+v, %v), want the failure back", got, err)
	}

	// A success overwrites the failure and drops the stale message with it.
	if err := r.WriteInstallReport(ctx, "ns", testsupport.Host, core.InstallReport{Succeeded: true}); err != nil {
		t.Fatalf("second WriteInstallReport: %v", err)
	}

	if got, _ = r.ReadInstallReport(ctx, "ns", testsupport.Host); got == nil || !got.Succeeded || got.Message != "" {
		t.Errorf("ReadInstallReport = %+v, want a clean success", got)
	}

	if err := r.ClearInstallReport(ctx, "ns", testsupport.Host); err != nil {
		t.Fatalf("ClearInstallReport: %v", err)
	}

	if got, err = r.ReadInstallReport(ctx, "ns", testsupport.Host); err != nil || got != nil {
		t.Errorf("after clear = (%v, %v), want (nil, nil)", got, err)
	}
}

// Starting an install has to drop the previous verdict, or a reinstall reads the
// last run's success on its first pass and finishes without installing.
func TestClearInstallReportDropsThePreviousVerdict(t *testing.T) {
	ctx := t.Context()
	r := newCallbackResolver(t, hostWithBMC("ns", testsupport.Host, "bmc-creds"))

	if err := r.WriteInstallReport(ctx, "ns", testsupport.Host, core.InstallReport{Succeeded: true}); err != nil {
		t.Fatalf("WriteInstallReport: %v", err)
	}

	if err := r.ClearInstallReport(ctx, "ns", testsupport.Host); err != nil {
		t.Fatalf("ClearInstallReport: %v", err)
	}

	if got, _ := r.ReadInstallReport(ctx, "ns", testsupport.Host); got != nil {
		t.Errorf("report = %+v, want it cleared by a new install", got)
	}
}

// The install clock is BMO's own, stamped when the host entered provisioning.
func TestInstallStartedAtReadsOperationHistory(t *testing.T) {
	ctx := t.Context()
	started := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	host := hostWithBMC("ns", testsupport.Host, "bmc-creds")
	host.Status.OperationHistory.Provision.Start = started

	r := newCallbackResolver(t, host)

	got, err := r.InstallStartedAt(ctx, "ns", testsupport.Host)
	if err != nil || !got.Equal(started.Time) {
		t.Errorf("InstallStartedAt = (%v, %v), want %v", got, err, started.Time)
	}
}

func TestFindHostsByMAC(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "ns")

	boot := hostWithMACs("boot-host", testsupport.MAC)
	inspected := hostWithMACs("nic-host", "", "aa:bb:cc:dd:ee:02")

	// The same MAC in another namespace, which the lookup must never return.
	foreign := hostWithMACs("foreign-boot", testsupport.MAC)
	foreign.Namespace = "other"

	r := newCallbackResolver(t, boot, inspected, foreign)
	ctx := t.Context()

	cases := []struct {
		name string
		want string
		macs []string
	}{
		{"boot MAC hit", "boot-host", []string{testsupport.MAC}},
		// A host that reported the MAC during inspection but declares no boot
		// MAC cannot be provisioned, so serving it a kickstart is pointless.
		{"inspected NIC is not a match", "", []string{"aa:bb:cc:dd:ee:02"}},
		{"case and separator insensitive", "boot-host", []string{"AA-BB-CC-DD-EE-01"}},
		{"unknown MAC", "", []string{"aa:bb:cc:dd:ee:ff"}},
		{"no MACs", "", nil},
		{"unparseable MAC", "", []string{"nonsense"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosts, err := r.FindHostsByMAC(ctx, tc.macs)
			if err != nil {
				t.Fatalf("FindHostsByMAC: %v", err)
			}

			if tc.want == "" {
				if len(hosts) != 0 {
					t.Fatalf("hosts = %+v, want none", hosts)
				}

				return
			}

			if len(hosts) != 1 || hosts[0].Name != tc.want {
				t.Fatalf("hosts = %+v, want just %s", hosts, tc.want)
			}

			if hosts[0].UID != "uid-"+tc.want {
				t.Errorf("host ref = %+v, want the UID filled in", hosts[0])
			}
		})
	}
}

// Two hosts declaring the same boot MAC is a conflict only their operator can
// resolve, so both come back and the caller logs it rather than picking blindly.
func TestFindHostsByMACReportsACollision(t *testing.T) {
	shared := testsupport.MAC
	r := newCallbackResolver(t, hostWithMACs("host-a", shared), hostWithMACs("host-b", shared))

	hosts, err := r.FindHostsByMAC(t.Context(), []string{shared})
	if err != nil {
		t.Fatalf("FindHostsByMAC: %v", err)
	}

	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v, want both so the caller can log the conflict", hosts)
	}
}

// The host supplied namespace wins, because an empty one would silently widen
// every scoped list to the whole cluster.
func TestHostResolverNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "from-env")

	if got := (&kube.HostResolver{PodNamespace: "from-host"}).Namespace(); got != "from-host" {
		t.Errorf("Namespace = %q, want the host supplied value", got)
	}

	if got := (&kube.HostResolver{}).Namespace(); got != "from-env" {
		t.Errorf("Namespace = %q, want the environment fallback", got)
	}
}
