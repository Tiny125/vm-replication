package controlplane

import (
	"fmt"
	"net/http"
	"strings"
)

// handleMetrics exposes replication health in Prometheus text exposition
// format. Alert on vm_repl_rpo_seconds > your RPO target, or on
// vm_repl_rpo_breached == 1.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.st.AllJobStatuses(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var b strings.Builder
	help := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	help("vm_repl_rpo_seconds", "gauge", "Seconds since the last successful sync (replication lag).")
	for _, st := range statuses {
		fmt.Fprintf(&b, "vm_repl_rpo_seconds{job=%q,state=%q} %.0f\n", st.Job.Name, st.Job.State, st.RPOSeconds)
	}

	help("vm_repl_rpo_breached", "gauge", "1 if RPO target is breached, else 0.")
	for _, st := range statuses {
		fmt.Fprintf(&b, "vm_repl_rpo_breached{job=%q} %d\n", st.Job.Name, b2i(st.RPOBreached))
	}

	help("vm_repl_rpo_target_seconds", "gauge", "Configured RPO target in seconds (0 = unset).")
	for _, st := range statuses {
		fmt.Fprintf(&b, "vm_repl_rpo_target_seconds{job=%q} %d\n", st.Job.Name, st.Job.RPOTargetSec)
	}

	help("vm_repl_syncs_total", "counter", "Total recorded sync passes for a job.")
	for _, st := range statuses {
		fmt.Fprintf(&b, "vm_repl_syncs_total{job=%q} %d\n", st.Job.Name, st.TotalSyncs)
	}

	help("vm_repl_last_sync_changed_blocks", "gauge", "Changed blocks moved in the last sync.")
	help("vm_repl_last_sync_bytes_on_wire", "gauge", "Bytes sent in the last sync.")
	help("vm_repl_last_sync_duration_ms", "gauge", "Duration of the last sync in milliseconds.")
	help("vm_repl_last_sync_ok", "gauge", "1 if the last sync succeeded, else 0.")
	for _, st := range statuses {
		if st.LastSync == nil {
			continue
		}
		ls := st.LastSync
		fmt.Fprintf(&b, "vm_repl_last_sync_changed_blocks{job=%q} %d\n", st.Job.Name, ls.ChangedBlocks)
		fmt.Fprintf(&b, "vm_repl_last_sync_bytes_on_wire{job=%q} %d\n", st.Job.Name, ls.BytesOnWire)
		fmt.Fprintf(&b, "vm_repl_last_sync_duration_ms{job=%q} %d\n", st.Job.Name, ls.DurationMS)
		fmt.Fprintf(&b, "vm_repl_last_sync_ok{job=%q} %d\n", st.Job.Name, b2i(ls.OK))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}
