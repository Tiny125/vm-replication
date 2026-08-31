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
<meta name="color-scheme" content="light dark">
<title>vm-replication documentation</title>
<script>
/* Apply the saved theme before first paint (see the console's copy). The key is
   shared with the console, which is same-origin, so one choice themes both. */
try{var t=localStorage.getItem('vmrepl-theme');if(t==='dark'||t==='light')document.documentElement.dataset.theme=t}catch(e){}
</script>
<style>
 :root{
   color-scheme:light;
   --header:#1a237e; --header-text:#ffffff;
   --bg:#ffffff; --side:#fafafa; --border:#e0e0e0; --surface:#ffffff;
   --text:#212121; --muted:#616161; --accent:#0b5cd5; --accent-soft:#e8f0fe;
   --code-bg:#f5f5f5; --code-border:#e8e8e8;
   --note:#448aff; --tip:#00897b; --warn:#e65100;
   --note-bg:#f2f7ff; --tip-bg:#eef8f6; --warn-bg:#fdf3ec;
   --nav-hover:#f0f0f0;
   /* Console control colours (mirror the real console so demos look identical) */
   --btn-blue:#0071e3; --btn-red:#d8302a; --btn-green:#1d9b50;
   --btn-on:#ffffff; --btn-disabled:#a9cdf5; --btn-danger-line:#f0b9b7; --btn-plain:#f5f5f7;
   --field-line:#c8dbf8; --field-fg:#153e75;
   --pill-ok-bg:#e3f4e9; --pill-ok-fg:#136c38;
   --pill-warn-bg:#fdf1de; --pill-warn-fg:#8a5a06;
   --pill-bad-bg:#fde7e6; --pill-bad-fg:#a1211c;
   --shot-mat:#ffffff;
   --shadow-img:0 2px 10px rgba(0,0,0,.07);
 }
 @media (prefers-color-scheme:dark){
  :root:not([data-theme="light"]){
   color-scheme:dark;
   --header:#151a3d; --header-text:#f2f2f4;
   --bg:#0f0f11; --side:#17171a; --border:#38383d; --surface:#1c1c1f;
   --text:#f2f2f4; --muted:#9a9aa2; --accent:#7fb3ff; --accent-soft:#1d2b47;
   --code-bg:#26262a; --code-border:#38383d;
   --note:#7fb3ff; --tip:#4fc3ae; --warn:#f5a45c;
   --note-bg:#1a2437; --tip-bg:#123330; --warn-bg:#372413;
   --nav-hover:#232329;
   --btn-blue:#4da3ff; --btn-red:#ff8079; --btn-green:#5ddb96;
   --btn-on:#0f0f11; --btn-disabled:#2f4a6b; --btn-danger-line:#7a2e28; --btn-plain:#26262a;
   --field-line:#33528c; --field-fg:#c8ddff;
   --pill-ok-bg:#1e4429; --pill-ok-fg:#8fe8b4;
   --pill-warn-bg:#402f0f; --pill-warn-fg:#f7d488;
   --pill-bad-bg:#4a1d19; --pill-bad-fg:#ffb3ad;
   /* Screenshots are light-background PNGs of the console; give them a permanent
      light mat so they read as framed artifacts instead of glaring rectangles. */
   --shot-mat:#ffffff;
   --shadow-img:0 2px 10px rgba(0,0,0,.5);
  }
 }
 :root[data-theme="dark"]{
   color-scheme:dark;
   --header:#151a3d; --header-text:#f2f2f4;
   --bg:#0f0f11; --side:#17171a; --border:#38383d; --surface:#1c1c1f;
   --text:#f2f2f4; --muted:#9a9aa2; --accent:#7fb3ff; --accent-soft:#1d2b47;
   --code-bg:#26262a; --code-border:#38383d;
   --note:#7fb3ff; --tip:#4fc3ae; --warn:#f5a45c;
   --note-bg:#1a2437; --tip-bg:#123330; --warn-bg:#372413;
   --nav-hover:#232329;
   --btn-blue:#4da3ff; --btn-red:#ff8079; --btn-green:#5ddb96;
   --btn-on:#0f0f11; --btn-disabled:#2f4a6b; --btn-danger-line:#7a2e28; --btn-plain:#26262a;
   --field-line:#33528c; --field-fg:#c8ddff;
   --pill-ok-bg:#1e4429; --pill-ok-fg:#8fe8b4;
   --pill-warn-bg:#402f0f; --pill-warn-fg:#f7d488;
   --pill-bad-bg:#4a1d19; --pill-bad-fg:#ffb3ad;
   /* Screenshots are light-background PNGs of the console; give them a permanent
      light mat so they read as framed artifacts instead of glaring rectangles. */
   --shot-mat:#ffffff;
   --shadow-img:0 2px 10px rgba(0,0,0,.5);
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
 header a.consolelink{color:var(--header-text);background:rgba(255,255,255,.14);border:1px solid rgba(255,255,255,.35);
   padding:6px 14px;border-radius:6px;font-size:13.5px;text-decoration:none;white-space:nowrap}
 header a.consolelink:hover{background:rgba(255,255,255,.24)}
 header button.themebtn{color:var(--header-text);background:rgba(255,255,255,.14);border:1px solid rgba(255,255,255,.35);
   padding:6px 14px;border-radius:6px;font-size:13.5px;cursor:pointer;white-space:nowrap;min-width:74px;font-family:inherit}
 header button.themebtn:hover{background:rgba(255,255,255,.24)}

 /* ---- Layout ---- */
 .layout{display:flex;max-width:1400px;margin:0 auto;padding-top:56px}
 nav.sidebar{width:290px;flex-shrink:0;position:sticky;top:56px;height:calc(100vh - 56px);overflow-y:auto;
   background:var(--side);border-right:1px solid var(--border);padding:18px 0 40px}
 main{flex:1;min-width:0;padding:36px 48px 120px;max-width:900px}

 /* ---- Sidebar ---- */
 .navfilter{margin:0 18px 14px}
 .navfilter input{width:100%;padding:8px 12px;border:1px solid var(--border);border-radius:6px;font-size:13.5px;background:var(--surface);color:var(--text)}
 .navgroup{margin-top:16px}
 .navgroup>.gtitle{font-size:11.5px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);padding:4px 22px}
 nav.sidebar a{display:block;padding:6px 22px;font-size:14px;color:var(--text);text-decoration:none;border-left:3px solid transparent}
 nav.sidebar a:hover{background:var(--nav-hover)}
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
 .codeblock button.copy{position:absolute;top:9px;right:9px;border:1px solid var(--border);background:var(--surface);border-radius:6px;
   padding:4px 12px;font-size:12.5px;cursor:pointer;color:var(--muted)}
 .codeblock button.copy:hover{color:var(--text)}

 /* ---- Admonitions (text label, no icons) ---- */
 .adm{border-left:4px solid var(--note);background:var(--note-bg);border-radius:0 8px 8px 0;padding:12px 16px;margin:16px 0;font-size:14.5px}
 .adm .t{font-weight:700;font-size:12px;letter-spacing:.06em;text-transform:uppercase;display:block;margin-bottom:4px;color:var(--note)}
 .adm.tip{border-left-color:var(--tip);background:var(--tip-bg)}.adm.tip .t{color:var(--tip)}
 .adm.warn{border-left-color:var(--warn);background:var(--warn-bg)}.adm.warn .t{color:var(--warn)}

 /* ---- Screenshots ---- */
 figure{margin:18px 0}
 figure img{max-width:100%;border:1px solid var(--border);border-radius:10px;box-shadow:var(--shadow-img);background:var(--shot-mat);padding:8px}
 figcaption{font-size:13px;color:var(--muted);margin-top:7px}

 /* ---- Console-button demos: replicas of the real console controls ---- */
 .btn-demo{display:inline-block;padding:5px 16px;border-radius:980px;font-size:13.5px;font-weight:500;vertical-align:middle;
   border:1px solid transparent;white-space:nowrap;line-height:1.5}
 .btn-demo.primary{background:var(--btn-blue);color:var(--btn-on)}
 .btn-demo.disabled{background:var(--btn-disabled);color:var(--btn-on)}
 .btn-demo.danger{background:var(--surface);color:var(--btn-red);border-color:var(--btn-danger-line)}
 .btn-demo.done{background:var(--btn-green);color:var(--btn-on)}
 .btn-demo.plain{background:var(--btn-plain);color:var(--text);border-color:var(--border)}
 .field{background:var(--accent-soft);border:1px solid var(--field-line);border-radius:5px;padding:1px 7px;font-size:13.5px;color:var(--field-fg);white-space:nowrap}
 .pill-demo{display:inline-block;padding:2px 11px;border-radius:980px;font-size:12.5px;font-weight:500}
 .pill-demo.ok{background:var(--pill-ok-bg);color:var(--pill-ok-fg)}.pill-demo.warn{background:var(--pill-warn-bg);color:var(--pill-warn-fg)}.pill-demo.bad{background:var(--pill-bad-bg);color:var(--pill-bad-fg)}

 .steps{counter-reset:step;list-style:none;margin-left:0}
 .steps>li{counter-increment:step;position:relative;padding-left:44px;margin:14px 0}
 .steps>li::before{content:counter(step);position:absolute;left:0;top:1px;width:28px;height:28px;border-radius:50%;
   background:var(--header);color:var(--btn-on);font-size:14px;font-weight:700;display:flex;align-items:center;justify-content:center}

 footer.docfoot{margin-top:70px;padding-top:18px;border-top:1px solid var(--border);color:var(--muted);font-size:13.5px}
 @media (max-width: 900px){nav.sidebar{display:none}main{padding:28px 22px 80px}}
</style>
</head><body>

<header>
  <div class="brand">vm-replication <span>documentation</span></div>
  <div class="grow"></div>
  <button id="themebtn" class="themebtn" onclick="cycleTheme()" title="Switch between automatic, light and dark appearance">Auto</button>
  <a class="consolelink" href="/">Open the console</a>
</header>

<div class="layout">
<nav class="sidebar" id="sidenav">
  <div class="navfilter"><input id="navq" type="text" placeholder="Filter the guide…" oninput="filterNav(this.value)"></div>
  <div class="navgroup"><div class="gtitle">Getting started</div>
    <a href="#introduction">Introduction</a>
    <a href="#supported-os">Supported operating systems</a>
    <a href="#install">Install the replication server</a>
    <a href="#sign-in">Sign in</a>
  </div>
  <div class="navgroup"><div class="gtitle">Console guide</div>
    <a href="#overview">Console overview</a>
    <a href="#api-token">Add your Linode API token</a>
    <a href="#source-check">Check the source first</a>
    <a href="#source-details">Find your source details</a>
  </div>
  <div class="navgroup"><div class="gtitle">Create a migration</div>
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
<p>Migrations are <b>block-for-block disk boot</b>: every source disk is copied whole, the boot disk lands on the new Linode's own <b>local NVMe disk</b> (free with the plan), and any further disks become Block Storage volumes attached to the same instance — see <a href="#disk-boot">Disk boot</a> for the full walkthrough.</p>
</section>

<section id="supported-os">
<h2>Supported operating systems</h2>
<p>These versions have been <b>migrated end to end and verified</b> — created, replicated, cut over, booted, and checked byte-for-byte against the source. This is a list of what has actually been tested, not a compatibility matrix: a version missing from it has not been proven, rather than known to fail.</p>
<table>
<tr><th>Operating system</th><th>Verified</th></tr>
<tr><td><b>Ubuntu 26.04 LTS</b></td><td>Migrated and booted; data verified</td></tr>
<tr><td><b>Ubuntu 25.10</b></td><td>Migrated and booted; data verified</td></tr>
<tr><td><b>Ubuntu 24.04 LTS</b></td><td>Migrated and booted; data verified</td></tr>
<tr><td><b>Ubuntu 22.04 LTS</b></td><td>Migrated and booted; data verified</td></tr>
<tr><td><b>Ubuntu 20.04 LTS</b></td><td>Migrated and booted; data verified</td></tr>
</table>
<p>Each was migrated with a <b>separate data volume</b> alongside the boot disk, so multi-disk is covered too. In every case the destination came up on <b>its own kernel</b> with its security module intact (AppArmor enabled with the same profile count as the source), the data volume mounted by UUID, and the application serving from it.</p>
<div class="adm"><span class="t">Scope</span><b>x86_64 only.</b> The replication agent is built for <code>linux/amd64</code>; ARM/aarch64 sources are not supported. Windows and other non-Linux systems are out of scope — the boot conversion is Linux-only.</div>
<div class="adm tip"><span class="t">Tip</span>Running something not on this list? Replication itself is distro-agnostic — it copies blocks and never parses your filesystem. Only the post-copy boot conversion is OS-aware, and it handles the Debian/Ubuntu (<code>apt</code>, <code>initramfs-tools</code>, <code>grub</code>) and RHEL (<code>dnf</code>, <code>dracut</code>, <code>grub2</code>) families. Run the <a href="#source-check">Source check</a> first: it reports a verdict for your specific server before you commit to anything.</div>
</section>

<section id="install">
<h2>Install the replication server</h2>
<p>You need one Linode to act as the replication server. A <b>2&nbsp;GB shared plan</b> is enough for 1–3 concurrent block disks (see <code>CONSOLE.md</code> in the repository for detailed sizing).</p>
<ol class="steps">
<li>Create a Linode (Ubuntu or Debian recommended) and SSH in as <b>root</b>.</li>
<li>Run the one-command installer:
<div class="codeblock"><pre>curl -fsSL https://raw.githubusercontent.com/Tiny125/vm-replication/main/scripts/bootstrap.sh | sudo bash</pre><button class="copy" onclick="cp(this)">Copy</button></div>
It downloads the prebuilt release tarball matching this machine's CPU architecture (linux/amd64 or linux/arm64), <b>verifies its SHA-256 against the release's <code>SHA256SUMS</code> before extracting anything</b>, unpacks it to <code>/usr/local/src/vm-replication-&lt;version&gt;</code> (with a stable <code>/usr/local/src/vm-replication</code> symlink to it), and runs the installer from there. No Go toolchain, no compiler, no <code>git clone</code> needed — the binaries are prebuilt and static. To pin an exact release instead of always installing latest — recommended for production, so a later re-run cannot silently move you to whatever is newest — pass the tag through the pipe: <code>curl -fsSL …/bootstrap.sh | sudo VMREPL_REF=v0.1.0 bash</code>. Re-running is safe and upgrades in place: an existing region and port are preserved.</li>
<li>The installer generates certificates and an <b>admin password</b>, installs the <code>applianced</code> systemd service, and prints a summary:
<div class="codeblock"><pre>================ REPLICATION SERVER READY ================
 Console:   https://203.0.113.10
 Guide:     https://203.0.113.10/documentation
 Password:  681af4b11221bacb88e34080
 Cert SHA-256 (verify this in your browser's certificate dialog):
   AB:CD:...:EF</pre></div></li>
<li>Keep that output — the password is also saved on the server at <code>/var/lib/vm-repl/initial-admin-password.txt</code>.</li>
</ol>
<div class="adm"><span class="t">Note</span>Useful installer flags: <code>--public-host &lt;ip&gt;</code> (if auto-detection picks the wrong address), <code>--region &lt;region&gt;</code> and <code>--port &lt;port&gt;</code> — both are only needed to <i>override</i> the defaults. The region is detected from the Linode Metadata service, and the port defaults to 443. Once set, region and port are stored in <code>/etc/vm-repl/applianced.env</code>; re-running the installer to upgrade refreshes the service without overwriting them, so edit that file (then <code>systemctl restart applianced</code>) to change them later.</div>
<div class="adm tip"><span class="t">Tip</span>Prefer to build from source instead? <code>git clone https://github.com/Tiny125/vm-replication.git &amp;&amp; cd vm-replication &amp;&amp; sudo scripts/install-replication-server.sh</code> — the installer bootstraps its own build dependencies (Go toolchain, <code>make</code>, <code>gcc</code>). Useful when working from a branch, or on an architecture without a published release.</div>
</section>

<section id="sign-in">
<h2>Sign in</h2>
<p>Browse to <code>https://&lt;replication-server-ip&gt;</code>. The console uses a <b>self-signed certificate</b>, so your browser warns on first visit — that is expected. Before entering the password, open the browser's certificate dialog and confirm the <b>SHA-256 fingerprint matches</b> the one the installer printed. Then sign in:</p>
<figure><img src="/documentation/img/login.png" alt="The console sign-in card"><figcaption>The sign-in page. The password was generated at install time.</figcaption></figure>
<div class="adm tip"><span class="t">Tip</span>Forgot the password? Retrieve it on the replication server, without disturbing anything: <code>sudo /usr/local/bin/applianced -data-dir /var/lib/vm-repl -show-password</code></div>
</section>

<section id="overview">
<h2>Console overview</h2>
<p>Everything happens on one page:</p>
<figure><img src="/documentation/img/console-overview.png" alt="The whole console page"><figcaption>Top to bottom: the Linode automation (API token) card, the New migration form, and one card per migration.</figcaption></figure>
<ul>
<li><b>Linode automation</b> — paste your Linode API token here (next section).</li>
<li><b>New migration</b> — register a source server and its disks.</li>
<li><b>Migrations</b> — one card per migration: status pills such as <span class="pill-demo warn">waiting for agent</span> and <span class="pill-demo ok">agent connected</span>, live progress, validation checks, the activity log, and the action buttons. The page refreshes itself; the <span class="btn-demo plain">Refresh</span> button on each card forces it.</li>
</ul>
</section>

<section id="api-token">
<h2>Add your Linode API token</h2>
<p>The token lets the console act on your Linode account: provision replication volumes, clone disks, and launch the cutover instance. It's needed for everything past evaluation — provisioning volumes, cloning disks, and launching the cutover instance.</p>
<ol class="steps">
<li>Sign in to <a href="https://cloud.linode.com/profile/tokens" target="_blank" rel="noopener">Linode Cloud Manager → Profile → API Tokens</a> and create a <b>Personal Access Token</b> with scopes:
<table>
<tr><th>Scope</th><th>Access</th><th>Used for</th></tr>
<tr><td>Linodes</td><td>Read/Write</td><td>launching the cutover instance</td></tr>
<tr><td>Volumes</td><td>Read/Write</td><td>replication + image volumes</td></tr>
<tr><td>Images</td><td>Read/Write</td><td>disk-boot conversion</td></tr>
<tr><td>Object Storage</td><td>Read/Write</td><td>optional audit logs</td></tr>
</table></li>
<li>Paste the token into the <b>Linode automation</b> card and press <span class="btn-demo plain">Save</span>:
<figure><img src="/documentation/img/settings-token.png" alt="The Linode automation card"><figcaption>The token is stored encrypted at rest (AES-256-GCM) and only ever sent to api.linode.com.</figcaption></figure></li>
</ol>
<div class="adm warn"><span class="t">Warning</span>Without a token, disk boot cannot size a plan and a migration cannot be created — add one in the <b>Linode automation</b> card first.</div>
</section>

<section id="source-check">
<h2>Check the source first</h2>
<p>Before creating a migration, run the <b>Source check</b> (its own tab in the console header) — a <b>read-only pre-migration assessment</b> that tells you whether a server can migrate with disk boot, and why not if it can't.</p>
<ol class="steps">
<li>Open the <b>Source check</b> tab and press <span class="btn-demo primary">Generate check command</span>.</li>
<li>Run the shown one-line command on the <b>source server</b> as root (it is valid for 30 minutes). The command only <b>reads</b> system facts — OS, CPU architecture, disk layout, filesystems, SELinux, and live network reachability of the replication port — sends one report back, and exits. <b>Nothing is installed</b>, so there is nothing to remove afterwards.</li>
<li>The tab updates by itself when the report arrives:</li>
</ol>

<figure><img src="/documentation/img/source-check.png" alt="A completed source check"><figcaption>A completed check: the source&rsquo;s own facts, a verdict for the migration method, and the disks it found &mdash; all before you commit to anything.</figcaption></figure>
<div class="adm tip"><span class="t">Tip</span>Run this on every server you plan to migrate, before anything else. A "Not supported" verdict (for example a LUKS-encrypted root) tells you up front that the server needs remediation before it can migrate — instead of finding out at cutover.</div>
<div class="adm"><span class="t">Note</span>The full result is also printed <b>in the source server's own terminal</b>. If the source cannot reach the replication server, the terminal result still appears in full, with a note that the network to the migration instance is not accessible — fix the ports (console port + TCP 5000–5100), then re-run so the console receives it too.</div>
</section>

<section id="source-details">
<h2>Find your source details</h2>
<p>The form needs a few facts about the source. Expand <b>How do I find the source details?</b> at the top of the New-migration form and run the copyable command on your source server — it prints everything the form asks for:</p>
<figure><img src="/documentation/img/source-helper.png" alt="The source-details helper"><figcaption>Hostname, reachable IP, OS, and every real data disk to add as a row on the New-migration form.</figcaption></figure>
</section>

<section id="disk-boot">
<h2>Disk boot</h2>
<p>Every migration is a <b>block-for-block</b> copy of every source disk — the exact disk contents, not just files — replicated continuously (an initial full sync, then deltas every ~60&nbsp;s). At cutover the boot image is converted for Linode and <b>validated as bootable before you're asked to power off the source</b>. The boot disk lands on the destination's own <b>local NVMe disk</b> at no extra cost, and any further disks become Block Storage volumes attached to the same instance.</p>
<figure><img src="/documentation/img/new-migration.png" alt="The New-migration form"><figcaption>The New-migration form: source disks and plan. The Migration method field is fixed to local disk — this console offers no other method.</figcaption></figure>
<ol class="steps">
<li>Add <b>one disk row per whole source disk</b> (e.g. <code>/dev/sda</code>, 25&nbsp;GB — use whole disks, not partitions). The disk holding <code>/</code> must be the <b>first row</b> (it becomes the boot disk). Round sizes up. A migration is capped at <b>8 disks</b>.</li>
<li>Pick a plan whose <b>local disk fits the boot disk</b> — the console only lists plans that do, and names a bigger one if your first choice is too small — then press <span class="btn-demo primary">Create migration</span>.</li>
<li><a href="#enroll">Enroll</a> the source and <a href="#replicate">start replication</a>; the agent replicates every disk to the appliance.</li>
<li><a href="#cutover">Cut over</a>: stop the source, click <span class="btn-demo primary">Cutover instance</span> — the appliance takes a final pass and <b>validates the converted boot image while the source is still running</b>.</li>
<li>Power off the source, then click <span class="btn-demo primary">Launch instance</span>: the new Linode boots into <b>Rescue Mode</b> and the card shows a <b>one-line copy command</b> — open the instance's Lish console and paste it. The image streams onto the local disk with live progress; the instance powers itself off and the appliance boots it from the local disk automatically.</li>
</ol>
<div class="adm warn"><span class="t">Warning</span>The Lish paste is genuine manual work — plan for someone to be watching cutover, not walking away after clicking Launch. The plan-fit requirement is also real, but only the <b>boot disk</b> has to fit the plan's local disk — any further disks become Block Storage and aren't limited by it — so a smaller plan than you'd expect is often enough; the console names the smallest plan that fits if your first pick is too small.</div>
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
<p>Press <span class="btn-demo primary">Start replication</span> (enabled once the agent is connected). Confirm the dialog and watch:</p>
<ul>
<li><b>Progress</b> — live percentage and transfer rate for the initial full sync.</li>
<li><b>Validation checks</b> — <b>Initial full sync complete</b> is the gate that enables cutover. The pre-migration checks (agent connection, replication lag) track environment readiness while replicating.</li>
<li><b>RPO</b> — how old the last completed sync is. After the baseline, delta passes run every ~60&nbsp;s, so the copy stays current.</li>
</ul>
<p>You can <span class="btn-demo danger">Pause replication</span> at any time; <span class="btn-demo primary">Resume replication</span> continues with an incremental delta — never a full re-copy.</p>
</section>

<section id="cutover">
<h2>Cut over</h2>
<p>When the baseline is done, <span class="btn-demo primary">Cutover instance</span> enables. Cutover is guided in <b>three steps</b>, and the card tells you exactly when it is safe to power off the source:</p>
<ol class="steps">
<li><b>Stop replication &amp; prepare (this button).</b> The appliance takes one final consistent pass (the source root is briefly remounted read-only), then <b>converts the boot image and validates it is bootable — before you power anything off</b>. If validation fails, the cutover aborts with the reason and the source keeps running. If the source is already powered off or idle, tick <i>"skip the read-only snapshot"</i> in the dialog.</li>
<li><b>Power off the source</b> — only when the card shows <i>"it is now safe to power off the source server."</i></li>
<li><b>Launch.</b> Press <span class="btn-demo primary">Launch instance</span>: the new Linode boots into Rescue Mode and the card shows a one-line copy command — paste it in the instance's Lish console. The image streams onto the local disk and the instance boots automatically once the copy finishes.</li>
</ol>
<div class="adm tip"><span class="t">Tip</span>Before starting the cutover, stop the source's databases/heavy writers and let the RPO lag drop to ~0 so the final pass is small and current.</div>
</section>

<section id="finish">
<h2>Finish and clean up</h2>
<ol class="steps">
<li>When the card shows the green completion banner, press <span class="btn-demo done">Migration complete — remove source agent</span> and run the shown one-liner on the source to uninstall the agent.</li>
<li>Press <span class="btn-demo danger">Close migration</span> to clear the card. Your migrated Linode is kept, untouched; the appliance's temporary replication volume is removed.</li>
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
<tr><td>Local-disk boot rejects the plan / can't size it</td><td>No API token is saved, or the token lacks the Linodes scope. Add/fix it in the <b>Linode automation</b> card (<a href="#api-token">guide</a>).</td></tr>
<tr><td>Status stays <span class="pill-demo bad">connection failed</span> after enrolling</td><td>The source can't reach TCP 5000–5100 on the replication server — open the range in every firewall in the path. The agent retries every 60&nbsp;s on its own.</td></tr>
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

/* Appearance: same three states and the same storage key as the console, so a
   choice made on either surface applies to both. */
function themePref(){
  try{ var t=localStorage.getItem('vmrepl-theme'); return (t==='dark'||t==='light')?t:'auto'; }
  catch(e){ return 'auto'; }
}
function applyTheme(pref){
  if(pref==='auto') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme=pref;
  try{ pref==='auto' ? localStorage.removeItem('vmrepl-theme') : localStorage.setItem('vmrepl-theme',pref); }catch(e){}
  var b=document.getElementById('themebtn');
  if(b) b.textContent = pref==='auto' ? 'Auto' : (pref==='light' ? 'Light' : 'Dark');
}
function cycleTheme(){
  var order={auto:'light',light:'dark',dark:'auto'};
  applyTheme(order[themePref()]||'auto');
}
applyTheme(themePref());
</script>
</body></html>
`
