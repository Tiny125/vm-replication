# vm-replication — Design

A self-hosted, AWS MGN–style tool for **continuous, block-level Linux→Linux
replication and migration**, with **Akamai Cloud (Linode)** as the target. It
replicates from any source (on-prem, AWS, GCP, Azure, Alibaba Cloud, …) to a
Linode instance and cuts over with minimal downtime.

This document describes the **MVP** that lives in this repo today and the road
to the fuller architecture.

---

## 1. Design principles

1. **Agent-based, block-level.** Replicate the raw disk, not files. The target
   ends up byte-identical, so the OS, apps, and config come along for free.
2. **No kernel module for the MVP.** Change-block tracking (CBT) is done in
   userspace by fingerprinting blocks. It runs on *any* Linux source without
   custom drivers — the fastest path to a real, testable migration.
3. **The copy is the easy part; booting on Linode is the hard part.** A
   byte-perfect disk still won't boot on Linode KVM without virtio drivers in
   the initramfs, a correct bootloader, sane fstab, Lish serial console, and
   reset networking. The *machine-conversion* step is treated as a first-class
   component.
4. **Verifiable and durable.** Every block is SHA-256 verified on arrival; the
   receiver fsyncs before acknowledging; the agent only advances its checkpoint
   after the receiver confirms.
5. **Secure by default.** All data moves over cert-pinned mutual TLS 1.3.

---

## 2. Component map (sketch → MVP)

| Layer | Architecture sketch (v0.1) | MVP in this repo |
|---|---|---|
| Change capture | Custom kernel CBT / dm-snapshot | **Userspace block diff** — `internal/blockdiff` reads the device, SHA-256s each block, persists a manifest checkpoint |
| Transport | gRPC + dedup + LZ4 over mTLS | **Framed binary protocol** (`internal/protocol`) over **mTLS 1.3** (`internal/transport`), optional DEFLATE per block (`internal/codec`) |
| Source | Kernel agent + delta extractor | **`cmd/agent`** — diff + stream changed blocks; pluggable CBT (`internal/cbt`) and consistency (`internal/snapshot`) |
| Target | Receiver, O_DIRECT writer, WAL | **`cmd/receiver`** — verify + write at offset, fsync, target manifest |
| Control plane | Go API server + Postgres + Temporal | **`cmd/controld`** — REST API + dashboard + Prometheus over SQLite (`internal/store`); driven by **`cmd/replctl`**; agents report each sync |
| Change tracking | Custom kernel CBT | **`hashdiff`** (default) + **`dmera`** device-mapper era backend (`internal/cbt`) |
| Consistency | — | **`internal/snapshot`** — LVM snapshot / fsfreeze + quiesce hooks |
| Service mgmt | — | **systemd** units + installer (`deploy/systemd`, `scripts/install.sh`) |
| Cutover engine | Snapshot + reverse-sync rollback | **Linode provisioning + machine-conversion scripts** (`scripts/`) + `docs/CUTOVER.md` |

---

## 3. How replication works (MVP)

```
 SOURCE (any cloud / on-prem)                 TARGET (Linode, Rescue Mode)
 ┌────────────────────────────┐               ┌────────────────────────────┐
 │ agent                      │   mTLS 1.3    │ receiver                   │
 │  read /dev/sdX in blocks   │  TCP :4444    │  verify SHA-256 per block  │
 │  SHA-256 each 4 MiB block  ├──────────────►│  pwrite at block offset    │
 │  diff vs manifest (CBT)    │  changed      │  fsync on Done             │
 │  ship only changed blocks  │  blocks only  │  write target manifest     │
 │  advance checkpoint on ack │               │  → raw disk /dev/sda       │
 └────────────────────────────┘               └────────────────────────────┘
```

- **First run** (no manifest, or `--full`): every block is sent (initial full
  scan / baseline snapshot).
- **Subsequent runs**: the agent recomputes block hashes, compares against its
  on-disk manifest checkpoint, and ships only blocks whose hash changed. Run it
  on a `systemd` timer or cron for continuous near-RPO replication.
- The **manifest** (`*.cbt`) is the checkpoint: a binary file of one SHA-256 per
  block. It plays the role of the "last confirmed synced offset" in the sketch.
  A geometry change (resize / different block size) auto-promotes to a full sync.

### Consistency note
Reading a live device gives a *crash-consistent* image (as if the machine lost
power) — fine for most filesystems with journaling, and for the final cutover
sync where the source app is quiesced. For application-consistent snapshots
(databases), quiesce or use an LVM/filesystem snapshot as the agent's
`--device` for the final pass. Filesystem-level freeze + dm-snapshot is on the
roadmap.

---

## 4. Wire protocol

Length-prefixed framing over one mTLS stream. Each frame:

```
[1 byte type][4 bytes big-endian payload length][payload]
```

| Type | Direction | Payload |
|---|---|---|
| `Hello` (1) | agent→recv | JSON: job id, device size, block size, full-sync flag |
| `HelloAck` (2) | recv→agent | JSON: accepted + message |
| `Block` (3) | agent→recv | binary header `[offset u64][rawLen u32][codec u8][sha256 32B]` + (maybe compressed) payload |
| `Done` (4) | agent→recv | JSON: totals / stats |
| `DoneAck` (5) | recv→agent | JSON: ok, blocks written, error |

Backpressure is handled by TCP flow control. The block hash covers the
*uncompressed* bytes; the receiver verifies it after decompression.

---

## 5. Cutover model

See `docs/CUTOVER.md` for the runbook. In short:

1. **Drain & quiesce** — stop/flush source apps, run one final delta sync so the
   target is current.
2. **Convert** — `scripts/machine-convert.sh` chroots into the replicated disk
   and fixes virtio initramfs, GRUB, fstab, serial console, networking.
3. **Boot target** — create a Linode config profile that boots the raw disk
   (GRUB 2 or Direct Disk), reboot out of rescue, validate OS + app health.
4. **Rollback** — the source is untouched until you're satisfied; if cutover
   fails, boot the source back up. (Automated reverse-sync is roadmap.)
5. **Test mode** — clone the raw disk to a second disk and boot a throwaway
   config profile to rehearse cutover without disrupting replication.

---

## 6. Security model

- **mTLS 1.3** both directions; each end pins the other to an internal CA
  (`scripts/gen-certs.sh`). The receiver requires and verifies a client cert.
- The receiver writes raw blocks to a disk you control in your Linode account;
  it never executes payload content.
- Per-block SHA-256 detects corruption/tampering in flight.
- **Production hardening (roadmap):** short-lived certs issued by the control
  server, network policy restricting :4444 to the source IP, at-rest encryption
  on the staging disk.

---

## 7. Roadmap (toward the full sketch)

| Status | Item | Notes |
|---|---|---|
| ✅ Done | Control plane (Go API + SQLite) | `controld` + `replctl`: inventory, jobs, RPO tracking, dashboard, `/metrics` |
| ✅ Done | `systemd` units + installer | `deploy/systemd` + `scripts/install.sh` for agent/receiver/controld |
| ✅ Done | `dm-era` CBT | `internal/cbt` `dmera` backend + `scripts/dm-era-setup.sh` |
| ✅ Done | Application-consistent snapshots | `internal/snapshot`: LVM snapshot / fsfreeze + quiesce hooks |
| Med | Resume + per-block checkpoint acks | restart an interrupted sync mid-stream |
| Med | Dedup + LZ4/zstd, parallel streams | throughput on large/slow links |
| Med | Job scheduling in `controld` | push schedules to agents instead of local timers |
| Low | gRPC transport | swap the framed protocol; ecosystem tooling |
| Low | Automated reverse-sync rollback / bidirectional | hot standby of the old source |

---

## 8. Repo layout

```
cmd/agent/        source-side agent (diff + stream)
cmd/receiver/     target-side daemon (verify + write)
internal/
  protocol/       wire framing + block encode/verify
  blockdiff/      device access, block geometry, CBT manifest
  codec/          optional DEFLATE block compression
  transport/      mTLS client/server configs
scripts/
  gen-certs.sh        internal CA + agent/receiver certs
  smoke-test.sh       end-to-end proof on file images
  linode-provision.sh stand up a Rescue-Mode staging target via Linode API
  machine-convert.sh  make a replicated disk bootable on Linode
docs/
  DESIGN.md           this document
  CUTOVER.md          step-by-step migration runbook
```
