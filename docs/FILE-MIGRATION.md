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
 │ appliance  │  launches the destination, tells the agent where to stream
 └────────────┘
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

- **Enroll** bakes `-mode file -root /` into the agent's ExecStart.
- **Start** opens the gate; the agent streams the file tree to the appliance's
  per-migration file receiver, which stages it under
  `<datadir>/filemig-<id>-root/` (`handleFileSession`). Delta passes update it.
- **Cutover** (guided freeze → power off source → complete) runs `finalizeFile`:
  launches the destination from `os_image` + plan (`CreateInstanceFromImage`),
  waits for it to boot, then shows a **one-line Lish command** on the card. That
  command pulls the staged tree as a **token-gated tar** (`/cutover/files.tar`),
  extracts it over the live root (owners/perms preserved), pings
  `/cutover/done`, and reboots. The appliance sees the ping and marks the
  migration **launched**. Reuses the disk-boot token/pin pattern — no
  per-destination certificates.
- **Complete → remove agent → Close** is the shared cycle (Close has no volume
  to delete in file mode).

## Status

- ✅ **Built:** the full method end to end — additive model, create-flow branch,
  data path (protocol + agent walk + receiver sink), console method selector
  (default file) + OS/used-space helper + OS dropdown, appliance staging,
  destination launch, and tar-delivery cutover.
- ✅ **Tested (unit + regression):** `TestFileSessionRoundTrip`,
  `TestFileDelivery` (tar + done ping + token gating), `TestConsoleMigration
  MethodSelector`, `TestExcludedFromFileCopy`, `TestIsFileMethod`, plus the full
  existing suite + end-to-end appliance smoke staying green (block methods
  untouched).
- 🧪 **Needs live validation:** launching a real destination Linode from an
  image and the Lish tar-extract + reboot — these touch the Linode API and a
  real instance, so confirm on a live run (same as how the disk-boot rescue flow
  was validated). The data path and delivery mechanics are unit-proven.

---

## Limitations (documented)

- **x86_64 → x86_64** only (copied binaries run on the destination CPU) — the
  same architecture guard as the block methods.
- **Match the OS family/version** source→destination (auto-detected and
  pre-selected at create) — the destination keeps its own kernel.
- **Stop databases** before the final pass for a file-consistent copy (same as
  any live file copy).
- Copies the **root filesystem**; additional mounted data filesystems are
  separate (a future enhancement can add extra roots).
