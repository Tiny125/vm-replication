# vm-replication

A self-hosted, **AWS MGN–style** tool for **continuous, block-level
Linux→Linux replication and migration**, built to migrate servers from anywhere
(on-prem, AWS, GCP, Azure, Alibaba Cloud, …) **to Akamai Cloud (Linode)**.

It installs a lightweight **agent** on the source, streams **only changed
blocks** of the raw disk over **mutually-authenticated TLS** to a **receiver**
running on a Linode in Rescue Mode, then **converts** the replicated disk so it
boots natively on Linode — with a near-zero-downtime cutover.

> Status: **working MVP** — end-to-end replication + Linode provisioning and
> boot-conversion tooling. See [`docs/DESIGN.md`](docs/DESIGN.md) for the
> architecture and roadmap, and [`docs/CUTOVER.md`](docs/CUTOVER.md) for the
> full migration runbook.

## How it works

```
 SOURCE (any platform)                         TARGET (Linode, Rescue Mode)
  agent  ──diff raw disk, SHA-256 blocks──►  receiver  ──verify + pwrite──►  /dev/sda
         ──ship only changed blocks (mTLS)──►          ──fsync + manifest──►  (raw disk)
```

- **Userspace change tracking** — no kernel module. The agent fingerprints each
  block and a manifest checkpoint lets later runs send only deltas. Runs on any
  Linux source.
- **Verified & durable** — every block is SHA-256 checked on arrival; the
  receiver fsyncs before acknowledging; the agent advances its checkpoint only
  after the target confirms.
- **Boots on Linode** — `machine-convert.sh` fixes the things a raw copy can't:
  virtio initramfs, GRUB, fstab, Lish serial console, and networking.

## Quick start (try it locally in 30 seconds)

No Linode account needed — this replicates between two file images on one host:

```bash
make smoke
```

It builds the binaries, generates mTLS certs, runs a **full sync**, mutates a
few blocks, runs a **delta sync**, and verifies the target is byte-identical and
that only the changed blocks moved.

## Build

```bash
make build      # static bin/agent and bin/receiver (CGO-free, portable)
make test       # unit tests
make vet        # go vet
```

## Migrate a real server to Linode

See [`docs/CUTOVER.md`](docs/CUTOVER.md). The short version:

```bash
# 1. Stand up a Rescue-Mode staging target on Linode (creates a raw disk).
LINODE_TOKEN=... scripts/linode-provision.sh \
    --label mig-web01 --region us-ord --type g6-standard-2 \
    --disk-mb 81920 --root-pass 'Strong!'

# 2. Generate mTLS certs pinned to the Linode IP.
scripts/gen-certs.sh certs <LINODE_IP>

# 3. On the Linode rescue shell: receive into the raw disk.
./receiver -listen :4444 -device /dev/sda -cert receiver.crt -key receiver.key -ca ca.crt

# 4. On the source: replicate the disk (repeat for low-RPO deltas).
./agent -device /dev/sda -target <LINODE_IP>:4444 -server-name <LINODE_IP> \
        -cert agent.crt -key agent.key -ca ca.crt

# 5. Quiesce, final sync, then convert + boot the disk on Linode.
./machine-convert.sh /dev/sda        # in rescue mode
# create a boot config profile and reboot — see docs/CUTOVER.md
```

## Repository layout

| Path | What |
|---|---|
| `cmd/agent` | source-side agent: diff + stream changed blocks |
| `cmd/receiver` | target-side daemon: verify + write blocks to a disk |
| `internal/protocol` | length-prefixed wire framing + block encode/verify |
| `internal/blockdiff` | device access, block geometry, CBT manifest |
| `internal/codec` | optional DEFLATE block compression |
| `internal/transport` | mTLS client/server configs |
| `scripts/` | cert gen, smoke test, Linode provisioning, machine conversion |
| `docs/` | `DESIGN.md` (architecture) and `CUTOVER.md` (runbook) |

## Limitations & roadmap

This is an MVP. It gives crash-consistent replication (use an LVM/fs snapshot for
app-consistency on the final pass), block-granularity CBT (re-reads the disk each
cycle), and scripted orchestration. On the roadmap: a control plane (inventory,
scheduling, RPO tracking), `dm-era` true CBT, application-consistent snapshots,
resume/checkpoint acks, dedup + zstd, and automated reverse-sync rollback. Full
list in [`docs/DESIGN.md`](docs/DESIGN.md#7-roadmap-toward-the-full-sketch).
