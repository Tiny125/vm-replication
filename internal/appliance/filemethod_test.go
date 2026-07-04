package appliance

import (
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

// The file-transfer method is additive: isFileMethod is the single predicate
// every file-specific branch guards on, and it must be TRUE only for the new
// "file" boot target — never for the existing block methods, so their code
// paths are provably unaffected.
func TestIsFileMethod(t *testing.T) {
	if !isFileMethod(api.BootTargetFile) {
		t.Error(`isFileMethod("file") must be true`)
	}
	for _, bt := range []string{api.BootTargetVolume, api.BootTargetDisk, "", "bogus"} {
		if isFileMethod(bt) {
			t.Errorf("isFileMethod(%q) must be false — block methods must not enter file branches", bt)
		}
	}
}

// provisionsBlockStorage reports whether a boot target creates a Linode Block
// Storage volume at create time. The file method must NOT (its data streams to
// a launched destination, not an appliance volume); the block methods must.
func TestProvisionsBlockStorage(t *testing.T) {
	if provisionsBlockStorage(api.BootTargetFile) {
		t.Error("file migrations must not provision block storage")
	}
	for _, bt := range []string{api.BootTargetVolume, api.BootTargetDisk, ""} {
		if !provisionsBlockStorage(bt) {
			t.Errorf("block method %q must still provision its replication volume", bt)
		}
	}
}
