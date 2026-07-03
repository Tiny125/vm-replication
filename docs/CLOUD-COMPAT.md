# Cloud-Source Compatibility: AWS EC2, Azure VM, GCP Compute Engine

A deep validation of migrating **AWS EC2**, **Azure Virtual Machines**, and
**GCP Compute Engine** Linux servers to Linode with this tool — for **both**
boot methods (separate Block Storage **volume boot** and Linode local **disk
boot**).

**Methodology & confidence.** This is a code-path validation: every claim about
the tool's behavior was verified against the actual replication/cutover code
and `machine-convert.sh`, and cross-checked against each provider's documented
platform characteristics (device naming, boot firmware, agents, network
config). It is **not** a live test — no cloud instances were booted for it —
so each finding is tagged: ✅ **handled** (verified in code), ⚠️ **risk**
(real gap, mitigation described), 🧪 **verify live** (expected fine, confirm
on a real instance). A per-provider live-test checklist is at the end.

---

## 1. What the pipeline already handles (provider-agnostic, verified)

These apply to every source, cloud or on-prem:

| Area | Status | How |
|---|---|---|
| Block-level replication | ✅ | The agent reads the raw device; the filesystem/distro/provider is irrelevant to the copy itself. |
| Interrupted passes | ✅ | Delta passes apply **atomically** (staged, discarded whole on interruption) — a network blip or source power-off can never tear the target. |
| Wrong-disk / stale-agent protection | ✅ | Per-enrollment job id + device-size guard reject bad sessions at the handshake. |
| Serial console on Linode | ✅ | GRUB cmdline `console=ttyS0,19200n8`, `GRUB_TERMINAL/SERIAL_COMMAND`, `serial-getty@ttyS0` enabled. |
| Predictable-NIC renaming | ✅ | `net.ifnames=0 biosdevname=0` on the kernel cmdline **plus** a full network-config reset — covers EC2's `ens5`, GCP's `ens4`, Azure's `eth0`. |
| Network config reset to DHCP/eth0 | ✅ | netplan, systemd-networkd, ifupdown, NetworkManager, and RHEL `ifcfg-*` are all backed up and replaced with one authoritative DHCP config — the source's static IP/DNS cannot leak onto the new instance. |
| virtio drivers in the initramfs | ✅ | Injected for both **initramfs-tools** (Debian/Ubuntu) and **dracut** (RHEL family); all mainstream distro kernels ship the modules. |
| GRUB regeneration + verification | ✅ | For partitioned disks, GRUB config is regenerated and **verified to contain boot entries** — the convert fails loudly rather than leaving a `grub>` disk. |
| Separate `/boot` partition | ✅ | Mounted from the image's fstab before the chroot. |
| Stale swap entries | ✅ | fstab swap pointing at a non-migrated device (separate cloud swap disk) is disabled, avoiding a ~90s boot stall. |
| Root password / SSH key seeding | ✅ | Cutover dialog seeds root access so the instance is reachable without rescue surgery. |
| Fresh machine-id | ✅ | Reset so the migrated host doesn't collide with the still-existing source identity. |
| xfs roots (RHEL family) | ✅ | `xfs_repair` path exists alongside e2fsck. |

**Boot-firmware note that matters for all three clouds:** Linode's "GRUB 2"
boot mode runs a **host-side GRUB that reads the guest's `grub.cfg`** — it
does not chain the disk's MBR. So even a **UEFI-only source** (no BIOS boot
path on disk) generally boots on Linode, because the convert regenerates a
full `grub.cfg` and only needs that file to be valid. The in-chroot
`grub-install --target=i386-pc` may warn on GPT disks without a `bios_grub`
partition — that warning is tolerated by design; the hard requirement
(verified, loud failure) is a valid `grub.cfg`.

---

## 2. Provider-by-provider

### 2.1 AWS EC2

| Aspect | Finding |
|---|---|
| Device naming | 🧪 Nitro instances expose EBS as `/dev/nvme0n1` (partitions `nvme0n1p1…`); older Xen types use `/dev/xvda`. Enter the **whole disk** (`/dev/nvme0n1`, not `p1`) on the create form — the "find source details" helper prints it. The agent reads NVMe devices like any block device. |
| Boot firmware | ✅/🧪 Most x86 AMIs are BIOS; AL2023/Ubuntu UEFI-preferred AMIs are covered by the host-side GRUB 2 note above. |
| Root filesystem | ✅ Ubuntu (ext4) and Amazon Linux / RHEL (xfs, plain partition) convert fine. ⚠️ **LVM roots are NOT supported** (see §3.1). |
| Cloud agents | ⚠️ cloud-init (EC2 datasource), amazon-ssm-agent, hibinit-agent stay enabled (§3.2). |
| Instance-store (ephemeral) disks | ⚠️ Never add them as migration disks — they are not the OS disk and their data is disposable by design. Migrate **EBS** volumes only. |
| Network egress | 🧪 Default SG egress is allow-all; the source needs outbound TCP 8080 + 5000–5100 to the appliance. Private subnets need a NAT route. |
| ARM (Graviton) | ❌ **Cannot migrate** — Linode compute is x86_64; an aarch64 image will not boot. x86_64 instances only. |

### 2.2 Azure Virtual Machines

| Aspect | Finding |
|---|---|
| Device naming | 🧪 OS disk is `/dev/sda`. ⚠️ `/dev/sdb` is the **ephemeral resource disk** — never migrate it; migrate `/dev/sda` only. |
| Boot firmware | ✅ Gen1 VMs are BIOS (cleanest case). 🧪 Gen2 VMs are UEFI-only — expected to boot via Linode's host-side GRUB 2 after the convert regenerates `grub.cfg`; verify live. |
| Resource-disk fstab | ✅/🧪 Azure images mount the resource disk with `nofail` (no boot hang); waagent-managed swap on it disappears silently — the new stale-swap handling covers fstab-declared swap. |
| Cloud agents | ⚠️ **waagent (walinuxagent)** and cloud-init's Azure datasource stay enabled — the Azure datasource **polls the IMDS/wireserver at boot and can add minutes of delay** before giving up on a non-Azure platform. Highest agent-related risk of the three clouds (§3.2). |
| RHEL-family images | ⚠️ SELinux enforcing + our config writes → relabel needed (§3.3). |
| Network egress | 🧪 Default NSG allows outbound; same port requirements. |

### 2.3 GCP Compute Engine

| Aspect | Finding |
|---|---|
| Device naming | 🧪 Boot disk is `/dev/sda` (partitioned; newer images UEFI with GPT). |
| Boot firmware | 🧪 Newer GCP images are UEFI-only — same host-side GRUB 2 expectation as Azure Gen2; verify live. |
| Cloud agents | ⚠️ google-guest-agent + google-osconfig-agent stay enabled — they poll the GCP metadata server (absent on Linode), log errors, and normally manage **user accounts and SSH keys** from metadata (§3.2). |
| OS Login | ⚠️ If the project used **OS Login**, sshd/PAM are wired to Google's `google_authorized_keys`/`pam_oslogin` — on Linode those return nothing, so **metadata-based SSH users stop working**. Mitigation already built in: seed a root password/SSH key at cutover (the dialog fields); local accounts keep working. |
| Network egress | 🧪 Default VPC egress is allow; same port requirements. |
| ARM (Tau T2A) | ❌ Not migratable (x86_64 only), same as EC2 Graviton. |

---

## 3. Cross-cutting risks (the loopholes found)

### 3.1 ⚠️ LVM root filesystems are not supported (affects RHEL-style layouts)
`machine-convert.sh` locates the root by probing **partitions**; it never runs
`vgchange -ay`, so a root inside an LVM logical volume is not found and the
cutover aborts with "could not locate a root filesystem" (safely — nothing is
launched). Ubuntu/Debian/Amazon Linux/GCP/Azure marketplace images don't use
LVM by default, but custom RHEL/CentOS installs often do.
**Status:** aborts safely; **fix available** (activate VGs and probe `/dev/mapper/*`).

### 3.2 ⚠️ Cloud agents are left enabled on the migrated instance
cloud-init (provider datasource), Azure **waagent**, GCP **google-guest-agent**
/ osconfig, AWS **amazon-ssm-agent** all survive the migration and start on
Linode, where their platform APIs don't exist. Consequences range from log
noise (GCP/AWS) to **multi-minute first-boot delays** (Azure's datasource
polling) and, for GCP OS Login, broken metadata-managed SSH users. The network
config they'd normally manage is already neutralized by the convert's DHCP
reset, so they can't break connectivity.
**Status:** boot works but degraded UX; **fix available** (disable the
provider agents + write `/etc/cloud/cloud-init.disabled` during conversion).

### 3.3 ⚠️ SELinux-enforcing sources (RHEL family) may need a relabel
The convert writes files (network config, fstab, sshd edits) from outside the
guest's SELinux policy, so they land unlabeled. On an enforcing system that
can break `sshd`/`NetworkManager` on first boot.
**Status:** **fix available** (touch `/.autorelabel` when
`/etc/selinux/config` says enforcing — one extra reboot, correct labels).

### 3.4 ⚠️ Disk-boot fit check misses unshrinkable (partitioned) images
Disk-boot shrinks only **whole-disk ext4** images. Cloud images are
**partitioned**, so no shrink happens — and the fit check only runs when a
shrink reported a size. If the source disk is nominally the same size as the
plan's disk (Linode's actual disk is a sliver smaller), the rescue copy `dd`
fails at the very end and the cutover times out instead of failing fast.
**Practical rule until fixed:** for disk boot, pick a plan whose disk is
**comfortably larger** than the source disk (e.g. 30 GiB source → 80 GiB
plan). **Fix available** (fail fast when `sourceBytes > actualDiskMB` and no
shrink happened).
**Volume boot has no such constraint** — the volume is sized ≥ the source at
create time.

### 3.5 ❌ Architecture: x86_64 only
ARM sources (EC2 Graviton, GCP T2A, Azure Ampere) cannot boot on Linode.
Checked nowhere today — the failure would appear at first boot.
**Fix available** (agent reports `uname -m`; refuse enrollment of non-x86_64).

### 3.6 Method comparison for cloud sources

| | Volume boot | Disk boot |
|---|---|---|
| Size constraints | ✅ none beyond volume ≥ source | ⚠️ §3.4 — oversize the plan |
| Extra manual step | none | one Lish paste (rescue copy) |
| Recovery/debug surface | volume attachable anywhere | rescue mode on the instance |
| Recommendation | **Use first** for a new provider | Use once volume-boot has proven the image converts and boots |

Both methods share the replication, conversion, and agent/SELinux/LVM
considerations above — the differences are only in how the image reaches the
instance.

---

## 4. Live-test checklist (per provider, per method)

Run one cheap instance per provider (e.g. Ubuntu 22.04 and a RHEL-family
image), then for **volume boot first, disk boot second**:

1. Enroll: does the printed device match (`/dev/nvme0n1` on EC2 Nitro,
   `/dev/sda` elsewhere)? Agent connects, identity-checked, full sync ≈ disk's
   used+zero blocks at expected speed.
2. Cutover: freeze banner → power-off banner → (disk boot: rescue paste) →
   instance boots.
3. On the migrated instance verify: SSH with the seeded root credentials;
   `ip a` shows `eth0` with a Linode DHCP address; `systemd-analyze blame` for
   agent-induced boot delays; `journalctl -p err -b` for cloud-agent noise;
   swap state (`swapon --show`); on RHEL-family, `getenforce` + service health;
   application starts and serves.
4. Note first-boot wall-clock vs a native Linode — >2 min extra usually means
   the cloud agents (§3.2) are worth disabling.

---

*Last validated: code-level, against the current `main`. Re-validate after
material changes to `machine-convert.sh` or the cutover flows.*
