package protocol

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := []byte("hello payload")
	if err := WriteFrame(&buf, MsgHello, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	typ, got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if typ != MsgHello {
		t.Errorf("type = %d, want %d", typ, MsgHello)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("payload = %q, want %q", got, want)
	}
}

func TestBlockEncodeDecodeValidate(t *testing.T) {
	raw := bytes.Repeat([]byte("blockdata"), 100)
	sum := sha256.Sum256(raw)
	hdr := BlockHeader{Offset: 4 << 20, RawLen: uint32(len(raw)), Codec: CodecRaw, Hash: sum}

	frame := EncodeBlock(hdr, raw)
	gotHdr, gotPayload, err := DecodeBlock(frame)
	if err != nil {
		t.Fatalf("DecodeBlock: %v", err)
	}
	if gotHdr.Offset != hdr.Offset || gotHdr.RawLen != hdr.RawLen || gotHdr.Codec != hdr.Codec || gotHdr.Hash != hdr.Hash {
		t.Errorf("header mismatch: got %+v want %+v", gotHdr, hdr)
	}
	if !bytes.Equal(gotPayload, raw) {
		t.Errorf("payload mismatch")
	}
	if err := gotHdr.Validate(gotPayload); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateRejectsCorruption(t *testing.T) {
	raw := []byte("the original block contents")
	hdr := BlockHeader{Offset: 0, RawLen: uint32(len(raw)), Codec: CodecRaw, Hash: sha256.Sum256(raw)}
	corrupt := append([]byte{}, raw...)
	corrupt[0] ^= 0xff
	if err := hdr.Validate(corrupt); err == nil {
		t.Error("expected hash mismatch error, got nil")
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	// Hand-craft a header announcing a payload above MaxPayload.
	hdr := []byte{byte(MsgBlock), 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); err != ErrFrameTooLarge {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
}
