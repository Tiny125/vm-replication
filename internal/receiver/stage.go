package receiver

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tiny125/vm-replication/internal/blockdiff"
)

// stageFile buffers one delta pass's validated blocks on the appliance's local
// disk so the pass can be applied to the target device ATOMICALLY once it
// completes. If the pass is interrupted (source powered off mid-stream, network
// drop, cutover freeze) the file is discarded and the device stays exactly at
// the last complete pass — a partial delta must never land, because deltas
// stream in ascending disk order and a partial apply tears the filesystem's
// early metadata away from its later blocks. Record format: 8-byte LE offset,
// 4-byte LE length, raw block bytes.
type stageFile struct {
	f       *os.File
	w       *bufio.Writer
	applied bool
}

// newStageFile creates the staging file next to the manifest (the appliance's
// data directory), or in the system temp dir when no manifest path is set
// (standalone receiver).
func newStageFile(manifestPath string) (*stageFile, error) {
	dir := ""
	if manifestPath != "" {
		dir = filepath.Dir(manifestPath)
	}
	f, err := os.CreateTemp(dir, ".vmrepl-staging-*")
	if err != nil {
		return nil, err
	}
	return &stageFile{f: f, w: bufio.NewWriterSize(f, 1<<20)}, nil
}

// add appends one validated block.
func (s *stageFile) add(offset int64, raw []byte) error {
	var hdr [12]byte
	binary.LittleEndian.PutUint64(hdr[:8], uint64(offset))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(raw)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return fmt.Errorf("stage block at %d: %w", offset, err)
	}
	if _, err := s.w.Write(raw); err != nil {
		return fmt.Errorf("stage block at %d: %w", offset, err)
	}
	return nil
}

// Apply replays every staged block onto the device (the pass is complete),
// then removes the staging file.
func (s *stageFile) Apply(dev *blockdiff.Device) error {
	if err := s.w.Flush(); err != nil {
		return fmt.Errorf("flush staging file: %w", err)
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(s.f, 1<<20)
	var hdr [12]byte
	var buf []byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break // clean end of the staged records
			}
			return fmt.Errorf("read staged block header: %w", err)
		}
		off := int64(binary.LittleEndian.Uint64(hdr[:8]))
		n := int(binary.LittleEndian.Uint32(hdr[8:]))
		if n <= 0 || n > 64<<20 {
			return fmt.Errorf("staged block at %d has implausible length %d", off, n)
		}
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("read staged block at %d: %w", off, err)
		}
		if err := writeAt(dev, buf, off); err != nil {
			return fmt.Errorf("write staged block at %d: %w", off, err)
		}
	}
	s.applied = true
	s.close()
	return nil
}

// Discard removes an unapplied staging file (no-op after Apply) — the
// interrupted pass simply never happened as far as the device is concerned.
func (s *stageFile) Discard() {
	if s.applied {
		return
	}
	s.close()
}

func (s *stageFile) close() {
	name := s.f.Name()
	_ = s.f.Close()
	_ = os.Remove(name)
}
