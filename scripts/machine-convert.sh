#!/usr/bin/env bash
# machine-convert.sh — make a replicated disk bootable on Linode.
#
# Replication gives you a byte-identical copy of the source disk, but a copy is
# not automatically bootable on Linode's KVM/QEMU platform. This script performs
# the "machine conversion" (the equivalent of AWS MGN's post-launch conversion):
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
log() { echo ">> $*"; }
cleanup() {
  for m in dev/pts dev proc sys run; do
    mountpoint -q "$MNT/$m" && umount -l "$MNT/$m" 2>/dev/null || true
  done
  mountpoint -q "$MNT" && umount -l "$MNT" 2>/dev/null || true
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
[ -b "$DEV" ] || { echo "$DEV is not a block device"; exit 1; }

log "Re-reading partition table on $DEV"
partprobe "$DEV" 2>/dev/null || true
sleep 1

# Find the root partition: largest ext4/xfs/btrfs partition with /etc/fstab.
ROOT_PART=""
mapfile -t PARTS < <(lsblk -lnpo NAME,FSTYPE "$DEV" | awk '$2 ~ /ext4|ext3|xfs|btrfs/ {print $1}')
for p in "${PARTS[@]}"; do
  umount "$MNT" 2>/dev/null || true
  if mount -o ro "$p" "$MNT" 2>/dev/null && [ -f "$MNT/etc/fstab" ] && [ -d "$MNT/boot" ]; then
    ROOT_PART="$p"; umount "$MNT"; break
  fi
  umount "$MNT" 2>/dev/null || true
done
[ -n "$ROOT_PART" ] || { echo "could not locate a root partition with /etc/fstab on $DEV"; exit 1; }
log "Root partition: $ROOT_PART"

log "Mounting root and binding kernel filesystems"
mount "$ROOT_PART" "$MNT"
# If /boot is a separate partition listed in fstab, mount it too.
if grep -qE '^\S+\s+/boot\s' "$MNT/etc/fstab"; then
  BOOT_SRC=$(awk '$2=="/boot"{print $1}' "$MNT/etc/fstab" | head -1)
  BOOT_DEV=$(blkid -t "${BOOT_SRC#UUID=}" -o device 2>/dev/null || echo "")
  [ -n "$BOOT_DEV" ] && mount "$BOOT_DEV" "$MNT/boot" 2>/dev/null || true
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

# 4) Reinstall + regenerate GRUB so it points at this disk.
DISK="\$(lsblk -no pkname "\$(findmnt -no SOURCE /)" 2>/dev/null | head -1)"
[ -n "\$DISK" ] && DISK="/dev/\$DISK" || DISK="$DEV"
if command -v grub-install >/dev/null 2>&1; then
  log "Installing GRUB to \$DISK (BIOS)"
  grub-install --target=i386-pc "\$DISK" || true
  update-grub || grub-mkconfig -o /boot/grub/grub.cfg || true
elif command -v grub2-install >/dev/null 2>&1; then
  log "Installing GRUB2 to \$DISK (BIOS)"
  grub2-install "\$DISK" || true
  grub2-mkconfig -o /boot/grub2/grub.cfg || true
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
