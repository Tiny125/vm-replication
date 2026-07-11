# Turnkey Appliance — Web Console Migration

This is the turnkey way to use the **vm-replication tool**: install one service on a
**replication server** (a Linode), open a **web console**, and drive migrations
from the browser. No manual cert copying or command crafting — the console
generates a one-line command for each source.

> **The same guide, with screenshots of every step, is served by your own
> replication server at `https://<ip>:8080/documentation`** (also linked from
> the console's header). This file is the Markdown companion with extra sizing
> and operational detail.

For the manual/CLI workflow instead, see [`GETTING_STARTED.md`](docs/GETTING_STARTED.md).

---

## The flow at a glance

```
 ┌─ Replication server (a Linode) ──────────────────────────────┐
 │  one command installs everything → prints a password          │
 │  ┌────────────────────────────────────────────────────────┐  │
 │  │  Web console (https://<ip>:8080) ← log in with password │  │
 │  │   • enter source details → get a copy-paste command     │  │
 │  │   • watch replication status + validation checks        │  │
 │  │   • click "Start migration" → produces a Linode Image   │  │
 │  └────────────────────────────────────────────────────────┘  │
 │  embeds one receiver per migration; data lands on a per-source │
 │  Block Storage volume attached to this server                  │
 └───────────────────────────────────────────────────────────────┘
              ▲ enrollment command (curl | sudo bash)
              │
        ┌─────┴───────┐
        │ Source server│  runs the agent (installed by the command)
        └─────────────┘
```

---

## Prerequisites — sizing the replication server

One appliance migrates **many servers (and many disks) in parallel** — each
migration/disk gets its own receiver port, its own Block Storage volume, and its
own worker. Two things to size: the **appliance instance** (compute) and your
**Block Storage** (where the data lands).

**Key fact:** replicated data lands on **per-disk Block Storage volumes**, *not*
on the appliance's own boot disk — so the appliance instance itself stays small;
your Block Storage scales with the sources.

### Appliance instance (compute)

| Concurrency | Recommended plan |
|---|---|
| 1–3 disks replicating at once | **2 vCPU / 4 GB** shared (e.g. `g6-standard-2`) |
| 4–8 disks, or large/fast links | **Dedicated 4–8 vCPU / 8–16 GB** (e.g. `g6-dedicated-4`) |

- **CPU is the scaling factor** — each in-flight transfer does SHA-256
  verification + decompression at line rate. Prefer **Dedicated CPU** for many
  concurrent full syncs. (A multi-disk source counts as multiple concurrent
  transfers — an EC2 with 3 disks ≈ 3 workers.)
- **RAM is tiny:** a few MB per active transfer plus ~8 MB of checkpoint per
  **1 TB** of source. 4 GB is comfortable; 8 GB for heavy parallelism.
- **Network** (source→appliance bandwidth) is usually the real bottleneck, not
  the appliance.

### Appliance boot disk

Small — binaries, the SQLite DB, certs. **25–50 GB is plenty.** ⚠️ Only if you
run *without* a Linode token (file-fallback mode) does data land on the boot
disk; with a token (the normal path) it goes to the volumes.

### Block Storage to provision (this is what scales)

> peak Block Storage ≈ **sum of all source disk sizes replicating concurrently**
> + **the largest single disk again** during its clone at cutover.

Example: migrating one EC2 with 3 EBS volumes of 80 + 200 + 200 GB → ~480 GB of
volumes during replication, peaking ~680 GB while the 200 GB disk clones. Make
sure your **account Block Storage quota** covers it (raise it via a support
ticket if needed).

### Limits to know

- **Volumes attachable to one Linode** caps simultaneous disks per appliance
  (Linode allows up to 8 here; the launched instance also uses `sda`–`sdh`, so a
  **single migration is limited to 8 disks**). For more parallelism, run more
  appliances or migrate in batches.
- The installer opens receiver ports **5000–5100**; that covers ~100 disks over
  the appliance's lifetime. Widen the firewall range if you'll exceed that.

---

## 1. Stand up the replication server

Create a Linode (Ubuntu/Debian/RHEL-family — see sizing above), SSH in as root,
and run:

```bash
git clone https://github.com/Tiny125/vm-replication.git
cd vm-replication
sudo scripts/install-replication-server.sh
```

The installer **bootstraps its own dependencies** — it installs `git`, `make`,
`gcc`, `curl`, `openssl`, `jq`, `tar`, and a recent **Go** toolchain via the
system package manager (apt/dnf/yum/zypper), then builds the binaries. A bare
server with internet access is all you need.

It builds the binaries, generates certificates and an **admin password**,
installs a systemd service (`applianced`), and prints:

```
================ REPLICATION SERVER READY ================
 Console:   https://203.0.113.10:8080
 Password:  681af4b11221bacb88e34080
 Cert SHA-256 (verify this in your browser's certificate dialog):
   AB:CD:...:EF
...
```

Options: `--public-host <ip>` (if auto-detect is wrong), `--region us-ord`,
`--port 8080`.

**Sessions & password recovery.** A console session lasts **12 hours from
sign-in** (a fixed lifetime — it is not extended by activity); after that you'll
be asked to sign in again. Signing out of the console only clears your browser
session — it never stops a migration. Replication runs in the `applianced`
service independent of console logins, so sign-out, a timeout, or a closed
browser leaves every migration running. Forgot the console password? Retrieve
it on the replication server without disturbing anything:

```
sudo /usr/local/bin/applianced -data-dir /var/lib/vm-repl -show-password
```

This only reads the saved password file, so it's safe to run while the service
is live. (The password is stored only as a hash; the plaintext lives in
`<data-dir>/initial-admin-password.txt`. If that file was deleted it can't be
recovered without resetting.)

> The console is served over **HTTPS** with a self-signed certificate generated
> at install, so the password and Linode token are encrypted in transit. The
> replication **data plane is always mutual TLS**, and the Linode token (step 3)
> is stored **encrypted at rest**. Restrict the console port to trusted networks
> where you can. (Advanced: pass `--insecure-http` to serve plain HTTP behind
> your own TLS-terminating proxy.)

---

## 2. Open the console and sign in

Browse to `https://<replication-server-ip>:8080`. Because the certificate is
self-signed, your browser warns on first visit — that's expected. Before
entering the password, open the browser's **certificate dialog** and confirm the
**SHA-256 fingerprint matches** the one the installer printed (also in
`journalctl -u applianced`). Then sign in with the generated password (also saved
at `/var/lib/vm-repl/initial-admin-password.txt`).

---

## 3. (Optional but recommended) Add your Linode API token

The Linode API token (Linode calls it a **Personal Access Token**) lets the
appliance act on your Linode account on your behalf, so it can do the cloud-side
work automatically. It enables the appliance to:
- provision a **Block Storage volume** per migration (sized to the source) and
  attach it to the replication server,
- after replication, **clone that volume** into the launchable artifact,
- optionally **create and boot a new Linode instance** from the artifact.

Without a token, the appliance still replicates (into a file on the server) so
you can evaluate the flow; the finalize step then just reports where the data
landed.

The token is stored **encrypted at rest** (AES-256-GCM) on your replication
server and is only ever sent to `api.linode.com`.

### How to get the token

1. Sign in to **Linode Cloud Manager** → <https://cloud.linode.com>.
2. Go to **API Tokens** (username menu → *API Tokens*, or
   <https://cloud.linode.com/profile/tokens>).
3. Click **Create a Personal Access Token**.
4. Give it a **label** (e.g. `vm-replication-appliance`) and an **expiry**.
5. **Set the permissions (scopes).** For least privilege, set everything to
   **None** except:

   | Scope | Access | Why |
   |---|---|---|
   | **Linodes** | **Read/Write** | create the migrated instance, its boot config, and boot it |
   | **Volumes** | **Read/Write** | create, attach, and clone the replication volume |
   | **Images** | **Read/Write** | used by the local-disk boot method |
   | **Object Storage** | **Read/Write** | upload the audit logs (see below) |

   (Leave Account, NodeBalancers, Domains, etc. at **None** — they aren't used.)
6. Click **Create Token** and **copy it immediately** — Linode shows the value
   only once.
7. Paste it into the console's token field and **Save**. The console will show
   "✔ Linode API token stored".

> Create the token on the **same account** that owns the replication server, and
> make sure that account can create volumes in the server's region. Treat the
> token like a password — anyone holding it can create/delete resources (and
> incur charges) on your account. You can revoke it anytime from the same API
> Tokens page; provisioning/finalize will then fail until you save a new one.

### Audit logs (Linode Object Storage)

When you save a token, the appliance also provisions a **Linode Object Storage
bucket** and shows a tick beside the token card once it's created. The bucket is
named `vmrep-audit-<appliance-id>` (e.g. `vmrep-audit-99334138`) — the Linode
instance id is globally unique, so the name never collides across accounts or
appliances. From then on it keeps an audit trail there:

- **`main.log`** — every action taken in the console (logins, connection tests,
  migration create/start/delete, token changes) plus appliance system messages,
  so you can review *what was done* on the console.
- **`migrations/<id>-<name>.log`** — one file per migration capturing everything
  the appliance did for it: activity events, machine-convert output, receiver/
  agent activity, the cutover steps, and Linode instance status transitions.

Browse and download these in **Cloud Manager → Object Storage**. The trail is
kept in the appliance database and re-uploaded on change, so the files are
always the full current log. This needs the token to have **Object Storage:
Read/Write** and Object Storage enabled on the account; if provisioning fails,
the token card shows why and migrations still work — only audit upload is
skipped.

The bucket is created in the **appliance's own region** by default (so a
Singapore appliance gets a Singapore bucket). Override with the
`-obj-region <region>` flag if you want it elsewhere.

**Managing the bucket from the token card:**

- **Re-create audit bucket** — creates `vmrep-audit-<appliance-id>` if it doesn't
  exist (e.g. after you deleted it, here or in Cloud Manager). If the bucket
  already exists it just tells you so and re-points the console at it — it never
  overwrites or duplicates.
- **Delete audit bucket** — **empties and permanently deletes** the bucket and
  **all** logs in it (the `main.log` and every `migrations/…` log). To guard
  against mistakes it is only allowed when **no migration is active** (none in a
  created/running state — finish, launch, or delete them first) and it requires
  you to **type the console password**. Re-create afterwards to start a fresh,
  empty bucket. This is **idempotent**: if the bucket was already removed (in
  Cloud Manager, or an earlier session), clicking Delete simply clears the
  console's view of it — it won't error. The console also self-heals within
  ~20s if the bucket disappears out from under it (the button switches back to
  "Re-create").
- **Remove token** — deletes the stored Linode API token. Allowed once **no
  migration is active** — **completed** migrations (launched / image ready) don't
  block it, so you can remove the token after your servers are migrated. It's
  refused only while a migration is still created/running, because deleting such a
  migration uses the token to remove its Linode volumes (removing it first would
  orphan them). This never deletes anything in your Linode account.

> The launched instance's own OS boot console isn't available through the Linode
> API, so the per-migration log captures the instance's **status transitions**
> (provisioning → booting → running) rather than raw kernel/boot text — view the
> latter via **Lish** in Cloud Manager.

---

## 3.5 Check the source first (Source check tab)

Before creating a migration, run the **Source check** — a **read-only
pre-migration assessment** that tells you whether a server can migrate, **which
of the three methods it supports**, and which **destination OS image** to pick
for file transfer.

1. Open the **Source check** tab and click **Generate check command**.
2. Run the shown one-liner on the **source server** as root (valid for 30
   minutes). It only **reads** system facts — OS, CPU architecture, disk
   layout/filesystems (LVM/LUKS/RAID detection), SELinux, used storage, and a
   **live reachability test of the replication port range** (the script
   connects back to a temporary probe port on the appliance). It sends one
   report and exits — **nothing is installed**, so there is nothing to remove.
3. The tab updates itself when the report arrives: source facts as ✔/✘ checks,
   plus a verdict per method — **Supported**, **Supported with cautions** (with
   the exact reasons, e.g. *SELinux enforcing: file copy does not preserve
   contexts*), or **Not supported** (e.g. *LUKS-encrypted root cannot be
   converted — use file transfer*) — and the recommended Linode image
   (e.g. Ubuntu 24.04 source → `linode/ubuntu24.04`).

What it evaluates: x86_64 requirement (hard fail otherwise), distro/version
recognition, systemd presence (the agent installs as a systemd timer),
convertible root filesystem for the block methods (ext2/3/4/XFS supported; LVM
fine; btrfs cautioned; ZFS/LUKS refused), software-RAID caution, the 10 TiB
Block Storage per-volume limit, SELinux mode for file transfer, and data-plane
reachability (TCP 5000–5100).

---

## 4. Create a migration (single or multi-disk)

> **Migration method.** The **New migration** form offers three methods from one
> selector: **File transfer** (the default — copies only used files onto a fresh
> Linode you pick an OS image for) and the two **block** methods (separate Block
> Storage volume, or Linode local disk). The disk rows below apply to the **block**
> methods. For **File transfer** you instead pick a **destination OS image** + plan
> and, after creating the migration, use the card's **Create destination instance**
> step (name it + set a root password) — **Start replication** unlocks only once
> that destination's file receiver is confirmed ready (with a manual-install
> fallback if cloud-init can't reach it). See
> [`docs/FILE-MIGRATION.md`](docs/FILE-MIGRATION.md) for the full file-transfer flow.

For the **block** methods: click **New migration**, enter a **Name** and
**hostname**, then **add one disk row per source disk** (device + size in GB). The **first row is the boot disk**
(the one whose partitions include the root filesystem `/`); additional rows are
data disks. A server with everything on one disk just has a single row.

> **Migrating a multi-disk server (e.g. an EC2 with 3 EBS volumes)?** Add a row
> for each disk. The appliance creates **one Linode volume per disk** and
> replicates them independently and in parallel; at cutover each becomes its own
> image volume, and a launched instance gets them all attached (boot as `sda`,
> data as `sdb`, `sdc`, …). Up to 8 disks (Linode device slots `sda`–`sdh`).

Not sure of the devices/sizes? The form's **How do I find the source details?**
section has a copyable command that lists the hostname and **every** whole disk
with its size rounded up:

```bash
echo "Hostname : $(hostname)"; lsblk -b -d -n -o NAME,SIZE,TYPE | \
  awk '$3=="disk" && $2>0 && $1!~/^(nbd|loop|ram|zram|sr|fd)/{printf "Device   : /dev/%s\nSize(GB) : %d\n", $1, ($2+1073741823)/1073741824}'
```

The `awk` filter skips pseudo/empty block devices — `nbd*` (network block
devices, e.g. the 16 empty `/dev/nbd0..15` a loaded `nbd` module creates),
`loop`, `ram`/`zram`, `sr` (optical), `fd` (floppy) — so only real, non-zero
data disks are listed.

Use the **whole disks** (e.g. `/dev/sda`, `/dev/sdb`), not partitions. For LVM,
if a volume group spans multiple disks, add **all** of its member disks.

**Boot target & plan.** Choose how the cutover instance boots — a **separate
Block Storage volume** (default) or the Linode's **local disk** — and pick the
**Linode plan** (Shared or Dedicated). The form lists each plan's vCPU/RAM/disk
and price; for the volume option it also shows the estimated monthly Block
Storage cost (≈ $0.10/GB) and an estimated total. For local-disk boot only
plans whose disk fits your data are offered (single-disk migrations only). The
launched instance uses this plan at cutover.

> **Local-disk cutover — one manual step.** A Linode local disk can only be
> written from *inside* the instance, so the disk-boot cutover boots the new
> Linode into **Rescue Mode** (Finnix) with the blank local disk as `/dev/sda`,
> and the migration card shows a **one-line copy command**: open the instance's
> **Lish console** (Weblish link on the card) and paste it. The command streams
> the converted image **straight from the appliance's replication volume** onto
> the local disk with live `dd` progress, grows the root to fill the disk, and
> powers the instance off — the appliance then boots it from the local disk
> automatically. Typical copy time for an 80 GiB image is **15–30 minutes**
> (it reads the appliance's already-hydrated volume, not a slow fresh clone);
> **no temporary volume is created**. The activity log posts a status line
> every 15 minutes while it waits.

The console then shows a **one-line command**, e.g.:

```bash
curl -fsSL -k --pinnedpubkey 'sha256//…' 'https://203.0.113.10:8080/install/agent.sh?token=…' | sudo bash
```

The `--pinnedpubkey` flag pins this server's public key, so the download is
authenticated and tamper-proof even though the certificate is self-signed (`-k`
skips public-CA checking; the pin provides the security).

---

## 5. Run the command on your source server

SSH into the source as root and paste the command. It downloads the agent,
installs the mTLS material, and starts a **systemd timer** that replicates
**every disk of the migration** (one agent run per disk, each to its own
receiver port) to the appliance every 60 seconds. First run is a full copy;
later runs ship only changed blocks.

### If the first sync fails

**No reinstall is needed — the agent retries every 60 seconds automatically.**
Fix the cause and it heals on its own. The most common cause is the receiver
port being blocked: open **inbound TCP 5000–5100** on the replication server's
firewall — including any **Linode Cloud Firewall** attached to it in Cloud
Manager (the on-box `ufw` rule the installer adds does not cover that). Check
the real error with `journalctl -u vmrepl-agent -n 20`, and force an immediate
retry with:

```bash
sudo systemctl start vmrepl-agent.service
```

Re-running the enrollment one-liner is also safe at any time — it stops the
previous agent and replaces it atomically.

### Wrong-disk & stale-agent protection

Every agent session is checked at the handshake, before it can show as
"connected" or write a byte:

- **Identity.** Each enrollment runs its agent with a unique **job id** (baked
  into the install command). The receiver refuses any session with a different
  job id — so a stale, never-uninstalled agent from an **old migration**
  (possibly on another machine), whose timer still fires at a port that a new
  migration now uses, can no longer stream its disk into the new migration's
  volume. The rejection names the offending host: run the **uninstall
  one-liner** there. After **updating the appliance**, agents enrolled before
  the update are also refused until you re-run their enrollment command (safe
  and atomic).
- **Geometry.** The size of the device the agent is actually reading must
  roughly match the size the migration was created with. A gross mismatch —
  e.g. the migration says **80 GiB** but the agent's `/dev/sda` is a **512 MiB
  swap disk** — is rejected with a `refusing to replicate: … WRONG DISK` error.
  Run `lsblk -f` (or `findmnt -no SOURCE /`) on the source to find the disk
  that holds `/`, then delete the migration and re-create it with that device
  and its real size.

### Removing the agent (after migration completes)

One command removes everything enrollment installed (binary, timer, certs,
checkpoint). The console shows it with a Copy button on completed migrations:

```bash
curl -fsSL -k --pinnedpubkey 'sha256//…' 'https://<replication-server>:8080/install/uninstall.sh' | sudo bash
```

The agent only ever *reads* the source disk; removal changes nothing about the
server's data or OS.

---

## 6. Watch status and validation in the console

The card is fully live: an in-progress migration's **status pill, progress
bar/%, ETA, transferred + speed, and RPO** update **every second** on their own,
and the whole list refreshes every 5 seconds — no manual **Refresh** needed
(the button remains as a force-refresh). This works on both entry paths: a page
load with an existing session and a fresh sign-in through the login form (e.g.
right after the appliance was updated/restarted).

Each migration shows aggregate progress and a **per-disk table** (expand
**Disks**), plus a checklist that requires **all disks**:
- ✔ Agent connected — _N/N disks checked in_
- ✔ Initial full sync complete — _N/N disks baselined_
- ✔ Replication lag within target — _worst lag across disks_
- ✔ Storage provisioned — _N/N volumes ready_

When all checks pass, run **Pre-migration assessment**; on success the **Cutover
instance** button enables.

---

## 7. Cut over the instance

Cutover is **three steps: freeze the image, power off the source, launch** (the same for volume-boot and
local-disk boot):

1. Stop the source's apps/databases and let the **RPO lag drop to ~0** (shown on
   the card), so the frozen copy is current.
2. Click **Cutover instance**. In the dialog you can optionally set a **name
   for the new instance** (both boot methods) and, for volume boot, a **name
   for the cutover volume** — blank keeps the `<migration>-cutover` default
   (names are sanitized to Linode's label rules; instances ≤64 chars, volumes
   ≤32, multi-disk volumes get a `-N` suffix). You can also set a **root
   password / SSH key** (so you can log into the launched instance via the
   Lish console). Then click **Stop replication & continue**. The appliance stops new replication passes, takes
   **one consistent final pass** with the source root briefly **remounted
   read-only** (so the image is a clean point-in-time, not a live "smear" that
   fails `fsck` and boots to `grub>`), and then **converts the boot image and
   validates it is bootable** — *all while the source is still running*. So if
   anything is wrong (wrong disk, inconsistent/incomplete copy, a remount the
   root won't allow because apps are still writing), the cutover **fails fast
   BEFORE you power anything off** and tells you how to fix it — you lose no
   uptime. If the source is **already powered off or idle**, tick **"skip the
   read-only snapshot"** in the dialog. **Delta passes are applied atomically**:
   an interrupted pass is **discarded whole**, so the image is always the **last
   complete pass**. Once the image is validated the migration pauses in state
   `awaiting_cutover`.

   The card guides the timing: while step 1 runs it shows **"Preparing &
   validating the boot image — keep the source server running"** (don't power
   off yet); once the image is validated it switches to **"it is now safe to
   power off the source server"**. Follow the card — there is no need to guess.
3. **Power off the source server** (now that the image is validated), then click
   **Launch instance**. The appliance:
   - **clones every disk's volume** into an image volume
     (`vmrepl-img-<id>-<diskIndex>`) — your migrated "snapshot(s)" (the boot
     conversion already ran and was validated in step 1),
   - **launches a new Linode** with all image volumes attached (boot as `sda`,
     data disks as `sdb`, `sdc`, …) and boots it.

So **a multi-disk migration produces multiple image volumes** — one per source
disk. When it finishes, the completed banner lists them and links to
**cloud.linode.com/volumes**.

Once launched, the card shows a green **✓ Migration complete** header, and the
**RPO** column switches to a dash **—** (replication has stopped, so a lag figure
would only mislead).

### Finishing up: remove the agent, then Close the migration

A completed migration keeps a temporary **replication volume** (`vmrep-<name>`)
attached to the appliance — the target the agent streamed into. Once the launched
Linode is up you no longer need it. Finish the cycle from the card:

1. Click **✓ Migration complete — remove source agent**. It shows the one-line
   command to uninstall the agent from the source (Copy it, run it), then click
   **Done**.
2. A **Close migration** button now appears next to it. Click it and confirm:
   the appliance **deletes the `vmrep-<name>` replication volume** and clears the
   card. **Your launched Linode and its volumes are kept, untouched** — only the
   temporary replication volume is removed.

> The **Delete** button is **hidden once a migration is complete** so the migrated
> server can't be torn down by accident. (Delete is the opposite of Close: it
> removes the *launched* Linode and its cutover volumes but keeps the replication
> volume — use it only for a migration you're abandoning before it launched.)

> **Why freeze then power off?** It gives a clean, final copy without needing LVM
> or a read-only remount of the running root (which fails on most cloud images
> with "/: mount point is busy"). Powering the source off before launching also
> ensures the old and new machines aren't both up at once. For the lowest-RPO
> cutover, stop the source's writers and wait for the lag to reach ~0 before
> step 2.

### Launching later / manually from the image volumes

If you didn't auto-launch (or want more copies):
1. In Cloud Manager, **create a Linode** in the **same region** as the volumes
   (no distribution needed).
2. **Attach** the image volumes: the boot volume first, then the data volumes.
3. Create a **Configuration Profile**: Kernel **GRUB 2**, set **/dev/sda** to the
   boot image volume, **/dev/sdb**…​ to the data image volumes, root device
   `/dev/sda`, and enable the **Network Helper**.
4. **Boot** into that profile and open the **Lish console** to confirm it comes
   up. Data disks mount automatically if the source's `/etc/fstab` uses UUIDs
   (the usual case) — the UUIDs are preserved in the clones. If fstab uses device
   names like `/dev/xvdf`, update those entries to match the new `sdb`/`sdc`…
   or (better) to `UUID=` once.

> The image volumes are independent copies — launching from them doesn't disturb
> the migration. You can keep replicating and cut over again, or launch several
> instances from the same set of image volumes.

---

## What runs where

| Role | Host | Component | Installed by |
|---|---|---|---|
| Replication server | a Linode you create | `applianced` (console + receivers) | `install-replication-server.sh` |
| Source | your existing server | `agent` (systemd timer) | the console's one-line command |

---

## Try the whole flow locally (no Linode)

```bash
bash scripts/appliance-smoke.sh
```

It starts the appliance in file-fallback mode, logs in with the generated
password, creates a migration via the console API, runs the agent against the
embedded receiver, verifies the replicated image is byte-identical, and drives
the migration to `image_ready`.

---

## Notes & limitations

- **HTTPS console** with a self-signed certificate (auto-generated at install);
  the browser warns on first visit — verify the printed fingerprint and click
  through. For a CA-signed cert, pass `--tls-cert`/`--tls-key` to `applianced`,
  or run behind a TLS-terminating proxy with `--insecure-http`.
- The **Linode finalize** (volume clone → image → launch) requires a real token
  and is exercised against the live API, not in CI.
- **Block Storage volume** sizing rounds the source disk up to whole GiB (min
  10 GiB). The replication server's plan must allow attaching volumes of that
  size.
- One appliance manages many migrations concurrently (one receiver port each,
  starting at 5000).
