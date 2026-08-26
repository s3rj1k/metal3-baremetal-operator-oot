// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"metal3.local/anaconda/internal/core"
	"metal3.local/anaconda/internal/httpapi"
	"metal3.local/anaconda/internal/testsupport"
)

// ksRequest asks for a kickstart the way anaconda does, with the MAC headers and
// nothing else to go on.
func ksRequest(t *testing.T, srv *httpapi.PluginServer, macs ...string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, httpapi.KickstartPathPrefix+"kickstart", http.NoBody)
	for i, mac := range macs {
		req.Header.Set(fmt.Sprintf("X-RHN-Provisioning-MAC-%d", i), fmt.Sprintf("eth%d %s", i, mac))
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	return rec
}

// The host's own preprovisioning Secret is the kickstart, resolved at request
// time so a host is servable the moment it exists.
func TestKickstartServedFromThePreprovisioningSecret(t *testing.T) {
	stub := &stubResolver{
		uid: testsupport.UID,
		hosts: []core.HostRef{{
			Name:            testsupport.Host,
			Namespace:       "ns",
			UID:             testsupport.UID,
			BootMAC:         testsupport.MAC,
			KickstartSecret: testsupport.Secret,
		}},
		kickstart: map[string]string{testsupport.Secret: "text\nnetwork --hostname={{ .Name }}\n# {{ .CallbackURL }}\n"},
	}

	srv := &httpapi.PluginServer{
		Config:   core.Config{BaseURL: "http://bmo:8080"},
		Resolver: stub,
		Log:      logr.Discard(),
	}

	rec := ksRequest(t, srv, testsupport.MAC)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "--hostname=node-1") {
		t.Errorf("body = %q, want the host's rendered kickstart", body)
	}

	// The callback URL has to be rendered in, it is what ends the provision, and
	// the uid in the path is what keeps it unguessable.
	if !strings.Contains(body, "http://bmo:8080"+httpapi.CallbackPathPrefix+"bmh-uid/ns/node-1") {
		t.Errorf("body = %q, want the callback URL rendered in", body)
	}
}

// A host with no usable Secret gets the inert fallback, never a wipe. A 404 or
// an error would drop anaconda to an interactive prompt on a live machine.
func TestKickstartFallsBackWithoutAUsableSecret(t *testing.T) {
	cases := map[string]*stubResolver{
		"host names no secret": {
			hosts: []core.HostRef{{Name: testsupport.Host, Namespace: "ns", KickstartSecret: ""}},
		},
		"secret is absent": {
			hosts:     []core.HostRef{{Name: testsupport.Host, Namespace: "ns", KickstartSecret: "gone"}},
			kickstart: map[string]string{},
		},
		"no host declares the mac": {
			hosts: nil,
		},
	}

	for name, stub := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &httpapi.PluginServer{Config: core.Config{}, Resolver: stub, Log: logr.Discard()}

			rec := ksRequest(t, srv, testsupport.MAC)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 so anaconda does not sit at a prompt", rec.Code)
			}

			if rec.Body.String() != httpapi.DefaultFallbackKickstart {
				t.Errorf("body = %q, want the compiled in fallback", rec.Body.String())
			}
		})
	}
}

// The host's own hint beats the fleet default, so one odd machine is fixed on
// its BareMetalHost rather than in the operator's config.
func TestKickstartInstallDiskPrefersTheHostHint(t *testing.T) {
	const tmpl = "ignoredisk --only-use={{ .InstallDisk }}\n"

	cases := map[string]struct {
		hostDisk string
		cfgDisk  string
		want     string
	}{
		"host hint wins":    {hostDisk: "nvme0n1", cfgDisk: testsupport.Disk, want: "nvme0n1"},
		"config fills in":   {hostDisk: "", cfgDisk: testsupport.Disk, want: testsupport.Disk},
		"by-id link passes": {hostDisk: "disk/by-id/wwn-0x5000c500", want: "disk/by-id/wwn-0x5000c500"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &stubResolver{
				hosts: []core.HostRef{{
					Name: testsupport.Host, Namespace: "ns", BootMAC: testsupport.MAC,
					KickstartSecret: testsupport.Secret, InstallDisk: tc.hostDisk,
				}},
				kickstart: map[string]string{testsupport.Secret: tmpl},
			}

			srv := &httpapi.PluginServer{
				Config:   core.Config{InstallDisk: tc.cfgDisk},
				Resolver: stub,
				Log:      logr.Discard(),
			}

			body := ksRequest(t, srv, testsupport.MAC).Body.String()
			if body != "ignoredisk --only-use="+tc.want+"\n" {
				t.Errorf("body = %q, want the disk resolved to %q", body, tc.want)
			}
		})
	}
}

// An unresolved disk renders "ignoredisk --only-use=" with nothing after it,
// which anaconda rejects, dropping to a prompt on a machine nobody is watching.
func TestKickstartWithoutAnyInstallDiskFallsBack(t *testing.T) {
	stub := &stubResolver{
		hosts:     []core.HostRef{{Name: testsupport.Host, Namespace: "ns", KickstartSecret: testsupport.Secret}},
		kickstart: map[string]string{testsupport.Secret: "clearpart --all --drives={{ .InstallDisk }}\n"},
	}

	srv := &httpapi.PluginServer{Config: core.Config{}, Resolver: stub, Log: logr.Discard()}

	rec := ksRequest(t, srv, testsupport.MAC)
	if rec.Body.String() != httpapi.DefaultFallbackKickstart {
		t.Errorf("body = %q, want the fallback rather than a wipe with no disk named", rec.Body.String())
	}
}

// A kickstart naming its own disk predates the variable and has to keep working
// with no default configured, or every existing host starts powering itself off.
func TestKickstartWithAHardcodedDiskIsUnaffected(t *testing.T) {
	const tmpl = "ignoredisk --only-use=sdb\n"

	stub := &stubResolver{
		hosts:     []core.HostRef{{Name: testsupport.Host, Namespace: "ns", KickstartSecret: testsupport.Secret}},
		kickstart: map[string]string{testsupport.Secret: tmpl},
	}

	srv := &httpapi.PluginServer{Config: core.Config{}, Resolver: stub, Log: logr.Discard()}

	if body := ksRequest(t, srv, testsupport.MAC).Body.String(); body != tmpl {
		t.Errorf("body = %q, want the kickstart served unchanged", body)
	}
}

// Without inst.ks.sendmac there is nothing to match on, and guessing a host
// would install the wrong kickstart on a machine.
func TestKickstartWithoutMACHeadersFallsBack(t *testing.T) {
	stub := &stubResolver{hosts: []core.HostRef{{Name: testsupport.Host, Namespace: "ns", KickstartSecret: testsupport.Secret}}}
	srv := &httpapi.PluginServer{Config: core.Config{}, Resolver: stub, Log: logr.Discard()}

	rec := ksRequest(t, srv)
	if rec.Body.String() != httpapi.DefaultFallbackKickstart {
		t.Errorf("body = %q, want the fallback when no MACs were reported", rec.Body.String())
	}
}
