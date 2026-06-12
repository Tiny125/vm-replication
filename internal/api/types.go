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

// ===========================================================================
// Appliance model: a Migration is the turnkey, console-driven unit of work. It
// supersedes the lower-level Job for appliance users — one migration moves one
// source server to a Linode Image.
// ===========================================================================

// MigrationState is the lifecycle of an appliance migration.
type MigrationState string

const (
	MigCreated       MigrationState = "created"        // record exists; provisioning storage
	MigAwaitingAgent MigrationState = "awaiting_agent" // enrollment command issued; no agent yet
	MigReplicating   MigrationState = "replicating"    // agent checked in; syncing
	MigReady         MigrationState = "ready"          // validations passed; can cut over
	MigMigrating     MigrationState = "migrating"      // converting + imaging
	MigImageReady    MigrationState = "image_ready"    // Linode Image created
	MigLaunched      MigrationState = "launched"       // new instance launched from the image
	MigFailed        MigrationState = "failed"
)

// Disk is one source block device within a migration. A migration has one or
// more disks (index 0 is the boot disk); each gets its own receiver port, its
// own replication volume on the appliance, and its own cloned artifact.
type Disk struct {
	ID           int64  `json:"id"`
	Index        int    `json:"index"` // 0 = boot disk
	SourceDevice string `json:"source_device"`
	SizeBytes    int64  `json:"size_bytes"`

	ReceiverPort int    `json:"receiver_port"`
	VolumeID     int64  `json:"volume_id,omitempty"`
	VolumeDevice string `json:"volume_device,omitempty"`
	ArtifactID   string `json:"artifact_id,omitempty"` // cloned volume, "volume:<id>"

	FullSyncDone  bool      `json:"full_sync_done"`
	TotalBlocks   int64     `json:"total_blocks"`
	ChangedBlocks int64     `json:"changed_blocks"`
	BytesOnWire   int64     `json:"bytes_on_wire"`
	LastSyncAt    time.Time `json:"last_sync_at"`
	AgentLastSeen time.Time `json:"agent_last_seen"`
	LastError     string    `json:"last_error,omitempty"` // last receiver-side session error
}

// Event is one entry in a migration's activity log.
type Event struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Level   string    `json:"level"` // info | warn | error
	Message string    `json:"message"`
}

// Migration is one source→Linode migration managed by the appliance console.
// It moves one source server (one or more disks) to launchable Linode volumes.
type Migration struct {
	ID    int64          `json:"id"`
	Name  string         `json:"name"`
	State MigrationState `json:"state"`

	// Source details entered in the console.
	SourceHostname string `json:"source_hostname"`
	SourceDevice   string `json:"source_device"`    // boot disk (mirror of Disks[0])
	SourceDiskSize int64  `json:"source_disk_size"` // boot disk size (bytes)

	// Disks (boot first). Authoritative per-disk state.
	Disks []Disk `json:"disks"`

	// Finalize result.
	ImageID    string `json:"image_id,omitempty"` // primary (boot) artifact, "volume:<id>"
	LaunchedID int64  `json:"launched_linode_id,omitempty"`

	LastError string `json:"last_error,omitempty"`

	// Pre-migration assessment + migration run timestamps.
	AssessedAt      time.Time `json:"assessed_at"`
	MigrateStarted  time.Time `json:"migrate_started"`
	MigrateFinished time.Time `json:"migrate_finished"`

	CreatedAt time.Time `json:"created_at"`
}

// DeviceSpec is one source disk in a create-migration request.
type DeviceSpec struct {
	Device    string `json:"device"`
	SizeBytes int64  `json:"size_bytes"`
}

// CreateMigrationRequest is the console "New migration" form. Devices lists the
// source disks (first = boot). For single-disk back-compat, SourceDevice +
// SourceDiskSize are accepted when Devices is empty.
type CreateMigrationRequest struct {
	Name           string       `json:"name"`
	SourceHostname string       `json:"source_hostname"`
	Devices        []DeviceSpec `json:"devices"`
	SourceDevice   string       `json:"source_device,omitempty"`
	SourceDiskSize int64        `json:"source_disk_size,omitempty"`
}

// ValidationCheck is one pre-cutover gate shown in the console.
type ValidationCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// MigrationView is a migration plus its computed validation checks and the
// enrollment command for the source. The token itself is only included right
// after creation / on explicit request.
type MigrationView struct {
	Migration    Migration         `json:"migration"`
	RPOSeconds   float64           `json:"rpo_seconds"`
	Validations  []ValidationCheck `json:"validations"`
	CanMigrate   bool              `json:"can_migrate"`
	Assessed     bool              `json:"assessed"` // pre-migration assessment passed
	EnrollCmd    string            `json:"enroll_cmd,omitempty"`
	UninstallCmd string            `json:"uninstall_cmd,omitempty"`

	// Live progress for the console: Phase is a human label ("initial sync",
	// "finalizing", …); PercentDone/ETASeconds are -1 when unknown.
	Phase          string  `json:"phase"`
	PercentDone    float64 `json:"percent_done"`
	ETASeconds     int64   `json:"eta_seconds"`
	ElapsedSeconds int64   `json:"elapsed_seconds"`
}

// ConnTestRequest asks the appliance to probe network reachability to a source.
type ConnTestRequest struct {
	IP string `json:"ip"`
}

// PortProbe is the result of a single TCP connect attempt.
type PortProbe struct {
	Port   int    `json:"port"`
	Open   bool   `json:"open"`
	Detail string `json:"detail"`
}

// ConnTestResult reports reachability from the appliance to a source host: an
// ICMP ping plus a sampled TCP probe of the replication port range.
type ConnTestResult struct {
	IP         string      `json:"ip"`
	PingOK     bool        `json:"ping_ok"`
	PingDetail string      `json:"ping_detail"`
	Ports      []PortProbe `json:"ports"`
}

// LoginRequest authenticates to the console.
type LoginRequest struct {
	Password string `json:"password"`
}

// SetLinodeTokenRequest stores the Linode API token on the appliance.
type SetLinodeTokenRequest struct {
	Token string `json:"token"`
}

// FinalizeRequest controls what happens when a migration is cut over.
type FinalizeRequest struct {
	LaunchInstance bool   `json:"launch_instance"` // also boot a new Linode from the image
	Region         string `json:"region,omitempty"`
	Type           string `json:"type,omitempty"`
	Label          string `json:"label,omitempty"`
}
