# Migration & Cutover Runbook

End-to-end procedure to migrate a running Linux server (on-prem / AWS / GCP /
Azure / Alibaba / …) to a **Linode** instance with this tool. Read
`docs/DESIGN.md` first for the architecture.

> ⚠️ Keep the source server intact until you have **booted and validated** the
> target. Nothing here destroys the source.

---

## 0. Prerequisites

- A Linode API token (`LINODE_TOKEN`) with read/write on Linodes.
- The source's root disk device path (e.g. `/dev/sda`, `/dev/vda`,
  `/dev/nvme0n1`) and its size. Check with `lsblk -b -dn -o NAME,SIZE`.
- `go`, `bash`, `curl`, `jq` on the machine running the scripts.
- Outbound TCP from the source to the Linode on port 4444 (or your choice).

Build the static binaries:

```bash
make build        # produces bin/agent and bin/receiver (CGO-free, portable)
```

---

## 1. Provision a staging target on Linode

Size the raw disk **>= the source disk size in MiB**. Example for an 80 GiB
source:

```bash
export LINODE_TOKEN=...your token...
scripts/linode-provision.sh \
  --label mig-web01 --region us-ord --type g6-standard-2 \
  --disk-mb 81920 --swap-mb 512 --root-pass 'ChangeMe-Strong!'
```

This creates the instance, attaches a **raw** disk (and swap), and boots it into
**Rescue Mode (Finnix)** with the raw disk on `/dev/sda`. Note the printed
`LINODE_ID`, `RAW_DISK_ID`, `SWAP_DISK_ID`, and **public IP**.

> Pick a `--type` whose plan disk capacity is ≥ your raw + swap sizes.

---

## 2. Generate mTLS certificates (pin the receiver to the Linode IP)

```bash
scripts/gen-certs.sh certs <LINODE_PUBLIC_IP>
# writes certs/ca.crt, certs/receiver.{crt,key}, certs/agent.{crt,key}
```

The receiver cert's SAN must match what the agent verifies (`-server-name`).

---

## 3. Start the receiver on the Linode (in Rescue Mode)

SSH into the rescue shell (`ssh root@<LINODE_PUBLIC_IP>`, password = your
`--root-pass`), then copy the binary + certs and run it:

```bash
# from your workstation:
scp bin/receiver certs/receiver.crt certs/receiver.key certs/ca.crt \
    root@<LINODE_PUBLIC_IP>:/root/

# on the Linode rescue shell:
./receiver -listen :4444 -device /dev/sda \
    -cert receiver.crt -key receiver.key -ca ca.crt
```

It waits for the agent. (Drop `-once` here — keep it running for repeated delta
syncs. Add `-once` only for one-shot tests.)

---

## 4. Initial full sync from the source

On the **source server**, copy `bin/agent` + `certs/agent.*` + `certs/ca.crt`,
then:

```bash
./agent -device /dev/sda -target <LINODE_PUBLIC_IP>:4444 \
    -server-name <LINODE_PUBLIC_IP> \
    -cert agent.crt -key agent.key -ca ca.crt
```

The first run ships the whole disk and writes a `*.cbt` checkpoint next to it.

> **Tip:** For a busy source, replicate from an LVM/filesystem snapshot for a
> cleaner point-in-time image: snapshot the LV, point `-device` at the snapshot.

---

## 5. Continuous delta syncs (keep RPO low)

Re-run the **same agent command** periodically. Each run ships only changed
blocks and reports lag, e.g. `delta sync complete: 12/20480 blocks changed`.

Automate on the source with a `systemd` timer (or cron):

```ini
# /etc/systemd/system/vm-repl.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/agent -device /dev/sda -target <IP>:4444 \
  -server-name <IP> -cert /etc/vm-repl/agent.crt -key /etc/vm-repl/agent.key \
  -ca /etc/vm-repl/ca.crt
```
```ini
# /etc/systemd/system/vm-repl.timer
[Timer]
OnUnitActiveSec=60s
AccuracySec=5s
[Install]
WantedBy=timers.target
```
```bash
systemctl enable --now vm-repl.timer
```

Watch the "blocks changed" count fall and stabilize — that's your replication
lag (RPO).

---

## 6. Cutover

When you're ready and lag is small:

1. **Quiesce the source.** Stop application services (and DBs cleanly). Optional
   but recommended for app-consistency.

   > **Non-LVM sources (e.g. a plain whole-disk cloud image) must be quiesced for
   > a *bootable* image, not just app-consistency.** The appliance can only take an
   > automatic crash-consistent snapshot when the source root is on **LVM**. Without
   > it, a block copy of the live, mounted root is internally inconsistent and the
   > conversion fails ("could not locate a root filesystem"). To get a clean image:
   >
   > ```bash
   > # on the source, after stopping apps:
   > sync
   > mount -o remount,ro /          # make the root filesystem static + consistent
   > mount | grep ' / '             # confirm it now shows "ro"
   > ```
   >
   > Let the agent run one more pass (so it captures the now-static root), then run
   > the cutover with **"skip snapshot"** — you have already made it consistent, so
   > the appliance should not wait for an LVM snapshot it cannot take. (If
   > `remount,ro` reports the device is busy, stop more services or do it from
   > single-user mode. Alternatively, power the source off and use skip-snapshot —
   > but then no further delta sync is possible.)
   >
   > **Console: Guided shutdown (recommended for non-LVM sources).** In the cutover
   > dialog, tick **"Guided shutdown"**. Phase 1 quiesces the source for you (the
   > agent remounts its root read-only for one consistent pass), then the migration
   > **pauses** in state `awaiting_cutover`. Power off the source, then click
   > **Complete cutover** to convert, clone and launch. This needs the up-to-date
   > agent, so **re-enroll the source** (re-run the install one-liner) after
   > upgrading the appliance — older agents lack the `-cutover-quiesce` capability
   > and the quiesce will time out.
2. **Final delta sync.** Run the agent once more so the target is fully current.
3. **Stop the receiver** on the Linode (Ctrl-C) so the disk is idle.
4. **Convert the disk to boot on Linode** (still in Rescue Mode):

   ```bash
   scp bin/.. scripts/machine-convert.sh root@<LINODE_PUBLIC_IP>:/root/   # or paste it
   ./machine-convert.sh /dev/sda
   ```

   This fixes virtio initramfs, GRUB, fstab, Lish serial console, and resets
   networking to DHCP/eth0. **Review its output.**

   The migrated disk keeps the **source's** logins — and cloud images usually
   leave root locked/password-less, so the Lish console has nothing to log in
   as. To set root access (so you can reach the instance without rescue surgery),
   pass it via the environment — it is never written to disk:

   ```bash
   VMREPL_ROOT_PASSWORD='s3cret' \
   VMREPL_SSH_AUTHORIZED_KEY='ssh-ed25519 AAAA... you@host' \
     ./machine-convert.sh /dev/sda
   ```

   From the **appliance console**, the Cutover dialog has the same two optional
   fields, so you don't need to do this by hand.

5. **Create a boot config profile** that boots the raw disk, then boot it:

   ```bash
   # Create a config profile: GRUB 2 kernel, raw disk as sda, swap as sdb.
   curl -fsS -X POST "https://api.linode.com/v4/linode/instances/<LINODE_ID>/configs" \
     -H "Authorization: Bearer $LINODE_TOKEN" -H "Content-Type: application/json" \
     -d '{
           "label": "boot-replica",
           "kernel": "linode/grub2",
           "devices": { "sda": {"disk_id": <RAW_DISK_ID>}, "sdb": {"disk_id": <SWAP_DISK_ID>} },
           "root_device": "/dev/sda",
           "helpers": { "network": true, "updatedb_disabled": true, "distro": false }
         }'

   # Reboot the instance into that config (use the returned config id):
   curl -fsS -X POST "https://api.linode.com/v4/linode/instances/<LINODE_ID>/reboot" \
     -H "Authorization: Bearer $LINODE_TOKEN" -H "Content-Type: application/json" \
     -d '{ "config_id": <CONFIG_ID> }'
   ```

   - `helpers.network: true` is **Linode Network Helper** — it injects working
     network config at boot, which complements step 4.
   - If GRUB 2 fails to boot, try `"kernel": "linode/direct-disk"` (boots the
     disk's MBR directly) — useful for non-GRUB or unusual layouts.

6. **Validate.** Open the **Lish console** (Glish/Weblish in the Linode UI, or
   `ssh <user>@<linode-id>@lish-<region>.linode.com`) to watch boot. Confirm:
   - kernel finds the disk (virtio) and mounts root,
   - network is up (you can SSH in),
   - your application starts and serves traffic.

---

## 7. Rollback

If the target won't boot or the app is broken:

- The **source is untouched** — start its services back up and you're live again.
- On the Linode, reboot back into **Rescue Mode** and re-run `machine-convert.sh`
  after fixing the issue, or resume delta syncs and try again.
- Keep the source as a hot standby until the target has run cleanly in
  production for a bake-in period.

---

## 8. Test-launch (rehearse without disrupting replication)

To rehearse cutover while replication keeps running:

1. Clone the raw disk (Linode UI/API: clone disk, or `dd` it in rescue to a
   second disk).
2. Run `machine-convert.sh` against the **clone**.
3. Boot a throwaway config profile from the clone, validate, then delete it.

The live raw disk and ongoing syncs are never touched.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Kernel panic: can't find root / no disk | virtio not in initramfs | re-run `machine-convert.sh`; verify `virtio_blk`/`virtio_pci` baked in |
| Boots but no network | source NIC naming / static config | Network Helper on; check eth0 DHCP config from step 4 |
| No Lish console output | serial console not configured | ensure `console=ttyS0,19200n8` and `serial-getty@ttyS0` enabled |
| `fsck` / wrong root mounted | fstab UUID mismatch | confirm root UUID; for separate `/boot`, mount it before chroot (the script does) |
| Agent rejected at TLS handshake | SAN/`-server-name` mismatch | regenerate certs with the exact IP/DNS the agent connects to |
| Target larger blocks re-sent every run | source written but content identical | expected if mtime/journal blocks churn; lower `--block-size` for finer granularity |
