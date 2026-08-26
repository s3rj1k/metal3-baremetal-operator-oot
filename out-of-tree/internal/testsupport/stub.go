// SPDX-License-Identifier: Apache-2.0

// Package testsupport holds the fixtures and Redfish stubs shared by more than
// one package, in one place so the stubs and the assertions cannot drift apart.
package testsupport

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"
)

// The one host the tests drive.
const (
	Host   = "node-1"
	UID    = "bmh-uid"
	MAC    = "aa:bb:cc:dd:ee:01"
	Secret = "node-1-ks"
	Disk   = "vda"
)

// BMC credentials the stub services demand, proving they reach the builtin.
const (
	User = "admin"
	Pass = "s3cret"
)

// Redfish paths the stub services answer on.
const (
	Endpoint    = "https://bmc.example"
	RootPath    = "/redfish/v1/"
	SystemsPath = "/redfish/v1/Systems"
	SystemPath  = "/redfish/v1/Systems/1"
	storagePath = SystemPath + "/Storage/RAID.1"
	drivePath   = storagePath + "/Drives/vda"
)

// ISO is the image the provision tests deploy.
const ISO = "http://boot.example/ks.iso"

// Strings the assertions look for.
const (
	Failure        = "disk sda not found"
	UnsupportedErr = "does not support"
)

// inspectDocs is the Redfish tree the collector walks, keyed by request path.
var inspectDocs = map[string]string{
	RootPath: `{"@odata.id":"/redfish/v1/","Id":"RootService","Name":"Root Service",
		"RedfishVersion":"1.6.0","Systems":{"@odata.id":"/redfish/v1/Systems"}}`,
	SystemsPath: `{"Members@odata.count":1,
		"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`,
	SystemPath: `{"@odata.id":"/redfish/v1/Systems/1","Id":"1","Name":"System",
		"Manufacturer":"Contoso","Model":"PowerServe R720","SerialNumber":"SN0123456789",
		"BiosVersion":"2.14.1","PowerState":"On","Status":{"Health":"OK"},
		"MemorySummary":{"TotalSystemMemoryGiB":128},
		"ProcessorSummary":{"Count":2,"LogicalProcessorCount":64},
		"Processors":{"@odata.id":"/redfish/v1/Systems/1/Processors"},
		"EthernetInterfaces":{"@odata.id":"/redfish/v1/Systems/1/EthernetInterfaces"},
		"Storage":{"@odata.id":"/redfish/v1/Systems/1/Storage"}}`,
	"/redfish/v1/Systems/1/Processors": `{"Members":[
		{"@odata.id":"/redfish/v1/Systems/1/Processors/CPU.1"}]}`,
	"/redfish/v1/Systems/1/Processors/CPU.1": `{"Id":"CPU.1","Name":"Processor 1",
		"InstructionSet":"x86-64","Model":"Contoso Xeon 6338 32C"}`,
	"/redfish/v1/Systems/1/EthernetInterfaces": `{"Members":[
		{"@odata.id":"/redfish/v1/Systems/1/EthernetInterfaces/NIC.1"}]}`,
	"/redfish/v1/Systems/1/EthernetInterfaces/NIC.1": `{"Id":"NIC.1",
		"Name":"Integrated NIC 1 Port 1","MACAddress":"aa:bb:cc:dd:ee:01"}`,
	"/redfish/v1/Systems/1/Storage": `{"Members":[
		{"@odata.id":"/redfish/v1/Systems/1/Storage/RAID.1"}]}`,
	"/redfish/v1/Systems/1/Storage/RAID.1": `{"Id":"RAID.1","Name":"Storage Controller",
		"Drives":[{"@odata.id":"/redfish/v1/Systems/1/Storage/RAID.1/Drives/Disk.0"}]}`,
	"/redfish/v1/Systems/1/Storage/RAID.1/Drives/Disk.0": `{"Id":"Disk.0","Name":"Disk 0",
		"CapacityBytes":960197124096,"MediaType":"SSD","Protocol":"NVMe",
		"Model":"Contoso NVMe 960G","Manufacturer":"Contoso","SerialNumber":"S3W1NA0M700001",
		"Identifiers":[{"DurableName":"naa.5000c500a1b2c3d4","DurableNameFormat":"NAA"}]}`,
}

// RedfishService serves inspectDocs, requiring credentials everywhere except the
// service root, which DSP0266 and the collector both expect to be anonymous.
func RedfishService(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RootPath {
			user, pass, ok := r.BasicAuth()
			if !ok || user != User || pass != Pass {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}
		}

		body, ok := inspectDocs[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

// LiveISOState is the BMC state a live ISO deploy actually changes, what the
// drive holds, whether the machine runs, and the boot override it was handed.
type LiveISOState struct {
	Boot  map[string]any
	Image string
	// RewriteImage stands for a BMC that fetches the image itself and reports
	// back its own URL rather than the one it was handed.
	RewriteImage string
	Resets       []string
	// Erased records the drives cleaning sanitized, EraseFails stands for a BMC
	// that will not do a cryptographic erase.
	Erased []string
	Ejects int
	mu     sync.Mutex
	// DropInsert and DropBoot accept the write and change nothing, which is how
	// an emulator that never reaches the hypervisor behaves.
	DropInsert bool
	DropBoot   bool
	EraseFails bool
	PowerOn    bool
	Inserted   bool
}

// bootJSON renders the Boot object a compliant BMC echoes back, empty when the
// stub is pretending to have ignored the override.
func (s *LiveISOState) bootJSON() string {
	target, _ := s.Boot["BootSourceOverrideTarget"].(string)
	if s.DropBoot {
		target = ""
	}

	return fmt.Sprintf(`"Boot":{"BootSourceOverrideTarget":%q},`, target)
}

// powerStateName renders the Redfish PowerState for the stub's current state.
func powerStateName(on bool) string {
	if on {
		return "On"
	}

	return "Off"
}

// LiveISOService serves a Redfish tree whose virtual media and power state
// mutate, which the static inspection stub cannot express.
func LiveISOService(t *testing.T, state *LiveISOState) *httptest.Server {
	t.Helper()

	const (
		mediaPath  = SystemPath + "/VirtualMedia/CD"
		resetPath  = SystemPath + "/Actions/ComputerSystem.Reset"
		insertPath = mediaPath + "/Actions/VirtualMedia.InsertMedia"
		ejectPath  = mediaPath + "/Actions/VirtualMedia.EjectMedia"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RootPath {
			user, pass, ok := r.BasicAuth()
			if !ok || user != User || pass != Pass {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}
		}

		state.mu.Lock()
		defer state.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		switch r.Method + " " + r.URL.Path {
		case "GET /redfish/v1/":
			// No Managers link, so virtual media is discovered off the system,
			// which is the fallback RedfishVirtualMedia takes.
			_, _ = io.WriteString(w, `{"@odata.id":"/redfish/v1/","Id":"RootService",
				"RedfishVersion":"1.6.0","Systems":{"@odata.id":"/redfish/v1/Systems"}}`)

		case "GET /redfish/v1/Systems":
			_, _ = fmt.Fprintf(w, `{"Members@odata.count":1,"Members":[{"@odata.id":%q}]}`, SystemPath)

		case "GET " + SystemPath:
			_, _ = fmt.Fprintf(w, `{"@odata.id":%q,"Id":"1","Name":"System","PowerState":%q,
				"Status":{"Health":"OK"},"VirtualMedia":{"@odata.id":%q},%s
				"Storage":{"@odata.id":%q},
				"Actions":{"#ComputerSystem.Reset":{"target":%q}}}`,
				SystemPath, powerStateName(state.PowerOn), SystemPath+"/VirtualMedia",
				state.bootJSON(), SystemPath+"/Storage", resetPath)

		case "GET " + SystemPath + "/Storage":
			_, _ = fmt.Fprintf(w, `{"Members":[{"@odata.id":%q}]}`, storagePath)

		case "GET " + storagePath:
			_, _ = fmt.Fprintf(w, `{"@odata.id":%q,"Id":"RAID.1",
				"Drives":[{"@odata.id":%q},{"@odata.id":%q}]}`, storagePath, drivePath, drivePath+"b")

		case "GET " + drivePath, "GET " + drivePath + "b":
			//nolint:gosec // a stub echoing its own request path back as JSON, never rendered as markup
			_, _ = fmt.Fprintf(w, `{"@odata.id":%q,"Id":%q,
				"Actions":{"#Drive.SecureErase":{"target":%q}}}`,
				r.URL.Path, path.Base(r.URL.Path), r.URL.Path+"/Actions/erase")

		case "POST " + drivePath + "/Actions/erase", "POST " + drivePath + "b/Actions/erase":
			if state.EraseFails {
				w.WriteHeader(http.StatusNotImplemented)

				return
			}

			state.Erased = append(state.Erased, r.URL.Path)

			w.WriteHeader(http.StatusNoContent)

		case "PATCH " + SystemPath:
			var payload struct {
				Boot map[string]any
			}

			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			state.Boot = payload.Boot

			w.WriteHeader(http.StatusNoContent)

		case "POST " + resetPath:
			var payload struct {
				ResetType string
			}

			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			state.Resets = append(state.Resets, payload.ResetType)
			state.PowerOn = payload.ResetType == "On"

			w.WriteHeader(http.StatusNoContent)

		case "GET " + SystemPath + "/VirtualMedia":
			_, _ = fmt.Fprintf(w, `{"Members@odata.count":1,"Members":[{"@odata.id":%q}]}`, mediaPath)

		case "GET " + mediaPath:
			_, _ = fmt.Fprintf(w, `{"@odata.id":%q,"Id":"CD","MediaTypes":["CD","DVD"],
				"Inserted":%t,"Image":%q,
				"Actions":{"#VirtualMedia.InsertMedia":{"target":%q},
					"#VirtualMedia.EjectMedia":{"target":%q}}}`,
				mediaPath, state.Inserted, state.Image, insertPath, ejectPath)

		case "POST " + insertPath:
			var payload struct {
				Image string
			}

			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			// Accepted either way, the drive only changes when the stub is not
			// standing in for a BMC that never reaches its hypervisor.
			if !state.DropInsert {
				state.Inserted = true
				state.Image = cmp.Or(state.RewriteImage, payload.Image)
			}

			w.WriteHeader(http.StatusNoContent)

		case "POST " + ejectPath:
			state.Inserted = false
			state.Image = ""
			state.Ejects++

			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}
