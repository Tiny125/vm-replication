package appliance

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// handleConnTest probes network reachability from the appliance to a source
// host: an ICMP ping plus a sampled TCP connect across the replication port
// range (BaseReceiverPort .. BaseReceiverPort+100). It's a read-only diagnostic
// — it opens no listeners and changes nothing on either side.
func (s *Server) handleConnTest(w http.ResponseWriter, r *http.Request) {
	var req api.ConnTestRequest
	if !readJSON(w, r, &req) {
		return
	}
	host := strings.TrimSpace(req.IP)
	if host == "" {
		writeErr(w, http.StatusBadRequest, "source IP or hostname is required")
		return
	}
	// Validate to a safe charset: a literal IP, or a DNS hostname. This both
	// rejects junk early and guarantees the value handed to exec("ping") can
	// never be an option or shell metacharacter (exec uses no shell anyway).
	if !validHost(host) {
		writeErr(w, http.StatusBadRequest, "invalid IP or hostname")
		return
	}

	res := api.ConnTestResult{IP: host}
	res.PingOK, res.PingDetail = pingHost(r.Context(), host)
	res.Ports = probePorts(r.Context(), host, s.cfg.BaseReceiverPort)
	writeJSON(w, http.StatusOK, res)
}

// validHost accepts a literal IPv4/IPv6 address or a DNS hostname made of
// [A-Za-z0-9.-] (1–253 chars). Everything else is rejected.
func validHost(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	for _, c := range h {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' {
			continue
		}
		return false
	}
	// reject leading dash so it can't look like a CLI flag
	return h[0] != '-'
}

// pingHost runs a short ICMP ping. ICMP may be unavailable (no CAP_NET_RAW /
// restrictive ping_group_range); in that case we report the reason rather than
// failing the whole test, since the TCP probes are the authoritative signal.
func pingHost(ctx context.Context, host string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// -c 2 packets, -W 2s per-packet wait. Linux iputils syntax.
	cmd := exec.CommandContext(ctx, "ping", "-c", "2", "-W", "2", host)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err == nil {
		if line := lastMatch(text, "min/avg/max"); line != "" {
			return true, "reachable — " + line
		}
		return true, "reachable"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, "no ICMP reply within timeout (host down, or ICMP blocked by a firewall)"
	}
	// ping exited non-zero: 100% loss, name resolution failure, or unavailable.
	if line := lastMatch(text, "packet loss"); line != "" {
		return false, "no ICMP reply (" + line + ") — host down or ICMP filtered"
	}
	if strings.Contains(text, "not known") || strings.Contains(text, "resolve") {
		return false, "could not resolve host name"
	}
	if text == "" {
		text = err.Error()
	}
	return false, text
}

// probePorts TCP-connects to a sample of the replication port range (every 10th
// port across base..base+100) so the operator can confirm the network path /
// security groups allow the data plane. Probes run concurrently with a short
// per-port timeout.
func probePorts(ctx context.Context, host string, base int) []api.PortProbe {
	ports := []int{}
	for p := base; p <= base+100; p += 10 {
		ports = append(ports, p)
	}
	out := make([]api.PortProbe, len(ports))
	var wg sync.WaitGroup
	d := net.Dialer{Timeout: 2 * time.Second}
	for i, p := range ports {
		wg.Add(1)
		go func(i, p int) {
			defer wg.Done()
			addr := net.JoinHostPort(host, fmt.Sprintf("%d", p))
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err == nil {
				_ = conn.Close()
				out[i] = api.PortProbe{Port: p, Open: true, Detail: "TCP connect succeeded"}
				return
			}
			out[i] = api.PortProbe{Port: p, Open: false, Detail: classifyDialErr(err)}
		}(i, p)
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool { return out[a].Port < out[b].Port })
	return out
}

// classifyDialErr turns a dial error into a short operator-friendly reason.
func classifyDialErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "refused"):
		return "connection refused (reachable, nothing listening)"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return "timed out (filtered by a firewall / security group, or host down)"
	case strings.Contains(s, "no route"):
		return "no route to host"
	case strings.Contains(s, "resolve") || strings.Contains(s, "not known"):
		return "name resolution failed"
	default:
		return s
	}
}

// lastMatch returns the trimmed last line of text containing sub, or "".
func lastMatch(text, sub string) string {
	var found string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, sub) {
			found = strings.TrimSpace(line)
		}
	}
	return found
}
