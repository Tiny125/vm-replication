# Getting Started — A Complete Step-by-Step Guide

This is the **beginner-friendly walkthrough** for using vm-replication to migrate
a Linux server to Akamai Cloud (Linode). It explains *what each piece is*, *what
you must prepare*, and *exactly what to run on each machine*.

If you just want the terse runbook, see [`CUTOVER.md`](CUTOVER.md). For running
it as a long-lived managed service (systemd, control-plane dashboard, dm-era,
snapshots) see [`OPERATIONS.md`](OPERATIONS.md). For the architecture, see
[`DESIGN.md`](DESIGN.md). This guide ties them together.

---

## 0. The mental model — three roles

vm-replication copies a server's **whole disk, block by block**, from a SOURCE
machine to a TARGET disk on Linode, then makes that disk bootable. There are
three roles. One physical machine can play more than one role.

```
   ┌──────────────────┐        encrypted block stream        ┌────────────────────────┐
   │  SOURCE           │   ───────────────────────────────▶  │  TARGET (Linode)        │
   │  your live server │        (mTLS, only changed blocks)   │  raw disk in Rescue Mode │
   │  runs: AGENT      │                                      │  runs: RECEIVER          │
   └──────────────────┘                                      └────────────────────────┘
              │  reports each sync (optional)
              ▼
   ┌──────────────────────────────┐
   │  CONTROL PLANE (optional)     │
   │  any host you control         │
   │  runs: CONTROLD + dashboard   │
   └──────────────────────────────┘
```

| Role | What it is | Which program runs here |
|---|---|---|
| **Source** | The existing server you want to migrate (on-prem, AWS, GCP, Azure, …) | **`agent`** |
| **Target / Replication** | A Linode you create, booted into **Rescue Mode**, with an empty raw disk that will receive the data | **`receiver`** |
| **Control plane** | Optional dashboard/inventory/RPO tracking. Any always-on host. | **`controld`** (+ `replctl` CLI) |

> **Key point:** there is no separate "replication server" you manage by hand. The
> **Linode target itself, booted in Rescue Mode**, is the replication/staging
> instance — the `receiver` writes incoming blocks straight onto its raw disk.

---

## 1. What you need to prepare

### 1.1 On your workstation (where you build and orchestrate)
- **Go 1.21+** (to build the binaries; the module fetches the exact toolchain it
  needs). For the **turnkey appliance**, you don't even need this — its installer
  auto-installs Go and the rest (see [`CONSOLE.md`](../CONSOLE.md)).
- **bash, openssl** (certificates), **curl + jq** (Linode provisioning script).
- This repository checked out.

### 1.2 Source server (the machine being migrated)
- Root/sudo access.
- Know your **root disk device** and its **size**. Find it with:
  ```bash
  lsblk -b -d -o NAME,SIZE,TYPE
  ```
  The whole disk is usually `/dev/sda`, `/dev/vda`, or `/dev/nvme0n1` — pick the
  **whole disk**, not a partition like `/dev/sda1`.
- **Outbound TCP** to the Linode on your chosen port (default **4444**).
- Enough free CPU/IO headroom — replication reads the whole disk.

### 1.3 Linode account
- A **Linode API token** with read/write on Linodes:
  Cloud Manager → *My Profile* → *API Tokens* → *Create a Personal Access Token*.
- Decide a **region** (e.g. `us-ord`) and an **instance type** (e.g.
  `g6-standard-2`) whose plan disk is **≥ your source disk size**.

### 1.4 Decisions to make up front
- **Port** for the receiver (default 4444).
- **Block size** (default 4 MiB — leave it unless you have a reason).
- **Consistency**: plain disk read (default), or an app-consistent **LVM
  snapshot** if you're migrating a database. See §8.
- Whether to run the **control plane** (recommended if migrating more than one
  server, or you want a lag/RPO dashboard).

> ⚠️ **Golden rule:** your SOURCE is never modified. Keep it running until you
> have **booted and validated** the target. Everything here is reversible up to
> the moment you decide to repoint DNS/traffic.

---

## 2. Try it locally first (no Linode, 30 seconds)

Before touching real servers, prove the mechanism on your workstation. This
replicates between two **file images** (no root, no Linode needed):

```bash
make smoke
```

It builds the binaries, generates certs, does a full sync, changes a few blocks,
does a delta sync, and verifies the target is byte-identical. If that passes,
the tooling works on your machine. (To also exercise the control plane:
`bash scripts/controld-smoke.sh`.)

---

## 3. Build the binaries

On your workstation:

```bash
make build
ls bin/
# agent  controld  receiver  replctl
```

These are **static** (no external libraries), so you can copy a single file onto
any 64-bit Linux host — including the minimal Linode Rescue environment.

| Binary | Goes on… | Purpose |
|---|---|---|
| `agent` | Source server | Reads the disk, sends changed blocks |
| `receiver` | Target Linode (Rescue Mode) | Writes blocks to the raw disk |
| `controld` | Control-plane host (optional) | API + dashboard + metrics |
| `replctl` | Wherever you run commands | CLI for the control plane |

---

## 4. Generate the TLS certificates

All traffic uses **mutual TLS** — both ends prove their identity with a
certificate signed by a tiny private CA you create here. The agent also verifies
the receiver's certificate matches the Linode's IP (or DNS name), so you create
the certs **after** you know the target IP (Step 5). If you want to pre-generate
with a placeholder you can re-run this later.

```bash
# Syntax: scripts/gen-certs.sh <output-dir> <receiver-IP-or-DNS>
scripts/gen-certs.sh certs <LINODE_PUBLIC_IP>
```

This writes into `certs/`:
- `ca.crt` — the CA. **Both** ends need this.
- `receiver.crt`, `receiver.key` — for the **target** (the receiver).
- `agent.crt`, `agent.key` — for the **source** (the agent).

> The `<receiver-IP-or-DNS>` you pass becomes the certificate's SAN. The agent's
> `-server-name` must match it exactly, or the TLS handshake fails.

---

## 5. Provision the Linode target (the replication/staging instance)

This creates a Linode, attaches an **empty raw disk** sized to your source, and
boots it into **Rescue Mode (Finnix)** so the receiver can write straight to the
disk at `/dev/sda`.

```bash
export LINODE_TOKEN=<your-linode-api-token>

scripts/linode-provision.sh \
  --label mig-web01 \
  --region us-ord \
  --type g6-standard-2 \
  --disk-mb 81920 \          # >= source disk size in MiB (here 80 GiB)
  --swap-mb 512 \
  --root-pass 'Choose-A-Strong-Password!'
```

When it finishes it prints the **public IP** and the IDs you'll need later
(`LINODE_ID`, `RAW_DISK_ID`, `SWAP_DISK_ID`). **Save those.**

> Sizing: `--disk-mb` must be **at least** your source disk size in MiB
> (`source_bytes / 1048576`). Round up. The Linode plan (`--type`) must have
> enough total disk for the raw + swap disks.

If you generated certs with a placeholder in Step 4, re-run:
`scripts/gen-certs.sh certs <LINODE_PUBLIC_IP>` now that you know the IP.

---

## 6. Start the receiver on the target

SSH into the Rescue shell (wait ~30s for Finnix to boot):

```bash
ssh root@<LINODE_PUBLIC_IP>      # password = the --root-pass you set
```

From your **workstation**, copy the receiver binary + its certs to the Linode:

```bash
scp bin/receiver certs/receiver.crt certs/receiver.key certs/ca.crt \
    root@<LINODE_PUBLIC_IP>:/root/
```

Back **on the Linode**, start the receiver, pointing it at the raw disk:

```bash
./receiver -listen :4444 -device /dev/sda \
    -cert receiver.crt -key receiver.key -ca ca.crt
```

It prints `receiver listening …` and waits. Leave it running (open a second SSH
session for anything else). The receiver verifies every block's checksum and
`fsync`s before acknowledging, so the target is always consistent.

> Use `/dev/sda` because the provisioning script attached the raw disk there in
> Rescue Mode. Confirm with `lsblk` on the Linode if unsure.

---

## 7. Run the initial full sync from the source

Copy the agent + certs to the **source server**:

```bash
# from your workstation:
scp bin/agent certs/agent.crt certs/agent.key certs/ca.crt \
    user@<SOURCE_SERVER>:/tmp/
```

On the **source server** (as root, so it can read the raw disk):

```bash
cd /tmp
sudo ./agent \
  -device /dev/sda \                  # the WHOLE source disk
  -target <LINODE_PUBLIC_IP>:4444 \
  -server-name <LINODE_PUBLIC_IP> \   # must match the cert SAN from Step 4
  -cert agent.crt -key agent.key -ca ca.crt
```

The first run sends the **entire disk** and writes a checkpoint file
(`sda.cbt`) next to the agent. You'll see:

```
full sync complete: 20480/20480 blocks changed, 80.0 GiB on wire in 14m3s (97.1 MiB/s)
```

> **Tip for busy servers:** replicate from a snapshot for a cleaner image — see
> §8. For a first pass on a mostly-idle server, the plain device read is fine.

---

## 8. Keep it in sync (continuous delta syncs)

Re-run the **exact same agent command** periodically. Each run reads the disk,
compares against the `sda.cbt` checkpoint, and sends **only changed blocks**:

```
delta sync complete: 37/20480 blocks changed, 148.0 MiB on wire in 6s (...)
```

The "blocks changed" count is your **replication lag** — watch it shrink and
stabilize. Two ways to automate:

**Quick (cron-like loop), on the source:**
```bash
while true; do sudo ./agent -device /dev/sda -target <IP>:4444 \
  -server-name <IP> -cert agent.crt -key agent.key -ca ca.crt; sleep 60; done
```

**Proper (systemd timer), on the source** — see §10 and `OPERATIONS.md`.

### Application-consistent sync (databases)
For a clean DB image, quiesce the app and snapshot instead of reading live:
```bash
sudo ./agent -device /dev/vg/root \
  --snapshot lvm --lvm-snapshot-size 10G \
  --pre-hook  'mysql -e "FLUSH TABLES WITH READ LOCK; FLUSH LOGS;"' \
  --post-hook 'mysql -e "UNLOCK TABLES;"' \
  -target <IP>:4444 -server-name <IP> \
  -cert agent.crt -key agent.key -ca ca.crt
```
(Requires LVM + root on the source. See `OPERATIONS.md` §4.)

---

## 9. Cutover — switch to the Linode

When replication lag is small and you're ready to migrate:

1. **Quiesce the source.** Stop application services (and stop databases
   cleanly) so no new writes happen.
2. **Final delta sync.** Run the agent **once more** so the target is fully
   current.
3. **Stop the receiver** on the Linode (`Ctrl-C`) so the disk is idle.
4. **Make the disk bootable on Linode.** Still in Rescue Mode, copy
   `scripts/machine-convert.sh` to the Linode and run it:
   ```bash
   # on the Linode (Rescue Mode):
   ./machine-convert.sh /dev/sda
   ```
   This fixes the things a raw copy can't boot without: **virtio drivers in the
   initramfs**, **GRUB**, **fstab UUIDs**, the **Lish serial console**, and
   **network reset to DHCP/eth0**. Read its output.
5. **Create a boot config profile and reboot into it** (from your workstation,
   using the IDs saved in Step 5):
   ```bash
   curl -fsS -X POST "https://api.linode.com/v4/linode/instances/<LINODE_ID>/configs" \
     -H "Authorization: Bearer $LINODE_TOKEN" -H "Content-Type: application/json" \
     -d '{
           "label": "boot-replica",
           "kernel": "linode/grub2",
           "devices": { "sda": {"disk_id": <RAW_DISK_ID>}, "sdb": {"disk_id": <SWAP_DISK_ID>} },
           "root_device": "/dev/sda",
           "helpers": { "network": true, "distro": false }
         }'
   # reboot into the returned config id:
   curl -fsS -X POST "https://api.linode.com/v4/linode/instances/<LINODE_ID>/reboot" \
     -H "Authorization: Bearer $LINODE_TOKEN" -H "Content-Type: application/json" \
     -d '{ "config_id": <CONFIG_ID> }'
   ```
   - `"helpers": {"network": true}` is **Linode Network Helper** — it injects
     working network config at boot.
   - If GRUB 2 won't boot, try `"kernel": "linode/direct-disk"`.
6. **Validate.** Open the **Lish console** in Cloud Manager (or `ssh
   <user>@<linode-id>@lish-<region>.linode.com`) to watch it boot. Confirm the
   kernel finds the disk (virtio), the network comes up, you can SSH in, and
   your app serves traffic.
7. **Repoint traffic** (DNS / load balancer) to the Linode only after validation.

Full details + the API field reference are in [`CUTOVER.md`](CUTOVER.md).

---

## 10. (Optional) Run it as a managed service + dashboard

Instead of running the agent by hand, install systemd units and a control plane.

**On the control-plane host:**
```bash
sudo scripts/install.sh controld
# edit /etc/vm-repl/controld.env (set CONTROL_TOKEN), then:
sudo systemctl enable --now vm-repl-controld.service
# dashboard at http://<host>:8088/  ·  metrics at /metrics
```

**Create a job and watch RPO** (from anywhere):
```bash
export CONTROL_URL=http://<host>:8088 CONTROL_TOKEN=...
replctl register   -name web01 -role source -device /dev/sda
replctl create-job -name mig-web01 -target <LINODE_IP>:4444 -rpo 60   # prints a job id
replctl jobs        # status + lag table
```

**On the source** — install the agent timer and point it at the control plane:
```bash
sudo scripts/install.sh agent
# edit /etc/vm-repl/agent.env: DEVICE, TARGET, SERVER_NAME, CONTROL_URL,
# CONTROL_TOKEN, CONTROL_JOB (the id above); put certs in /etc/vm-repl/
sudo systemctl enable --now vm-repl-agent.timer   # replicates every 60s
```

**On the target** — you can also run the receiver as a service (instead of by
hand in Rescue): `sudo scripts/install.sh receiver`. See [`OPERATIONS.md`](OPERATIONS.md)
for the env files, dm-era low-RPO tracking, and snapshot details.

---

## 11. Rollback

Nothing here is final until you repoint traffic.
- The **source is untouched** — if the Linode won't boot or the app is broken,
  just start the source's services again; you're live where you were.
- On the Linode, reboot back into **Rescue Mode**, fix the issue, re-run
  `machine-convert.sh`, and try the boot config again — or resume delta syncs.
- Keep the source as a hot standby for a bake-in period after cutover.

---

## 12. Which command runs where — cheat sheet

| Step | Machine | Command |
|---|---|---|
| Build | workstation | `make build` |
| Certs | workstation | `scripts/gen-certs.sh certs <IP>` |
| Provision target | workstation | `LINODE_TOKEN=… scripts/linode-provision.sh …` |
| Receive | target Linode (Rescue) | `./receiver -listen :4444 -device /dev/sda -cert … -key … -ca …` |
| Replicate | source server | `sudo ./agent -device /dev/sda -target <IP>:4444 -server-name <IP> -cert … -key … -ca …` |
| Convert | target Linode (Rescue) | `./machine-convert.sh /dev/sda` |
| Boot/cutover | workstation | Linode API: create config profile + reboot |
| Dashboard | control-plane host | `controld` / `replctl` |

---

## 13. Troubleshooting (quick hits)

| Symptom | Cause | Fix |
|---|---|---|
| Agent: TLS handshake / `bad certificate` | `-server-name` ≠ cert SAN | regenerate certs with the exact target IP/DNS, pass it as `-server-name` |
| Receiver rejects session: "block size … out of range" | agent `--block-size` outside 512B–32MiB | use the default or a value in range |
| `connection refused` | receiver not running / firewall | start the receiver; open the port outbound from source |
| Target won't boot, kernel panic "no disk" | virtio not in initramfs | re-run `machine-convert.sh`; confirm `virtio_blk`/`virtio_pci` baked in |
| Boots but no network | source NIC config | ensure `helpers.network: true` and the eth0/DHCP reset from `machine-convert.sh` |
| No Lish console output | serial console not set | `machine-convert.sh` handles this; confirm `console=ttyS0` + `serial-getty@ttyS0` |

More in [`CUTOVER.md`](CUTOVER.md#troubleshooting).

---

## 14. Where to go next

- [`CUTOVER.md`](CUTOVER.md) — concise migration runbook + full API field reference.
- [`OPERATIONS.md`](OPERATIONS.md) — control plane, systemd, dm-era low-RPO CBT, snapshots.
- [`DESIGN.md`](DESIGN.md) — how it works internally, the wire protocol, security model.
