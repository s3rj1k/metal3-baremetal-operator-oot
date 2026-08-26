// SPDX-License-Identifier: Apache-2.0

// Package anaconda provisions a BareMetalHost by booting an anaconda installer
// ISO over Redfish virtual media and serving it a per host kickstart.
package anaconda

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"

	"metal3.local/anaconda/internal/core"
	"metal3.local/anaconda/internal/httpapi"
	"metal3.local/anaconda/internal/redfish"
)

// HostStore is the per host install state the provisioner reads and writes.
type HostStore interface {
	ReadInstallReport(ctx context.Context, namespace, name string) (*core.InstallReport, error)
	ClearInstallReport(ctx context.Context, namespace, name string) error
	InstallStartedAt(ctx context.Context, namespace, name string) (time.Time, error)
	HasKickstart(ctx context.Context, namespace, name string) (bool, error)
	HostInstallDisk(ctx context.Context, namespace, name string) (string, error)
	HostHardwareData(ctx context.Context, namespace, name string) (*metal3api.HardwareDetails, error)
}

// Factory holds everything shared across per host provisioner instances.
type Factory struct {
	Store HostStore
	Log   logr.Logger
	Cfg   core.Config
	// CallbackEnabled is set only once the listener actually binds, so a host is
	// never told to wait for a report on an endpoint nothing is serving.
	CallbackEnabled bool
}

// Provisioner is the per host state every Provisioner method works from.
type Provisioner struct {
	Store     HostStore
	Publisher provisioner.EventPublisher
	HostData  provisioner.HostData
	Log       logr.Logger
	Cfg       core.Config

	CallbackEnabled bool
}

// NewProvisionerFactory starts the listener when one is configured and returns a
// Factory building per host provisioners. Interface args keep kube out of here.
func NewProvisionerFactory(cfg core.Config, store HostStore, resolver httpapi.ServerResolver) (provisioner.Factory, error) {
	f := &Factory{Cfg: cfg, Store: store, Log: core.Log}

	if !cfg.Enabled() {
		core.Log.Info("no listener address configured, kickstart and callback are disabled",
			"env", core.EnvListenAddr)

		return f, nil
	}

	server := &httpapi.PluginServer{
		Config:   cfg,
		Resolver: resolver,
		Log:      core.Log.WithName("http"),
	}

	// A failed bind leaves the facility off rather than aborting the operator,
	// power management still works and only installs cannot complete.
	if err := server.Start(context.Background()); err != nil {
		core.Log.Error(err, "plugin listener failed to start, kickstart and callback are disabled")

		return f, nil
	}

	f.CallbackEnabled = true

	return f, nil
}

// NewProvisioner creates a per host provisioner (ctx unused, present for the
// Factory interface).
func (f *Factory) NewProvisioner(
	_ context.Context,
	hostData provisioner.HostData, //nolint:gocritic // the Factory interface fixes this signature
	publisher provisioner.EventPublisher,
) (provisioner.Provisioner, error) {
	return &Provisioner{
		Cfg:             f.Cfg,
		Store:           f.Store,
		HostData:        hostData,
		Log:             f.Log.WithValues("host", hostData.ObjectMeta.Name),
		Publisher:       publisher,
		CallbackEnabled: f.CallbackEnabled,
	}, nil
}

// Conn builds the Redfish connection for this host.
func (p *Provisioner) Conn() (redfish.Conn, error) {
	addr, err := redfish.ParseRedfishAddress(p.HostData.BMCAddress)
	if err != nil {
		return redfish.Conn{}, err
	}

	return redfish.Conn{
		Endpoint: addr.Endpoint,
		SystemID: addr.SystemID,
		Username: p.HostData.BMCCredentials.Username,
		Password: p.HostData.BMCCredentials.Password,
	}, nil
}

// Namespace and name are the host coordinates the store is keyed by.
func (p *Provisioner) Namespace() string { return p.HostData.ObjectMeta.Namespace }

func (p *Provisioner) Name() string { return p.HostData.ObjectMeta.Name }

// Publish emits an event when the controller supplied a publisher.
func (p *Provisioner) Publish(reason, message string) {
	if p.Publisher != nil {
		p.Publisher(reason, message)
	}
}
