// SPDX-License-Identifier: Apache-2.0

// Out of band inventory collection, shaped to metal3api.HardwareDetails. Every
// section is best effort, a partial record still lets the host leave Inspecting.

package redfish

import (
	"cmp"
	"context"
	"fmt"
	"math"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	gofish "github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"
)

// MebibytesPerGibibyte converts the summary GiB value to the MiB field
// HardwareDetails wants.
const MebibytesPerGibibyte = 1024

// Numeric constrains the kinds gofish uses for optional sizes and counts,
// which is int for capacities, uint for counts and float64 for memory.
type Numeric interface {
	~int | ~uint | ~float64
}

// Positive returns the pointer value when it is non nil and above zero.
func Positive[T Numeric](p *T) (T, bool) {
	if p != nil && *p > 0 {
		return *p, true
	}

	var zero T

	return zero, false
}

// SystemVendor reports the manufacturer, product, and serial of the system.
func SystemVendor(sys *schemas.ComputerSystem) metal3api.HardwareSystemVendor {
	return metal3api.HardwareSystemVendor{
		Manufacturer: sys.Manufacturer,
		ProductName:  sys.Model,
		SerialNumber: sys.SerialNumber,
	}
}

// MemoryMiB reports total memory from the summary, or the sum of the modules
// when the summary is absent.
func MemoryMiB(sys *schemas.ComputerSystem) int {
	if g, ok := Positive(sys.MemorySummary.TotalSystemMemoryGiB); ok {
		return int(g * MebibytesPerGibibyte)
	}

	mods, err := sys.Memory()
	if err != nil {
		return 0
	}

	total := 0

	for _, m := range mods {
		if m == nil {
			continue
		}

		if v, ok := Positive(m.CapacityMiB); ok {
			total += v
		}
	}

	return total
}

// CPUArch maps a Redfish instruction set to the arch string Ironic uses, so a
// host inspected here is described the same way as one inspected in band.
func CPUArch(is schemas.InstructionSet) string {
	switch is {
	case schemas.X8664InstructionSet:
		return "x86_64"
	case schemas.ARMA64InstructionSet:
		return "aarch64"
	default:
		return string(is)
	}
}

// CPUDetails reports processor count, arch, and model best effort.
func CPUDetails(sys *schemas.ComputerSystem) metal3api.CPU {
	var cpu metal3api.CPU

	if c, ok := Positive(sys.ProcessorSummary.LogicalProcessorCount); ok {
		cpu.Count = int(c)
	} else if c, ok := Positive(sys.ProcessorSummary.Count); ok {
		cpu.Count = int(c)
	}

	procs, err := sys.Processors()
	if err != nil {
		return cpu
	}

	for _, p := range procs {
		if p == nil {
			continue
		}

		cpu.Arch = CPUArch(p.InstructionSet)
		cpu.Model = p.Model

		break
	}

	return cpu
}

// NICs returns one entry per interface that exposes a MAC address.
func NICs(sys *schemas.ComputerSystem) []metal3api.NIC {
	ifaces, err := sys.EthernetInterfaces()
	if err != nil {
		return nil
	}

	var out []metal3api.NIC

	for _, ni := range ifaces {
		if ni == nil {
			continue
		}

		mac := cmp.Or(ni.MACAddress, ni.PermanentMACAddress)
		if mac == "" {
			continue
		}

		out = append(out, metal3api.NIC{
			Name: cmp.Or(ni.Name, ni.ID),
			MAC:  mac,
		})
	}

	return out
}

// SimpleStorage reports devices from the legacy SimpleStorage resource, which
// is all some BMCs expose.
func SimpleStorage(sys *schemas.ComputerSystem) []metal3api.Storage {
	simples, err := sys.SimpleStorage()
	if err != nil {
		return nil
	}

	var out []metal3api.Storage

	for _, s := range simples {
		if s == nil {
			continue
		}

		for i := range s.Devices {
			dev := &s.Devices[i]
			entry := metal3api.Storage{
				Name:   dev.Name,
				Model:  dev.Model,
				Vendor: dev.Manufacturer,
			}

			// Capacity is signed, so a BMC reporting more than MaxInt64 would
			// otherwise wrap to a negative size.
			if v, ok := Positive(dev.CapacityBytes); ok && v <= math.MaxInt64 {
				entry.SizeBytes = metal3api.Capacity(v)
			}

			// SimpleStorage lists empty bays too, and a bay with no name, model
			// or size is a slot rather than a disk.
			if entry.Name == "" && entry.Model == "" && entry.Vendor == "" && entry.SizeBytes == 0 {
				continue
			}

			out = append(out, entry)
		}
	}

	return out
}

// DriveWWN returns the drive WWN from its NAA durable name when present.
func DriveWWN(d *schemas.Drive) string {
	for _, id := range d.Identifiers {
		if id.DurableNameFormat == schemas.NAADurableNameFormat && id.DurableName != "" {
			return id.DurableName
		}
	}

	return ""
}

// DriveType maps the Redfish media type and protocol to a metal3api DiskType.
func DriveType(d *schemas.Drive) metal3api.DiskType {
	if d.Protocol == schemas.NVMeProtocol {
		return metal3api.NVME
	}

	switch d.MediaType {
	case schemas.HDDMediaType, schemas.SMRMediaType:
		return metal3api.HDD
	case schemas.SSDMediaType:
		return metal3api.SSD
	default:
		return ""
	}
}

// Drive maps one Redfish drive to a HardwareDetails storage entry.
func Drive(d *schemas.Drive) metal3api.Storage {
	s := metal3api.Storage{
		Name:         cmp.Or(d.Name, d.ID),
		Rotational:   d.MediaType == schemas.HDDMediaType || d.MediaType == schemas.SMRMediaType,
		Type:         DriveType(d),
		Model:        d.Model,
		Vendor:       d.Manufacturer,
		SerialNumber: d.SerialNumber,
		WWN:          DriveWWN(d),
	}

	if v, ok := Positive(d.CapacityBytes); ok {
		s.SizeBytes = metal3api.Capacity(v)
	}

	return s
}

// Drives collects the drives behind every storage controller, which inventory
// reports on and cleaning erases.
func Drives(sys *schemas.ComputerSystem) []*schemas.Drive {
	var out []*schemas.Drive

	storages, err := sys.Storage()
	if err != nil {
		return nil
	}

	for _, s := range storages {
		if s == nil {
			continue
		}

		drives, derr := s.Drives()
		if derr != nil {
			continue
		}

		for _, d := range drives {
			if d != nil {
				out = append(out, d)
			}
		}
	}

	return out
}

// Storage reports drives from the Storage resources, falling back to the legacy
// SimpleStorage collection.
func Storage(sys *schemas.ComputerSystem) []metal3api.Storage {
	drives := Drives(sys)
	out := make([]metal3api.Storage, 0, len(drives))

	for _, d := range drives {
		out = append(out, Drive(d))
	}

	if len(out) > 0 {
		return out
	}

	return SimpleStorage(sys)
}

// Inventory collects whatever Redfish serves. Every section is best effort, so
// a BMC that answers only part of the tree still yields a usable record.
func (c Conn) Inventory(ctx context.Context) (*metal3api.HardwareDetails, error) {
	details := &metal3api.HardwareDetails{}

	err := c.WithClient(ctx, func(client *gofish.APIClient) error {
		sys, serr := c.System(client)
		if serr != nil {
			return serr
		}

		details.SystemVendor = SystemVendor(sys)
		details.Firmware = metal3api.Firmware{BIOS: metal3api.BIOS{Version: sys.BiosVersion}}
		details.RAMMebibytes = MemoryMiB(sys)
		details.CPU = CPUDetails(sys)
		details.NIC = NICs(sys)
		details.Storage = Storage(sys)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("redfish inventory: %w", err)
	}

	return details, nil
}

// Empty reports whether an inventory carries nothing an operator could use, which
// is how a BMC that answers every request with an empty document shows up.
func InventoryEmpty(d *metal3api.HardwareDetails) bool {
	return d == nil || (d.RAMMebibytes == 0 &&
		d.CPU.Count == 0 &&
		len(d.NIC) == 0 &&
		len(d.Storage) == 0 &&
		d.SystemVendor == (metal3api.HardwareSystemVendor{}))
}
