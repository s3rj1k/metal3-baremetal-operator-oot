// SPDX-License-Identifier: Apache-2.0

// The Provisioner interface over Redfish only. Anything needing an agent inside
// the host is reported unsupported rather than silently succeeding.

package anaconda

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
	"github.com/stmcginnis/gofish/schemas"

	"metal3.local/anaconda/internal/core"
	"metal3.local/anaconda/internal/redfish"
)

// LiveISO is the only metal3api.Image format this provisioner deploys.
const LiveISO = "live-iso"

// Requeue builds a dirty result asking to be called again after d.
func Requeue(d time.Duration) provisioner.Result {
	return provisioner.Result{Dirty: true, RequeueAfter: d}
}

// Requeue delays. Power and media transitions settle in seconds, so polling
// harder than this only adds BMC load.
const (
	PowerRequeueDelay   = 15 * time.Second
	InspectRequeueDelay = 30 * time.Second
)

// ConnAndPowerState opens the connection and reads where the machine currently
// is, which is the first thing every power decision needs.
func (p *Provisioner) ConnAndPowerState(ctx context.Context) (redfish.Conn, schemas.PowerState, error) {
	conn, err := p.Conn()
	if err != nil {
		return redfish.Conn{}, "", err
	}

	state, err := conn.PowerState(ctx)
	if err != nil {
		return redfish.Conn{}, "", err
	}

	return conn, state, nil
}

// PowerOn powers the system on over Redfish.
func (p *Provisioner) PowerOn(ctx context.Context, _ bool) (provisioner.Result, error) {
	conn, state, err := p.ConnAndPowerState(ctx)
	if err != nil {
		return provisioner.Result{}, err
	}

	if state == schemas.OnPowerState {
		return provisioner.Result{}, nil
	}

	if state == schemas.PoweringOnPowerState {
		return Requeue(PowerRequeueDelay), nil
	}

	p.Log.Info("powering on", "powerState", state)

	if err := conn.PowerOn(ctx); err != nil {
		return provisioner.Result{}, err
	}

	p.Publish("PowerOn", "Host powered on over Redfish")

	return Requeue(PowerRequeueDelay), nil
}

// PowerOff powers the system off, gracefully unless forced.
func (p *Provisioner) PowerOff(
	ctx context.Context,
	rebootMode metal3api.RebootMode,
	force bool,
	_ metal3api.AutomatedCleaningMode,
) (provisioner.Result, error) {
	// Runs before deletion, so refusing here would wedge the finalizer.
	if !redfish.UsableBMCAddress(p.HostData.BMCAddress) {
		p.Log.Error(nil, "no usable BMC address, treating the host as off")

		return provisioner.Result{}, nil
	}

	conn, state, err := p.ConnAndPowerState(ctx)
	if err != nil {
		return provisioner.Result{}, err
	}

	if state == schemas.OffPowerState {
		return provisioner.Result{}, nil
	}

	if state == schemas.PoweringOffPowerState {
		return Requeue(PowerRequeueDelay), nil
	}

	if rebootMode == metal3api.RebootModeSoft && !force {
		p.Log.Info("graceful shutdown", "powerState", state)

		if err := conn.PowerSoft(ctx); err != nil {
			return provisioner.Result{}, err
		}
	} else {
		p.Log.Info("forced power off", "powerState", state)

		if err := conn.PowerOff(ctx); err != nil {
			return provisioner.Result{}, err
		}
	}

	p.Publish("PowerOff", "Host powered off over Redfish")

	return Requeue(PowerRequeueDelay), nil
}

// BeginInstall gets the requested ISO into the drive and the machine running.
func (p *Provisioner) BeginInstall(
	ctx context.Context,
	conn redfish.Conn,
	media redfish.MediaStatus,
	data *provisioner.ProvisionData,
) (provisioner.Result, error) {
	url := data.Image.URL

	// The kickstart is served by matching the MACs anaconda reports, so a host
	// with none declared boots only to take the fallback and power itself off.
	if p.HostData.BootMACAddress == "" {
		return provisioner.Result{
			ErrorMessage: "provision: no boot MAC for " + p.Namespace() + "/" + p.Name() +
				", set spec.bootMACAddress to one of the NICs on status.hardwareDetails",
		}, nil
	}

	// Booting without one wastes an install, the machine comes up, takes the
	// fallback and powers itself off with nothing saying why.
	ks, err := p.Store.HasKickstart(ctx, p.Namespace(), p.Name())
	if err != nil {
		return provisioner.Result{}, err
	}

	if !ks {
		return provisioner.Result{
			ErrorMessage: "provision: no kickstart for " + p.Namespace() + "/" + p.Name() +
				", set spec.preprovisioningNetworkDataName to a Secret carrying a " + core.KickstartSecretKey + " key",
		}, nil
	}

	// clearpart runs against whatever disk is named, so the target is never
	// inferred. BMO would hand us the hardware profile guess of /dev/sda instead.
	disk, err := p.Store.HostInstallDisk(ctx, p.Namespace(), p.Name())
	if err != nil {
		return provisioner.Result{}, err
	}

	if disk == "" {
		return provisioner.Result{
			ErrorMessage: "provision: no root device hints for " + p.Namespace() + "/" + p.Name() +
				", set spec.rootDeviceHints to a deviceName, wwn or serialNumber",
		}, nil
	}

	// Swapping media under a running machine leaves it booted from the old
	// source, so the host goes down first.
	state, err := conn.PowerState(ctx)
	if err != nil {
		return provisioner.Result{}, err
	}

	if state != schemas.OffPowerState {
		if state == schemas.PoweringOffPowerState {
			return Requeue(PowerRequeueDelay), nil
		}

		p.Log.Info("powering off to insert the live ISO", "image", url)

		if err := conn.PowerOff(ctx); err != nil {
			return provisioner.Result{}, err
		}

		return Requeue(PowerRequeueDelay), nil
	}

	if media.Inserted {
		// A BMC that echoes back a rewritten URL never matches the request and
		// would land here every pass, so the swap is logged rather than silent.
		p.Log.Info("ejecting media that is not the requested image", "attached", media.Image, "wanted", url)

		if err := conn.EjectMedia(ctx); err != nil {
			return provisioner.Result{}, err
		}
	}

	p.Log.Info("inserting live ISO", "image", url)

	if err := conn.InsertMedia(ctx, url); err != nil {
		return provisioner.Result{}, err
	}

	// A BMC can accept the insert and attach nothing, which boots the host off
	// its disk and leaves the install waiting on a callback that cannot come.
	inserted, err := conn.MediaStatus(ctx)
	if err != nil {
		return provisioner.Result{}, err
	}

	if !inserted.Inserted {
		return provisioner.Result{
			ErrorMessage: "provision: the BMC accepted " + url +
				" and reports the virtual media drive empty, so the host would boot from disk",
		}, nil
	}

	// Only worth a log, since a rewritten URL is how some BMCs report an image
	// they fetched themselves.
	if inserted.Image != url {
		p.Log.Info("BMC reports a different image than the one inserted", "attached", inserted.Image, "wanted", url)
	}

	// A one time override, so the reboot ending the install lands on disk rather
	// than starting the installer over again.
	uefi := data.BootMode == metal3api.UEFI
	if err := conn.SetBoot(ctx, schemas.CdBootSource, schemas.OnceBootSourceOverrideEnabled, uefi); err != nil {
		return provisioner.Result{}, err
	}

	// Same again for the override. Accepting the write and booting the disk
	// anyway is the failure this whole sequence exists to catch.
	src, err := conn.BootSource(ctx)
	if err != nil {
		return provisioner.Result{}, err
	}

	if src != schemas.CdBootSource {
		return provisioner.Result{
			ErrorMessage: "provision: the BMC accepted the Cd boot override and reports " +
				strconv.Quote(string(src)) + ", so the host would boot from disk",
		}, nil
	}

	// Without this a reinstall reads the previous run's report on its first pass
	// and reports provisioned before anaconda has booted.
	if err := p.Store.ClearInstallReport(ctx, p.Namespace(), p.Name()); err != nil {
		return provisioner.Result{}, err
	}

	if err := conn.PowerOn(ctx); err != nil {
		return provisioner.Result{}, err
	}

	p.Publish("ProvisioningStarted", "Booting live ISO "+url)

	return Requeue(PowerRequeueDelay), nil
}

// SyntheticDetails is the minimum HardwareDetails that lets a host leave
// Inspecting, the configured boot MAC and the host name.
func (p *Provisioner) SyntheticDetails() *metal3api.HardwareDetails {
	return &metal3api.HardwareDetails{
		Hostname: p.Name(),
		NIC: []metal3api.NIC{{
			Name: "boot",
			MAC:  p.HostData.BootMACAddress,
			PXE:  true,
		}},
	}
}

// ReportBootMACCandidates names the MACs inspection found when the host declares
// no boot MAC, since nothing can be provisioned until an operator picks one.
func (p *Provisioner) ReportBootMACCandidates(details *metal3api.HardwareDetails) {
	if p.HostData.BootMACAddress != "" || len(details.NIC) == 0 {
		return
	}

	macs := make([]string, 0, len(details.NIC))
	for i := range details.NIC {
		macs = append(macs, details.NIC[i].MAC)
	}

	found := strings.Join(macs, ", ")

	p.Log.Info("host declares no boot MAC, provisioning waits until one is set", "candidates", found)
	p.Publish("BootMACRequired", "Set spec.bootMACAddress to one of "+found)
}

// InspectHardware reads the BMC unless an inventory is already recorded.
// Returning nil details would loop the host in Inspecting forever.
func (p *Provisioner) InspectHardware(
	ctx context.Context,
	_ provisioner.InspectData,
	_, refresh, _ bool,
) (provisioner.Result, bool, *metal3api.HardwareDetails, error) {
	// BMO drops the refresh annotation only when a provisioner reports inspection
	// started, and re-enters inspecting forever while the annotation is there.
	if refresh {
		p.Log.Info("acknowledging an inspection refresh request")

		return provisioner.Result{}, true, nil, nil
	}

	// The CR is what BMO rewrites on every inspection, while the copy on the
	// status is one the plugin itself overwrites and can be stale.
	details, err := p.Store.HostHardwareData(ctx, p.Namespace(), p.Name())
	if err != nil {
		return provisioner.Result{}, false, nil, err
	}

	if details != nil {
		p.Log.Info("ingesting the hardware data recorded for this host", "nics", len(details.NIC))
		p.ReportBootMACCandidates(details)

		return provisioner.Result{}, false, details, nil
	}

	// The boot MAC resolves the kickstart and the install disk is what gets
	// wiped, so a host declaring both needs nothing an inventory could add.
	disk, err := p.Store.HostInstallDisk(ctx, p.Namespace(), p.Name())
	if err != nil {
		return provisioner.Result{}, false, nil, err
	}

	if mac := p.HostData.BootMACAddress; mac != "" && disk != "" {
		p.Log.Info("skipping inspection, the host declares its boot MAC and install disk", "mac", mac, "disk", disk)
		p.Publish("InspectionSkipped", "Boot MAC and install disk are known, no out of band inspection needed")

		return provisioner.Result{}, false, p.SyntheticDetails(), nil
	}

	conn, err := p.Conn()
	if err != nil {
		return provisioner.Result{}, false, nil, err
	}

	details, err = conn.Inventory(ctx)
	if err != nil {
		return provisioner.Result{}, false, nil, err
	}

	if redfish.InventoryEmpty(details) {
		// Requeueing forever is the only other option, and a host that declares
		// its boot MAC already carries the one datum provisioning needs.
		if p.HostData.BootMACAddress == "" {
			return Requeue(InspectRequeueDelay), false, nil, nil
		}

		p.Log.Info("BMC served no inventory, falling back to the declared boot MAC")
		p.Publish("InspectionIncomplete", "BMC served no inventory, recorded the declared boot MAC")

		return provisioner.Result{}, false, p.SyntheticDetails(), nil
	}

	p.Log.Info("collected out of band inventory",
		"nics", len(details.NIC), "disks", len(details.Storage), "ramMebibytes", details.RAMMebibytes)
	p.Publish("InspectionComplete", "Out of band inspection completed over Redfish")
	p.ReportBootMACCandidates(details)

	return provisioner.Result{}, false, details, nil
}

// Unsupported builds the error result for an out of scope request. An error
// rather than a silent success, so no host sits Provisioned on nothing.
func Unsupported(method, detail string) provisioner.Result {
	return provisioner.Result{
		ErrorMessage: method + ": " + detail + " is not supported by the anaconda provisioner",
	}
}

// Prepare reports nothing to prepare, and rejects RAID which needs an agent.
func (*Provisioner) Prepare(
	_ context.Context,
	data provisioner.PrepareData,
	_, _ bool,
) (provisioner.Result, bool, error) {
	for _, cfg := range []*metal3api.RAIDConfig{data.TargetRAIDConfig, data.ActualRAIDConfig} {
		if cfg == nil {
			continue
		}

		if len(cfg.HardwareRAIDVolumes) > 0 || len(cfg.SoftwareRAIDVolumes) > 0 {
			return Unsupported("prepare", "RAID configuration"), false, nil
		}
	}

	return provisioner.Result{}, false, nil
}

// IsLiveISO reports whether an image asks for the one format this deploys.
func IsLiveISO(img *metal3api.Image) bool {
	return img != nil && img.DiskFormat != nil && *img.DiskFormat == LiveISO
}

// Register validates the BMC address, probes Redfish once, and claims the host
// by UID. The probe is skipped in steady state so a reconcile costs no BMC call.
func (p *Provisioner) Register(
	ctx context.Context,
	_ provisioner.ManagementAccessData, //nolint:gocritic // the Provisioner interface fixes this signature
	credentialsChanged, _ bool,
) (provisioner.Result, string, error) {
	conn, err := p.Conn()
	if err != nil {
		return provisioner.Result{}, "", err
	}

	provID := p.HostData.ProvisionerID
	if provID == "" {
		provID = string(p.HostData.ObjectMeta.UID)
	}

	if provID == "" {
		return Requeue(PowerRequeueDelay), "", errors.New("register: BareMetalHost has no UID to claim")
	}

	if p.HostData.ProvisionerID == "" || credentialsChanged {
		info, serr := conn.SystemInfo(ctx)
		if serr != nil {
			return provisioner.Result{}, "", serr
		}

		p.Log.Info("registered over Redfish",
			"endpoint", conn.Endpoint, "manufacturer", info.Manufacturer, "model", info.Model)
		p.Publish("Registered", "Registered host over Redfish")
	}

	return provisioner.Result{}, provID, nil
}

// HasCapacity reports capacity, out of band work contends for nothing.
func (*Provisioner) HasCapacity(_ context.Context) (bool, error) {
	return true, nil
}

// PreprovisioningImageFormats reports none, nothing here boots a ramdisk.
func (*Provisioner) PreprovisioningImageFormats(_ context.Context) ([]metal3api.ImageFormat, error) {
	return nil, nil
}

// UpdateHardwareState reads the power state over Redfish.
func (p *Provisioner) UpdateHardwareState(ctx context.Context) (provisioner.HardwareState, error) {
	conn, err := p.Conn()
	if err != nil {
		return provisioner.HardwareState{}, err
	}

	state, err := conn.PowerState(ctx)
	if err != nil {
		return provisioner.HardwareState{}, err
	}

	on := state == schemas.OnPowerState

	return provisioner.HardwareState{PoweredOn: &on}, nil
}

// Adopt takes the host back, nothing is held out of band so this succeeds.
func (*Provisioner) Adopt(_ context.Context, _ provisioner.AdoptData, _ bool) (provisioner.Result, error) {
	return provisioner.Result{}, nil
}

// Service reports nothing to service, servicing needs an agent.
func (*Provisioner) Service(
	_ context.Context,
	_ provisioner.ServicingData,
	_, _ bool,
) (provisioner.Result, bool, error) {
	return provisioner.Result{}, false, nil
}

// FinishInstall lands a reported host on its own disk. The kickstart issues no
// power command, so taking the machine down is this provisioner's job.
func (p *Provisioner) FinishInstall(ctx context.Context, conn redfish.Conn, uefi bool) (provisioner.Result, error) {
	state, err := conn.PowerState(ctx)
	if err != nil {
		return provisioner.Result{}, err
	}

	if state != schemas.OffPowerState {
		return p.ShutDownAfterInstall(ctx, conn, state)
	}

	// Only now. Anaconda reads the installer image while it exits, so a drive
	// pulled out from under a running host aborts its shutdown partway.
	p.Log.Info("host is down, ejecting media")

	if err := conn.EjectMedia(ctx); err != nil {
		return provisioner.Result{}, err
	}

	// The one time override is spent by now, so this states the boot order
	// rather than trusting that the BIOS one lists the disk.
	if err := conn.SetBoot(ctx, schemas.HddBootSource, schemas.ContinuousBootSourceOverrideEnabled, uefi); err != nil {
		return provisioner.Result{}, err
	}

	if err := conn.PowerOn(ctx); err != nil {
		return provisioner.Result{}, err
	}

	p.Publish("ProvisioningComplete", "Anaconda reported the install finished")

	return provisioner.Result{}, nil
}

// ShutDownAfterInstall asks the installed host to go down gracefully, so
// systemd unmounts the target rather than losing whatever is still buffered.
func (p *Provisioner) ShutDownAfterInstall(ctx context.Context, conn redfish.Conn, state schemas.PowerState) (provisioner.Result, error) {
	if state == schemas.PoweringOffPowerState {
		return Requeue(PowerRequeueDelay), nil
	}

	started, err := p.Store.InstallStartedAt(ctx, p.Namespace(), p.Name())
	if err != nil {
		return provisioner.Result{}, err
	}

	// A host that ignores the request would hold provisioning open for good, so
	// it is cut once the install has had its whole budget.
	if !started.IsZero() && time.Since(started) > p.Cfg.InstallTimeout {
		p.Log.Info("host never went down after the install, forcing it off", "powerState", state)

		if err := conn.PowerOff(ctx); err != nil {
			return provisioner.Result{}, err
		}

		return Requeue(PowerRequeueDelay), nil
	}

	p.Log.Info("install reported complete, shutting the host down", "powerState", state)

	if err := conn.PowerSoft(ctx); err != nil {
		return provisioner.Result{}, err
	}

	return Requeue(PowerRequeueDelay), nil
}

// AwaitInstall holds the host in Provisioning until anaconda reports in, then
// takes it down and boots it onto its disk.
func (p *Provisioner) AwaitInstall(ctx context.Context, conn redfish.Conn, data *provisioner.ProvisionData) (provisioner.Result, error) {
	url := data.Image.URL

	// Without a listener nothing can ever report completion, so waiting would
	// hang the host until the timeout for no reason.
	if !p.CallbackEnabled {
		p.Log.Info("no callback listener, treating a booted ISO as provisioned", "image", url)
		p.Publish("ProvisioningComplete", "Booted live ISO "+url)

		return provisioner.Result{}, nil
	}

	report, err := p.Store.ReadInstallReport(ctx, p.Namespace(), p.Name())
	if err != nil {
		return provisioner.Result{}, err
	}

	if report != nil {
		if !report.Succeeded {
			// No eject on purpose, the drive stays as anaconda left it and the
			// spent boot override means a power cycle will not reinstall.

			// The verdict is sticky, every later pass reads the same annotation.
			// Recovery is a deprovision or a new image URL, both clear it.
			p.Log.Error(nil, "install reported failure", "detail", report.Message)

			return provisioner.Result{
				ErrorMessage: "provision: anaconda reported the install failed: " + report.Message,
			}, nil
		}

		return p.FinishInstall(ctx, conn, data.BootMode == metal3api.UEFI)
	}

	started, err := p.Store.InstallStartedAt(ctx, p.Namespace(), p.Name())
	if err != nil {
		return provisioner.Result{}, err
	}

	// BMO stamps this on the transition into provisioning, so a zero value means
	// the status has not been observed yet. Wait rather than time out on nothing.
	if started.IsZero() {
		return Requeue(core.InstallPollInterval), nil
	}

	if elapsed := time.Since(started); elapsed > p.Cfg.InstallTimeout {
		return provisioner.Result{
			ErrorMessage: fmt.Sprintf("provision: anaconda did not report completion within %s", p.Cfg.InstallTimeout),
		}, nil
	}

	return Requeue(core.InstallPollInterval), nil
}

// Provision boots the installer ISO and waits for anaconda to report finished.
// One step per reconcile, because booted is not the same as installed.
func (p *Provisioner) Provision(
	ctx context.Context,
	data provisioner.ProvisionData, //nolint:gocritic // the Provisioner interface fixes this signature
	_ bool,
) (provisioner.Result, error) {
	if !IsLiveISO(&data.Image) {
		format := ""
		if data.Image.DiskFormat != nil {
			format = *data.Image.DiskFormat
		}

		p.Log.Error(nil, "unsupported image format", "format", format, "url", data.Image.URL)

		return Unsupported("provision", "deploying a "+strconv.Quote(format)+" image"), nil
	}

	if data.Image.URL == "" {
		return provisioner.Result{ErrorMessage: "provision: live-iso image has no url"}, nil
	}

	conn, err := p.Conn()
	if err != nil {
		return provisioner.Result{}, err
	}

	media, err := conn.MediaStatus(ctx)
	if err != nil {
		return provisioner.Result{}, err
	}

	if media.Inserted && media.Image == data.Image.URL {
		return p.AwaitInstall(ctx, conn, &data)
	}

	return p.BeginInstall(ctx, conn, media, &data)
}

// Deprovision ejects the media and forgets the install. It tolerates a broken
// BMC address, refusing here would keep the finalizer and block deletion.
func (p *Provisioner) Deprovision(ctx context.Context, _ bool, mode metal3api.AutomatedCleaningMode) (provisioner.Result, error) {
	// Teardown must finish even if the reconcile is cut short, but the parent
	// still carries values worth keeping.
	ctx = context.WithoutCancel(ctx)

	if err := p.Store.ClearInstallReport(ctx, p.Namespace(), p.Name()); err != nil {
		p.Log.Error(err, "clearing install state failed")
	}

	if !redfish.UsableBMCAddress(p.HostData.BMCAddress) {
		p.Log.Error(nil, "BMC address is unusable, leaving virtual media alone")

		return provisioner.Result{}, nil
	}

	conn, err := p.Conn()
	if err != nil {
		return provisioner.Result{}, nil //nolint:nilerr // teardown must not wedge the finalizer
	}

	// Matched rather than negated, so only the one documented value wipes a disk
	// and an empty or unknown mode leaves the data alone.
	if mode == metal3api.CleaningModeMetadata {
		p.Log.Info("erasing disks", "mode", mode)

		if eraseErr := conn.SecureEraseDrives(ctx); eraseErr != nil {
			p.Log.Error(eraseErr, "secure erase failed, the disks may still hold data")
			p.Publish("SecureEraseFailed", eraseErr.Error())
		}
	}

	media, err := conn.MediaStatus(ctx)
	if err != nil || !media.Inserted {
		return provisioner.Result{}, nil //nolint:nilerr // nothing to eject, or the BMC is gone
	}

	p.Log.Info("ejecting virtual media")

	if err := conn.EjectMedia(ctx); err != nil {
		return provisioner.Result{}, err
	}

	p.Publish("DeprovisioningComplete", "Ejected virtual media over Redfish")

	return provisioner.Result{}, nil
}

// Delete releases the host. Nothing external holds a record of it.
func (*Provisioner) Delete(_ context.Context) (provisioner.Result, error) {
	return provisioner.Result{}, nil
}

// Detach is the same as delete for this provisioner.
func (*Provisioner) Detach(_ context.Context, _ bool) (provisioner.Result, error) {
	return provisioner.Result{}, nil
}

// GetFirmwareSettings reports none, reading BIOS attributes is out of scope.
func (*Provisioner) GetFirmwareSettings(
	_ context.Context,
	_ bool,
) (metal3api.SettingsMap, map[string]metal3api.SettingSchema, error) {
	return nil, nil, nil
}

// AddBMCEventSubscriptionForNode rejects subscriptions, no event API is exposed.
func (*Provisioner) AddBMCEventSubscriptionForNode(
	_ context.Context,
	_ *metal3api.BMCEventSubscription,
	_ provisioner.HTTPHeaders,
) (provisioner.Result, error) {
	result := Unsupported("add_bmc_event_subscription", "BMC event subscriptions")

	// The subscription controller discards the Result, so a failure has to be an error.
	return result, errors.New(result.ErrorMessage)
}

// RemoveBMCEventSubscriptionForNode reports the subscription gone, none existed.
func (*Provisioner) RemoveBMCEventSubscriptionForNode(
	_ context.Context,
	_ metal3api.BMCEventSubscription, //nolint:gocritic // the Provisioner interface fixes this signature
) (provisioner.Result, error) {
	return provisioner.Result{}, nil
}

// GetFirmwareComponents reports none. Nothing here can flash firmware, and BMO
// runs this every reconcile, so answering costs a BMC round trip for nothing.
func (*Provisioner) GetFirmwareComponents(_ context.Context) ([]metal3api.FirmwareComponentStatus, error) {
	return nil, nil
}

// GetDataImageStatus reports whether virtual media currently holds an image.
func (p *Provisioner) GetDataImageStatus(ctx context.Context) (bool, error) {
	// The DataImage controller builds HostData with no BMC, and refusing there
	// would strand the CR forever.
	if !redfish.UsableBMCAddress(p.HostData.BMCAddress) {
		return false, nil
	}

	conn, err := p.Conn()
	if err != nil {
		return false, err
	}

	media, err := conn.MediaStatus(ctx)
	if err != nil {
		return false, err
	}

	return media.Inserted, nil
}

// AttachDataImage inserts a data image into virtual media. It shares the slot
// with a live-iso deploy, so attaching replaces whatever the host booted.
func (p *Provisioner) AttachDataImage(ctx context.Context, url string) error {
	conn, err := p.Conn()
	if err != nil {
		return err
	}

	p.Log.Info("attaching data image", "image", url)

	return conn.InsertMedia(ctx, url)
}

// DetachDataImage ejects the mounted data image.
func (p *Provisioner) DetachDataImage(ctx context.Context) error {
	conn, err := p.Conn()
	if err != nil {
		return err
	}

	p.Log.Info("detaching data image")

	return conn.EjectMedia(ctx)
}

// HasPowerFailure reports no power fault, the Redfish power state models none.
func (*Provisioner) HasPowerFailure(_ context.Context) bool {
	return false
}

// GetHealth maps the Redfish system health rollup to a controller health string.
// It returns no error by contract, so a failure is logged and published instead.
func (p *Provisioner) GetHealth(ctx context.Context) string {
	conn, err := p.Conn()
	if err != nil {
		return ""
	}

	healthy, err := conn.Healthy(ctx)
	if err != nil {
		p.Log.Error(err, "health check failed")
		p.Publish("HealthCheckError", err.Error())

		return ""
	}

	if healthy {
		return provisioner.HealthOK
	}

	return provisioner.HealthCritical
}
