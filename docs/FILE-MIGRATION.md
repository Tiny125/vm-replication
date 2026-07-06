# File-transfer migration method

A third migration method (alongside **volume boot** and **disk boot**) that
copies the source server's **files** — only used storage — onto a freshly
launched destination Linode running an OS image you choose, rather than a
block-for-block image of the whole disk.

Why it's attractive:

- **Only used data moves** (a mostly-empty 80 GB disk copies its ~4 GB, not
  80 GB), so it's the cheapest and often fastest method.
- **No block-layout concerns at all** — no LVM/UEFI/partition/virtio issues,
  because the destination is a normal, already-bootable Linode.
- The destination is a **first-class Linode** (native `ext4` disk, Backups
  supported), not a raw imported image.

It is a wholly **additive** method: every file-specific path guards on
`isFileMethod` / `Hello.Mode == "file"` / new message types, so the existing
block methods are untouched (proven by the full test suite staying green).

---

## Architecture

```
 source server                         destination Linode (launched at Start)
 ┌────────────┐   files (mTLS)          ┌──────────────────────────────┐
 │  agent     │ ───────────────────────▶│ file receiver → live rootfs   │
 │ -mode file │                         │ (atomic per-file, safe paths) │
 └─────┬──────┘                         └──────────────────────────────┘
       │ control (gating / target handoff)
       ▼
 ┌────────────┐
 │ appliance  │  launches the destination + tells the agent where to stream
 └────────────┘  (control only — the file data never passes through it)
```

- **Data path (built + tested).** New protocol messages `MsgFileEntry` /
  `MsgFileData` / `MsgFileDone` and `Hello.Mode="file"`. The agent
  (`replicateFiles`) walks the source root — staying on the root filesystem,
  skipping virtual dirs (`/proc`, `/sys`, …), the destination's own
  boot/kernel/network/identity plumbing, and its own install — hashing each
  file so later passes skip unchanged content. The receiver
  (`handleFileSession`) writes each file to a temp then atomically renames it,
  refuses any path escaping the output root, and (on a **complete** pass)
  removes files deleted on the source — never touching protected destination
  paths. Reuses the existing mTLS, per-enrollment JobID identity, and Hold
  gating unchanged.

- **Excluded from the copy** (source side, with a receiver-side backstop):
  `/proc /sys /dev /run /tmp /mnt /media /var/tmp lost+found`; `boot vmlinuz
  initrd.img lib/modules`; `etc/fstab etc/machine-id etc/resolv.conf
  etc/netplan etc/systemd/network etc/NetworkManager/system-connections
  etc/network/interfaces`; and the agent's own files. The destination keeps its
  native kernel, boot loader, and network config and simply gains your files.

---

## Console flow (as designed)

1. **Create** — pick **File transfer** (the default). A copy-paste helper prints
   the source's **OS + version** and **used storage**, so you size a small plan
   by *used* data and pre-select a matching **destination OS image**. No block
   volume is provisioned.
2. **Enroll** — same one-line agent install (with `-mode file`).
3. **Start** — launches the destination from your OS image + plan, then the
   copy begins (source → destination). Live progress and delta passes as usual.
4. **Cutover** — final pass, then reboot the destination so every copied service
   starts from the migrated files.
5. **Remove the agent** — same as the block methods.

---

## How the pieces fit (implementation)

- **Enroll** bakes `-mode file -root /` into the agent's ExecStart (target = the
  appliance, for control/gating).
- **Start replication** launches the destination from `os_image` + plan with
  **cloud-init user-data** that downloads the receiver binary + the appliance's
  data-plane certs (both token-gated: `/dest/receiver`, `/dest/cert`) and runs
  the receiver on the destination, applying files to `/` (`vmrepl-receiver`
  systemd service, port 5999).
- **The agent** dials the appliance (control). Once the destination's receiver
  is reachable, the appliance answers with a **HelloAck redirect**
  (`DataTarget` = the destination), and the agent re-dials the destination and
  streams the files **straight into it** — nothing is staged on the appliance.
  Until then the agent is told to Hold ("destination launching"). The
  destination presents the **appliance's** receiver cert, so the agent keeps
  `-server-name` pointed at the appliance and needs **no per-destination
  certificate** (Go verifies the cert against `ServerName`, not the dialed IP).
- **Cutover** (guided freeze → power off source → Launch) just **reboots the
  destination** so it boots into the migrated files, then marks the migration
  **launched**. No Lish paste, no tar.
- **Complete → remove agent → Close** is the shared cycle (nothing to delete in
  file mode).

> Fallback: with **no Linode token** (evaluation/file-fallback mode), the agent
> applies files to a staging tree on the appliance instead (`handleFileSession`).

## Status

- ✅ **Built:** the full method end to end — additive model, create-flow branch,
  data path (protocol + agent walk + receiver sink), console method selector
  (default file) + OS/used-space helper + OS dropdown, **direct-to-destination**
  streaming (destination launched at Start with a cloud-init receiver install,
  HelloAck redirect, agent two-hop), and reboot-into-migrated-files cutover.
- ✅ **Tested (unit + regression):** `TestFileSessionRoundTrip`,
  `TestFileSessionRedirect` (HelloAck redirect / Hold), `TestDestBootstrap`
  (cloud-init + token-gated receiver/cert endpoints), `TestValidationsFileMethod`,
  `TestConsoleMigrationMethodSelector`, `TestExcludedFromFileCopy`,
  `TestIsFileMethod`, plus the full existing suite + end-to-end appliance smoke
  staying green (block methods untouched).
- 🧪 **Needs live validation:** launching a real destination Linode, its
  cloud-init installing + starting the receiver, and the agent's two-hop stream
  into it. These touch the Linode API + a real instance and cloud-init/metadata
  support on the image (Ubuntu/Debian have it), so confirm on a live run — same
  posture as the disk-boot rescue flow. The protocol/agent/receiver mechanics and
  the bootstrap are unit-proven.

### Requirements / caveats (direct mode)
- The destination image must support **cloud-init + the Linode Metadata service**
  (Ubuntu/Debian/RHEL-family cloud images do). Without it the receiver can't
  auto-install; the card warns after 15 min and we can add a manual paste
  fallback.
- The **source must reach the destination's public IP** on TCP 5999.
- A leftover `vmrepl-receiver` systemd service remains on the migrated instance
  (harmless — it just listens); the completion note tells you to
  `systemctl disable --now vmrepl-receiver` if you want it gone.
