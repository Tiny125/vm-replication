#!/usr/bin/env bash
# machine-convert.sh — make a replicated disk bootable on Linode.
#
# Replication gives you a byte-identical copy of the source disk, but a copy is
# not automatically bootable on Linode's KVM/QEMU platform. This script performs
# the "machine conversion" (the post-cutover boot fixup):
# it chroots into the replicated root and fixes the things that differ between
# the source hypervisor and Linode.
#
# RUN THIS IN RESCUE MODE on the Linode, against the raw disk, AFTER the final
# delta sync and after stopping the receiver. Example:
#
#     sudo ./machine-convert.sh /dev/sda
#
# It is best-effort and supports Debian/Ubuntu (apt/grub/initramfs-tools) and
# RHEL-family (dnf/grub2/dracut). Review the log; some source images need manual
# tweaks. Always keep the source until you have booted and validated the target.
set -euo pipefail

DEV="${1:-/dev/sda}"
MNT="$(mktemp -d)"
KPARTX_USED=0
log() { echo ">> $*"; }
cleanup() {
  for m in dev/pts dev proc sys run; do
    mountpoint -q "$MNT/$m" && umount -l "$MNT/$m" 2>/dev/null || true
  done
  mountpoint -q "$MNT" && umount -l "$MNT" 2>/dev/null || true
  rmdir "$MNT" 2>/dev/null || true
  [ "$KPARTX_USED" = 1 ] && kpartx -d "$DEV" 2>/dev/null || true
}
trap cleanup EXIT

[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
# Resolve a /dev/disk/by-id/... symlink (how Linode volumes attach) to the
# canonical device so partition re-read and node names are predictable.
DEV="$(readlink -f "$DEV" 2>/dev/null || echo "$DEV")"
[ -b "$DEV" ] || { echo "$DEV is not a block device"; exit 1; }

# The data arrived as a raw block stream, so the kernel's cached partition table
# for this device is stale. Force a re-read through every mechanism available,
# then let udev settle so the partition nodes + fs metadata appear.
log "Re-reading partition table on $DEV"
partprobe "$DEV" 2>/dev/null || true
blockdev --rereadpt "$DEV" 2>/dev/null || true
partx -u "$DEV" 2>/dev/null || true
command -v udevadm >/dev/null 2>&1 && udevadm settle --timeout=15 2>/dev/null || true
sleep 1

# Enumerate child partitions. We do NOT pre-filter by filesystem type — udev may
# not have probed it yet — and instead probe each by trying to mount it.
list_parts() { lsblk -lnpo NAME,TYPE "$DEV" 2>/dev/null | awk '$2=="part"{print $1}'; }
mapfile -t PARTS < <(list_parts)

# If the kernel still won't expose partition nodes (device busy, GPT backup
# mismatch after writing a smaller image onto a larger volume), map them with
# kpartx via device-mapper as a fallback.
if [ "${#PARTS[@]}" -eq 0 ] && command -v kpartx >/dev/null 2>&1; then
  log "No kernel partition nodes; mapping with kpartx"
  kpartx -as "$DEV" 2>/dev/null || true
  KPARTX_USED=1
  sleep 1
  base="$(basename "$DEV")"
  mapfile -t PARTS < <(ls /dev/mapper/"${base}"* 2>/dev/null || true)
fi

# Some sources (and many cloud images) put the root filesystem directly on the
# whole disk with NO partition table. In that case mount the device itself.
PARTITIONED=1
if [ "${#PARTS[@]}" -eq 0 ]; then
  PARTS=("$DEV")
  PARTITIONED=0
fi

# Find the root: the candidate carrying /etc/fstab and a real root tree.
ROOT_PART=""
is_root() { [ -f "$1/etc/fstab" ] && { [ -d "$1/sbin" ] || [ -L "$1/sbin" ] || [ -d "$1/bin" ] || [ -L "$1/bin" ]; }; }
for p in "${PARTS[@]}"; do
  [ -b "$p" ] || continue
  umount "$MNT" 2>/dev/null || true
  if mount -o ro "$p" "$MNT" 2>/dev/null; then
    if is_root "$MNT"; then ROOT_PART="$p"; umount "$MNT" 2>/dev/null || true; break; fi
    umount "$MNT" 2>/dev/null || true
  fi
done
if [ -z "$ROOT_PART" ]; then
  echo "could not locate a root filesystem with /etc/fstab on $DEV (candidates: ${PARTS[*]:-none})"
  lsblk -po NAME,TYPE,FSTYPE,SIZE "$DEV" 2>/dev/null || true
  exit 1
fi
log "Root filesystem: $ROOT_PART (partitioned=$PARTITIONED)"
# Emit a machine-readable layout marker so the caller can pick the right Linode
# boot kernel/root_device (a partitionless disk boots via the Linode kernel; a
# partitioned disk boots via GRUB2 once we reinstall it below).
if [ "$PARTITIONED" -eq 1 ]; then
  echo "vmrepl-layout: partitioned"
else
  echo "vmrepl-layout: wholedisk"
fi
# Report the root device as it will appear on the launched Linode (the boot
# volume attaches as /dev/sda). For a partitioned disk this is /dev/sda<N> where
# N is the root partition number; for a whole-disk filesystem it is /dev/sda.
# The caller uses this for the Linode config's root_device — using /dev/sda for a
# partitioned disk makes a Linode-kernel boot panic with "unable to mount root".
ROOT_PARTNUM=""
if [ "$PARTITIONED" -eq 1 ]; then
  ROOT_PARTNUM="$(printf '%s' "$ROOT_PART" | grep -oE '[0-9]+$' || true)"
fi
if [ -n "$ROOT_PARTNUM" ]; then
  echo "vmrepl-root: /dev/sda${ROOT_PARTNUM}"
else
  echo "vmrepl-root: /dev/sda"
fi

# A block-level copy of a RUNNING server is crash-consistent (like a power-loss
# snapshot), so the filesystem's journal may need replaying before it will mount
# cleanly. Run the matching repair tool automatically so the migrated instance
# boots without manual intervention. (This only tidies a complete-but-dirty copy;
# it can't recover blocks that were never replicated.)
fsck_clean() {
  local part="$1" fstype
  [ -b "$part" ] || return 0
  fstype="$(blkid -s TYPE -o value "$part" 2>/dev/null || true)"
  case "$fstype" in
    ext2|ext3|ext4)
      command -v e2fsck >/dev/null 2>&1 || { log "e2fsck not available; skipping check on $part"; return 0; }
      log "Checking $part ($fstype) before mount"
      e2fsck -fy "$part" 2>&1 | sed 's/^/   [fsck] /' || true ;;
    xfs)
      command -v xfs_repair >/dev/null 2>&1 || { log "xfs_repair not available; skipping check on $part"; return 0; }
      log "Repairing $part (xfs) before mount"
      xfs_repair "$part" 2>&1 | sed 's/^/   [fsck] /' || true ;;
    *)
      log "no automatic fs check for $part (${fstype:-unknown}); skipping" ;;
  esac
}

fsck_clean "$ROOT_PART"
log "Mounting root and binding kernel filesystems"
mount "$ROOT_PART" "$MNT"
# If /boot is a separate partition listed in fstab, mount it too.
if grep -qE '^\S+\s+/boot\s' "$MNT/etc/fstab"; then
  BOOT_SRC=$(awk '$2=="/boot"{print $1}' "$MNT/etc/fstab" | head -1)
  BOOT_DEV=$(blkid -t "${BOOT_SRC#UUID=}" -o device 2>/dev/null || echo "")
  if [ -n "$BOOT_DEV" ]; then
    umount "$MNT/boot" 2>/dev/null || true
    fsck_clean "$BOOT_DEV"
    mount "$BOOT_DEV" "$MNT/boot" 2>/dev/null || true
  fi
fi
mount --bind /dev "$MNT/dev"
mount --bind /dev/pts "$MNT/dev/pts"
mount -t proc proc "$MNT/proc"
mount -t sysfs sys "$MNT/sys"
mount --bind /run "$MNT/run" 2>/dev/null || true

ROOT_UUID=$(blkid -s UUID -o value "$ROOT_PART")
log "Root filesystem UUID: $ROOT_UUID"

# Stage the conversion steps inside the chroot.
cat > "$MNT/root/.convert-inner.sh" <<INNER
#!/usr/bin/env bash
set -euo pipefail
ROOT_UUID="$ROOT_UUID"
log() { echo "   [chroot] \$*"; }

# 1) DNS so package tooling works inside the chroot.
echo 'nameserver 8.8.8.8' > /etc/resolv.conf 2>/dev/null || true

# 2) Serial console for Linode's Lish, plus DHCP-friendly kernel cmdline.
if [ -f /etc/default/grub ]; then
  log "Configuring GRUB for serial console + virtio"
  sed -i 's/^GRUB_CMDLINE_LINUX=.*/GRUB_CMDLINE_LINUX="console=ttyS0,19200n8 net.ifnames=0 biosdevname=0"/' /etc/default/grub || true
  grep -q '^GRUB_TERMINAL' /etc/default/grub || echo 'GRUB_TERMINAL="serial console"' >> /etc/default/grub
  grep -q '^GRUB_SERIAL_COMMAND' /etc/default/grub || echo 'GRUB_SERIAL_COMMAND="serial --speed=19200 --unit=0 --word=8 --parity=no --stop=1"' >> /etc/default/grub
  grep -q '^GRUB_TIMEOUT=' /etc/default/grub && sed -i 's/^GRUB_TIMEOUT=.*/GRUB_TIMEOUT=5/' /etc/default/grub
  # GRUB_DISABLE_OS_PROBER avoids noisy probes in a single-OS image.
  grep -q '^GRUB_DISABLE_OS_PROBER' /etc/default/grub || echo 'GRUB_DISABLE_OS_PROBER=true' >> /etc/default/grub
fi

# 3) Ensure virtio drivers are in the initramfs (critical: without virtio_blk /
#    virtio_pci the kernel can't see the disk on Linode and panics).
if command -v update-initramfs >/dev/null 2>&1; then
  log "Adding virtio modules to initramfs (initramfs-tools)"
  for m in virtio virtio_pci virtio_blk virtio_net virtio_scsi; do
    grep -qx "\$m" /etc/initramfs-tools/modules 2>/dev/null || echo "\$m" >> /etc/initramfs-tools/modules
  done
  update-initramfs -u -k all || true
elif command -v dracut >/dev/null 2>&1; then
  log "Rebuilding initramfs with virtio (dracut)"
  echo 'add_drivers+=" virtio_blk virtio_pci virtio_net virtio_scsi "' > /etc/dracut.conf.d/virtio.conf
  KVER=\$(ls /lib/modules | sort -V | tail -1)
  dracut -f --kver "\$KVER" || true
fi

# 4) Reinstall GRUB to the boot disk and (re)generate its config. This is what
#    makes Linode's "GRUB 2" boot mode work; if grub.cfg is missing/empty the
#    instance drops to a "grub>" prompt, so we VERIFY it and fail loudly instead
#    of silently leaving an unbootable disk.
DISK="\$(lsblk -no pkname "\$(findmnt -no SOURCE /)" 2>/dev/null | head -1)"
[ -n "\$DISK" ] && DISK="/dev/\$DISK" || DISK="$DEV"
PTTYPE="\$(blkid -s PTTYPE -o value "\$DISK" 2>/dev/null || true)"
log "Boot disk \$DISK (partition table: \${PTTYPE:-unknown})"
INSTALL=""; GCFG=""; MK=""
if command -v grub-install >/dev/null 2>&1; then
  INSTALL="grub-install"; GCFG="/boot/grub/grub.cfg"
  if command -v update-grub >/dev/null 2>&1; then MK="update-grub"; else MK="grub-mkconfig -o \$GCFG"; fi
elif command -v grub2-install >/dev/null 2>&1; then
  INSTALL="grub2-install"; GCFG="/boot/grub2/grub.cfg"; MK="grub2-mkconfig -o \$GCFG"
fi
if [ -n "\$INSTALL" ]; then
  log "Installing GRUB (BIOS/i386-pc) to \$DISK"
  \$INSTALL --target=i386-pc --recheck "\$DISK" 2>&1 | sed 's/^/   [grub] /' || \
    log "WARNING: grub-install failed (a GPT disk needs a small BIOS-boot/bios_grub partition for BIOS GRUB)"
  log "Generating \$GCFG"
  \$MK 2>&1 | sed 's/^/   [grub] /' || true
  if [ -s "\$GCFG" ] && grep -qE '^[[:space:]]*(linux|linux16|menuentry)' "\$GCFG"; then
    log "GRUB config OK: \$GCFG"
  else
    echo "   [chroot] ERROR: \$GCFG is missing or has no boot entries; the disk would drop to a grub> prompt" >&2
    exit 3
  fi
else
  log "no grub-install found — relying on the Linode kernel to boot the root filesystem"
fi

# 5) Network: let Linode's Network Helper manage it. Reset to DHCP and strip
#    source-specific persistent NIC naming so the NIC comes up as eth0.
log "Resetting network to DHCP / eth0"
rm -f /etc/udev/rules.d/70-persistent-net.rules 2>/dev/null || true
if [ -d /etc/netplan ]; then
  cat > /etc/netplan/01-linode.yaml <<NET
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
      dhcp6: true
NET
  chmod 600 /etc/netplan/01-linode.yaml || true
fi
if [ -f /etc/network/interfaces ]; then
  printf 'auto lo\niface lo inet loopback\nauto eth0\niface eth0 inet dhcp\n' > /etc/network/interfaces
fi
if [ -d /etc/sysconfig/network-scripts ]; then
  cat > /etc/sysconfig/network-scripts/ifcfg-eth0 <<NET
DEVICE=eth0
BOOTPROTO=dhcp
ONBOOT=yes
NET
fi

# 6) Enable serial getty so Lish gives you a console login.
systemctl enable serial-getty@ttyS0.service 2>/dev/null || true

# 7) Fresh machine-id so cloned hosts don't collide.
: > /etc/machine-id 2>/dev/null || true
rm -f /var/lib/dbus/machine-id 2>/dev/null || true

log "Inner conversion complete"
INNER
chmod +x "$MNT/root/.convert-inner.sh"

log "Entering chroot to perform conversion"
chroot "$MNT" /root/.convert-inner.sh
rm -f "$MNT/root/.convert-inner.sh"

log "Syncing"
sync
log "Conversion done. Next: in the Linode API/UI, create a config profile that"
log "boots this disk (Kernel = GRUB 2, or Direct Disk) with the raw disk as sda,"
log "then reboot out of rescue. See docs/CUTOVER.md."
