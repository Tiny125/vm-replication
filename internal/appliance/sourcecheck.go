package appliance

// Source check: a pre-migration assessment of a source server, run BEFORE
// creating any migration, so the operator knows up front whether (and how) the
// server can migrate.
//
// Deliberately agentless: the console mints a short-lived token and shows a
// one-line command; the operator runs it on the source; the script only READS
// system facts (os-release, lsblk, findmnt, …), POSTs them back to a
// token-gated endpoint, and exits. Nothing is installed, so there is nothing
// to remove afterwards — and the POST itself proves console-port reachability,
// while a throwaway probe listener in the receiver port range lets the script
// prove the DATA path too (the thing that actually breaks migrations).
// The verdict logic runs on the appliance (assessSource), so it is unit-tested
// and upgrades with the appliance rather than being baked into a script.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// sourceCheck tracks one minted assessment: the probe listener (if we could
// open one) and, once the script reports, the raw facts.
type sourceCheck struct {
	expires   time.Time
	probePort int
	probeLn   net.Listener
	report    *api.SourceCheckReport
}

const sourceCheckTTL = 30 * time.Minute

// handleSourceCheckCreate mints a check token, opens a data-plane probe
// listener (best-effort), and returns the one-line command for the source.
// POST /api/v1/sourcecheck (console session required).
func (s *Server) handleSourceCheckCreate(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	chk := &sourceCheck{expires: time.Now().Add(sourceCheckTTL)}

	// Probe listener: pick a free port from the TOP of the receiver range so we
	// never collide with per-disk receivers (allocated from the bottom). The
	// script connects to it to prove the source can reach the data plane. Purely
	// best-effort — no listener just means "not tested".
	top := s.cfg.BaseReceiverPort + 100
	if s.cfg.BaseReceiverPort == 0 {
		top = 5100
	}
	for p := top; p > top-5; p-- {
		if ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p)); err == nil {
			chk.probePort, chk.probeLn = p, ln
			go acceptAndDiscard(ln)
			break
		}
	}
	s.sourceChecks.Store(tok, chk)
	time.AfterFunc(sourceCheckTTL, func() { s.dropSourceCheck(tok) })

	cmd := fmt.Sprintf("curl -fsSL %s'%s://%s:%d/check/source.sh?token=%s' | sudo bash",
		s.curlPinFlag(), s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort, tok)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "cmd": cmd, "ttl_minutes": int(sourceCheckTTL.Minutes())})
}

// acceptAndDiscard accepts and immediately closes connections until the
// listener is closed — the probe only needs the TCP handshake to succeed.
func acceptAndDiscard(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// dropSourceCheck forgets a check and closes its probe listener.
func (s *Server) dropSourceCheck(tok string) {
	if v, ok := s.sourceChecks.LoadAndDelete(tok); ok {
		if chk := v.(*sourceCheck); chk.probeLn != nil {
			_ = chk.probeLn.Close()
		}
	}
}

// lookupSourceCheck resolves a live token.
func (s *Server) lookupSourceCheck(tok string) (*sourceCheck, bool) {
	v, ok := s.sourceChecks.Load(tok)
	if !ok {
		return nil, false
	}
	chk := v.(*sourceCheck)
	if time.Now().After(chk.expires) {
		s.dropSourceCheck(tok)
		return nil, false
	}
	return chk, true
}

// handleSourceCheckScript serves the fact-gathering script (GET
// /check/source.sh?token=, token-gated, unauthenticated like the installers).
func (s *Server) handleSourceCheckScript(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	chk, ok := s.lookupSourceCheck(tok)
	if !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired check token — generate a fresh command in the console")
		return
	}
	base := fmt.Sprintf("%s://%s:%d", s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort)
	script := fmt.Sprintf(sourceCheckScript, base, tok, s.cfg.PublicHost, chk.probePort, s.curlPinFlag())
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(script))
}

// handleSourceCheckReport accepts the script's facts (POST /check/report?token=).
func (s *Server) handleSourceCheckReport(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	chk, ok := s.lookupSourceCheck(tok)
	if !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired check token")
		return
	}
	var rep api.SourceCheckReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&rep); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed report: "+err.Error())
		return
	}
	chk.report = &rep
	if chk.probeLn != nil {
		_ = chk.probeLn.Close() // probe done; free the port
		chk.probeLn = nil
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// handleSourceCheckStatus is the console's poll endpoint
// (GET /api/v1/sourcecheck/{token}, console session required).
func (s *Server) handleSourceCheckStatus(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	if tok == "" { // direct handler tests hit the raw path
		if i := strings.LastIndex(r.URL.Path, "/"); i >= 0 {
			tok = r.URL.Path[i+1:]
		}
	}
	chk, ok := s.lookupSourceCheck(tok)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown or expired check — generate a fresh command")
		return
	}
	if chk.report == nil {
		writeJSON(w, http.StatusOK, api.SourceCheckStatus{Status: "pending"})
		return
	}
	a := assessSource(*chk.report)
	writeJSON(w, http.StatusOK, api.SourceCheckStatus{Status: "done", Report: chk.report, Assessment: &a})
}

// ---------------------------------------------------------------------------
// Assessment logic (pure; unit-tested).
// ---------------------------------------------------------------------------

// convertibleRootFS are root filesystems machine-convert.sh can check/repair
// and make bootable on Linode.
var convertibleRootFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true}

// maxVolumeBytes is Linode Block Storage's per-volume ceiling (10 TiB).
const maxVolumeBytes = int64(10) << 40

// assessSource turns raw source facts into per-method verdicts plus the
// general checks list shown in the console.
func assessSource(r api.SourceCheckReport) api.SourceAssessment {
	var a api.SourceAssessment

	// ---- General checks (shown as the ✔/✘ list) ----
	archOK := r.Arch == "x86_64" || r.Arch == "amd64"
	a.Checks = append(a.Checks, api.ValidationCheck{
		Name: "CPU architecture", OK: archOK,
		Detail: orDefault(r.Arch, "unknown") + map[bool]string{true: " — supported", false: " — the agent and the Linode target are x86_64-only"}[archOK],
	})
	osKnown := r.OSID != ""
	a.Checks = append(a.Checks, api.ValidationCheck{
		Name: "Operating system", OK: osKnown,
		Detail: orDefault(r.OSPretty, "could not read /etc/os-release"),
	})
	a.Checks = append(a.Checks, api.ValidationCheck{
		Name: "systemd", OK: r.HasSystemd,
		Detail: map[bool]string{true: "present — the agent installs as a systemd timer", false: "not found — the agent must be scheduled manually"}[r.HasSystemd],
	})
	rootDetail := orDefault(r.RootFS, "unknown")
	if r.RootOnLVM {
		rootDetail += " on LVM"
	}
	if r.RootOnRAID {
		rootDetail += " on software RAID"
	}
	if r.RootOnLUKS {
		rootDetail += " on LUKS encryption"
	}
	a.Checks = append(a.Checks, api.ValidationCheck{Name: "Root filesystem", OK: r.RootFS != "", Detail: rootDetail})
	if r.DataPortOK != nil {
		a.Checks = append(a.Checks, api.ValidationCheck{
			Name: "Replication port reachable", OK: *r.DataPortOK,
			Detail: map[bool]string{
				true:  fmt.Sprintf("source → appliance TCP %d connected", r.DataPortTried),
				false: fmt.Sprintf("source could NOT reach TCP %d — open 5000–5100 on every firewall in the path", r.DataPortTried),
			}[*r.DataPortOK],
		})
	} else {
		a.Checks = append(a.Checks, api.ValidationCheck{
			Name: "Replication port reachable", OK: true,
			Detail: "not tested — ensure TCP 5000–5100 (source → appliance) is open",
		})
	}

	// ---- Per-method verdicts ----
	type vb struct {
		verdict string
		reasons []string
	}
	worsen := func(v *vb, to string, reason string) {
		if to == "fail" || (to == "warn" && v.verdict == "ok") {
			v.verdict = to
		}
		v.reasons = append(v.reasons, reason)
	}
	newVB := func() *vb { return &vb{verdict: "ok"} }

	file, vol, dsk := newVB(), newVB(), newVB()
	all := []*vb{file, vol, dsk}

	if !archOK {
		for _, v := range all {
			worsen(v, "fail", "the source CPU is "+orDefault(r.Arch, "unknown")+"; only x86_64 servers can migrate")
		}
	}
	if !osKnown {
		for _, v := range all {
			worsen(v, "warn", "the OS could not be identified (/etc/os-release missing) — compatibility is unverified")
		}
	}
	if !r.HasSystemd {
		for _, v := range all {
			worsen(v, "warn", "no systemd: the replication agent's timer cannot be installed automatically (manual scheduling required)")
		}
	}
	if r.DataPortOK != nil && !*r.DataPortOK {
		for _, v := range all {
			worsen(v, "warn", fmt.Sprintf("the source could not reach the appliance's replication port (TCP %d) — replication will not start until 5000–5100 is open", r.DataPortTried))
		}
	}

	// File transfer specifics: it copies files, so disk layout/encryption don't
	// matter — but metadata that isn't copied does.
	if strings.EqualFold(r.SELinux, "enforcing") {
		worsen(file, "warn", "SELinux is enforcing: file copy does not preserve SELinux contexts/xattrs — plan `touch /.autorelabel` on the destination, or prefer a block method")
	}

	// Block-method specifics: the disk must be convertible to boot on Linode.
	blockPair := []*vb{vol, dsk}
	switch {
	case r.RootOnLUKS:
		for _, v := range blockPair {
			worsen(v, "fail", "the root filesystem is LUKS-encrypted — the block image cannot be converted to boot on Linode; use file transfer")
		}
	case r.RootFS == "":
		for _, v := range blockPair {
			worsen(v, "warn", "the root filesystem type could not be determined — boot conversion is unverified")
		}
	case convertibleRootFS[strings.ToLower(r.RootFS)]:
		// fully supported
	case strings.EqualFold(r.RootFS, "btrfs"):
		for _, v := range blockPair {
			worsen(v, "warn", "btrfs root: the boot conversion cannot check/repair btrfs, so a bootable result is not guaranteed — file transfer is safer")
		}
	default: // zfs, nfs, overlay, …
		for _, v := range blockPair {
			worsen(v, "fail", r.RootFS+" root cannot be converted to boot on Linode — use file transfer")
		}
	}
	if r.RootOnRAID {
		for _, v := range blockPair {
			worsen(v, "warn", "root on software RAID: the destination has a single virtual disk, so the array is flattened — verify the result boots before decommissioning the source")
		}
	}
	for _, d := range r.Disks {
		if d.Ephemeral {
			// A cloud scratch disk (Azure's temporary "resource disk"): its contents
			// are discarded by the provider, so it must NOT be part of a block
			// migration — and it doesn't count against any size limit. File transfer
			// is unaffected (it never copies /mnt).
			for _, v := range blockPair {
				worsen(v, "warn", fmt.Sprintf("disk %s looks like the cloud's ephemeral/resource disk (temporary storage mounted at /mnt) — do NOT add it to a block migration; its contents are provider-discarded scratch space", d.Name))
			}
			continue
		}
		if d.SizeBytes > maxVolumeBytes {
			worsen(vol, "fail", fmt.Sprintf("disk %s is %s — larger than Linode Block Storage's 10 TiB volume limit", d.Name, humanGB(d.SizeBytes)))
			worsen(dsk, "warn", fmt.Sprintf("disk %s is %s — a plan with that much local disk is required", d.Name, humanGB(d.SizeBytes)))
		}
	}

	a.Methods = []api.MethodAssessment{
		{Method: "file", Verdict: file.verdict, Reasons: file.reasons,
			RecommendedImage: recommendedImage(r.OSID, r.OSVersion), RecommendedImageNote: recommendedImageNote(r.OSID)},
		{Method: "volume", Verdict: vol.verdict, Reasons: vol.reasons},
		{Method: "disk", Verdict: dsk.verdict, Reasons: dsk.reasons},
	}
	return a
}

// recommendedImageNote qualifies APPROXIMATE image recommendations so the
// operator knows when the mapped image is not the same OS as the source.
func recommendedImageNote(osID string) string {
	switch strings.ToLower(strings.TrimSpace(osID)) {
	case "amzn":
		return "Amazon Linux has no Linode image; AlmaLinux is RHEL-family but not a drop-in replacement — validate your application stack on it, or use a block method to keep the exact OS"
	case "rhel", "redhat":
		return "Linode has no RHEL images; AlmaLinux is the binary-compatible rebuild of the same major version"
	case "sles", "sled", "suse":
		return "SUSE Linux Enterprise has no Linode image; openSUSE Leap shares its codebase but is not identical — validate, or use a block method to keep the exact OS"
	default:
		return ""
	}
}

// recommendedImage maps a source distro/version to the closest Linode image
// for the file-transfer destination. Empty means "no close match — pick
// manually in the dropdown".
func recommendedImage(osID, version string) string {
	id := strings.ToLower(strings.TrimSpace(osID))
	major := version
	if i := strings.Index(version, "."); i > 0 && id != "ubuntu" {
		major = version[:i]
	}
	switch id {
	case "ubuntu":
		switch {
		case strings.HasPrefix(version, "24.04"), strings.HasPrefix(version, "24.10"):
			return "linode/ubuntu24.04"
		case strings.HasPrefix(version, "22"):
			return "linode/ubuntu22.04"
		case strings.HasPrefix(version, "20"):
			return "linode/ubuntu20.04"
		default:
			return "linode/ubuntu24.04"
		}
	case "debian":
		switch major {
		case "12":
			return "linode/debian12"
		case "11":
			return "linode/debian11"
		default:
			return "linode/debian12"
		}
	case "almalinux":
		return "linode/almalinux" + orDefault(major, "9")
	case "rocky":
		return "linode/rocky" + orDefault(major, "9")
	case "rhel", "redhat":
		// No RHEL images on Linode: AlmaLinux is the closest binary-compatible rebuild.
		return "linode/almalinux" + orDefault(major, "9")
	case "centos":
		if major == "7" {
			return "linode/centos7"
		}
		return "linode/centos-stream" + orDefault(major, "9")
	case "fedora":
		return "linode/fedora" + orDefault(major, "40")
	case "opensuse", "opensuse-leap", "sles", "sled", "suse":
		// SUSE Linux Enterprise (common on Azure for SAP) shares its codebase with
		// openSUSE Leap, which is the closest Linode image.
		return "linode/opensuse15.6"
	case "arch":
		return "linode/arch"
	case "alpine":
		return "linode/alpine3.20"
	case "gentoo":
		return "linode/gentoo"
	case "amzn":
		// Amazon Linux is RHEL-family; AlmaLinux 9 is the closest match.
		return "linode/almalinux9"
	default:
		return ""
	}
}

func humanGB(b int64) string { return fmt.Sprintf("%d GB", (b+(1<<30)-1)>>30) }

// sourceCheckScript is the fact-gathering script (args: base URL, token,
// appliance host, probe port, curl pin flag). It only reads system facts and
// POSTs one JSON report; it installs nothing.
const sourceCheckScript = `#!/usr/bin/env bash
# vm-replication source check — READ-ONLY pre-migration assessment.
# Gathers system facts and reports them to your replication console.
# Installs nothing; changes nothing.
set -u
BASE=%q
TOKEN=%q
APPLIANCE_HOST=%q
PROBE_PORT=%d
PIN=%q
[ "$(id -u)" -eq 0 ] || { echo "run as root (use sudo)"; exit 1; }
command -v curl >/dev/null || { echo "curl is required"; exit 1; }

# js: JSON-escape a value (strip control chars, escape backslash + quote).
js(){ printf '%%s' "$1" | tr -d '\000-\037' | sed 's/\\/\\\\/g; s/"/\\"/g'; }

. /etc/os-release 2>/dev/null || true
ARCH="$(uname -m 2>/dev/null || echo unknown)"
KERNEL="$(uname -r 2>/dev/null || echo unknown)"
HOSTN="$(hostname 2>/dev/null || echo unknown)"
VIRT="$(systemd-detect-virt 2>/dev/null || echo unknown)"
HAS_SYSTEMD=false; [ -d /run/systemd/system ] && HAS_SYSTEMD=true
ROOTSRC="$(findmnt -no SOURCE / 2>/dev/null || echo "")"
ROOTFS="$(findmnt -no FSTYPE / 2>/dev/null || echo "")"
EFI=false; [ -d /sys/firmware/efi ] && EFI=true
SELINUX="$(getenforce 2>/dev/null | tr 'A-Z' 'a-z' || echo "")"

# Device stack under the root source: detect LVM / LUKS / RAID anywhere in it.
LVM=false; LUKS=false; RAID=false
if [ -n "$ROOTSRC" ] && [ -e "$ROOTSRC" ]; then
  CHAIN="$(lsblk -s -n -o TYPE "$ROOTSRC" 2>/dev/null || true)"
  echo "$CHAIN" | grep -q lvm   && LVM=true
  echo "$CHAIN" | grep -q crypt && LUKS=true
  echo "$CHAIN" | grep -q raid  && RAID=true
fi

# Real data disks (skip nbd/loop/ram/zram/sr/fd and zero-size). On Azure
# (virt=microsoft) the temporary "resource disk" is mounted at /mnt or
# /mnt/resource — mark it ephemeral: its contents are provider-discarded
# scratch space and must NOT be block-migrated (nor count against size limits).
DISKS_JSON=""; EPHDISKS=""
while read -r name size type; do
  [ "$type" = "disk" ] || continue
  [ "${size:-0}" -gt 0 ] || continue
  case "$name" in nbd*|loop*|ram*|zram*|sr*|fd*) continue;; esac
  EPH=false
  if [ "$VIRT" = "microsoft" ]; then
    MPS=" $(lsblk -n -o MOUNTPOINT "/dev/$name" 2>/dev/null | tr '\n' ' ') "
    case "$MPS" in *" /mnt "*|*" /mnt/resource "*) EPH=true; EPHDISKS="$EPHDISKS $name";; esac
  fi
  [ -n "$DISKS_JSON" ] && DISKS_JSON="$DISKS_JSON,"
  DISKS_JSON="$DISKS_JSON{\"name\":\"$(js "$name")\",\"size_bytes\":$size,\"ephemeral\":$EPH}"
  [ "$EPH" = "false" ] && [ "$size" -gt "${MAXDISK:-0}" ] && MAXDISK=$size
done < <(lsblk -b -d -n -o NAME,SIZE,TYPE 2>/dev/null)
MAXDISK="${MAXDISK:-0}"

USED="$(df -B1 --total -x tmpfs -x devtmpfs -x overlay 2>/dev/null | awk '/^total/{print $3}')"
USED="${USED:-0}"

# Data-plane probe: can this source reach the appliance's replication ports?
PORT_OK=null
if [ "$PROBE_PORT" -gt 0 ]; then
  if timeout 5 bash -c "exec 3<>/dev/tcp/$APPLIANCE_HOST/$PROBE_PORT" 2>/dev/null; then
    PORT_OK=true
  else
    PORT_OK=false
  fi
  exec 3>&- 2>/dev/null || true
fi

REPORT="{
 \"hostname\":\"$(js "$HOSTN")\",
 \"os_id\":\"$(js "${ID:-}")\",
 \"os_version\":\"$(js "${VERSION_ID:-}")\",
 \"os_pretty\":\"$(js "${PRETTY_NAME:-}")\",
 \"arch\":\"$(js "$ARCH")\",
 \"kernel\":\"$(js "$KERNEL")\",
 \"virt\":\"$(js "$VIRT")\",
 \"has_systemd\":$HAS_SYSTEMD,
 \"root_fs\":\"$(js "$ROOTFS")\",
 \"root_device\":\"$(js "$ROOTSRC")\",
 \"root_on_lvm\":$LVM,
 \"root_on_luks\":$LUKS,
 \"root_on_raid\":$RAID,
 \"efi_boot\":$EFI,
 \"selinux\":\"$(js "$SELINUX")\",
 \"disks\":[$DISKS_JSON],
 \"used_bytes\":$USED,
 \"data_port_ok\":$PORT_OK,
 \"data_port_tried\":$PROBE_PORT
}"

# ---------------------------------------------------------------------------
# Local assessment — printed HERE so the operator sees the full result in this
# terminal even when the report cannot reach the console. Mirrors the core
# rules of the appliance's assessment (which stays authoritative when online).
# ---------------------------------------------------------------------------
FILE_V=ok; VOL_V=ok; DSK_V=ok
FILE_N=""; VOL_N=""; DSK_N=""
worsen(){ # worsen <FILE|VOL|DSK> <warn|fail> <note>
  local cur; eval "cur=\$${1}_V"
  if [ "$2" = fail ]; then eval "${1}_V=fail"
  elif [ "$cur" = ok ]; then eval "${1}_V=warn"; fi
  eval "${1}_N=\"\${${1}_N}     - $3\n\""
}
if [ "$ARCH" != "x86_64" ] && [ "$ARCH" != "amd64" ]; then
  for m in FILE VOL DSK; do worsen $m fail "CPU is $ARCH - only x86_64 servers can migrate"; done
fi
[ -n "${ID:-}" ] || for m in FILE VOL DSK; do worsen $m warn "OS could not be identified (/etc/os-release missing)"; done
if [ "$HAS_SYSTEMD" != "true" ]; then
  for m in FILE VOL DSK; do worsen $m warn "no systemd: the replication agent must be scheduled manually"; done
fi
if [ "$PORT_OK" = "false" ]; then
  for m in FILE VOL DSK; do worsen $m warn "replication port TCP $PROBE_PORT blocked - open 5000-5100 (source to appliance)"; done
fi
[ "$SELINUX" = "enforcing" ] && worsen FILE warn "SELinux enforcing: file copy does not preserve contexts - plan /.autorelabel or use a block method"
if [ "$LUKS" = "true" ]; then
  worsen VOL fail "root is LUKS-encrypted - the block image cannot boot on Linode; use file transfer"
  worsen DSK fail "root is LUKS-encrypted - the block image cannot boot on Linode; use file transfer"
else
  case "$ROOTFS" in
    ext2|ext3|ext4|xfs) : ;;
    btrfs) worsen VOL warn "btrfs root: boot conversion is unvalidated - file transfer is safer"
           worsen DSK warn "btrfs root: boot conversion is unvalidated - file transfer is safer" ;;
    "")    worsen VOL warn "root filesystem type unknown - boot conversion unverified"
           worsen DSK warn "root filesystem type unknown - boot conversion unverified" ;;
    *)     worsen VOL fail "$ROOTFS root cannot be converted to boot on Linode - use file transfer"
           worsen DSK fail "$ROOTFS root cannot be converted to boot on Linode - use file transfer" ;;
  esac
fi
if [ "$RAID" = "true" ]; then
  worsen VOL warn "root on software RAID - the array is flattened on the destination"
  worsen DSK warn "root on software RAID - the array is flattened on the destination"
fi
if [ "${MAXDISK:-0}" -gt 10995116277760 ] 2>/dev/null; then
  worsen VOL fail "a disk exceeds Linode Block Storage's 10 TiB volume limit"
  worsen DSK warn "a disk exceeds 10 TiB - a plan with that much local disk is required"
fi
if [ -n "$EPHDISKS" ]; then
  worsen VOL warn "disk(s)$EPHDISKS = the cloud's ephemeral/resource disk (temporary storage at /mnt) - do NOT add to a block migration"
  worsen DSK warn "disk(s)$EPHDISKS = the cloud's ephemeral/resource disk (temporary storage at /mnt) - do NOT add to a block migration"
fi
# Recommended destination image for file transfer (closest Linode image).
REC=""; MAJ="${VERSION_ID%%%%.*}"
case "${ID:-}" in
  ubuntu) case "${VERSION_ID:-}" in 24*) REC=linode/ubuntu24.04;; 22*) REC=linode/ubuntu22.04;; 20*) REC=linode/ubuntu20.04;; *) REC=linode/ubuntu24.04;; esac;;
  debian) case "$MAJ" in 11) REC=linode/debian11;; *) REC=linode/debian12;; esac;;
  almalinux) REC=linode/almalinux${MAJ:-9};;
  rocky) REC=linode/rocky${MAJ:-9};;
  rhel|redhat) REC=linode/almalinux${MAJ:-9};;
  centos) [ "$MAJ" = "7" ] && REC=linode/centos7 || REC=linode/centos-stream${MAJ:-9};;
  fedora) REC=linode/fedora${MAJ:-40};;
  opensuse|opensuse-leap) REC=linode/opensuse15.6;;
  arch) REC=linode/arch;;
  alpine) REC=linode/alpine3.20;;
  gentoo) REC=linode/gentoo;;
  amzn) REC=linode/almalinux9;;
esac
label(){ case "$1" in ok) echo "SUPPORTED";; warn) echo "SUPPORTED WITH CAUTIONS";; *) echo "NOT SUPPORTED";; esac; }
USED_GB=$(( (USED + 1073741823) / 1073741824 ))
echo
echo "==================== SOURCE CHECK RESULT ===================="
echo " OS:        ${PRETTY_NAME:-unknown} ($ARCH, kernel $KERNEL)"
echo " Root:      ${ROOTFS:-unknown} on ${ROOTSRC:-unknown}$( [ "$LVM" = true ] && echo ' (LVM)')$( [ "$LUKS" = true ] && echo ' (LUKS)')$( [ "$RAID" = true ] && echo ' (RAID)')"
echo " systemd:   $( [ "$HAS_SYSTEMD" = true ] && echo present || echo 'NOT FOUND')     SELinux: ${SELINUX:-n/a}"
echo " Used:      ${USED_GB} GB (size a file-transfer plan by this)"
case "$PORT_OK" in
  true)  echo " Network:   replication port TCP $PROBE_PORT reachable";;
  false) echo " Network:   replication port TCP $PROBE_PORT BLOCKED - open 5000-5100";;
  *)     echo " Network:   replication ports not tested - ensure TCP 5000-5100 is open";;
esac
echo
echo " File transfer : $(label $FILE_V)"
[ -n "$REC" ] && echo "     recommended destination OS image: $REC" || echo "     recommended destination OS image: no close match - pick manually"
case "${ID:-}" in
  amzn) echo "     note: Amazon Linux has no Linode image - AlmaLinux is RHEL-family but not a drop-in; validate your stack, or use a block method to keep the exact OS";;
  rhel|redhat) echo "     note: AlmaLinux is the binary-compatible rebuild of RHEL (same major version)";;
  sles|sled|suse) echo "     note: openSUSE Leap shares the SUSE codebase but is not identical - validate, or use a block method";;
esac
[ -n "$FILE_N" ] && printf '%%b' "$FILE_N"
echo " Volume boot   : $(label $VOL_V)   (destination boots your migrated disk)"
[ -n "$VOL_N" ] && printf '%%b' "$VOL_N"
echo " Disk boot     : $(label $DSK_V)   (destination boots your migrated disk)"
[ -n "$DSK_N" ] && printf '%%b' "$DSK_N"
if [ "$FILE_V" = fail ] && [ "$VOL_V" = fail ] && [ "$DSK_V" = fail ]; then
  echo " VERDICT: this server cannot migrate with any method (see reasons above)."
else
  echo " VERDICT: this server can migrate - use a supported method above."
fi
echo "============================================================="
echo

CURL="curl -fsSL"
[ -n "$PIN" ] && CURL="$CURL $PIN"
echo ">> Delivering the result to your migration console…"
if echo "$REPORT" | $CURL -X POST -H 'Content-Type: application/json' --data-binary @- "$BASE/check/report?token=$TOKEN" >/dev/null 2>&1; then
  echo ">> Delivered. The Source check tab in the console now shows this result too."
else
  echo
  echo "!! NOTE: the result could NOT be delivered to the migration console."
  echo "!! The network from this server to the migration instance ($BASE)"
  echo "!! is not accessible, so the console will keep showing 'waiting'."
  echo "!! The result printed above is complete and valid."
  echo "!! Before migrating, open the console port and TCP 5000-5100 from this"
  echo "!! server to the migration instance, then re-run this command."
fi
`
