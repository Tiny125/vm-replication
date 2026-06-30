# Compatibility — supported sources, prerequisites & limits

What this tool can migrate to Linode, what it can't, and what the source needs.

---

## How it works (and why it's broadly compatible)

The agent replicates at the **block level**: it reads the raw disk device,
fingerprints each block with SHA-256, and ships only the changed blocks to the
receiver. It never parses your filesystem, distro, or application — it just
mirrors the bytes of the disk.

That means **replication itself is distro- and hypervisor-agnostic.** The only
place the guest OS matters is the post-copy **boot conversion**
(`machine-convert.sh`), which makes the mirrored disk boot on Linode's KVM/QEMU
platform (virtio drivers, bootloader, serial console, networking).

```
SOURCE (any x86-64 Linux, any platform)        TARGET (Linode)
  agent ── raw-disk diff, SHA-256 blocks ──►  receiver ── verify + write ──► volume
                                              cutover  ── machine-convert  ──► boots on Linode
```

---

## Supported sources

Any **x86-64 (amd64) Linux** server, regardless of where it runs:

- **On-prem / bare-metal**
- **AWS EC2**, **GCP**, **Azure**, **Alibaba Cloud**, other clouds
- **VMware**, **KVM/QEMU**, **Hyper-V**, **Xen**, and other hypervisors

The source's firmware and platform drivers don't matter — the conversion injects
**virtio** drivers and reinstalls **BIOS GRUB**, so a source that booted via
UEFI, used AWS ENA/NVMe drivers, or VMware's drivers will still boot on Linode.

### Guest OS (for the automated boot conversion)

| Family | Tooling handled | Examples |
|---|---|---|
| **Debian / Ubuntu** | `apt`, `initramfs-tools`, `grub-install` (`/boot/grub`) | Debian, Ubuntu |
| **RHEL family** | `dnf`, `dracut`, `grub2-install` (`/boot/grub2`) | **CentOS Stream, Fedora, Rocky, AlmaLinux**, RHEL |

**Filesystems** auto-checked/repaired before boot: **ext2/3/4** and **xfs**
(covers the Ubuntu and RHEL/Fedora defaults). Other filesystems replicate fine
but get no automatic repair pass.

> Distros outside these two families still **replicate** correctly (it's a block
> copy) — they just may need a manual boot fixup using the runbook in
> [`CUTOVER.md`](CUTOVER.md).

---

## Prerequisites

### On the source
1. **Linux on x86-64 (amd64).** (ARM is not supported — the agent binary is `linux/amd64`.)
2. **Root access** — the agent reads the raw block device (and, optionally, takes an LVM snapshot / `fsfreeze`).
3. **Outbound network to the appliance** on **TCP 5000–5100** over **TLS 1.3** (the receiver ports). On-prem sources behind NAT/firewall must allow this.
4. Ability to run the **enrollment one-liner** (`curl … | bash`), which installs the agent as a **systemd** service + timer.
5. Knowledge of which **block device(s)** to replicate (e.g. `/dev/sda`, `/dev/nvme0n1`). **Up to 8 disks** per migration (Linode slots `sda`–`sdh`).

### On the target / appliance
1. A **Linode account** and an **API token** with **Linodes: Read/Write** and **Volumes: Read/Write**.
2. The **appliance** (replication server) running on a Linode — see [`console.md`](console.md).
3. The target volume is sized **≥ the source disk** (the console rounds up).

### For the cleanest cutover (recommended, not required)
- **LVM root** on the source → enables a true crash-consistent point-in-time snapshot at cutover (no downtime). Without LVM the source is read live and the appliance proceeds best-effort with a warning.
- **ext4 or xfs** root → gets the automatic `fsck`/repair pass on cutover.

---

## What it supports

- **Online, continuous, block-level whole-disk replication** (OS + apps + data), with near-live change tracking — `hashdiff` (rescan + hash) or low-RPO `dmera` (device-mapper era).
- **Multi-disk** migrations (up to 8 disks).
- **Crash-consistent cutover** via LVM point-in-time snapshot (falls back to a live read + warning when no snapshot mechanism exists).
- **Automated boot conversion** for Linode: virtio initramfs, GRUB reinstall (or Linode-kernel boot for partitionless disks), `fsck`, Lish serial console, fresh machine-id, and a **DHCP network reset** (strips the source's static IP/DNS config so the new instance gets its own Linode IP).
- **Console/SSH access seeding** at cutover (set a root password and/or install an SSH key so the launched instance is reachable).
- **Choice of boot target and plan** at create time: a **separate Block Storage volume** (default), or the Linode's **local disk** (NVMe — faster, no separate volume cost, single-disk only). Either way you pick the launch plan from a Shared/Dedicated list (local-disk offers only plans whose disk fits); the volume option shows the estimated monthly Block Storage cost alongside the plan price.
- **Reboot-based cutover** that launches a new Linode from the replicated image.

## What it does **not** support

- **Windows or any non-Linux OS** (boot conversion is Linux-only).
- **Non-x86 architectures** (e.g. ARM/aarch64) — the agent is built for `linux/amd64`.
- **Live / zero-downtime migration.** Replication is continuous, but the final cutover involves a brief stop + reboot into the new Linode (it is *near*-zero-downtime, not live VM motion).
- **Application- or database-aware migration.** It moves the whole disk image, not individual apps/DBs. Use the optional quiesce **pre/post hooks** for app-consistent snapshots if needed.
- **Encrypted root (LUKS).** Encrypted blocks replicate, but the conversion can't unlock/mount the root to fix it up — needs manual handling.
- **Automatic repair for btrfs / ZFS roots** (no `fsck` pass; replication still works, boot fixup is best-effort).
- **Multi-disk with local-disk boot.** Local-disk boot supports a **single disk**; multi-disk migrations must use Separate-volume boot.

---

## Notes

- The initial **full sync** copies the whole disk once; duration depends on disk size and bandwidth. Subsequent passes ship only deltas.
- Keep the **source running and validated** until you've booted and tested the launched Linode — don't decommission it based on a successful cutover alone.
- For the manual/CLI path and rescue-mode runbook, see [`GETTING_STARTED.md`](GETTING_STARTED.md) and [`CUTOVER.md`](CUTOVER.md); for error messages, [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md).
