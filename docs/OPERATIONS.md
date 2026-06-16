# Operations Guide

How to run vm-replication as a managed service with a control plane, low-RPO
change tracking, and application-consistent snapshots. For the one-shot
migration walkthrough see [`CUTOVER.md`](CUTOVER.md); for architecture see
[`DESIGN.md`](DESIGN.md).

---

## 1. Control plane (`controld` + `replctl` + dashboard)

`controld` is a small REST API + dashboard + Prometheus exporter backed by
SQLite. Agents register themselves and report every sync; it computes RPO/lag.

### Run it

```bash
make build
CONTROL_TOKEN=$(openssl rand -hex 32)
./bin/controld -listen :8088 -db /var/lib/vm-repl/controld.db -token "$CONTROL_TOKEN"
```

Open `http://<host>:8088/` for the dashboard (it prompts for the token once),
and point Prometheus at `http://<host>:8088/metrics` (bearer auth).

### Drive it with `replctl`

```bash
export CONTROL_URL=http://<host>:8088 CONTROL_TOKEN=...

replctl register   -name web01 -role source -device /dev/sda
replctl create-job -name mig-web01 -target 203.0.113.10:4444 -rpo 60   # prints job id
replctl jobs                                                            # status + RPO table
replctl set-state  -job 1 -state cutover
```

### Point the agent at it

Add to the agent invocation (or `EXTRA_ARGS` / env in the systemd unit):

```bash
./agent ... -control http://<host>:8088 -control-token "$CONTROL_TOKEN" -control-job 1 -source-name web01
```

The agent reports each pass (success or failure) best-effort — reporting never
blocks or fails replication.

### Key endpoints / metrics

| Endpoint | Purpose |
|---|---|
| `GET /` | dashboard |
| `GET /api/v1/status` | per-job RPO/health JSON |
| `POST /api/v1/jobs/{id}/syncs` | agent sync report |
| `GET /metrics` | Prometheus |

Alert on `vm_repl_rpo_breached == 1` or `vm_repl_rpo_seconds > <target>`.

---

## 2. Run as a managed service (systemd)

Static binaries + unit files live in `deploy/systemd`. Install per role:

```bash
sudo scripts/install.sh controld    # control plane host
sudo scripts/install.sh receiver    # target / staging host
sudo scripts/install.sh agent       # source host
```

Each installs the binary to `/usr/local/bin`, seeds `/etc/vm-repl/<role>.env`
(edit it), installs the unit(s), and runs `daemon-reload`. Then:

```bash
# source: replicate every 60s (tune in vm-repl-agent.timer)
sudo systemctl enable --now vm-repl-agent.timer
sudo systemctl start vm-repl-agent.service     # trigger one pass now

# target:
sudo systemctl enable --now vm-repl-receiver.service

# control plane:
sudo systemctl enable --now vm-repl-controld.service
```

Place TLS material (`agent.crt/key`, `receiver.crt/key`, `ca.crt`) in
`/etc/vm-repl` (see `scripts/gen-certs.sh`). The agent timer is a `oneshot`
fired on a cadence; lower `OnUnitActiveSec` for a tighter RPO.

---

## 3. Low-RPO change tracking with dm-era (`--cbt dmera`)

The default `hashdiff` backend re-reads the whole disk each cycle to find
changes. For large disks, use a device-mapper **era** target so the kernel
tracks dirty blocks and the agent reads only those.

> Requires root, device-mapper, and `thin-provisioning-tools` (`era_invalidate`).
> You need a small separate metadata device (a few MiB).

```bash
# One-time: wrap the data disk in an era target (metadata on a spare LV/partition)
sudo scripts/dm-era-setup.sh --name vmrepl-data --data /dev/sda --meta /dev/vg/era_meta

# Anything using the disk must now use /dev/mapper/vmrepl-data so writes are seen.
# Replicate via the era device with the dmera backend:
./agent -device /dev/mapper/vmrepl-data \
        --cbt dmera --dmera-name vmrepl-data --dmera-meta /dev/vg/era_meta \
        -target 203.0.113.10:4444 -server-name 203.0.113.10 \
        -cert agent.crt -key agent.key -ca ca.crt
```

The first run full-syncs (no baseline era); subsequent runs ship only blocks the
kernel marked written. The agent still hashes candidate blocks before sending,
so over-reporting is harmless — correctness never depends on the tracker being
exact. Teardown: `sudo dmsetup remove vmrepl-data`.

---

## 4. Application-consistent snapshots (`--snapshot`)

Reading a live device is only *crash-consistent*. For databases and the final
cutover pass, take a consistent point-in-time view.

### LVM snapshot (recommended — source keeps running)

```bash
./agent -device /dev/vg/root --snapshot lvm --lvm-snapshot-size 10G \
        --pre-hook  'mysql -e "FLUSH TABLES WITH READ LOCK; FLUSH LOGS;"' \
        --post-hook 'mysql -e "UNLOCK TABLES;"' \
        -target 203.0.113.10:4444 -server-name 203.0.113.10 ...
```

Flow: pre-hook quiesces the app → filesystem is frozen for an instant → an LVM
CoW snapshot is taken → thawed → post-hook resumes the app → the agent
replicates from the **stable snapshot** while the source keeps serving → the
snapshot is removed on exit.

### fsfreeze (short final pass only)

`--snapshot fsfreeze` freezes the filesystem for the duration of the read
(writes blocked). Use it only for a brief final cutover sync on a quiesced app,
not for continuous replication. Pass `--mountpoint` if auto-detection fails.

These modes need root + LVM2/util-linux and operate on real devices, so they are
exercised on a real host; the default `--snapshot none` keeps the tool fully
usable everywhere.

### Automatic crash-consistent cutover (appliance console)

When you cut over from the **appliance console**, you don't need to set
`--snapshot` yourself: the appliance does it for you automatically.

- **Steady-state replication stays live** (`--snapshot none`) so there is **no
  downtime** while data is catching up — intermediate target states are never
  launched, so a "smeared" in-progress copy doesn't matter.
- **At cutover**, the appliance asks each disk's agent (over the receiver
  hello-ack) for **one final point-in-time pass** before it clones and launches.
  The agent re-reads from a snapshot it picks automatically:
  - an **LVM snapshot** if the source root is an LVM logical volume (zero
    downtime, the source keeps serving), or
  - **`fsfreeze`** otherwise — writes pause only for that single final read,
    which is the normal brief cutover window.
- The cloned image is therefore a **single crash-consistent instant** (like
  pulling the plug once): the target's journaling filesystem replays its journal
  on first boot and comes up cleanly, instead of failing `fsck` on a
  multi-minute "smear".

If you pin `-snapshot lvm|fsfreeze` in the enrollment yourself, the appliance
honors that choice for the cutover pass instead of auto-detecting. If no agent is
checking in (or it's an older build that ignores the request), cutover proceeds
on the current replicated data after a short wait, with a warning in the activity
log.

---

## 5. Verify locally

```bash
make smoke            # replication full+delta on file images
bash scripts/controld-smoke.sh   # control plane + agent reporting + metrics
make test             # unit tests (protocol, blockdiff, store, control plane)
```
