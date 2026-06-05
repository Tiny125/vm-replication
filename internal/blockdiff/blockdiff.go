// Package blockdiff implements the userspace change-block-tracking core: it
// reads a source device (real block device or a regular file image) in fixed
// blocks, fingerprints each block with SHA-256, and persists those fingerprints
// as a manifest so the next run can ship only the blocks that changed.
//
// This is the pragmatic, no-kernel-module CBT used by the MVP. The manifest
// plays the role of the "checkpoint table / last confirmed synced offset" in
// the architecture: it is the source of truth for what the target already has.
package blockdiff

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// DefaultBlockSize is the fixed CBT granularity. 4 MiB balances per-block
// overhead against re-sending a whole block for a tiny write. Real CBT (dm-era)
// would track at 4 KiB; we trade RPO granularity for simplicity here.
const DefaultBlockSize = 4 << 20 // 4 MiB

// Device is an opened source or target whose total size is known.
type Device struct {
	*os.File
	Size int64
}

// OpenDeviceRead opens path read-only and determines its size. Works for both
// regular files and block devices (lseek(SEEK_END) returns the size of a block
// device on Linux).
func OpenDeviceRead(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	size, err := deviceSize(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Device{File: f, Size: size}, nil
}

// OpenDeviceWrite opens path for writing. If it is a regular file it is created
// and truncated to wantSize; if it is an existing block device its size must be
// at least wantSize.
func OpenDeviceWrite(path string, wantSize int64) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Mode().IsRegular() {
		if err := f.Truncate(wantSize); err != nil {
			f.Close()
			return nil, err
		}
		return &Device{File: f, Size: wantSize}, nil
	}
	// Block device (or other special file): verify capacity, never resize.
	size, err := deviceSize(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if size < wantSize {
		f.Close()
		return nil, fmt.Errorf("blockdiff: target %s is %d bytes, need at least %d", path, size, wantSize)
	}
	return &Device{File: f, Size: size}, nil
}

func deviceSize(f *os.File) (int64, error) {
	// Seek to end returns the size for both regular files and block devices.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}

// NumBlocks returns how many blocks of blockSize cover size bytes (last block
// may be short).
func NumBlocks(size int64, blockSize int) int64 {
	return (size + int64(blockSize) - 1) / int64(blockSize)
}

// BlockLen returns the length of block index i for a device of total size.
func BlockLen(size int64, blockSize int, i int64) int {
	start := i * int64(blockSize)
	if start+int64(blockSize) <= size {
		return blockSize
	}
	return int(size - start)
}

// Manifest holds one SHA-256 fingerprint per block. It is the CBT checkpoint.
type Manifest struct {
	BlockSize int
	DeviceSize int64
	Hashes    [][sha256.Size]byte
}

// NewManifest allocates an empty (all-zero) manifest sized for the device.
func NewManifest(deviceSize int64, blockSize int) *Manifest {
	return &Manifest{
		BlockSize:  blockSize,
		DeviceSize: deviceSize,
		Hashes:     make([][sha256.Size]byte, NumBlocks(deviceSize, blockSize)),
	}
}

const manifestMagic = 0x43425431 // "CBT1"

// Save writes the manifest atomically (temp file + rename).
func (m *Manifest) Save(path string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	var hdr [24]byte
	binary.BigEndian.PutUint32(hdr[0:], manifestMagic)
	binary.BigEndian.PutUint32(hdr[4:], uint32(m.BlockSize))
	binary.BigEndian.PutUint64(hdr[8:], uint64(m.DeviceSize))
	binary.BigEndian.PutUint64(hdr[16:], uint64(len(m.Hashes)))
	if _, err := f.Write(hdr[:]); err != nil {
		f.Close()
		return err
	}
	for i := range m.Hashes {
		if _, err := f.Write(m.Hashes[i][:]); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadManifest reads a manifest. A missing file yields (nil, nil) so callers can
// treat "no checkpoint yet" as a full sync.
func LoadManifest(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var hdr [24]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, fmt.Errorf("blockdiff: read manifest header: %w", err)
	}
	if binary.BigEndian.Uint32(hdr[0:]) != manifestMagic {
		return nil, fmt.Errorf("blockdiff: %s is not a valid manifest", path)
	}
	m := &Manifest{
		BlockSize:  int(binary.BigEndian.Uint32(hdr[4:])),
		DeviceSize: int64(binary.BigEndian.Uint64(hdr[8:])),
	}
	n := binary.BigEndian.Uint64(hdr[16:])
	m.Hashes = make([][sha256.Size]byte, n)
	for i := uint64(0); i < n; i++ {
		if _, err := io.ReadFull(f, m.Hashes[i][:]); err != nil {
			return nil, fmt.Errorf("blockdiff: read manifest entry %d: %w", i, err)
		}
	}
	return m, nil
}

// Matches reports whether prev is a usable baseline for a device of the given
// geometry. A geometry change (resize / different block size) forces a full sync.
func (m *Manifest) Matches(deviceSize int64, blockSize int) bool {
	return m != nil && m.BlockSize == blockSize && m.DeviceSize == deviceSize
}
