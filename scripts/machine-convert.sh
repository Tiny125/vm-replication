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

# Never trust the inherited PATH: applianced runs us with whatever environment it
# was started in (a systemd unit, a bare shell, a container), and a PATH missing
# /usr/bin makes coreutils like `ln` vanish — which later breaks the chroot's
# update-initramfs and our own symlinks (exit 127), masquerading as a "filesystem
# inconsistent" boot failure. Pin a known-good PATH up front.
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

DEV="${1:-/dev/sda}"
MNT="$(mktemp -d)"
KPARTX_USED=0
log() { echo ">> $*"; }
# ensure_dir_mount guarantees a chroot pseudo-filesystem mount point exists AS A
# DIRECTORY before we mount onto it. A crash-consistent copy (or a stripped-down
# source image) can leave /proc, /sys, /run — or even /dev — missing, or present
# as a non-directory. Mounting onto a non-directory fails with
#   mount: <mnt>/proc: mount point is not a directory
# which aborts the conversion of an otherwise-healthy disk (exit 32) and gets
# mis-reported as an "inconsistent filesystem". Drop any stray non-directory,
# then create the directory so the mount always has a valid target.
ensure_dir_mount() {
  local d="$1"
  if [ -e "$d" ] && [ ! -d "$d" ]; then
    rm -f "$d"
  fi
  mkdir -p "$d"
}
cleanup() {
  for m in dev/pts dev proc sys run; do
    mountpoint -q "$MNT/$m" && umount -l "$MNT/$m" 2>/dev/null || true
  done
  mountpoint -q "$MNT" && umount -l "$MNT" 2>/dev/null || true
  rmdir "$MNT" 2>/dev/null || true
  [ "$KPARTX_USED" = 1 ] && kpartx -d "$DEV" 2>/dev/null || true
}
# Test hook: sourcing with VMREPL_CONVERT_LIB=1 loads the helper functions above
# without running the conversion (no root, block device, traps or mounts), so
# scripts/machine-convert-test.sh can exercise ensure_dir_mount in isolation.
if [ -n "${VMREPL_CONVERT_LIB:-}" ]; then return 0 2>/dev/null || exit 0; fi
trap cleanup EXIT

[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
# Resolve a /dev/disk/by-id/... symlink (how Linode volumes attach) to the
# canonical device so partition re-read and node names are predictable.
DEV="$(readlink -f "$DEV" 2>/dev/null || echo "$DEV")"
[ -b "$DEV" ] || { echo "$DEV is not a block device"; exit 1; }

# Shrink-only mode (disk-mode cutover). The appliance calls this AFTER it has
# created the instance's local disk, so the shrink can be sized to the real disk
# Linode handed it rather than a guess against the plan's nominal size. Shrink a
# whole-disk ext{2,3,4} filesystem on $DEV to VMREPL_SHRINK_MB MiB so the 1:1 copy
# onto that local disk fits, print a "vmrepl-shrink:" result, and exit. Other
# layouts are left untouched (the caller then requires a larger plan). Does not
# convert anything else.
if [ -n "${VMREPL_SHRINK_ONLY:-}" ]; then
  target="${VMREPL_SHRINK_MB:-0}"
  # resize2fs needs the device unmounted; the appliance unmounted it after
  # conversion, but a lazy unmount can lag, so make sure (and retry).
  for _t in 1 2 3 4 5; do
    findmnt -nro TARGET -S "$DEV" >/dev/null 2>&1 || break
    umount "$DEV" 2>/dev/null || true
    sleep 1
  done
  if findmnt -nro TARGET -S "$DEV" >/dev/null 2>&1; then
    log "could not unmount $DEV to shrink it (still busy); skipping shrink"
    echo "vmrepl-shrink: failed unmount"
    exit 0
  fi
  # Current filesystem size in MiB, read straight from the superblock.
  fsmib() {
    bc=$(tune2fs -l "$DEV" 2>/dev/null | awk -F: '/Block count/{gsub(/ /,"",$2);print $2}')
    bz=$(tune2fs -l "$DEV" 2>/dev/null | awk -F: '/Block size/{gsub(/ /,"",$2);print $2}')
    if [ -n "$bc" ] && [ -n "$bz" ]; then echo $(( bc * bz / 1048576 )); else echo 0; fi
  }
  devmib=$(( $(blockdev --getsize64 "$DEV" 2>/dev/null || echo 0) / 1048576 ))
  fstype="$(blkid -s TYPE -o value "$DEV" 2>/dev/null || true)"
  case "$fstype" in
    ext2|ext3|ext4)
      if [ "$target" -le 0 ] || [ "$target" -ge "$devmib" ]; then
        log "shrink target ${target}MiB is not smaller than $DEV (${devmib}MiB); nothing to shrink"
        echo "vmrepl-shrink: ok ${devmib}M"
        exit 0
      fi
      log "Shrinking $fstype on $DEV to fit the local disk (target ${target}MiB)"
      rc=0; e2fsck -fy "$DEV" >/dev/null 2>&1 || rc=$?
      if [ "$rc" -ge 4 ]; then
        log "e2fsck could not clean $DEV (rc=$rc); skipping shrink"
        echo "vmrepl-shrink: failed e2fsck rc=$rc"
        exit 0
      fi
      rout="$(resize2fs "$DEV" "${target}M" 2>&1)"; rrc=$?
      echo "$rout" | sed 's/^/   [resize2fs] /'
      cur=$(fsmib)
      # Don't trust resize2fs's exit code alone: some images report success
      # ("Nothing to do") without actually shrinking, which then fails the in-guest
      # copy with "does not fit". If the filesystem is still larger than the target,
      # force a minimize so the copy is guaranteed to fit; the first normal boot
      # grows the root back to fill the whole local disk.
      if [ "$rrc" -eq 0 ] && [ "${cur:-0}" -gt "$target" ]; then
        log "targeted shrink left the filesystem at ${cur}MiB (> ${target}); minimizing instead"
        rout="$(resize2fs -M "$DEV" 2>&1)"; rrc=$?
        echo "$rout" | sed 's/^/   [resize2fs -M] /'
        cur=$(fsmib)
      fi
      # Flush so the (subsequent) volume clone captures the shrunk filesystem, not
      # stale cached blocks from before the resize.
      sync; blockdev --flushbufs "$DEV" 2>/dev/null || true
      if [ "$rrc" -eq 0 ] && [ "${cur:-0}" -gt 0 ] && [ "${cur:-0}" -le "$target" ]; then
        echo "vmrepl-shrink: ok ${cur}M"
      else
        echo "vmrepl-shrink: failed resize2fs rc=$rrc fs=${cur:-?}M $(echo "$rout" | tr '\n' ' ' | tail -c 140)"
      fi ;;
    *)
      log "filesystem on $DEV is ${fstype:-unknown}, not a whole-disk ext fs - cannot shrink to fit a smaller local disk"
      echo "vmrepl-shrink: skipped ${fstype:-unknown}" ;;
  esac
  exit 0
fi

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
# Make sure every pseudo-filesystem mount point exists as a directory first, so
# the binds/mounts below can't fail with "mount point is not a directory" on a
# source whose /dev, /proc, /sys or /run came across missing or as a stray file.
for _pd in dev dev/pts proc sys run; do
  ensure_dir_mount "$MNT/$_pd"
done
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
# Inside the chroot, resolve binaries from the TARGET filesystem with a complete,
# known-good PATH. chroot does not reset PATH, so without this we inherit the
# appliance's (possibly /usr/bin-less) PATH and \`ln\`, \`update-initramfs\` hooks,
# etc. fail with "command not found" (exit 127), aborting the conversion.
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
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
  # If the distro has no netplan, networkd is the only manager — give it a DHCP
  # config for eth0 so we don't leave the interface with no configuration.
  if [ ! -d /etc/netplan ]; then
    cat > /etc/systemd/network/10-vmrepl-eth0.network <<NET
[Match]
Name=eth0

[Network]
DHCP=yes
NET
  fi
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

# 9) Local-disk boot installer (disk-mode cutover only; gated by env). Installs a
#    one-shot service that runs ONLY while booted from a Linode Volume: it copies
#    the volume onto the instance's blank local disk and powers off, so the
#    appliance can then boot the instance from that local disk. On a normal
#    (local-disk) boot the guard makes it a no-op, so it never fires again.
if [ -n "\${VMREPL_DISK_INSTALL:-}" ]; then
  log "Installing local-disk boot one-shot service"
  cat > /usr/local/sbin/vmrepl-diskinstall.sh <<'DISKINSTALL'
#!/bin/sh
# Copy the root Linode Volume onto the local disk, then power off. Runs only
# during the disk-mode cutover's volume-boot phase (guarded below). Logs to the
# serial console so progress is visible over Lish.
exec >>/var/log/vmrepl-diskinstall.log 2>&1
log() { echo "vmrepl-diskinstall: \$*" > /dev/console 2>/dev/null; echo "vmrepl-diskinstall: \$*"; }
# Fast no-op on a normal boot: the install phase has TWO real disks (the source
# volume + the blank local disk); a normal boot of the migrated instance has only
# one. This avoids any wait/delay on every later reboot. (Exclude zero-size nbd
# devices, which show up as type "disk".)
ndisks=\$(lsblk -dnbo NAME,TYPE,SIZE 2>/dev/null | awk '\$2=="disk" && \$3+0>0 {n++} END{print n+0}')
if [ "\${ndisks:-0}" -lt 2 ]; then
  # Normal boot of the migrated instance. The root filesystem may be smaller than
  # its disk — we shrank it to fit the cutover, or a smaller image was copied onto
  # a larger plan — so grow it to fill the disk. Online grow on the mounted root is
  # safe and idempotent (a no-op once the fs already fills the device).
  rootsrc=\$(findmnt -no SOURCE / 2>/dev/null)
  rootfs=\$(findmnt -no FSTYPE / 2>/dev/null)
  case "\$rootfs" in
    ext2|ext3|ext4) [ -b "\$rootsrc" ] && resize2fs "\$rootsrc" >/dev/null 2>&1 && log "grew \$rootfs root (\$rootsrc) to fill the disk" || true ;;
    xfs) command -v xfs_growfs >/dev/null 2>&1 && xfs_growfs / >/dev/null 2>&1 && log "grew xfs root to fill the disk" || true ;;
  esac
  log "only \${ndisks:-0} disk(s) -> normal boot, nothing else to do"
  exit 0
fi
# Install phase: the Linode Volume by-id symlinks can lag early boot; wait for them.
i=0
while [ \$i -lt 15 ]; do
  ls /dev/disk/by-id/scsi-0Linode_Volume_* >/dev/null 2>&1 && break
  i=\$((i+1)); sleep 2
done
rootsrc=\$(findmnt -no SOURCE / 2>/dev/null)
rootdisk=\$(lsblk -no pkname "\$rootsrc" 2>/dev/null | head -1)
[ -n "\$rootdisk" ] || rootdisk=\$(basename "\$rootsrc")
log "boot: root=\$rootsrc disk=\$rootdisk"
# Guard: only act when root is backed by a Linode Volume (the transient install
# boot). On the final local-disk boot root is NOT a volume, so this no-ops and
# can never re-fire.
if ! ls -l /dev/disk/by-id/scsi-0Linode_Volume_* 2>/dev/null | grep -q "/\${rootdisk}\$"; then
  log "root is not a Linode Volume -> final boot, nothing to do"
  exit 0
fi
# Target = a whole local disk that is NOT a Linode Volume.
target=
for d in \$(lsblk -dno NAME,TYPE | awk '\$2=="disk"{print \$1}'); do
  [ "\$d" = "\$rootdisk" ] && continue
  if ! ls -l /dev/disk/by-id/scsi-0Linode_Volume_* 2>/dev/null | grep -q "/\${d}\$"; then
    target="/dev/\$d"; break
  fi
done
if [ -z "\$target" ]; then
  log "no local target disk found; leaving instance on the volume"
  exit 0
fi
srcsz=\$(blockdev --getsize64 "/dev/\$rootdisk" 2>/dev/null)
tgtsz=\$(blockdev --getsize64 "\$target" 2>/dev/null)
log "copying /dev/\$rootdisk (\${srcsz:-?} B) -> \$target (\${tgtsz:-?} B); this can take a few minutes"
sync
rc=0
if [ -n "\$srcsz" ] && [ -n "\$tgtsz" ] && [ "\$tgtsz" -lt "\$srcsz" ]; then
  # The local disk is smaller than the source device. This is only safe if the
  # FILESYSTEM fits (the appliance shrinks a whole-disk ext fs for exactly this);
  # never truncate a filesystem that doesn't fit — that produces a broken boot.
  blkcnt=\$(tune2fs -l "/dev/\$rootdisk" 2>/dev/null | awk -F: '/Block count/{gsub(/ /,"",\$2);print \$2}')
  blksz=\$(tune2fs -l "/dev/\$rootdisk" 2>/dev/null | awk -F: '/Block size/{gsub(/ /,"",\$2);print \$2}')
  fsbytes=0; [ -n "\$blkcnt" ] && [ -n "\$blksz" ] && fsbytes=\$((blkcnt*blksz))
  if [ "\$fsbytes" -gt 0 ] && [ "\$fsbytes" -le "\$tgtsz" ]; then
    cnt=\$((tgtsz/1048576))
    log "filesystem is \$fsbytes B (fits); copying the first \${cnt} MiB onto the local disk"
    derr=\$(dd if="/dev/\$rootdisk" of="\$target" bs=1M count=\$cnt conv=fsync 2>&1) || rc=\$?
  else
    log "ERROR: image filesystem (\${fsbytes} B) does not fit the local disk (\$tgtsz B) — recreate the migration on a larger plan"
    exit 1
  fi
else
  derr=\$(dd if="/dev/\$rootdisk" of="\$target" bs=64M conv=fsync 2>&1) || rc=\$?
fi
if [ "\$rc" -ne 0 ]; then
  log "dd failed (rc=\$rc): \$(echo "\$derr" | tail -2 | tr '\n' ' ')"
  exit 1
fi
sync
log "copy complete; powering off so the appliance can switch to the local disk"
sleep 2
systemctl poweroff -i 2>/dev/null || true
sleep 5
poweroff -f 2>/dev/null || halt -f 2>/dev/null || { echo o > /proc/sysrq-trigger 2>/dev/null; }
DISKINSTALL
  chmod +x /usr/local/sbin/vmrepl-diskinstall.sh
  cat > /etc/systemd/system/vmrepl-diskinstall.service <<'DISKUNIT'
[Unit]
Description=vm-replication local-disk install (volume-boot only, one-shot)
After=local-fs.target systemd-udev-settle.service
Wants=systemd-udev-settle.service
Before=multi-user.target getty.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/vmrepl-diskinstall.sh
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
DISKUNIT
  # Enable by creating the wants symlink directly: this works offline in the
  # convert chroot, whereas `systemctl enable` needs a running systemd and often
  # fails here (the previous cause of the unit never running). Try systemctl too.
  mkdir -p /etc/systemd/system/multi-user.target.wants
  ln -sf /etc/systemd/system/vmrepl-diskinstall.service /etc/systemd/system/multi-user.target.wants/vmrepl-diskinstall.service
  systemctl enable vmrepl-diskinstall.service 2>/dev/null || true
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
