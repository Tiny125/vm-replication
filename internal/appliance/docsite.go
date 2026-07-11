package appliance

// The documentation site: a self-contained guide to installing and using the
// migration console, served (unauthenticated, like the install scripts) at
// /documentation with its screenshots under /documentation/img/. The layout
// follows the MkDocs-Material documentation style (as used by newapi.ai): a
// dark top app bar, a grouped left sidebar with a filter box, a readable
// content column, admonition callouts, copyable code blocks — and console
// buttons reproduced inline (styled exactly like the real ones) so the reader
// recognises what to click. No icons anywhere.

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

//go:embed docsimg/*.png
var docsImages embed.FS

// handleDocs serves the documentation page (GET /documentation).
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsHTML))
}

// handleDocsImage serves an embedded screenshot (GET /documentation/img/{name}).
// Only bare *.png names ship; anything else is a 404 (no traversal).
func (s *Server) handleDocsImage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/documentation/img/")
	if name != path.Base(name) || !strings.HasSuffix(name, ".png") {
		http.NotFound(w, r)
		return
	}
	b, err := docsImages.ReadFile("docsimg/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

const docsHTML = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>vm-replication documentation</title>
<style>
 :root{
   --header:#1a237e; --header-text:#ffffff;
   --bg:#ffffff; --side:#fafafa; --border:#e0e0e0;
   --text:#212121; --muted:#616161; --accent:#0b5cd5; --accent-soft:#e8f0fe;
   --code-bg:#f5f5f5; --code-border:#e8e8e8;
   --note:#448aff; --tip:#00897b; --warn:#e65100;
   /* Console button colours (mirror the real console so demos look identical) */
   --btn-blue:#0071e3; --btn-red:#d8302a; --btn-green:#1d9b50;
 }
 *{margin:0;padding:0;box-sizing:border-box}
 html{scroll-behavior:smooth;scroll-padding-top:72px}
 body{background:var(--bg);color:var(--text);font:16px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;-webkit-font-smoothing:antialiased}

 /* ---- Top app bar ---- */
 header{position:fixed;top:0;left:0;right:0;height:56px;background:var(--header);color:var(--header-text);
   display:flex;align-items:center;gap:14px;padding:0 20px;z-index:50;box-shadow:0 2px 6px rgba(0,0,0,.2)}
 header .brand{font-size:17px;font-weight:700;letter-spacing:.2px;white-space:nowrap}
 header .brand span{font-weight:400;opacity:.85}
 header .grow{flex:1}
 header a.consolelink{color:#fff;background:rgba(255,255,255,.14);border:1px solid rgba(255,255,255,.35);
   padding:6px 14px;border-radius:6px;font-size:13.5px;text-decoration:none;white-space:nowrap}
 header a.consolelink:hover{background:rgba(255,255,255,.24)}

 /* ---- Layout ---- */
 .layout{display:flex;max-width:1400px;margin:0 auto;padding-top:56px}
 nav.sidebar{width:290px;flex-shrink:0;position:sticky;top:56px;height:calc(100vh - 56px);overflow-y:auto;
   background:var(--side);border-right:1px solid var(--border);padding:18px 0 40px}
 main{flex:1;min-width:0;padding:36px 48px 120px;max-width:900px}

 /* ---- Sidebar ---- */
 .navfilter{margin:0 18px 14px}
 .navfilter input{width:100%;padding:8px 12px;border:1px solid var(--border);border-radius:6px;font-size:13.5px;background:#fff}
 .navgroup{margin-top:16px}
 .navgroup>.gtitle{font-size:11.5px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);padding:4px 22px}
 nav.sidebar a{display:block;padding:6px 22px;font-size:14px;color:var(--text);text-decoration:none;border-left:3px solid transparent}
 nav.sidebar a:hover{background:#f0f0f0}
 nav.sidebar a.active{color:var(--accent);border-left-color:var(--accent);background:var(--accent-soft);font-weight:600}
 nav.sidebar a.sub{padding-left:38px;font-size:13.5px;color:var(--muted)}
 nav.sidebar a.sub.active{color:var(--accent)}

 /* ---- Content typography ---- */
 h1{font-size:32px;font-weight:700;letter-spacing:-.02em;margin:8px 0 6px}
 .lede{font-size:17.5px;color:var(--muted);margin-bottom:26px}
 section{margin-bottom:8px}
 h2{font-size:24px;font-weight:700;margin:44px 0 12px;padding-bottom:8px;border-bottom:1px solid var(--border)}
 h3{font-size:18.5px;font-weight:650;margin:28px 0 8px}
 p{margin:10px 0}
 ul,ol{margin:10px 0 10px 26px}
 li{margin:5px 0}
 a{color:var(--accent)}
 table{border-collapse:collapse;width:100%;margin:14px 0;font-size:14.5px}
 th,td{border:1px solid var(--border);padding:9px 12px;text-align:left;vertical-align:top}
 th{background:var(--side);font-weight:650}
 hr{border:none;border-top:1px solid var(--border);margin:26px 0}

 /* ---- Code ---- */
 code{background:var(--code-bg);border:1px solid var(--code-border);border-radius:4px;padding:1.5px 6px;font:13.5px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
 .codeblock{position:relative;margin:14px 0}
 .codeblock pre{background:var(--code-bg);border:1px solid var(--code-border);border-radius:8px;padding:14px 88px 14px 16px;
   overflow-x:auto;font:13.5px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:pre-wrap;word-break:break-all}
 .codeblock button.copy{position:absolute;top:9px;right:9px;border:1px solid var(--border);background:#fff;border-radius:6px;
   padding:4px 12px;font-size:12.5px;cursor:pointer;color:var(--muted)}
 .codeblock button.copy:hover{color:var(--text)}

 /* ---- Admonitions (text label, no icons) ---- */
 .adm{border-left:4px solid var(--note);background:#f2f7ff;border-radius:0 8px 8px 0;padding:12px 16px;margin:16px 0;font-size:14.5px}
 .adm .t{font-weight:700;font-size:12px;letter-spacing:.06em;text-transform:uppercase;display:block;margin-bottom:4px;color:var(--note)}
 .adm.tip{border-left-color:var(--tip);background:#eef8f6}.adm.tip .t{color:var(--tip)}
 .adm.warn{border-left-color:var(--warn);background:#fdf3ec}.adm.warn .t{color:var(--warn)}

 /* ---- Screenshots ---- */
 figure{margin:18px 0}
 figure img{max-width:100%;border:1px solid var(--border);border-radius:10px;box-shadow:0 2px 10px rgba(0,0,0,.07)}
 figcaption{font-size:13px;color:var(--muted);margin-top:7px}

 /* ---- Console-button demos: replicas of the real console controls ---- */
 .btn-demo{display:inline-block;padding:5px 16px;border-radius:980px;font-size:13.5px;font-weight:500;vertical-align:middle;
   border:1px solid transparent;white-space:nowrap;line-height:1.5}
 .btn-demo.primary{background:var(--btn-blue);color:#fff}
 .btn-demo.disabled{background:#a9cdf5;color:#fff}
 .btn-demo.danger{background:#fff;color:var(--btn-red);border-color:#f0b9b7}
 .btn-demo.done{background:var(--btn-green);color:#fff}
 .btn-demo.plain{background:#f5f5f7;color:var(--text);border-color:var(--border)}
 .field{background:var(--accent-soft);border:1px solid #c8dbf8;border-radius:5px;padding:1px 7px;font-size:13.5px;color:#153e75;white-space:nowrap}
 .pill-demo{display:inline-block;padding:2px 11px;border-radius:980px;font-size:12.5px;font-weight:500}
 .pill-demo.ok{background:#e3f4e9;color:#136c38}.pill-demo.warn{background:#fdf1de;color:#8a5a06}.pill-demo.bad{background:#fde7e6;color:#a1211c}

 .steps{counter-reset:step;list-style:none;margin-left:0}
 .steps>li{counter-increment:step;position:relative;padding-left:44px;margin:14px 0}
 .steps>li::before{content:counter(step);position:absolute;left:0;top:1px;width:28px;height:28px;border-radius:50%;
   background:var(--header);color:#fff;font-size:14px;font-weight:700;display:flex;align-items:center;justify-content:center}

 footer.docfoot{margin-top:70px;padding-top:18px;border-top:1px solid var(--border);color:var(--muted);font-size:13.5px}
 @media (max-width: 900px){nav.sidebar{display:none}main{padding:28px 22px 80px}}
</style>
</head><body>

<header>
  <div class="brand">vm-replication <span>documentation</span></div>
  <div class="grow"></div>
  <a class="consolelink" href="/">Open the console</a>
</header>

<div class="layout">
<nav class="sidebar" id="sidenav">
  <div class="navfilter"><input id="navq" type="text" placeholder="Filter the guide…" oninput="filterNav(this.value)"></div>
  <div class="navgroup"><div class="gtitle">Getting started</div>
    <a href="#introduction">Introduction</a>
    <a href="#install">Install the replication server</a>
    <a href="#sign-in">Sign in</a>
  </div>
  <div class="navgroup"><div class="gtitle">Console guide</div>
    <a href="#overview">Console overview</a>
    <a href="#api-token">Add your Linode API token</a>
    <a href="#source-details">Find your source details</a>
  </div>
  <div class="navgroup"><div class="gtitle">Create a migration</div>
    <a href="#choose-method">Choose a migration method</a>
    <a href="#file-transfer">File transfer (default)</a>
    <a href="#volume-boot">Volume boot</a>
    <a href="#disk-boot">Disk boot</a>
  </div>
  <div class="navgroup"><div class="gtitle">Run the migration</div>
    <a href="#enroll">Enroll the source server</a>
    <a href="#replicate">Start replication and monitor</a>
    <a href="#cutover">Cut over</a>
    <a href="#finish">Finish and clean up</a>
  </div>
  <div class="navgroup"><div class="gtitle">Reference</div>
    <a href="#sessions">Sessions and security</a>
    <a href="#troubleshooting">Troubleshooting</a>
  </div>
</nav>

<main>
<h1>vm-replication console</h1>
<p class="lede">Migrate Linux servers from anywhere — on-prem, AWS, GCP, Azure, other clouds — to Akamai Cloud (Linode), driven entirely from a web console. This guide walks you from a fresh install to a finished migration.</p>

<section id="introduction">
<h2>Introduction</h2>
<p>The <b>replication server</b> (a small Linode you create once) hosts the migration console. From the console you register each <b>source server</b>, copy one generated command onto it, and drive the whole migration — replication, validation, cutover — from the browser. Data flows from the source over <b>mutually-authenticated TLS</b>; nothing is ever pulled from the destination side.</p>
<div class="codeblock"><pre>source server ──(agent, one-line install)──► replication server (console) ──► destination on Linode</pre></div>
<p>Three migration methods are available from the same console. <b>File transfer is the default</b> and the right choice for most Linux servers:</p>
<table>
<tr><th>Method</th><th>What moves</th><th>Destination</th><th>Best for</th></tr>
<tr><td><b>File transfer</b> (default)</td><td>Only the <b>used files</b> (a mostly-empty 80&nbsp;GB disk copies its ~4&nbsp;GB)</td><td>A brand-new Linode running an OS image you pick</td><td>Most servers. Cheapest and usually fastest; no boot/partition concerns.</td></tr>
<tr><td><b>Volume boot</b></td><td>Every disk, <b>block for block</b></td><td>Block Storage volume(s) cloned into launchable image volumes</td><td>Exact disk-level replicas; multi-disk servers; keeping volumes as artifacts.</td></tr>
<tr><td><b>Disk boot</b></td><td>Every disk, block for block</td><td>The new Linode's own <b>local NVMe disk</b></td><td>Disk-level replica without a separate volume (faster disk, no volume cost).</td></tr>
</table>
</section>

<section id="install">
<h2>Install the replication server</h2>
<p>You need one Linode to act as the replication server. A <b>2&nbsp;GB shared plan</b> is enough for file transfers and 1–3 concurrent block disks (see <code>CONSOLE.md</code> in the repository for detailed sizing).</p>
<ol class="steps">
<li>Create a Linode (Ubuntu or Debian recommended) and SSH in as <b>root</b>.</li>
<li>Clone the repository and run the installer — it bootstraps everything it needs (Go toolchain, build tools) on a bare server:
<div class="codeblock"><pre>git clone https://github.com/Tiny125/vm-replication.git
cd vm-replication
sudo scripts/install-replication-server.sh</pre><button class="copy" onclick="cp(this)">Copy</button></div></li>
<li>The installer builds the binaries, generates certificates and an <b>admin password</b>, installs the <code>applianced</code> systemd service, and prints a summary:
<div class="codeblock"><pre>================ REPLICATION SERVER READY ================
 Console:   https://203.0.113.10:8080
 Password:  681af4b11221bacb88e34080
 Cert SHA-256 (verify this in your browser's certificate dialog):
   AB:CD:...:EF</pre></div></li>
<li>Keep that output — the password is also saved on the server at <code>/var/lib/vm-repl/initial-admin-password.txt</code>.</li>
</ol>
<div class="adm"><span class="t">Note</span>Useful installer flags: <code>--public-host &lt;ip&gt;</code> (if auto-detection picks the wrong address), <code>--region us-ord</code>, <code>--port 8080</code>.</div>
</section>

<section id="sign-in">
<h2>Sign in</h2>
<p>Browse to <code>https://&lt;replication-server-ip&gt;:8080</code>. The console uses a <b>self-signed certificate</b>, so your browser warns on first visit — that is expected. Before entering the password, open the browser's certificate dialog and confirm the <b>SHA-256 fingerprint matches</b> the one the installer printed. Then sign in:</p>
<figure><img src="/documentation/img/login.png" alt="The console sign-in card"><figcaption>The sign-in page. The password was generated at install time.</figcaption></figure>
<div class="adm tip"><span class="t">Tip</span>Forgot the password? Retrieve it on the replication server, without disturbing anything: <code>sudo /usr/local/bin/applianced -data-dir /var/lib/vm-repl -show-password</code></div>
</section>

<section id="overview">
<h2>Console overview</h2>
<p>Everything happens on one page:</p>
<figure><img src="/documentation/img/console-overview.png" alt="The whole console page"><figcaption>Top to bottom: the Linode automation (API token) card, the New migration form, and one card per migration.</figcaption></figure>
<ul>
<li><b>Linode automation</b> — paste your Linode API token here (next section).</li>
<li><b>New migration</b> — register a source server and pick the migration method.</li>
<li><b>Migrations</b> — one card per migration: status pills such as <span class="pill-demo warn">waiting for agent</span> and <span class="pill-demo ok">agent connected</span>, live progress, validation checks, the activity log, and the action buttons. The page refreshes itself; the <span class="btn-demo plain">Refresh</span> button on each card forces it.</li>
</ul>
</section>

<section id="api-token">
<h2>Add your Linode API token</h2>
<p>The token lets the console act on your Linode account: launch the file-transfer destination, provision replication volumes, clone disks, and launch instances at cutover. <b>The file-transfer method requires it</b> (the OS image and plan lists come from the Linode API); the block methods need it for everything past evaluation.</p>
<ol class="steps">
<li>Sign in to <a href="https://cloud.linode.com/profile/tokens" target="_blank" rel="noopener">Linode Cloud Manager → Profile → API Tokens</a> and create a <b>Personal Access Token</b> with scopes:
<table>
<tr><th>Scope</th><th>Access</th><th>Used for</th></tr>
<tr><td>Linodes</td><td>Read/Write</td><td>launching the destination or cutover instance</td></tr>
<tr><td>Volumes</td><td>Read/Write</td><td>replication + image volumes (block methods)</td></tr>
<tr><td>Images</td><td>Read/Write</td><td>the OS image list, disk-boot conversion</td></tr>
<tr><td>Object Storage</td><td>Read/Write</td><td>optional audit logs</td></tr>
</table></li>
<li>Paste the token into the <b>Linode automation</b> card and press <span class="btn-demo plain">Save</span>:
<figure><img src="/documentation/img/settings-token.png" alt="The Linode automation card"><figcaption>The token is stored encrypted at rest (AES-256-GCM) and only ever sent to api.linode.com.</figcaption></figure></li>
</ol>
<div class="adm warn"><span class="t">Warning</span>Without a token, the <b>Destination OS image</b> dropdown on the New-migration form stays empty ("add a Linode token in Settings to load OS images") and a file-transfer migration cannot be created.</div>
</section>

<section id="source-details">
<h2>Find your source details</h2>
<p>The form needs a few facts about the source. Expand <b>How do I find the source details?</b> at the top of the New-migration form and run the copyable command on your source server — it prints everything the form asks for:</p>
<figure><img src="/documentation/img/source-helper.png" alt="The source-details helper"><figcaption>Hostname, reachable IP, OS (match it to the destination image), used storage in GB (sizes the plan for file transfer), and every real data disk (for the block methods).</figcaption></figure>
</section>

<section id="choose-method">
<h2>Choose a migration method</h2>
<p>The <span class="field">Migration method</span> selector on the New-migration form switches the whole flow. The fields below it change with the method:</p>
<figure><img src="/documentation/img/method-selector.png" alt="The migration-method selector"><figcaption>File transfer is pre-selected. The two block methods are in the same dropdown.</figcaption></figure>
</section>

<section id="file-transfer">
<h2>File transfer (default)</h2>
<p>Copies the source's <b>used files</b> straight onto a brand-new Linode running a clean OS image you pick. Only used data moves, the destination is a first-class Linode (native disk, Backups supported), and there are no partition/bootloader concerns.</p>
<figure><img src="/documentation/img/new-migration.png" alt="The New-migration form in file-transfer mode"><figcaption>The file-transfer form: OS image, used storage, and plan.</figcaption></figure>
<ol class="steps">
<li>Fill in <span class="field">Name</span>, <span class="field">Source hostname</span>, and <span class="field">Source IP address</span>.</li>
<li>Pick the <span class="field">Destination OS image</span> that matches the source's OS (the helper printed it — e.g. Ubuntu 24.04 → <code>linode/ubuntu24.04</code>).</li>
<li>Enter <span class="field">Used storage on the source (GB)</span> from the helper, and pick a <span class="field">Linode plan</span> whose disk comfortably fits that used size (the form suggests the fit).</li>
<li>Press <span class="btn-demo primary">Create migration</span>.</li>
<li>On the new card, press <span class="btn-demo primary">Create destination instance</span>: give the destination a name and a <b>root password</b> (so you can log into it later). The card walks through <i>launching → installing the file receiver → ready</i>. <span class="btn-demo disabled">Start replication</span> stays disabled until the destination is confirmed ready.</li>
<li><a href="#enroll">Enroll the source</a>, then press <span class="btn-demo primary">Start replication</span> — the agent copies your files straight into the destination, with delta passes keeping it current.</li>
</ol>
<div class="adm"><span class="t">Note</span>If the automatic receiver install stalls (some images/regions lack cloud-init), the card shows a <b>manual install command</b> — open the destination's Lish console, log in as root with the password you set, and paste it. Start replication unlocks as soon as the receiver answers.</div>
</section>

<section id="volume-boot">
<h2>Volume boot</h2>
<p>Replicates every disk <b>block for block</b> onto Block Storage volumes; at cutover each is cloned into a launchable image volume and a new Linode boots from them.</p>
<ol class="steps">
<li>Select <span class="field">Migration method</span> → <b>Block: separate Block Storage volume</b>.</li>
<li>Add <b>one disk row per whole source disk</b> (e.g. <code>/dev/sda</code>, 25&nbsp;GB — use whole disks, not partitions). The disk holding <code>/</code> must be the <b>first row</b>. Round sizes up.</li>
<li>Pick a plan, then press <span class="btn-demo primary">Create migration</span>. The appliance provisions one replication volume per disk (watch the <b>Storage provisioned</b> check turn green).</li>
<li><a href="#enroll">Enroll</a>, <a href="#replicate">start replication</a>, and <a href="#cutover">cut over</a> — at cutover the boot image is converted and <b>validated before you power off the source</b>.</li>
</ol>
</section>

<section id="disk-boot">
<h2>Disk boot</h2>
<p>Same block-for-block replication, but the destination boots from its own <b>local NVMe disk</b> — no separate volume is kept. Pick a plan whose disk fits the summed disk sizes.</p>
<ol class="steps">
<li>Select <span class="field">Migration method</span> → <b>Block: Linode local disk (NVMe)</b>, add the disk rows, create, enroll, replicate.</li>
<li>Cutover differs in one step: the appliance boots the new Linode into <b>Rescue Mode</b> and the card shows a <b>one-line copy command</b>. Open the instance's Lish console, paste the line, and watch the image stream onto the local disk — the instance then boots from it automatically.</li>
</ol>
</section>

<section id="enroll">
<h2>Enroll the source server</h2>
<p>Every migration card has an <b>Enroll the source server</b> panel with a one-line command generated for that migration:</p>
<figure><img src="/documentation/img/migration-card.png" alt="A migration card with the enrollment command"><figcaption>A freshly created migration: status <span class="pill-demo warn">waiting for agent</span>, validation checks, and the one-line enrollment command with its <span class="btn-demo plain">Copy</span> button.</figcaption></figure>
<ol class="steps">
<li>Press <span class="btn-demo plain">Copy</span> and run the command on the <b>source server</b> as root. It downloads the agent (integrity-pinned to your replication server's key), installs certificates, and starts a systemd timer.</li>
<li>Within about a minute the card's status flips to <span class="pill-demo ok">agent connected</span>. No data is copied yet — replication starts only when you say so.</li>
</ol>
<div class="adm warn"><span class="t">Warning</span>The source must be able to reach the replication server on the console port and TCP <b>5000–5100</b> (the per-migration receiver ports). "Connection failed" almost always means a firewall is blocking that range.</div>
</section>

<section id="replicate">
<h2>Start replication and monitor</h2>
<p>Press <span class="btn-demo primary">Start replication</span> (enabled once the agent is connected — and, for file transfer, once the destination is ready). Confirm the dialog and watch:</p>
<ul>
<li><b>Progress</b> — live percentage and transfer rate for block syncs; "copying files" with bytes-on-wire for file transfer.</li>
<li><b>Validation checks</b> — <b>Initial full sync complete</b> (or <b>Initial file copy complete</b>) is the gate that enables cutover. The pre-migration checks (agent connection, replication lag) track environment readiness while replicating.</li>
<li><b>RPO</b> — how old the last completed sync is. After the baseline, delta passes run every ~60&nbsp;s, so the copy stays current.</li>
</ul>
<p>You can <span class="btn-demo danger">Pause replication</span> at any time; <span class="btn-demo primary">Resume replication</span> continues with an incremental delta — never a full re-copy.</p>
</section>

<section id="cutover">
<h2>Cut over</h2>
<p>When the baseline is done, <span class="btn-demo primary">Cutover instance</span> enables. Cutover is guided in <b>three steps</b>, and the card tells you exactly when it is safe to power off the source:</p>
<ol class="steps">
<li><b>Stop replication &amp; prepare (this button).</b> For the block methods the appliance takes one final consistent pass (the source root is briefly remounted read-only), then <b>converts the boot image and validates it is bootable — before you power anything off</b>. If validation fails, the cutover aborts with the reason and the source keeps running. If the source is already powered off or idle, tick <i>"skip the read-only snapshot"</i> in the dialog. File transfer simply finishes its last copy pass.</li>
<li><b>Power off the source</b> — only when the card shows <i>"it is now safe to power off the source server."</i></li>
<li><b>Launch.</b> Press <span class="btn-demo primary">Launch instance</span>: volume boot clones the validated image and boots a new Linode from it; disk boot streams the image in Rescue Mode (one paste); file transfer <b>reboots the already-populated destination</b> into your migrated system.</li>
</ol>
<div class="adm tip"><span class="t">Tip</span>Before starting the cutover, stop the source's databases/heavy writers and let the RPO lag drop to ~0 so the final pass is small and current.</div>
</section>

<section id="finish">
<h2>Finish and clean up</h2>
<ol class="steps">
<li>When the card shows the green completion banner, press <span class="btn-demo done">Migration complete — remove source agent</span> and run the shown one-liner on the source to uninstall the agent.</li>
<li>Press <span class="btn-demo danger">Close migration</span> to clear the card. Your migrated Linode is kept, untouched; volume boot also removes the appliance's temporary replication volume.</li>
</ol>
</section>

<section id="sessions">
<h2>Sessions and security</h2>
<ul>
<li>A console session lasts <b>12 hours from sign-in</b> (fixed, not extended by activity). Signing out — or being signed out — <b>never stops replication</b>; migrations run in the <code>applianced</code> service independent of the browser.</li>
<li>The console is HTTPS with a self-signed certificate; the replication data plane is always <b>mutual TLS</b>. The Linode token is stored <b>encrypted at rest</b>.</li>
<li>Recover the password any time: <code>sudo /usr/local/bin/applianced -data-dir /var/lib/vm-repl -show-password</code></li>
<li>Restrict the console port to trusted networks where possible.</li>
</ul>
</section>

<section id="troubleshooting">
<h2>Troubleshooting</h2>
<table>
<tr><th>Symptom</th><th>Cause and fix</th></tr>
<tr><td>The <b>Destination OS image</b> dropdown says "add a Linode token"</td><td>No API token is saved. Add one in the <b>Linode automation</b> card (<a href="#api-token">guide</a>).</td></tr>
<tr><td>Status stays <span class="pill-demo bad">connection failed</span> after enrolling</td><td>The source can't reach TCP 5000–5100 on the replication server — open the range in every firewall in the path. The agent retries every 60&nbsp;s on its own.</td></tr>
<tr><td>File transfer: destination stuck "installing the file receiver"</td><td>The image/region lacks cloud-init/Metadata support. Use the <b>manual install command</b> shown on the card (Lish, as root). Start replication unlocks when the receiver answers.</td></tr>
<tr><td>Cutover fails with "the converted disk has no root/OS filesystem"</td><td>Either the wrong source device was selected (run <code>findmnt -no SOURCE /</code> on the source and migrate that whole disk), or the copy is incomplete/inconsistent — the message on the card gives the exact remedy. The failure now happens <b>before</b> you power off the source.</td></tr>
<tr><td>The source-details helper lists many <code>/dev/nbdN</code> "disks"</td><td>Harmless kernel network-block-device placeholders (the helper filters them out on current builds). Only migrate real disks such as <code>/dev/sda</code>.</td></tr>
<tr><td>Where are the logs?</td><td>Each card's <b>Activity log</b> (Expand for full history); on the replication server, <code>journalctl -u applianced -f</code>; on the source, <code>journalctl -u vmrepl-agent -n 50</code>.</td></tr>
</table>
</section>

<footer class="docfoot">vm-replication — migrate Linux servers to Akamai Cloud (Linode). This guide is served by your own replication server at <code>/documentation</code>.</footer>
</main>
</div>

<script>
// Copy button for code blocks.
function cp(btn){
  const pre=btn.parentElement.querySelector('pre');
  navigator.clipboard.writeText(pre.textContent).then(()=>{btn.textContent='Copied';setTimeout(()=>btn.textContent='Copy',1200);});
}
// Sidebar filter.
function filterNav(q){
  q=q.trim().toLowerCase();
  document.querySelectorAll('#sidenav .navgroup').forEach(g=>{
    let any=false;
    g.querySelectorAll('a').forEach(a=>{
      const hit=!q||a.textContent.toLowerCase().includes(q);
      a.style.display=hit?'':'none'; if(hit)any=true;
    });
    g.style.display=any?'':'none';
  });
}
// Scroll-spy: highlight the section currently in view.
const secs=[...document.querySelectorAll('main section[id]')];
const links=[...document.querySelectorAll('#sidenav a[href^="#"]')];
function spy(){
  let cur=secs[0]&&secs[0].id;
  for(const s of secs){ if(s.getBoundingClientRect().top<=90)cur=s.id; }
  links.forEach(a=>a.classList.toggle('active',a.getAttribute('href')==='#'+cur));
}
document.addEventListener('scroll',spy,{passive:true});spy();
</script>
</body></html>
`
