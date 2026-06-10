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
 │  │  Web console (http://<ip>:8080)  ← log in with password │  │
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

Create a Linode (Ubuntu/Debian, sized with enough disk for the appliance and
room to attach volumes), SSH in as root, clone this repo, and run:

```bash
sudo scripts/install-replication-server.sh
```

It builds the binaries, generates certificates and an **admin password**,
installs a systemd service (`applianced`), and prints:

```
================ REPLICATION SERVER READY ================
 Console:   http://203.0.113.10:8080
 Password:  681af4b11221bacb88e34080
...
```

Options: `--public-host <ip>` (if auto-detect is wrong), `--region us-ord`,
`--port 8080`.

> The console is HTTP. Restrict the port to trusted networks (firewall) or reach
> it over an SSH tunnel/VPN. The replication **data plane is always mutual TLS**.
> The Linode token (step 3) is stored **encrypted at rest**.

---

## 2. Open the console and sign in

Browse to `http://<replication-server-ip>:8080` and enter the generated
password. (It's also saved at `/var/lib/vm-repl/initial-admin-password.txt`.)

---

## 3. (Optional but recommended) Add your Linode API token

In the console, paste a Linode API token. This enables the appliance to:
- provision a **Block Storage volume** per migration (sized to the source),
- run the disk conversion and **create the launchable image** on "Start migration".

Without a token, the appliance still replicates (into a file on the server) so
you can evaluate the flow; the finalize step then just reports where the data
landed.

---

## 4. Create a migration

Click **New migration** and enter your source server's:
- **Name** (label), **hostname**,
- **disk device** (e.g. `/dev/sda` — the whole disk, not a partition),
- **disk size (GB)**.

The console immediately shows a **one-line command**, e.g.:

```bash
curl -fsSL 'http://203.0.113.10:8080/install/agent.sh?token=…' | sudo bash
```

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

- **HTTP console** for the MVP — put it behind a firewall/tunnel; HTTPS is a
  roadmap item.
- The **Linode finalize** (volume clone → image → launch) requires a real token
  and is exercised against the live API, not in CI.
- **Block Storage volume** sizing rounds the source disk up to whole GiB (min
  10 GiB). The replication server's plan must allow attaching volumes of that
  size.
- One appliance manages many migrations concurrently (one receiver port each,
  starting at 5000).
