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
# Optional console/SSH access for the migrated image (seeded into root) can be
# supplied via the environment so it never appears on the command line:
#
#     sudo VMREPL_ROOT_PASSWORD='s3cret' \
#          VMREPL_SSH_AUTHORIZED_KEY='ssh-ed25519 AAAA... you@host' \
#          ./machine-convert.sh /dev/sda
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

# A block-level copy of a RUNNING server is crash-consistent (like a power-loss
# snapshot), so the filesystem's journal may need replaying before it will even
# mount. Repair it with the matching tool BEFORE we try to mount it — both for
# the read-only probe below and the real mount — otherwise a dirty journal makes
# the mount fail and root detection wrongly reports "could not locate a root
# filesystem". (This only tidies a complete-but-dirty copy; it can't recover
# blocks that were never replicated.)
fsck_clean() {
  local part="$1" fstype rc out sb repaired
  [ -b "$part" ] || return 0
  fstype="$(blkid -s TYPE -o value "$part" 2>/dev/null || true)"
  out="$(mktemp)"
  case "$fstype" in
    ext2|ext3|ext4)
      command -v e2fsck >/dev/null 2>&1 || { log "e2fsck not available; skipping check on $part"; rm -f "$out"; return 0; }
      log "Checking $part ($fstype) before mount"
      rc=0; e2fsck -fy "$part" >"$out" 2>&1 || rc=$?
      sed 's/^/   [fsck] /' "$out"
      # e2fsck: 0=clean, 1=fixed, 2=fixed(reboot) are success; >=4 means errors
      # remained or the PRIMARY superblock is bad ("Bad magic number in
      # super-block"). Retry from a backup superblock, which often recovers a
      # crash-consistent copy whose primary superblock was caught mid-write.
      if [ "$rc" -ge 4 ]; then
        log "primary superblock unusable (e2fsck rc=$rc); retrying from a backup superblock"
        repaired=0
        for sb in 32768 8193 98304 163840 229376; do
          rc=0; e2fsck -fy -b "$sb" "$part" >"$out" 2>&1 || rc=$?
          sed 's/^/   [fsck] /' "$out"
          if [ "$rc" -lt 4 ]; then
            log "recovered $part using backup superblock $sb; re-checking"
            rc=0; e2fsck -fy "$part" >"$out" 2>&1 || rc=$?
            sed 's/^/   [fsck] /' "$out"
            repaired=1; break
          fi
        done
        [ "$repaired" = 1 ] || log "WARNING: $part could not be repaired (rc=$rc); the copy is likely inconsistent (a live source copied over many minutes). A fresh full sync of a quiesced/idle source is the reliable fix."
      fi ;;
    xfs)
      command -v xfs_repair >/dev/null 2>&1 || { log "xfs_repair not available; skipping check on $part"; rm -f "$out"; return 0; }
      log "Repairing $part (xfs) before mount"
      rc=0; xfs_repair "$part" >"$out" 2>&1 || rc=$?
      sed 's/^/   [fsck] /' "$out"
      if [ "$rc" -ne 0 ]; then
        log "xfs_repair failed (rc=$rc); retrying with -L to zap a dirty log"
        xfs_repair -L "$part" 2>&1 | sed 's/^/   [fsck] /' || true
      fi ;;
    *)
      log "no automatic fs check for $part (${fstype:-unknown}); skipping" ;;
  esac
  rm -f "$out"
}

# Find the root: the candidate carrying /etc/fstab and a real root tree. fsck
# each candidate first so a dirty journal doesn't block the probe mount.
ROOT_PART=""
is_root() { [ -f "$1/etc/fstab" ] && { [ -d "$1/sbin" ] || [ -L "$1/sbin" ] || [ -d "$1/bin" ] || [ -L "$1/bin" ]; }; }
for p in "${PARTS[@]}"; do
  [ -b "$p" ] || continue
  umount "$MNT" 2>/dev/null || true
  fsck_clean "$p"
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

# 4) Bootloader. A PARTITIONLESS whole-disk filesystem has no partition table to
#    install GRUB into; it boots via the Linode-supplied kernel instead, so skip
#    GRUB entirely here. For a PARTITIONED disk, reinstall GRUB and (re)generate
#    its config so Linode's "GRUB 2" mode works — and VERIFY grub.cfg so we fail
#    loudly instead of silently leaving a disk that drops to a "grub>" prompt.
if [ "$PARTITIONED" = "1" ]; then
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
else
  log "partitionless whole-disk filesystem — booting via the Linode kernel; skipping GRUB install"
fi

# 5) Network: reset to DHCP on eth0 and REMOVE the source's network config so it
#    cannot pin the OLD IP / nameservers on the new Linode. This is critical: a
#    leftover static config (e.g. the source's netplan 01-netcfg.yaml) makes the
#    new instance claim the source's IP — netplan merges every *.yaml and a
#    higher-sorting filename wins, so 01-netcfg.yaml overrides our 01-linode.yaml.
#    The result is no working connectivity (failed pings) and DNS timeouts that
#    make logins and commands crawl. We back up whatever was there and write one
#    authoritative DHCP config.
log "Resetting network to DHCP / eth0 (removing source-specific static config)"
rm -f /etc/udev/rules.d/70-persistent-net.rules 2>/dev/null || true
NETBAK="/var/lib/vmrepl-netbak"
mkdir -p "\$NETBAK" 2>/dev/null || true
if [ -d /etc/netplan ]; then
  for f in /etc/netplan/*.yaml /etc/netplan/*.yml; do
    [ -e "\$f" ] || continue
    mv "\$f" "\$NETBAK/" 2>/dev/null || rm -f "\$f"
  done
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
# systemd-networkd: a hand-written/Linode-written .network file (e.g.
# /etc/systemd/network/05-eth0.network) carries a static IP and, sorting before
# netplan's generated /run/systemd/network/10-netplan-*.network, WINS over our
# DHCP config. Source images that configure networking via networkd directly hit
# exactly this — the new instance comes up on the source's IP. Move these aside
# so netplan's DHCP config takes effect.
if [ -d /etc/systemd/network ]; then
  for f in /etc/systemd/network/*.network /etc/systemd/network/*.netdev; do
    [ -e "\$f" ] || continue
    mv "\$f" "\$NETBAK/" 2>/dev/null || rm -f "\$f"
  done
fi
if [ -f /etc/network/interfaces ]; then
  printf 'auto lo\niface lo inet loopback\nauto eth0\niface eth0 inet dhcp\n' > /etc/network/interfaces
fi
if [ -d /etc/network/interfaces.d ]; then
  for f in /etc/network/interfaces.d/*; do
    [ -e "\$f" ] || continue
    mv "\$f" "\$NETBAK/" 2>/dev/null || rm -f "\$f"
  done
fi
# A saved NetworkManager connection can also pin a static IP; move them aside so
# NM falls back to DHCP (servers normally just have eth0).
if [ -d /etc/NetworkManager/system-connections ]; then
  for f in /etc/NetworkManager/system-connections/*; do
    [ -e "\$f" ] || continue
    mv "\$f" "\$NETBAK/" 2>/dev/null || rm -f "\$f"
  done
fi
if [ -d /etc/sysconfig/network-scripts ]; then
  for f in /etc/sysconfig/network-scripts/ifcfg-*; do
    [ -e "\$f" ] || continue
    case "\$f" in */ifcfg-lo) continue ;; esac
    mv "\$f" "\$NETBAK/" 2>/dev/null || rm -f "\$f"
  done
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

# 8) Console/SSH access requested at cutover. Migrated disks carry the source's
#    accounts, and cloud images usually keep root locked/password-less, so the
#    Lish serial console has nothing to log in as. Seed whatever the operator
#    provided (passed via the environment so it is never written to this script).
if [ -n "\${VMREPL_ROOT_PASSWORD:-}" ]; then
  log "Setting and unlocking the root password"
  echo "root:\${VMREPL_ROOT_PASSWORD}" | chpasswd 2>/dev/null || log "WARNING: could not set root password"
  passwd -u root >/dev/null 2>&1 || true
fi
if [ -n "\${VMREPL_SSH_AUTHORIZED_KEY:-}" ]; then
  log "Installing the provided SSH public key for root"
  mkdir -p /root/.ssh && chmod 700 /root/.ssh
  printf '%s\n' "\${VMREPL_SSH_AUTHORIZED_KEY}" >> /root/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys
  # Permit key-based root SSH if the source config forbids it outright.
  if [ -f /etc/ssh/sshd_config ]; then
    if grep -qiE '^[[:space:]]*PermitRootLogin[[:space:]]+no' /etc/ssh/sshd_config; then
      sed -i 's/^[[:space:]]*PermitRootLogin[[:space:]]\+no/PermitRootLogin prohibit-password/I' /etc/ssh/sshd_config
    elif ! grep -qiE '^[[:space:]]*PermitRootLogin' /etc/ssh/sshd_config; then
      echo 'PermitRootLogin prohibit-password' >> /etc/ssh/sshd_config
    fi
  fi
fi

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
