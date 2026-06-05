// Package protocol defines the wire format spoken between the source agent and
// the target receiver. It is a small, length-prefixed binary framing designed
// to be trivial to read, debug, and swap out for gRPC later.
//
// Every frame is:
//
//	[1 byte  type]
//	[4 bytes payload length, big-endian]
//	[payload ...]
//
// Control frames (Hello, HelloAck, Done, DoneAck) carry a JSON payload so the
// schema can evolve without breaking the wire. The hot-path Block frame uses a
// compact fixed binary header so we never pay JSON cost per block.
package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MsgType identifies a frame.
type MsgType byte

const (
	MsgHello    MsgType = 1 // agent -> receiver: start of a sync session
	MsgHelloAck MsgType = 2 // receiver -> agent: accept/reject + receiver state
	MsgBlock    MsgType = 3 // agent -> receiver: one changed block
	MsgDone     MsgType = 4 // agent -> receiver: end of stream + stats
	MsgDoneAck  MsgType = 5 // receiver -> agent: final result
)

// Codec identifies how a block payload is encoded on the wire.
type Codec byte

const (
	CodecRaw   Codec = 0 // payload is the raw block bytes
	CodecFlate Codec = 1 // payload is DEFLATE-compressed block bytes
)

// MaxPayload caps a single frame's payload to guard against malformed/hostile
// peers. Block payloads (block size, at most a few MiB) sit well under this.
const MaxPayload = 64 << 20 // 64 MiB

// HashLen is the digest length we carry per block (SHA-256).
const HashLen = sha256.Size

// Hello is the opening control message from agent to receiver.
type Hello struct {
	ProtocolVersion int    `json:"protocol_version"`
	JobID           string `json:"job_id"`
	SourceHostname  string `json:"source_hostname"`
	DevicePath      string `json:"device_path"`  // informational: source device name
	DeviceSize      int64  `json:"device_size"`  // total bytes the target must hold
	BlockSize       int    `json:"block_size"`   // fixed block granularity
	FullSync        bool   `json:"full_sync"`    // true if agent is sending every block
}

// HelloAck is the receiver's response to Hello.
type HelloAck struct {
	Accepted bool   `json:"accepted"`
	Message  string `json:"message,omitempty"`
}

// Done closes the stream and reports what the agent sent.
type Done struct {
	TotalBlocks   int64 `json:"total_blocks"`
	ChangedBlocks int64 `json:"changed_blocks"`
	BytesOnWire   int64 `json:"bytes_on_wire"`
}

// DoneAck is the receiver's final word: how much it wrote and any error.
type DoneAck struct {
	OK            bool   `json:"ok"`
	BlocksWritten int64  `json:"blocks_written"`
	Error         string `json:"error,omitempty"`
}

// BlockHeader is the fixed binary header that precedes a block's payload bytes
// inside a MsgBlock frame. Layout (big-endian):
//
//	[8  bytes Offset]
//	[4  bytes RawLen]   uncompressed block length
//	[1  byte  Codec]
//	[32 bytes SHA-256]  digest of the *uncompressed* block
//
// followed by the (possibly compressed) payload for the remainder of the frame.
type BlockHeader struct {
	Offset int64
	RawLen uint32
	Codec  Codec
	Hash   [HashLen]byte
}

const blockHeaderLen = 8 + 4 + 1 + HashLen

var (
	// ErrFrameTooLarge is returned when a peer announces an oversized frame.
	ErrFrameTooLarge = errors.New("protocol: frame payload exceeds maximum")
	// ErrShortBlock indicates a Block frame too small to contain its header.
	ErrShortBlock = errors.New("protocol: block frame shorter than header")
	// ErrHashMismatch indicates a decoded block failed integrity verification.
	ErrHashMismatch = errors.New("protocol: block hash mismatch")
)

// WriteFrame writes a single typed frame with the given payload.
func WriteFrame(w io.Writer, t MsgType, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads one frame, returning its type and payload. The returned slice
// is freshly allocated and owned by the caller.
func ReadFrame(r io.Reader) (MsgType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return MsgType(hdr[0]), payload, nil
}

// WriteJSON marshals v and writes it as a frame of type t.
func WriteJSON(w io.Writer, t MsgType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}

// EncodeBlock builds the payload bytes for a MsgBlock frame (header + payload).
func EncodeBlock(h BlockHeader, payload []byte) []byte {
	buf := make([]byte, blockHeaderLen+len(payload))
	binary.BigEndian.PutUint64(buf[0:], uint64(h.Offset))
	binary.BigEndian.PutUint32(buf[8:], h.RawLen)
	buf[12] = byte(h.Codec)
	copy(buf[13:13+HashLen], h.Hash[:])
	copy(buf[blockHeaderLen:], payload)
	return buf
}

// DecodeBlock splits a MsgBlock payload into its header and payload bytes.
// The returned payload slice aliases frame.
func DecodeBlock(frame []byte) (BlockHeader, []byte, error) {
	if len(frame) < blockHeaderLen {
		return BlockHeader{}, nil, ErrShortBlock
	}
	var h BlockHeader
	h.Offset = int64(binary.BigEndian.Uint64(frame[0:]))
	h.RawLen = binary.BigEndian.Uint32(frame[8:])
	h.Codec = Codec(frame[12])
	copy(h.Hash[:], frame[13:13+HashLen])
	return h, frame[blockHeaderLen:], nil
}

// Validate checks a decoded, decompressed block against its advertised hash and
// length.
func (h BlockHeader) Validate(raw []byte) error {
	if uint32(len(raw)) != h.RawLen {
		return fmt.Errorf("protocol: block at %d: got %d bytes, header says %d", h.Offset, len(raw), h.RawLen)
	}
	sum := sha256.Sum256(raw)
	if sum != h.Hash {
		return ErrHashMismatch
	}
	return nil
}
