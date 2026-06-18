# Turnkey Appliance — Web Console Migration

This is the turnkey way to use the **vm-replication tool**: install one service on a
**replication server** (a Linode), open a **web console**, and drive migrations
from the browser. No manual cert copying or command crafting — the console
generates a one-line command for each source.

For the manual/CLI workflow instead, see [`GETTING_STARTED.md`](GETTING_STARTED.md).

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

**Sessions & password recovery.** Signing out of the console only clears your
browser session — it never stops a migration. Replication runs in the
`applianced` service independent of console logins, so sign-out (or a closed
browser) leaves every migration running. Forgot the console password? Retrieve
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

   (Leave Account, Images, NodeBalancers, Domains, etc. at **None** — they aren't
   used. If you later want the appliance to build a Linode *Image* rather than a
   cloned volume, add **Images: Read/Write**. For **audit logs** — see below —
   add **Object Storage: Read/Write**.)
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
named `vmrep-audit-<appliance-id>-NN` (e.g. `vmrep-audit-99334138-01`): it keeps
the appliance's Linode id and adds a number — the appliance lists the account's
existing buckets and claims the lowest free one, so **multiple appliances on the
same account each get their own bucket without colliding**. From then on it
keeps an audit trail there:

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
`-obj-region <region>` flag if you want it elsewhere. If a bucket ended up in
the wrong region, use **Re-create audit bucket** on the token card to provision
it again in the current region (then delete the stray bucket in Cloud Manager).

> The launched instance's own OS boot console isn't available through the Linode
> API, so the per-migration log captures the instance's **status transitions**
> (provisioning → booting → running) rather than raw kernel/boot text — view the
> latter via **Lish** in Cloud Manager.

---

## 4. Create a migration (single or multi-disk)

Click **New migration**, enter a **Name** and **hostname**, then **add one disk
row per source disk** (device + size in GB). The **first row is the boot disk**
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
  awk '$3=="disk"{printf "Device   : /dev/%s\nSize(GB) : %d\n", $1, ($2+1073741823)/1073741824}'
```

Use the **whole disks** (e.g. `/dev/sda`, `/dev/sdb`), not partitions. For LVM,
if a volume group spans multiple disks, add **all** of its member disks.

**Boot target & plan.** Choose how the cutover instance boots — a **separate
Block Storage volume** (default) or the Linode's **local disk** — and pick the
**Linode plan** (Shared or Dedicated). The form lists each plan's vCPU/RAM/disk
and price; for the volume option it also shows the estimated monthly Block
Storage cost (≈ $0.10/GB) and an estimated total. For local-disk boot only
plans whose disk fits your data are offered (single-disk migrations only). The
launched instance uses this plan at cutover.

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

Click **Cutover instance**. The appliance:
1. stops accepting new blocks (quiesces all disks),
2. runs the **machine conversion** on the **boot disk** so it boots on Linode
   (virtio initramfs, GRUB, fstab, Lish serial console, network reset),
3. **clones every disk's volume** into an immutable image volume
   (`vmrepl-img-<id>-<diskIndex>`) — your migrated "snapshot(s)",
4. optionally **launches a new Linode** with all image volumes attached (boot as
   `sda`, data disks as `sdb`, `sdc`, …) and boots it.

So **a multi-disk migration produces multiple image volumes** — one per source
disk. When it finishes, the completed banner lists them and links to
**cloud.linode.com/volumes**.

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
