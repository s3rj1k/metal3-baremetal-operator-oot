// SPDX-License-Identifier: Apache-2.0

// End to end cover for the provisioner against a stub BMC.

package anaconda_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/hardwareutils/bmc"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"metal3.local/anaconda"
	"metal3.local/anaconda/internal/core"
	"metal3.local/anaconda/internal/testsupport"
)

// fakeStore records the install state the provisioner keeps per host.
type fakeStore struct {
	report       *core.InstallReport
	hardwareData *metal3api.HardwareDetails
	started      time.Time
	cleared      int
	noKickstart  bool
	noRootHints  bool
}

// The disk is only ever the host's own hint, so a store that reports none stands
// for a BareMetalHost that left spec.rootDeviceHints unset.
func (f *fakeStore) HostInstallDisk(_ context.Context, _, _ string) (string, error) {
	if f.noRootHints {
		return "", nil
	}

	return testsupport.Disk, nil
}

func (f *fakeStore) HostHardwareData(_ context.Context, _, _ string) (*metal3api.HardwareDetails, error) {
	return f.hardwareData, nil
}

func (f *fakeStore) HasKickstart(_ context.Context, _, _ string) (bool, error) {
	return !f.noKickstart, nil
}

func (f *fakeStore) ReadInstallReport(_ context.Context, _, _ string) (*core.InstallReport, error) {
	return f.report, nil
}

func (f *fakeStore) ClearInstallReport(_ context.Context, _, _ string) error {
	f.cleared++
	f.report = nil

	return nil
}

func (f *fakeStore) InstallStartedAt(_ context.Context, _, _ string) (time.Time, error) {
	return f.started, nil
}

// testProvisioner points a provisioner at srv over the redfish+http scheme, the
// form a BareMetalHost carries for a plaintext BMC.
func testProvisioner(t *testing.T, srv *httptest.Server, store anaconda.HostStore, opts ...func(*anaconda.Provisioner)) *anaconda.Provisioner {
	t.Helper()

	authority := strings.TrimPrefix(srv.URL, "http://")

	p := &anaconda.Provisioner{
		Cfg:   core.Config{InstallTimeout: time.Hour},
		Store: store,
		Log:   logr.Discard(),
		HostData: provisioner.HostData{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "ns", Name: testsupport.Host, UID: testsupport.UID},
			BMCAddress:     "redfish-virtualmedia+http://" + authority + testsupport.SystemPath,
			BMCCredentials: bmc.Credentials{Username: testsupport.User, Password: testsupport.Pass},
			BootMACAddress: testsupport.MAC,
			ProvisionerID:  testsupport.UID,
		},
		CallbackEnabled: true,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// liveISOData builds the ProvisionData the BMH controller passes for a live ISO,
// which carries no checksum because BMO exempts the format from that check.
func liveISOData(url string) provisioner.ProvisionData {
	format := anaconda.LiveISO

	return provisioner.ProvisionData{
		Image:    metal3api.Image{URL: url, DiskFormat: &format},
		BootMode: metal3api.UEFI,
	}
}

// The whole point of the provisioner. An installer ISO reaches the drive, the
// machine boots it, and the host waits until anaconda says it finished.
func TestProvisionBootsLiveISOAndWaitsForCallback(t *testing.T) {
	const iso = "http://boot.example/rocky-10.2-x86_64-ks1.iso"

	state := &testsupport.LiveISOState{}
	store := &fakeStore{}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), store)
	ctx := t.Context()

	// First pass, the host is off, so the media goes in and the machine starts.
	result, err := p.Provision(ctx, liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if !result.Dirty || result.ErrorMessage != "" {
		t.Fatalf("result = %+v, want dirty while the install is in flight", result)
	}

	if !state.Inserted || state.Image != iso {
		t.Errorf("media = (inserted %v, image %q), want the ISO inserted", state.Inserted, state.Image)
	}

	// A once only override, so the reboot ending the install lands on disk.
	want := map[string]any{
		"BootSourceOverrideTarget":  "Cd",
		"BootSourceOverrideEnabled": "Once",
		"BootSourceOverrideMode":    "UEFI",
	}

	for key, wantValue := range want {
		if got := state.Boot[key]; got != wantValue {
			t.Errorf("boot[%s] = %v, want %v", key, got, wantValue)
		}
	}

	if len(state.Resets) != 1 || state.Resets[0] != "On" {
		t.Errorf("resets = %v, want a single power on", state.Resets)
	}

	if store.cleared != 1 {
		t.Errorf("report clears = %d, want the previous verdict dropped once", store.cleared)
	}

	// Second pass, booted but nothing reported yet, so keep waiting.
	result, err = p.Provision(ctx, liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision (waiting): %v", err)
	}

	if !result.Dirty || result.RequeueAfter != core.InstallPollInterval {
		t.Errorf("result = %+v, want a requeue at the poll interval", result)
	}

	if state.Ejects != 0 {
		t.Error("media was ejected before the install reported in")
	}

	// Third pass, the callback landed. The kickstart issues no power command, so
	// the host is asked to go down and the drive stays put while it does.
	store.report = &core.InstallReport{Succeeded: true}

	result, err = p.Provision(ctx, liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision (shutting down): %v", err)
	}

	if !result.Dirty {
		t.Errorf("result = %+v, want a requeue while the host shuts down", result)
	}

	if state.Ejects != 0 {
		t.Error("media was ejected while the host was still running, which aborts anaconda's shutdown")
	}

	if last := state.Resets[len(state.Resets)-1]; last != "GracefulShutdown" {
		t.Errorf("resets = %v, want a graceful shutdown so the target is unmounted", state.Resets)
	}

	// Fourth pass, the host is down, so the media comes out and it boots to disk.
	result, err = p.Provision(ctx, liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision (complete): %v", err)
	}

	if result.Dirty || result.ErrorMessage != "" {
		t.Errorf("result = %+v, want a clean finish once the host is down", result)
	}

	if state.Inserted || state.Ejects != 1 {
		t.Errorf("media = (inserted %v, ejects %d), want exactly one eject", state.Inserted, state.Ejects)
	}

	if !state.PowerOn {
		t.Error("the host was left off, want it booted onto its disk")
	}

	// A BMC that ignored the one time override would send the rebooted host back
	// to an empty drive, so the machine is pointed at disk explicitly.
	done := map[string]any{
		"BootSourceOverrideTarget":  "Hdd",
		"BootSourceOverrideEnabled": "Continuous",
		"BootSourceOverrideMode":    "UEFI",
	}

	for key, wantValue := range done {
		if got := state.Boot[key]; got != wantValue {
			t.Errorf("boot[%s] = %v, want %v after the install finished", key, got, wantValue)
		}
	}
}

// Swapping media under a running machine leaves it on the old boot source, so
// the host goes down before the drive is touched.
func TestProvisionPowersOffBeforeInsertingMedia(t *testing.T) {
	state := &testsupport.LiveISOState{PowerOn: true}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

	result, err := p.Provision(t.Context(), liveISOData(testsupport.ISO), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if !result.Dirty {
		t.Errorf("result = %+v, want dirty while the host powers down", result)
	}

	if state.Inserted {
		t.Error("media was inserted while the host was still running")
	}

	if len(state.Resets) != 1 || state.Resets[0] != "ForceOff" {
		t.Errorf("resets = %v, want a single forced power off", state.Resets)
	}
}

// An emulator that never reaches its hypervisor still answers the insert, and
// the host then boots its disk and waits out the whole install timeout.
func TestProvisionRefusesWhenTheDriveStaysEmpty(t *testing.T) {
	state := &testsupport.LiveISOState{DropInsert: true}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

	result, err := p.Provision(t.Context(), liveISOData(testsupport.ISO), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if !strings.Contains(result.ErrorMessage, "drive empty") {
		t.Errorf("result = %+v, want a refusal naming the empty drive", result)
	}

	if len(state.Resets) != 0 {
		t.Errorf("resets = %v, want the host left alone rather than booted to no media", state.Resets)
	}
}

// Some BMCs fetch the image themselves and report their own URL, which is not a
// failure and must not stop the install.
func TestProvisionAcceptsARewrittenImageURL(t *testing.T) {
	state := &testsupport.LiveISOState{RewriteImage: "file:///var/lib/bmc/cached.iso"}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

	result, err := p.Provision(t.Context(), liveISOData(testsupport.ISO), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.ErrorMessage != "" {
		t.Errorf("result = %+v, want the install to proceed despite the rewritten URL", result)
	}

	if !state.PowerOn {
		t.Error("the host was never powered on")
	}
}

// The override is what points the host at the drive, so a BMC that accepts the
// write and keeps booting the disk has to be caught before the host comes up.
func TestProvisionRefusesWhenTheBootOverrideIsIgnored(t *testing.T) {
	state := &testsupport.LiveISOState{DropBoot: true}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

	result, err := p.Provision(t.Context(), liveISOData(testsupport.ISO), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if !strings.Contains(result.ErrorMessage, "boot override") {
		t.Errorf("result = %+v, want a refusal naming the ignored override", result)
	}

	if state.PowerOn {
		t.Error("the host was powered on despite the override never taking")
	}
}

// A reinstall must not read the previous run's report and declare victory
// before anaconda has even booted.
func TestProvisionClearsAStaleCallback(t *testing.T) {
	const iso = testsupport.ISO

	state := &testsupport.LiveISOState{}
	store := &fakeStore{report: &core.InstallReport{Succeeded: true}}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), store)

	result, err := p.Provision(t.Context(), liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if !result.Dirty {
		t.Fatalf("result = %+v, want dirty, the install has only just started", result)
	}

	if store.report != nil {
		t.Error("the previous install's callback survived the start of a new one")
	}

	if state.Ejects != 0 {
		t.Error("media was ejected on the strength of a stale callback")
	}
}

// An install that never reports has to fail rather than requeue forever, or the
// host sits in provisioning with nothing to show for it.
func TestProvisionTimesOutWaitingForCallback(t *testing.T) {
	const iso = testsupport.ISO

	state := &testsupport.LiveISOState{PowerOn: true, Inserted: true, Image: iso}
	store := &fakeStore{started: time.Now().Add(-2 * time.Hour)}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), store)

	result, err := p.Provision(t.Context(), liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.ErrorMessage == "" {
		t.Fatalf("result = %+v, want an error once the install timed out", result)
	}

	if !strings.Contains(result.ErrorMessage, "did not report completion") {
		t.Errorf("ErrorMessage = %q, want it to name the timeout", result.ErrorMessage)
	}
}

// With no listener nothing can ever report in, so waiting would strand the host
// until the timeout for no reason.
func TestProvisionWithoutCallbackListenerFinishesAtBoot(t *testing.T) {
	const iso = testsupport.ISO

	state := &testsupport.LiveISOState{PowerOn: true, Inserted: true, Image: iso}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{}, func(p *anaconda.Provisioner) {
		p.CallbackEnabled = false
	})

	result, err := p.Provision(t.Context(), liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.Dirty || result.ErrorMessage != "" {
		t.Errorf("result = %+v, want a clean finish when nothing can report in", result)
	}
}

// Writing a disk image needs an agent on the host, which out of band Redfish
// cannot run, so it has to be refused rather than silently reported done.
func TestProvisionRefusesNonLiveISO(t *testing.T) {
	qcow2 := "qcow2"

	cases := map[string]provisioner.ProvisionData{
		"no image":   {},
		"disk image": {Image: metal3api.Image{URL: "http://images.example/debian.qcow2", DiskFormat: &qcow2}},
		"no format":  {Image: metal3api.Image{URL: "http://images.example/debian.img"}},
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			state := &testsupport.LiveISOState{}
			p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

			result, err := p.Provision(t.Context(), data, false)
			if err != nil {
				t.Fatalf("Provision: %v", err)
			}

			if !strings.Contains(result.ErrorMessage, "not supported") {
				t.Errorf("ErrorMessage = %q, want an unsupported report", result.ErrorMessage)
			}

			if state.Inserted {
				t.Error("media was inserted for an image that cannot be deployed")
			}
		})
	}
}

// A deprovisioned host must stop booting the installer, otherwise it reinstalls
// itself on the next power cycle.
func TestDeprovisionEjectsMediaAndForgetsTheInstall(t *testing.T) {
	state := &testsupport.LiveISOState{Inserted: true, Image: testsupport.ISO}
	store := &fakeStore{report: &core.InstallReport{Succeeded: true}, started: time.Now()}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), store)

	result, err := p.Deprovision(t.Context(), false, "")
	if err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	if result.Dirty || result.ErrorMessage != "" {
		t.Errorf("result = %+v, want a clean deprovision", result)
	}

	if state.Inserted || state.Ejects != 1 {
		t.Errorf("media = (inserted %v, ejects %d), want exactly one eject", state.Inserted, state.Ejects)
	}

	if store.cleared != 1 || store.report != nil {
		t.Errorf("install state was not forgotten, clears = %d report = %+v", store.cleared, store.report)
	}

	if len(state.Erased) != 0 {
		t.Errorf("erased = %v, want no wipe for an unset cleaning mode", state.Erased)
	}
}

// The one cleaning mode BMO defines sanitizes the disks, which is the only way
// a redeployed host does not hand the last tenant's data to the next one.
func TestDeprovisionErasesDisksWhenCleaningIsEnabled(t *testing.T) {
	state := &testsupport.LiveISOState{Inserted: true, Image: testsupport.ISO}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

	result, err := p.Deprovision(t.Context(), false, metal3api.CleaningModeMetadata)
	if err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	if result.Dirty || result.ErrorMessage != "" {
		t.Errorf("result = %+v, want a clean deprovision", result)
	}

	if len(state.Erased) != 2 {
		t.Errorf("erased = %v, want every drive the system reports", state.Erased)
	}
}

// Disabled means disabled. A host that opted out must keep its disks.
func TestDeprovisionLeavesDisksAloneWhenCleaningIsDisabled(t *testing.T) {
	state := &testsupport.LiveISOState{Inserted: true, Image: testsupport.ISO}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

	if _, err := p.Deprovision(t.Context(), false, metal3api.CleaningModeDisabled); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	if len(state.Erased) != 0 {
		t.Errorf("erased = %v, want the disks untouched", state.Erased)
	}

	if state.Ejects != 1 {
		t.Errorf("ejects = %d, want the media still released", state.Ejects)
	}
}

// A wedged finalizer is worse than a disk that still holds data, so a BMC that
// refuses the erase must not block the host from being deleted.
func TestDeprovisionSurvivesAFailedErase(t *testing.T) {
	state := &testsupport.LiveISOState{Inserted: true, Image: testsupport.ISO, EraseFails: true}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})

	result, err := p.Deprovision(t.Context(), false, metal3api.CleaningModeMetadata)
	if err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	if result.Dirty || result.ErrorMessage != "" {
		t.Errorf("result = %+v, want teardown to finish despite the failed erase", result)
	}

	if state.Ejects != 1 {
		t.Errorf("ejects = %d, want the media released even after the erase failed", state.Ejects)
	}
}

// Teardown must not depend on the BMC, otherwise a host with a broken address
// keeps its finalizer and cannot be deleted.
func TestTeardownIgnoresBrokenBMC(t *testing.T) {
	state := &testsupport.LiveISOState{}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})
	p.HostData.BMCAddress = "not-a-url"

	ctx := t.Context()

	cases := map[string]func() (provisioner.Result, error){
		"deprovision": func() (provisioner.Result, error) { return p.Deprovision(ctx, false, "") },
		"delete":      func() (provisioner.Result, error) { return p.Delete(ctx) },
		"detach":      func() (provisioner.Result, error) { return p.Detach(ctx, false) },
		"power off":   func() (provisioner.Result, error) { return p.PowerOff(ctx, metal3api.RebootModeSoft, false, "") },
	}

	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := call()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			if result.Dirty || result.ErrorMessage != "" {
				t.Errorf("result = %+v, want a clean no op", result)
			}
		})
	}
}

// Both data points provisioning needs are declared, so there is nothing an
// inventory could add and the BMC must not be asked for one.
func TestInspectSkipsRedfishWhenBootMACAndDiskAreKnown(t *testing.T) {
	touched := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		touched = true
	}))
	t.Cleanup(srv.Close)

	// The default store reports an install disk, standing for a host that set
	// spec.rootDeviceHints.
	p := testProvisioner(t, srv, &fakeStore{})
	p.HostData.BootMACAddress = testsupport.MAC

	result, started, details, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, false, false)
	if err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	if touched {
		t.Error("the BMC was inspected even though the boot MAC and the install disk were both known")
	}

	// BMO keeps a host in Inspecting forever when it is handed nil details, so
	// the synthesized record is what lets such a host move on.
	if details == nil {
		t.Fatal("details are nil, the host would loop in Inspecting")
	}

	if len(details.NIC) != 1 || details.NIC[0].MAC != testsupport.MAC || details.Hostname != testsupport.Host {
		t.Errorf("details = %+v, want the synthesized record", details)
	}

	if started || result.Dirty || result.ErrorMessage != "" {
		t.Errorf("result = %+v, started = %v, want a clean single pass", result, started)
	}
}

// A boot MAC on its own is not enough, the install disk still has to come from
// somewhere, so this host is inspected.
func TestInspectCollectsInventoryWithABootMAC(t *testing.T) {
	p := testProvisioner(t, testsupport.RedfishService(t), &fakeStore{noRootHints: true})
	p.HostData.BootMACAddress = testsupport.MAC

	result, started, details, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, false, false)
	if err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	if details == nil {
		t.Fatal("details are nil, the host would loop in Inspecting")
	}

	// A declared boot MAC used to short circuit this, which recorded a host with
	// no CPU, no memory and no disks.
	if details.RAMMebibytes == 0 || details.CPU.Count == 0 || len(details.Storage) == 0 {
		t.Errorf("details = %+v, want the collected inventory rather than the synthesized record", details)
	}

	if details.SystemVendor.Manufacturer != "Contoso" {
		t.Errorf("SystemVendor = %+v, want the stub vendor", details.SystemVendor)
	}

	if started || result.Dirty || result.ErrorMessage != "" {
		t.Errorf("result = %+v, started = %v, want a clean single pass", result, started)
	}
}

// A BMC that serves nothing would requeue forever, so a host that declares its
// boot MAC records the synthesized stand in instead.
func TestInspectFallsBackToTheBootMACOnAnEmptyInventory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testsupport.RootPath:
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/","Systems":{"@odata.id":"/redfish/v1/Systems"}}`))
		case testsupport.SystemsPath:
			_, _ = w.Write([]byte(`{"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`))
		case testsupport.SystemPath:
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/Systems/1","Id":"1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	p := testProvisioner(t, srv, &fakeStore{noRootHints: true})
	p.HostData.BootMACAddress = testsupport.MAC

	result, _, details, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, false, false)
	if err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	if details == nil {
		t.Fatal("details are nil, the host would loop in Inspecting")
	}

	if len(details.NIC) != 1 || details.NIC[0].MAC != testsupport.MAC || details.Hostname != testsupport.Host {
		t.Errorf("details = %+v, want the synthesized record", details)
	}

	if result.Dirty {
		t.Errorf("result = %+v, want a clean single pass rather than a requeue", result)
	}
}

// Without a boot MAC the inventory is collected out of band, which is the only
// way the kickstart lookup can learn the host's MACs.
func TestInspectCollectsInventoryWithoutABootMAC(t *testing.T) {
	p := testProvisioner(t, testsupport.RedfishService(t), &fakeStore{})
	p.HostData.BootMACAddress = ""

	_, _, details, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, false, false)
	if err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	if details == nil {
		t.Fatal("details are nil, want an inventory")
	}

	if details.RAMMebibytes != 131072 {
		t.Errorf("RAMMebibytes = %d, want 131072 from 128 GiB", details.RAMMebibytes)
	}

	if details.CPU.Count != 64 || details.CPU.Arch != "x86_64" {
		t.Errorf("CPU = %+v, want 64 logical x86_64 processors", details.CPU)
	}

	if len(details.NIC) != 1 || details.NIC[0].MAC != testsupport.MAC {
		t.Fatalf("NIC = %+v, want the stub interface", details.NIC)
	}

	if len(details.Storage) != 1 || details.Storage[0].Type != metal3api.NVME {
		t.Errorf("Storage = %+v, want the NVMe drive", details.Storage)
	}

	if details.SystemVendor.Manufacturer != "Contoso" {
		t.Errorf("SystemVendor = %+v, want the stub vendor", details.SystemVendor)
	}
}

// A BMC that answers every request with an empty document must not be recorded
// as a host with no hardware.
func TestInspectRequeuesOnAnEmptyInventory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testsupport.RootPath:
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/","Systems":{"@odata.id":"/redfish/v1/Systems"}}`))
		case testsupport.SystemsPath:
			_, _ = w.Write([]byte(`{"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`))
		case testsupport.SystemPath:
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/Systems/1","Id":"1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	p := testProvisioner(t, srv, &fakeStore{})
	p.HostData.BootMACAddress = ""

	result, _, details, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, false, false)
	if err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	if details != nil {
		t.Errorf("details = %+v, want nothing recorded from an empty BMC", details)
	}

	if !result.Dirty {
		t.Errorf("result = %+v, want a requeue rather than an empty inventory", result)
	}
}

// A reported failure has to reach the BareMetalHost as a provisioning error.
// Reporting success instead would leave an unusable machine marked Provisioned.
func TestProvisionSurfacesAReportedFailure(t *testing.T) {
	const iso = testsupport.ISO

	state := &testsupport.LiveISOState{PowerOn: true, Inserted: true, Image: iso}
	store := &fakeStore{
		started: time.Now(),
		report:  &core.InstallReport{Message: testsupport.Failure},
	}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), store)

	result, err := p.Provision(t.Context(), liveISOData(iso), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.ErrorMessage == "" || result.Dirty {
		t.Fatalf("result = %+v, want a non dirty provisioning error", result)
	}

	if !strings.Contains(result.ErrorMessage, testsupport.Failure) {
		t.Errorf("ErrorMessage = %q, want the installer's own reason", result.ErrorMessage)
	}

	// The drive stays as anaconda left it so the failure can be looked at.
	if state.Ejects != 0 || !state.Inserted {
		t.Errorf("media = (inserted %v, ejects %d), want it left in place on failure", state.Inserted, state.Ejects)
	}
}

// A host with no kickstart must not be touched at all. Booting it would waste an
// install and leave a machine that powered itself off with no reason recorded.
func TestProvisionRefusesWithoutAKickstart(t *testing.T) {
	state := &testsupport.LiveISOState{PowerOn: true}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{noKickstart: true})

	result, err := p.Provision(t.Context(), liveISOData(testsupport.ISO), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.ErrorMessage == "" || result.Dirty {
		t.Fatalf("result = %+v, want a non dirty provisioning error", result)
	}

	if !strings.Contains(result.ErrorMessage, "no kickstart") {
		t.Errorf("ErrorMessage = %q, want it to name the missing kickstart", result.ErrorMessage)
	}

	// Nothing may have happened to the machine.
	if len(state.Resets) != 0 {
		t.Errorf("resets = %v, want the host left alone", state.Resets)
	}

	if state.Inserted {
		t.Error("media was inserted for a host with no kickstart")
	}
}

// clearpart wipes whatever disk it is handed, so a host that names none must not
// boot. BMO would otherwise pass the profile default and take /dev/sda.
func TestProvisionRefusesWithoutRootDeviceHints(t *testing.T) {
	state := &testsupport.LiveISOState{PowerOn: true}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{noRootHints: true})

	result, err := p.Provision(t.Context(), liveISOData(testsupport.ISO), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.ErrorMessage == "" || result.Dirty {
		t.Fatalf("result = %+v, want a non dirty provisioning error", result)
	}

	if !strings.Contains(result.ErrorMessage, "no root device hints") {
		t.Errorf("ErrorMessage = %q, want it to name the missing hints", result.ErrorMessage)
	}

	// The machine has to be untouched, a wipe is not recoverable.
	if len(state.Resets) != 0 || state.Inserted {
		t.Errorf("resets = %v, inserted = %v, want the host left alone", state.Resets, state.Inserted)
	}
}

// The HardwareData CR is the record BMO rewrites on every inspection, so a host
// that has one is already inspected and must cost no BMC round trips.
func TestInspectIngestsHardwareData(t *testing.T) {
	touched := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		touched = true
	}))
	t.Cleanup(srv.Close)

	recorded := &metal3api.HardwareDetails{
		Hostname: "ingested",
		NIC:      []metal3api.NIC{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:09"}},
	}

	p := testProvisioner(t, srv, &fakeStore{hardwareData: recorded})

	_, started, details, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, false, false)
	if err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	if touched {
		t.Error("the BMC was inspected even though the host already has hardware data")
	}

	// Returning anything else leaves the CR and the status disagreeing, and BMO
	// only refreshes the status from the CR while it is still nil.
	if details != recorded || started {
		t.Errorf("details = %+v, started = %v, want the recorded data handed straight back", details, started)
	}
}

// BMO deletes the inspect annotation only when a provisioner reports inspection
// started, and moves the host back into Inspecting forever while it is there.
func TestInspectAcknowledgesARefresh(t *testing.T) {
	touched := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		touched = true
	}))
	t.Cleanup(srv.Close)

	p := testProvisioner(t, srv, &fakeStore{})

	_, started, details, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, true, false)
	if err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	if !started {
		t.Error("started = false, the refresh annotation would never be cleared")
	}

	if details != nil || touched {
		t.Errorf("details = %+v, BMC touched = %v, want the pass to only clear the annotation", details, touched)
	}
}

// Provisioning waits for a boot MAC, so inspection has to say which ones the
// machine actually has, otherwise there is nothing to choose from.
func TestInspectNamesBootMACCandidates(t *testing.T) {
	var events []string

	p := testProvisioner(t, testsupport.RedfishService(t), &fakeStore{}, func(p *anaconda.Provisioner) {
		p.Publisher = func(reason, message string) {
			events = append(events, reason+" "+message)
		}
	})
	p.HostData.BootMACAddress = ""

	if _, _, _, err := p.InspectHardware(t.Context(), provisioner.InspectData{}, false, false, false); err != nil {
		t.Fatalf("InspectHardware: %v", err)
	}

	found := false

	for _, e := range events {
		if strings.HasPrefix(e, "BootMACRequired") && strings.Contains(e, testsupport.MAC) {
			found = true
		}
	}

	if !found {
		t.Errorf("events = %v, want one naming the discovered MAC", events)
	}
}

// The kickstart is served by matching the MACs anaconda reports, so a host with
// none declared would install the inert fallback and power itself off.
func TestProvisionRefusesWithoutABootMAC(t *testing.T) {
	state := &testsupport.LiveISOState{}
	p := testProvisioner(t, testsupport.LiveISOService(t, state), &fakeStore{})
	p.HostData.BootMACAddress = ""

	result, err := p.Provision(t.Context(), liveISOData(testsupport.ISO), false)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.ErrorMessage == "" || result.Dirty {
		t.Fatalf("result = %+v, want a non dirty provisioning error", result)
	}

	if !strings.Contains(result.ErrorMessage, "bootMACAddress") {
		t.Errorf("ErrorMessage = %q, want it to name the missing field", result.ErrorMessage)
	}

	// Nothing may have happened to the machine.
	if len(state.Resets) != 0 || state.Inserted {
		t.Errorf("resets = %v, inserted = %v, want the host left alone", state.Resets, state.Inserted)
	}
}
