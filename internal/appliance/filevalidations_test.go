package appliance

import (
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

// File-transfer migrations don't provision a Block Storage volume, so the
// "Storage provisioned" pre-check must NOT appear for them (it would always be
// red and mislead). They get a destination-readiness check instead. Block
// migrations keep "Storage provisioned".
func TestValidationsFileMethod(t *testing.T) {
	s := &Server{}
	s.cfg.RPOTargetSec = 120

	file := api.Migration{
		BootTarget: api.BootTargetFile, OSImage: "linode/ubuntu24.04", LinodeType: "g6-nanode-1",
		Disks: []api.Disk{{ID: 1}},
	}
	checks := s.validations(file, 0)
	names := checkNames(checks)
	if names["Storage provisioned"] {
		t.Error("file migrations must not show the block 'Storage provisioned' check")
	}
	if !hasPrefix(names, "Destination") {
		t.Errorf("file migrations should show a destination-readiness check; got %v", names)
	}
	// The destination Linode is launched at Start, not at Create — the check
	// detail must say so, so a green tick isn't mistaken for "instance exists".
	destDetail := ""
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "Destination") {
			destDetail = c.Detail
		}
	}
	if !strings.Contains(strings.ToLower(destDetail), "start") {
		t.Errorf("destination check detail should note the Linode launches on Start; got %q", destDetail)
	}

	block := api.Migration{BootTarget: api.BootTargetVolume, Disks: []api.Disk{{ID: 1, VolumeDevice: "/dev/x"}}}
	if !checkNames(s.validations(block, 0))["Storage provisioned"] {
		t.Error("block migrations must keep the 'Storage provisioned' check")
	}
}

func checkNames(cs []api.ValidationCheck) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.Name] = true
	}
	return m
}
func hasPrefix(names map[string]bool, pre string) bool {
	for n := range names {
		if strings.HasPrefix(n, pre) {
			return true
		}
	}
	return false
}
