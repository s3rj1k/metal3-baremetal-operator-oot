// SPDX-License-Identifier: Apache-2.0

// Every knob the plugin reads from the environment, resolved once at load.

package core

import (
	"os"
	"strings"
	"time"
)

// Environment variable names, all prefixed so they cannot collide with BMO's own.
const (
	EnvListenAddr     = "ANACONDA_LISTEN_ADDR"
	EnvBaseURL        = "ANACONDA_BASE_URL"
	EnvInstallTimeout = "ANACONDA_INSTALL_TIMEOUT"
	EnvInstallDisk    = "ANACONDA_INSTALL_DISK"
)

// NormalizeDisk drops the /dev prefix anaconda does not want, so a hint written
// as /dev/vda still reaches ignoredisk and clearpart as vda.
func NormalizeDisk(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "/dev/")
}

// EnvDuration parses a duration, falling back to def on empty or invalid input.
func EnvDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		Log.Error(err, "ignoring invalid duration, using the default", "env", name, "value", raw, "default", def)

		return def
	}

	return d
}

// DefaultInstallTimeout is the fallback for the one tunable above.
const DefaultInstallTimeout = time.Hour

// InstallPollInterval is how often the wait for the anaconda callback requeues.
// Nothing reports faster than a machine installs, so this is not worth tuning.
const InstallPollInterval = 30 * time.Second

// Config is the resolved environment.
type Config struct {
	// ListenAddr set starts the plain HTTP listener serving /ks and /callback.
	// Unset leaves both off, a host that can be powered but never installed.
	ListenAddr string
	BaseURL    string

	// InstallDisk is the fleet wide default for {{ .InstallDisk }}, used by hosts
	// whose spec.rootDeviceHints name no disk kickstart can address.
	InstallDisk string

	// InstallTimeout bounds the wait for the anaconda callback, after which the
	// provision fails instead of requeueing forever.
	InstallTimeout time.Duration
}

// Enabled reports whether a listener address was configured.
func (c Config) Enabled() bool { return c.ListenAddr != "" }

// LoadConfig resolves the environment into a Config.
func LoadConfig() Config {
	return Config{
		ListenAddr: os.Getenv(EnvListenAddr),
		BaseURL:    os.Getenv(EnvBaseURL),

		InstallTimeout: EnvDuration(EnvInstallTimeout, DefaultInstallTimeout),
		InstallDisk:    NormalizeDisk(os.Getenv(EnvInstallDisk)),
	}
}
