// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"testing"

	"metal3.local/anaconda/internal/core"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv(core.EnvListenAddr, ":9080")
	t.Setenv(core.EnvBaseURL, "http://cb:9080")

	cfg := core.LoadConfig()

	if cfg.ListenAddr != ":9080" || cfg.BaseURL != "http://cb:9080" {
		t.Errorf("LoadConfig = %+v", cfg)
	}

	// An unset timeout must land on the default, a zero one would fail every
	// install the moment it started.
	if cfg.InstallTimeout != core.DefaultInstallTimeout {
		t.Errorf("install timeout = %s, want the default", cfg.InstallTimeout)
	}

	if !cfg.Enabled() {
		t.Error("a config with an address should be enabled")
	}
}

func TestConfigDisabledWithoutAddr(t *testing.T) {
	t.Setenv(core.EnvListenAddr, "")

	cfg := core.LoadConfig()

	if cfg.Enabled() {
		t.Error("a config without an address must be disabled")
	}
}

// An unparseable duration falls back rather than leaving a zero timeout that
// would fail every install immediately.
func TestLoadConfigIgnoresBadDurations(t *testing.T) {
	t.Setenv(core.EnvInstallTimeout, "not-a-duration")

	cfg := core.LoadConfig()

	if cfg.InstallTimeout != core.DefaultInstallTimeout {
		t.Errorf("install timeout = %s, want the default", cfg.InstallTimeout)
	}
}
