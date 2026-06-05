package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/protocol"
)

func TestApplyBlockBounds(t *testing.T) {
	const blockSize = 4096
	const devSize = int64(3 * blockSize)

	path := filepath.Join(t.TempDir(), "target.img")
	dev, err := blockdiff.OpenDeviceWrite(path, devSize)
	if err != nil {
		t.Fatalf("OpenDeviceWrite: %v", err)
	}
	defer dev.Close()
	applied := blockdiff.NewManifest(devSize, blockSize)

	// Valid block at offset blockSize.
	good := make([]byte, blockSize)
	for i := range good {
		good[i] = byte(i)
	}
	validHdr := protocol.BlockHeader{Offset: blockSize, RawLen: blockSize, Codec: protocol.CodecRaw, Hash: sha256.Sum256(good)}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(validHdr, good)); err != nil {
		t.Fatalf("valid block rejected: %v", err)
	}
	// The bytes should have landed at the right offset.
	readback := make([]byte, blockSize)
	if _, err := dev.ReadAt(readback, blockSize); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(readback) != string(good) {
		t.Errorf("written block does not match")
	}

	// RawLen larger than the block size must be rejected before allocation.
	tooBig := protocol.BlockHeader{Offset: 0, RawLen: blockSize + 1, Codec: protocol.CodecRaw}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(tooBig, good)); err == nil {
		t.Error("expected rejection of oversized RawLen")
	}

	// Hostile ~4 GiB RawLen with a tiny payload must be rejected (no huge alloc).
	huge := protocol.BlockHeader{Offset: 0, RawLen: 0xFFFFFFFF, Codec: protocol.CodecFlate}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(huge, []byte{0x01, 0x02})); err == nil {
		t.Error("expected rejection of hostile RawLen")
	}

	// A write that runs past the end of the device must be rejected.
	oob := protocol.BlockHeader{Offset: devSize, RawLen: blockSize, Codec: protocol.CodecRaw, Hash: sha256.Sum256(good)}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(oob, good)); err == nil {
		t.Error("expected rejection of out-of-bounds offset")
	}

	// Negative offset must be rejected.
	neg := protocol.BlockHeader{Offset: -1, RawLen: blockSize, Codec: protocol.CodecRaw, Hash: sha256.Sum256(good)}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(neg, good)); err == nil {
		t.Error("expected rejection of negative offset")
	}

	_ = os.Remove(path)
}
