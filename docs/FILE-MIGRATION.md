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

## Status

- ✅ **Built + tested (this PR):** additive model (`BootTargetFile`, `os_image`),
  create-flow branch (no block volume), the full file data path (protocol +
  agent walk + receiver sink), and the Linode `ListImages` /
  `CreateInstanceFromImage` primitives. Non-regression proven by the existing
  suite; the data path proven by `TestFileSessionRoundTrip` and friends.
- 🔜 **Next slice:** launch-the-destination orchestration at Start (install the
  file receiver on it, hand the agent its data target), the file **finalize**
  (final pass + reboot), and the **console** create-card (method selector +
  OS/used-space helper + OS dropdown) and cutover copy. These involve launching
  real Linodes and so need live validation, mirroring how the disk-boot rescue
  flow and cloud-compat work were landed.

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
