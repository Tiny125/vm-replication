// Package cbt abstracts change-block tracking so the agent can find changed
// blocks either by re-reading and hashing the whole device (the portable
// default) or by asking the kernel which blocks were written via a device-mapper
// "era" target (low-RPO, no full rescan).
//
// A Tracker reports *candidate* dirty blocks. The agent still hashes candidates
// and compares to its manifest before sending, so a tracker may over-report
// safely — correctness never depends on the tracker being exact, only on it not
// missing a real change.
package cbt

// Strategy selects a change-tracking backend.
type Strategy string

const (
	// StrategyHashDiff re-reads and hashes every block each run. Works on any
	// Linux source with no setup; costs a full-disk read per cycle.
	StrategyHashDiff Strategy = "hashdiff"
	// StrategyDMEra reads the dirty-block set from a dm-era target, so only
	// changed blocks are read. Requires one-time device-mapper setup (root).
	StrategyDMEra Strategy = "dmera"
)

// Tracker yields the block indices that may have changed since the last
// checkpoint.
type Tracker interface {
	// Candidates returns the indices to consider for a device of totalBlocks.
	// If all is true, indices is ignored and the caller considers every block
	// (used for full syncs and the hashdiff strategy).
	Candidates(totalBlocks int64) (indices []int64, all bool, err error)

	// Checkpoint advances the tracker's "last synced" marker after a successful
	// sync (e.g. records the current dm-era era). No-op for hashdiff.
	Checkpoint() error

	// Close releases any resources/temporary device-mapper state.
	Close() error
}

// HashDiff is the portable tracker: it always asks the caller to consider every
// block, relying on per-block hashing to find the real deltas.
type HashDiff struct{}

// Candidates always returns all=true.
func (HashDiff) Candidates(total int64) ([]int64, bool, error) { return nil, true, nil }

// Checkpoint is a no-op; the manifest is the checkpoint for hashdiff.
func (HashDiff) Checkpoint() error { return nil }

// Close is a no-op.
func (HashDiff) Close() error { return nil }
