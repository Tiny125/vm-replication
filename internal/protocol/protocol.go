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
	// File-transfer method (additive; block sessions never send these):
	MsgFileEntry MsgType = 6 // agent -> receiver: one file/dir/symlink's metadata (JSON)
	MsgFileData  MsgType = 7 // agent -> receiver: a raw content chunk for the current regular file
	MsgFileDone  MsgType = 8 // agent -> receiver: end of the file list (JSON: completeness + stats)
)

// ModeFile marks a Hello as a file-transfer session (empty Mode = block). A
// file session carries no block device geometry, so Validate skips the block
// checks for it.
const ModeFile = "file"

// Codec identifies how a block payload is encoded on the wire.
type Codec byte

const (
	CodecRaw   Codec = 0 // payload is the raw block bytes
	CodecFlate Codec = 1 // payload is DEFLATE-compressed block bytes
)

// MaxPayload caps a single frame's payload to guard against malformed/hostile
// peers. Block payloads (block size, at most a few MiB) sit well under this.
const MaxPayload = 64 << 20 // 64 MiB

// Block-size bounds for a negotiated session. The receiver rejects a Hello
// outside these, which prevents a buggy/hostile (but authenticated) agent from
// triggering a divide-by-zero (BlockSize 0) or an oversized per-block
// allocation. MaxBlockSize stays under MaxPayload so a raw block always fits a
// frame.
const (
	MinBlockSize = 512      // one sector
	MaxBlockSize = 32 << 20 // 32 MiB
)

// MaxDeviceSize bounds the device a session may declare (256 TiB). It guards the
// receiver's manifest allocation (one hash per block) against absurd sizes.
const MaxDeviceSize = 256 << 40

// Validate sanity-checks a decoded Hello before the receiver acts on it. A
// file-transfer Hello (Mode==ModeFile) carries no block geometry, so only the
// block-mode checks are skipped for it.
func (h Hello) Validate() error {
	if h.Mode == ModeFile {
		return nil
	}
	if h.BlockSize < MinBlockSize || h.BlockSize > MaxBlockSize {
		return fmt.Errorf("protocol: block size %d out of range [%d,%d]", h.BlockSize, MinBlockSize, MaxBlockSize)
	}
	if h.DeviceSize <= 0 || h.DeviceSize > MaxDeviceSize {
		return fmt.Errorf("protocol: device size %d out of range (0,%d]", h.DeviceSize, MaxDeviceSize)
	}
	return nil
}

// HashLen is the digest length we carry per block (SHA-256).
const HashLen = sha256.Size

// Hello is the opening control message from agent to receiver.
type Hello struct {
	ProtocolVersion int    `json:"protocol_version"`
	JobID           string `json:"job_id"`
	SourceHostname  string `json:"source_hostname"`
	DevicePath      string `json:"device_path"` // informational: source device name
	DeviceSize      int64  `json:"device_size"` // total bytes the target must hold
	BlockSize       int    `json:"block_size"`  // fixed block granularity
	FullSync        bool   `json:"full_sync"`   // true if agent is sending every block
	// Consistent reports that this pass was read from a point-in-time source
	// snapshot (LVM/fsfreeze), so the blocks form a single crash-consistent
	// instant rather than a multi-minute "smear". The receiver uses this at
	// cutover to know it has captured a launchable, crash-consistent image.
	Consistent bool `json:"consistent,omitempty"`
	// QuiesceError, when non-empty, means this is NOT a data session: the agent
	// tried to capture a crash-consistent image for cutover and couldn't (e.g. the
	// source root could not be remounted read-only because apps are still writing).
	// It carries the reason (and any blocking processes) so the appliance can fail
	// the cutover fast with an actionable message instead of waiting out its timeout.
	// The receiver skips Validate / data apply for such a Hello.
	QuiesceError string `json:"quiesce_error,omitempty"`
	// Mode selects the session type: empty (default) is a block session; ModeFile
	// is a file-transfer session (no block geometry; MsgFileEntry/MsgFileDone).
	Mode string `json:"mode,omitempty"`
}

// FileEntry is the metadata for one filesystem object in a file-transfer
// session — the JSON payload of a MsgFileEntry frame.
//
// Wire form: the agent sends one MsgFileEntry per object. For a regular file
// with content to transfer (Type=="file" && !Unchanged), the entry is followed
// by zero or more MsgFileData frames (raw chunks) totalling exactly Size bytes;
// the receiver knows the file is complete once it has read Size bytes. Dirs,
// symlinks, and Unchanged files carry no MsgFileData frames.
type FileEntry struct {
	Path     string `json:"path"`               // path relative to the source root (never absolute, never contains "..")
	Type     string `json:"type"`               // "file" | "dir" | "symlink"
	Mode     uint32 `json:"mode"`               // permission bits (os.FileMode &perm)
	UID      int    `json:"uid"`                // numeric owner
	GID      int    `json:"gid"`                // numeric group
	Size     int64  `json:"size,omitempty"`     // regular-file content length (bytes following this frame)
	ModTime  int64  `json:"mtime,omitempty"`    // unix seconds
	Linkname string `json:"linkname,omitempty"` // symlink target (verbatim, may be absolute)
	// Hash is the SHA-256 of a regular file's content, so a delta pass can skip
	// unchanged files without resending them.
	Hash string `json:"hash,omitempty"`
	// Unchanged marks a regular file whose content matches the receiver's last
	// pass (by Hash): metadata is refreshed but NO content bytes follow.
	Unchanged bool `json:"unchanged,omitempty"`
}

// FileDone ends a file-transfer session (JSON payload of MsgFileDone).
type FileDone struct {
	// Complete is true when the agent walked the whole tree this pass. Only then
	// may the receiver delete paths it did NOT see this pass (so removals on the
	// source propagate). A partial/aborted pass leaves Complete false and the
	// receiver deletes nothing.
	Complete    bool  `json:"complete"`
	Entries     int64 `json:"entries"`
	BytesOnWire int64 `json:"bytes_on_wire"`
}

// HelloAck is the receiver's response to Hello.
type HelloAck struct {
	Accepted bool   `json:"accepted"`
	Message  string `json:"message,omitempty"`
	// ConsistentResync asks the agent to abandon this (live) pass and immediately
	// re-read from a point-in-time snapshot, so the next applied image is
	// crash-consistent. The appliance sets this at cutover: continuous
	// replication stays live and disruption-free, and only the final pre-launch
	// pass is quiesced. Old agents that don't understand it simply ignore it.
	ConsistentResync bool `json:"consistent_resync,omitempty"`
	// Hold tells the agent that the connection is validated but replication has
	// NOT been started yet by the operator: the agent should treat this as a
	// successful connection check (not an error) and retry on its next tick
	// without streaming any data. The receiver sets it (with Accepted=false)
	// until the operator clicks "Start replication" in the console. Old agents
	// that don't understand it see Accepted=false and simply retry next tick.
	Hold bool `json:"hold,omitempty"`
	// DataTarget redirects a file-transfer agent to stream to a DIFFERENT
	// receiver (the launched destination Linode), instead of applying data here.
	// The appliance's control receiver returns it (with Accepted=false) once the
	// destination is up: the agent re-dials DataTarget, verifying the server
	// against DataServerName (which stays the appliance's cert SAN — the
	// destination presents the appliance's receiver cert, so no per-destination
	// certificate is needed). Empty means "no redirect".
	DataTarget     string `json:"data_target,omitempty"`
	DataServerName string `json:"data_server_name,omitempty"`
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
