# Turnkey Appliance — Web Console Migration

This is the **AWS MGN–style** way to use vm-replication: install one service on a
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

## 1. Stand up the replication server

Create a Linode (Ubuntu/Debian/RHEL-family, sized with enough disk for the
appliance and room to attach volumes), SSH in as root, and run:

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
   cloned volume, add **Images: Read/Write**.)
6. Click **Create Token** and **copy it immediately** — Linode shows the value
   only once.
7. Paste it into the console's token field and **Save**. The console will show
   "✔ Linode API token stored".

> Create the token on the **same account** that owns the replication server, and
> make sure that account can create volumes in the server's region. Treat the
> token like a password — anyone holding it can create/delete resources (and
> incur charges) on your account. You can revoke it anytime from the same API
> Tokens page; provisioning/finalize will then fail until you save a new one.

---

## 4. Create a migration

Click **New migration** and enter your source server's:
- **Name** (label), **hostname**,
- **disk device** (e.g. `/dev/sda` — the whole disk, not a partition),
- **disk size (GB)**.

Not sure of the device or size? The form has a **Copy** button for a discovery
command — run it on your source server and it prints all four values:

```bash
echo "Hostname : $(hostname)"; lsblk -b -d -n -o NAME,SIZE,TYPE | \
  awk '$3=="disk"{printf "Device   : /dev/%s\nSize(GB) : %d\n", $1, ($2+1073741823)/1073741824}'
```

Enter the **whole disk** (the one whose partitions include the root filesystem
`/`), not a partition, and the size **rounded up** (the command already rounds up
for you).

The console immediately shows a **one-line command**, e.g.:

```bash
curl -fsSL -k --pinnedpubkey 'sha256//…' 'https://203.0.113.10:8080/install/agent.sh?token=…' | sudo bash
```

The `--pinnedpubkey` flag pins this server's public key, so the agent download is
authenticated and tamper-proof even though the certificate is self-signed (`-k`
skips public-CA checking; the pin is what provides the security).

---

## 5. Run the command on your source server

SSH into the source as root and paste the command. It downloads the agent,
installs the mTLS material, and starts a **systemd timer** that replicates the
disk to the appliance every 60 seconds. (First run is a full copy; later runs
ship only changed blocks.)

---

## 6. Watch status and validation in the console

The migration row shows live progress (blocks current, bytes sent, RPO lag) and
a checklist:
- ✔ Agent connected
- ✔ Initial full sync complete
- ✔ Replication lag within target
- ✔ Storage provisioned

When all checks pass, the **Start migration** button enables.

---

## 7. Start the migration

Click **Start migration**. The appliance:
1. stops accepting new blocks (quiesces),
2. runs the **machine conversion** so the disk boots on Linode (virtio
   initramfs, GRUB, fstab, Lish serial console, network reset),
3. **clones the volume** into an immutable artifact (your migrated "snapshot"),
4. optionally **launches a new Linode instance** booting from that artifact (you
   choose when starting).

When it finishes, the row shows the artifact and (if launched) the new instance
id. You can launch further instances from the cloned volume any time.

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
