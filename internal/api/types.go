// Package api defines the data-transfer types shared between the control plane
// server, its storage layer, and the clients (agent/CLI). It intentionally has
// no external dependencies so that lightweight clients can import it without
// pulling in the storage engine.
package api

import "time"

// Role distinguishes a replication source from a target.
type Role string

const (
	RoleSource Role = "source"
	RoleTarget Role = "target"
)

// JobState is the lifecycle state of a replication job.
type JobState string

const (
	JobActive  JobState = "active"  // replicating (full + ongoing deltas)
	JobPaused  JobState = "paused"  // temporarily halted
	JobCutover JobState = "cutover" // final sync done, switching to target
	JobDone    JobState = "done"    // migration complete
	JobFailed  JobState = "failed"
)

// Valid reports whether s is a known job state. Used to reject arbitrary values
// at API ingest so they cannot reach the dashboard or metrics labels.
func (s JobState) Valid() bool {
	switch s {
	case JobActive, JobPaused, JobCutover, JobDone, JobFailed:
		return true
	}
	return false
}

// SyncMode records whether a sync sent every block or only deltas.
type SyncMode string

const (
	SyncFull  SyncMode = "full"
	SyncDelta SyncMode = "delta"
)

// Valid reports whether m is a known sync mode.
func (m SyncMode) Valid() bool {
	return m == SyncFull || m == SyncDelta
}

// Server is a registered source or target host in the inventory.
type Server struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	Hostname  string    `json:"hostname,omitempty"`
	Address   string    `json:"address,omitempty"` // host:port (target) or source IP
	Device    string    `json:"device,omitempty"`
	DiskSize  int64     `json:"disk_size,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// Job ties a source to a target and carries replication settings + RPO policy.
type Job struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	State          JobState  `json:"state"`
	SourceServerID int64     `json:"source_server_id,omitempty"`
	TargetServerID int64     `json:"target_server_id,omitempty"`
	TargetAddr     string    `json:"target_addr,omitempty"`
	Device         string    `json:"device,omitempty"`
	BlockSize      int       `json:"block_size,omitempty"`
	RPOTargetSec   int       `json:"rpo_target_seconds,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Sync is one completed replication pass, reported by the agent.
type Sync struct {
	ID            int64     `json:"id"`
	JobID         int64     `json:"job_id"`
	Mode          SyncMode  `json:"mode"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	TotalBlocks   int64     `json:"total_blocks"`
	ChangedBlocks int64     `json:"changed_blocks"`
	BytesOnWire   int64     `json:"bytes_on_wire"`
	DurationMS    int64     `json:"duration_ms"`
	OK            bool      `json:"ok"`
	Error         string    `json:"error,omitempty"`
}

// JobStatus is the computed health view for a job, used by the dashboard,
// /status, and /metrics. RPO fields describe replication lag.
type JobStatus struct {
	Job         Job     `json:"job"`
	LastSync    *Sync   `json:"last_sync,omitempty"`
	LastOKSync  *Sync   `json:"last_ok_sync,omitempty"`
	RPOSeconds  float64 `json:"rpo_seconds"`  // seconds since last OK sync
	RPOBreached bool    `json:"rpo_breached"` // RPOSeconds > job.RPOTargetSec
	TotalSyncs  int64   `json:"total_syncs"`
	Source      *Server `json:"source,omitempty"`
	Target      *Server `json:"target,omitempty"`
}

// ---- request payloads ----

// RegisterServerRequest registers/updates a server by name (idempotent).
type RegisterServerRequest struct {
	Name     string `json:"name"`
	Role     Role   `json:"role"`
	Hostname string `json:"hostname,omitempty"`
	Address  string `json:"address,omitempty"`
	Device   string `json:"device,omitempty"`
	DiskSize int64  `json:"disk_size,omitempty"`
}

// CreateJobRequest creates a replication job.
type CreateJobRequest struct {
	Name           string `json:"name"`
	SourceServerID int64  `json:"source_server_id,omitempty"`
	TargetServerID int64  `json:"target_server_id,omitempty"`
	TargetAddr     string `json:"target_addr,omitempty"`
	Device         string `json:"device,omitempty"`
	BlockSize      int    `json:"block_size,omitempty"`
	RPOTargetSec   int    `json:"rpo_target_seconds,omitempty"`
}

// ReportSyncRequest is posted by the agent after each replication pass.
type ReportSyncRequest struct {
	Mode          SyncMode  `json:"mode"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	TotalBlocks   int64     `json:"total_blocks"`
	ChangedBlocks int64     `json:"changed_blocks"`
	BytesOnWire   int64     `json:"bytes_on_wire"`
	DurationMS    int64     `json:"duration_ms"`
	OK            bool      `json:"ok"`
	Error         string    `json:"error,omitempty"`
}

// SetStateRequest changes a job's lifecycle state.
type SetStateRequest struct {
	State JobState `json:"state"`
}
