package blockdiff

import (
	"crypto/sha256"
	"path/filepath"
	"testing"
)

func TestNumBlocksAndBlockLen(t *testing.T) {
	const bs = 4 << 20
	// 10 MiB over 4 MiB blocks => 3 blocks, last one 2 MiB.
	size := int64(10 << 20)
	if got := NumBlocks(size, bs); got != 3 {
		t.Fatalf("NumBlocks = %d, want 3", got)
	}
	if got := BlockLen(size, bs, 0); got != bs {
		t.Errorf("BlockLen(0) = %d, want %d", got, bs)
	}
	if got := BlockLen(size, bs, 2); got != 2<<20 {
		t.Errorf("BlockLen(2) = %d, want %d", got, 2<<20)
	}
}

func TestManifestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.cbt")

	m := NewManifest(10<<20, 4<<20)
	for i := range m.Hashes {
		m.Hashes[i] = sha256.Sum256([]byte{byte(i)})
	}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if !got.Matches(m.DeviceSize, m.BlockSize) {
		t.Fatalf("geometry mismatch after reload")
	}
	for i := range m.Hashes {
		if got.Hashes[i] != m.Hashes[i] {
			t.Fatalf("hash %d mismatch after reload", i)
		}
	}
}

func TestLoadMissingManifestIsNil(t *testing.T) {
	got, err := LoadManifest(filepath.Join(t.TempDir(), "absent.cbt"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("manifest = %v, want nil", got)
	}
	// A nil manifest must report "no usable baseline" => full sync.
	if got.Matches(1<<20, 4<<20) {
		t.Errorf("nil manifest should not match any geometry")
	}
}
