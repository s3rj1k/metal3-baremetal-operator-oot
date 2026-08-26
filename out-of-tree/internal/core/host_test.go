// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"testing"

	"metal3.local/anaconda/internal/core"
	"metal3.local/anaconda/internal/testsupport"
)

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"aa:bb:cc:dd:ee:01":     testsupport.MAC,
		"AA:BB:CC:DD:EE:01":     testsupport.MAC,
		"aa-bb-cc-dd-ee-01":     testsupport.MAC,
		"aabb.ccdd.ee01":        testsupport.MAC,
		"  aa:bb:cc:dd:ee:01  ": testsupport.MAC,
		"":                      "",
		"not-a-mac":             "",
		"aa:bb:cc":              "",
		"eth0":                  "",
	}

	for in, want := range cases {
		if got := core.NormalizeMAC(in); got != want {
			t.Errorf("NormalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
