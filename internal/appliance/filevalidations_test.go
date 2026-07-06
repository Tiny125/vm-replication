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
	names := checkNames(s.validations(file, 0))
	if names["Storage provisioned"] {
		t.Error("file migrations must not show the block 'Storage provisioned' check")
	}
	if !hasPrefix(names, "Destination") {
		t.Errorf("file migrations should show a destination-readiness check; got %v", names)
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
