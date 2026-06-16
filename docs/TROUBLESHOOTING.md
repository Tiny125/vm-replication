# Troubleshooting & Error Reference

A reference for errors you may see in the **console activity log**, the
**migration card**, or the **`applianced` / agent logs**, with what each means
and how to remediate it. Errors in the activity log are shown in **red**
(errors) or **amber** (warnings).

> Tip: open a migration's **Activity log → Expand** for the full history, and
> on the source run `journalctl -u vmrepl-agent -f`; on the appliance run
> `sudo journalctl -u applianced -f`.

---

## 1. Replication errors (agent ↔ appliance receiver)

These appear in the activity log as
`disk N (/dev/sdX): replication attempt failed: <reason>`.

### "Harmless noise" — stray connections to the receiver port

| Message | What it means | Remediation |
|---|---|---|
| `read hello: tls: client offered only unsupported versions: [303 302 301]` | Something connected to the receiver port (5000–5100) speaking **TLS 1.0/1.1/1.2**. The receiver requires **TLS 1.3**. The replication agent already uses 1.3, so this is **not** your agent — it's a port scanner, load-balancer/health check, monitoring probe, or other client hitting the port. | **None needed.** The receiver rejects it and your agent keeps replicating (you'll see `agent connected, replicating` continue). To stop the noise, restrict TCP 5000–5100 to your source's IP in any firewall / Linode Cloud Firewall. |
| `read hello: tls: first record does not look like a TLS handshake` | A **plain, non-TLS** connection hit the receiver port (e.g. an HTTP probe or a raw `nc`/port scan). | Same as above — harmless; lock down the port range if you want it gone. |
| `read hello: unexpected EOF` | A client opened a TCP connection to the receiver port and **closed it immediately** without completing the handshake (classic TCP health check / scanner). | Same as above — harmless. |

> **Key point:** these three are logged as "replication attempt failed" but they
> are connections from **something other than your agent**. If, in between them,
> you still see `agent connected, replicating` and the **bytes received** /
> **percentage** keep climbing, replication is healthy — the noise is just
> rejected probes. They become a real concern only if your agent **never**
> connects and you see *only* these.

### Real agent/handshake failures

| Message | What it means | Remediation |
|---|---|---|
| `read hello: remote error: tls: bad certificate` | Your **agent** rejected the appliance's TLS certificate (or vice-versa) — usually the agent was enrolled against an **older appliance certificate**. | **Re-run the enrollment command** on the source (it reinstalls the agent with current certs). A 60s retry will not fix a stale cert. |
| `receiver rejected session: target N bytes, need at least M` | The replication **volume is smaller than the source disk**. | Recreate the migration entering a size **≥ the source disk** (use the value the source-details command prints — it rounds up). |
| `dial receiver: ... connection refused / timeout` (agent log) | The source can't reach the appliance on the receiver port. | Open **TCP 5000–5100** from the source to the appliance (security group / Linode Cloud Firewall). Use the **Connection test** tab to confirm reachability. |
| `block at N ... out of device bounds` / `block hash mismatch` | Corruption or a size mismatch mid-stream (rare). | Let the agent retry; if it persists, delete and recreate the migration. |

### Source server unresponsive — `task ... blocked for more than N seconds`

On the **source** Lish/console you see kernel hung-task warnings for
`vmrepl-agent`, `systemd-journal`, `rsyslog`, etc., **reads work (`ls`) but every
command that writes hangs**, and `Ctrl-C` does nothing.

| What it means | Remediation |
|---|---|
| The source root **filesystem is frozen** (`fsfreeze`) and writes are blocked system-wide; the blocked processes are in uninterruptible (`D`) state, so they ignore signals. Older builds auto-`fsfreeze`d a non-LVM source for the whole cutover read and could deadlock without thawing. | **Thaw it:** from a fresh session run `fsfreeze -u /` (reads work, so this executes), or **reboot from Cloud Manager** (a freeze never survives reboot). Then `systemctl disable --now vmrepl-agent.timer` before re-running. **Current builds never hold a freeze across the read** — non-LVM sources replicate live (the appliance proceeds with a warning), and a watchdog force-thaws after 60s. For a true point-in-time cutover image, put the source root on **LVM**. |

---

## 2. Validation checks (migration card)

The checks auto-update on each poll; a red ✘ is not necessarily an error, just
"not ready yet".

| Check | Red means | Notes |
|---|---|---|
| **Storage provisioned** | The appliance volume wasn't created/attached. | Check the Linode token and account limits; see §3. |
| **Agent connected** | No agent check-in in the last 5 min. | Run the enrollment command on the source; after a cutover the agent stops, so this goes red — expected. |
| **Replication lag within Ns** | The last completed sync is older than the RPO target. | Informational; turns green when a recent sync lands. Not required for cutover. |
| **Initial full sync complete** | The baseline hasn't finished on all disks. | **This is the only gate for cutover.** It turns green automatically at 100%. |

---

## 3. Migration create / provisioning errors

Shown inline on the New-migration form or as a failed create.

| Message | Meaning | Remediation |
|---|---|---|
| `name and at least one source device are required` | Missing name or disks. | Fill the form. |
| `source IP is not a valid IP address or hostname` | Malformed IP. | Enter a valid IP; it must pass the **Connection test**. |
| `at most 8 disks per migration (...)` | More than 8 disks. | Split into multiple migrations. |
| `... already exists` | Duplicate migration name. | Use a unique name (or delete the old migration). |
| `provision storage: linode POST /volumes: 400 ... Label must be unique` | A leftover volume already uses that label. | Remove the orphan `vmrep-<name>-<id>` volume in Cloud Manager, or use a different migration name. |
| `provision storage: ... Account limit / quota` | Your account's volume/storage quota is reached. | Raise the limit (Linode support) or free up volumes, then create again. |
| A failed create leaves **no** card | By design — a failed create rolls back its volume + record. | Fix the cause and create again. |
| `add a valid Linode API token in Settings before creating a migration` | No token is stored, but the appliance needs one to provision storage (and later to remove the volumes on delete). | Add a token under **Settings → Linode automation**, then create the migration. |
| `the stored Linode API token is not working (revoked, or missing Linodes + Volumes read/write)` | A token is stored but Linode rejected it at create time. | Re-create the token with **Linodes: Read/Write** + **Volumes: Read/Write** and save it again in Settings. |

---

## 4. Cutover / finalize errors

Shown in red as `cutover failed: <reason>`; the migration goes to **failed**
and you can **Retry cutover** (it cleans up the previous attempt first).

| Message | Meaning | Remediation |
|---|---|---|
| `cutover: timed out waiting for a crash-consistent snapshot from the source` (warn) | At cutover the appliance asks the source agent for one final **point-in-time** pass (so the launched image is a single instant and boots cleanly). This warning means no such pass arrived in time — the agent is offline or an older build — so it proceeded on the current replicated data, which may be a multi-minute "smear" that can fail `fsck` / drop to `grub>`. | Confirm the **source agent is checked in** (re-run enrollment if needed) so the next attempt can snapshot, then **Retry cutover**. Watch for `cutover: crash-consistent snapshot captured on all disks` in the activity log. |
| `machine-convert: ... Illegal option -o pipefail` | The convert script ran under `dash`. | Fixed in current builds (runs under bash). Redeploy the appliance. |
| `boot disk conversion did not complete (...)` (warn, not fatal) | The boot fixup couldn't finish; the image volume is still created. | The volume is usable; you may need to boot in Rescue Mode and run `machine-convert.sh /dev/sda`. See `docs/CUTOVER.md`. |
| `could not locate a root filesystem with /etc/fstab on /dev/sdX (candidates: none)` | The replicated disk is **partitionless** (whole-disk filesystem) or the partition table wasn't re-read. | Current builds handle partitionless disks and pick a Linode kernel automatically. Redeploy and **Retry cutover**. |
| `e2fsck: Bad magic number in super-block` / `Filesystem still has errors` | The replicated filesystem is **inconsistent** — almost always because the source kept changing during the (multi-minute) block copy, so the image isn't a single point in time. The convert auto-`fsck`s and retries from a backup superblock, but it cannot reconcile a genuinely "smeared" copy. | Replicate from a **quiesced or idle source** and let the **initial full sync complete cleanly**, then cut over. (For a real busy server, stop the app or freeze the filesystem just before the final sync.) The launched instance dropping to `grub>` is the downstream symptom. |
| `clone disk N ... / wait clone active` | A volume clone failed or didn't become active. | Check account limits / region; Retry cutover. |
| `create instance: ... / create boot config: ... / boot instance: ...` | A Linode API call failed during launch. | Read the quoted Linode error; common causes are account limits, region mismatch, or a volume not yet attachable. Retry cutover. |
| `could not delete previous cutover Linode/volume (...)` (warn) | Cleanup of a prior attempt couldn't finish. | Remove the leftover `<name>-cutover` instance/volume in Cloud Manager, then retry. |

### Launched instance has the wrong IP / no connectivity / very slow Lish

After cutover the new instance can't be pinged, or **Lish login and every command lag ~10s**, and `ip -br a` shows the **source's** IP (e.g. `… proto static`) instead of its own.

| What it means | Remediation |
|---|---|
| The migrated disk carried the source's **static** network config (e.g. netplan `01-netcfg.yaml`), which pins the **old IP/nameservers**. netplan merges every `*.yaml` and a higher-sorting filename wins, so it overrode the appliance's DHCP config. With the wrong IP there's no working route, so DNS lookups time out (~10s) and anything that resolves a name — the login MOTD, package tools — crawls. | **Current builds remove the source's network config and write a single DHCP config**, so this is fixed for new cutovers. To fix an instance already launched, in **Lish** run: `mv /etc/netplan/01-netcfg.yaml /var/lib/vmrepl-netbak/ 2>/dev/null; netplan apply` (the appliance's `01-linode.yaml` then takes over via DHCP). Confirm with `ip -br a` that eth0 now has the instance's **own** assigned IP. |

---

## 5. Console / auth errors

| Message | Meaning | Remediation |
|---|---|---|
| `not logged in` / `invalid password` | Session expired or wrong password. | Sign in again. Forgot it? On the appliance run `sudo /usr/local/bin/applianced -data-dir /var/lib/vm-repl -show-password`. |
| `token is required` / Linode calls failing with 401 | No/invalid Linode token. | Add a valid Personal Access Token (Linodes + Volumes read/write) in the console. |
| **Can't log in as root on the launched instance (Lish)** | The migrated disk carries the **source's** accounts. Cloud images (Ubuntu, etc.) usually keep **root locked/password-less** and log in via SSH key or a sudo user — so the Lish *serial* console, which only does password login, has nothing to authenticate against. The instance booted fine; this is purely credentials. | **Set root access at cutover:** the Cutover dialog has optional **Root password** and **SSH public key** fields — fill them and the migrated image is reachable immediately (Retry cutover if you already cut over). Or log in over SSH with your original source key/user. Or fix it after the fact in **Rescue Mode**: mount the volume, `chroot`, `passwd root` + `passwd -u root`. |
| `cannot remove the Linode API token while N migration(s) exist` | Token removal is blocked on purpose: deleting a migration uses the token to remove its Linode volumes, so removing it first would orphan them. | **Delete all migrations first** (each delete cleans up its volumes), then remove the token. |

> Signing out of the console **does not** stop a migration — replication runs in
> the `applianced` service independent of console sessions.

---

## 6. Installer errors (replication server)

| Message | Remediation |
|---|---|
| `run as root (sudo)` | Re-run with `sudo`. |
| `could not detect public IP; pass --public-host` | Re-run with `--public-host <ip>`. |
| `build tools missing after bootstrap (...)` | Install `make` and Go ≥ 1.21 manually, then re-run. |
| `unsupported CPU arch for Go auto-install` | Install Go manually for your architecture. |

---

## When in doubt

1. Open **Activity log → Expand** for the full, time-ordered history.
2. Appliance: `sudo journalctl -u applianced -f`.
3. Source: `journalctl -u vmrepl-agent -f`.
4. Confirm reachability with the **Connection test** tab and that **TCP
   5000–5100** is open from the source to the appliance.
