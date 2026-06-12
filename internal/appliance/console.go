package appliance

import "net/http"

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(consoleHTML))
}

const consoleHTML = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>vm-replication console</title>
<style>
 :root{
   --bg:#fbfbfd; --surface:#ffffff; --surface2:#f5f5f7; --border:#e3e3e6;
   --text:#1d1d1f; --muted:#6e6e73; --accent:#0071e3; --accent-press:#0060c0;
   --green:#1d9b50; --amber:#bf6a02; --red:#d8302a;
   --shadow:0 1px 3px rgba(0,0,0,.06),0 6px 20px rgba(0,0,0,.04);
 }
 *{margin:0;padding:0;box-sizing:border-box}
 body{background:var(--bg);color:var(--text);
   font-family:-apple-system,BlinkMacSystemFont,"SF Pro Text","Segoe UI",Roboto,Helvetica,Arial,sans-serif;
   line-height:1.5;-webkit-font-smoothing:antialiased;padding:0}
 .wrap{max-width:960px;margin:0 auto;padding:32px 24px 80px}
 h1{font-size:28px;font-weight:600;letter-spacing:-.02em}
 h1 .dot{color:var(--accent)}
 h2{font-size:15px;font-weight:600;letter-spacing:-.01em;margin:0 0 14px}
 .sub{color:var(--muted);font-size:14px;margin:4px 0 28px}
 .card{background:var(--surface);border:1px solid var(--border);border-radius:16px;
   padding:22px;margin-bottom:18px;box-shadow:var(--shadow)}
 .mig{background:var(--surface);border:1px solid var(--border);border-radius:16px;padding:20px;margin-bottom:14px;box-shadow:var(--shadow)}
 label{display:block;font-size:12px;font-weight:500;color:var(--muted);margin:10px 0 5px}
 input,select{font:inherit;font-size:14px;background:var(--surface);color:var(--text);
   border:1px solid var(--border);border-radius:10px;padding:9px 12px;width:100%;transition:border-color .15s,box-shadow .15s}
 input:focus,select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(0,113,227,.15)}
 .row{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px}
 button{font:inherit;font-size:14px;font-weight:500;background:var(--surface2);color:var(--text);
   border:1px solid var(--border);border-radius:980px;padding:8px 16px;cursor:pointer;
   transition:transform .08s ease,background .15s,box-shadow .15s,opacity .15s}
 button:hover{background:#ececef}
 button:active{transform:scale(.96)}
 button:disabled{opacity:.4;cursor:not-allowed;transform:none}
 button.primary{background:var(--accent);color:#fff;border-color:var(--accent)}
 button.primary:hover{background:var(--accent-press)}
 button.danger{color:var(--red);border-color:#f0c9c7;background:#fff}
 button.danger:hover{background:#fdeceb}
 button.busy{position:relative;color:transparent!important}
 button.busy::after{content:"";position:absolute;left:50%;top:50%;width:14px;height:14px;margin:-7px 0 0 -7px;
   border:2px solid rgba(255,255,255,.5);border-top-color:#fff;border-radius:50%;animation:spin .7s linear infinite}
 button.busy.light::after{border-color:rgba(0,0,0,.25);border-top-color:var(--text)}
 @keyframes spin{to{transform:rotate(360deg)}}
 .bar{display:flex;gap:10px;align-items:center;margin-bottom:24px;flex-wrap:wrap}
 button.tab{background:transparent;border-color:transparent;color:var(--muted);font-weight:600}
 button.tab:hover{background:var(--surface2)}
 button.tab.active{background:var(--surface);border-color:var(--border);color:var(--text);box-shadow:var(--shadow)}
 .hide{display:none}
 .muted{color:var(--muted)}
 .err{color:var(--red);font-size:13px;margin-top:8px}
 table{width:100%;border-collapse:collapse;font-size:13px}
 th,td{text-align:left;padding:9px 10px;border-bottom:1px solid var(--border);vertical-align:top}
 th{color:var(--muted);font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:.04em}
 tr:last-child td{border-bottom:none}
 .pill{display:inline-block;padding:3px 10px;border-radius:980px;font-size:12px;font-weight:500}
 .pill.ok{background:#e6f5ec;color:var(--green)} .pill.warn{background:#fbeedd;color:var(--amber)}
 .pill.bad{background:#fbe7e6;color:var(--red)} .pill.muted{background:var(--surface2);color:var(--muted)}
 .y{color:var(--green)} .x{color:var(--red)}
 code,pre{background:var(--surface2);border:1px solid var(--border);border-radius:10px;padding:10px;
   display:block;white-space:pre-wrap;word-break:break-all;font-size:12.5px;
   font-family:"SF Mono",ui-monospace,Menlo,Consolas,monospace;color:#0a4ea0}
 .prog{height:8px;background:var(--surface2);border-radius:980px;overflow:hidden;margin-top:8px}
 .prog>div{height:100%;background:var(--accent);transition:width .4s ease;border-radius:980px}
 .prog.indet>div{width:35%;animation:slide 1.1s ease-in-out infinite}
 @keyframes slide{0%{margin-left:-35%}100%{margin-left:100%}}
 details{margin:10px 0;border-top:1px solid var(--border);padding-top:10px}
 details>summary{cursor:pointer;color:var(--accent);font-size:13px;font-weight:500;list-style:none;user-select:none}
 details>summary::-webkit-details-marker{display:none}
 details>summary::before{content:"›";display:inline-block;margin-right:6px;transition:transform .15s}
 details[open]>summary::before{transform:rotate(90deg)}
 details>div{margin-top:10px}
 .banner{border:1px solid #cde8d8;background:#f1faf4;border-radius:12px;padding:12px 14px;margin:10px 0;font-size:13.5px;color:#0f5c30}
 .banner a{color:var(--accent)}
 .actions{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-top:14px}
 .resultbox{margin-top:8px;font-size:13px;border-radius:10px;padding:9px 12px;border:1px solid var(--border);background:var(--surface2)}
 .resultbox.ok{background:#f1faf4;border-color:#cde8d8;color:#0f5c30}
 .resultbox.bad{background:#fdeceb;border-color:#f0c9c7;color:#a3201c}
 .log{max-height:240px;overflow:auto;font-size:12.5px;font-family:"SF Mono",ui-monospace,Menlo,monospace}
 .logrow{display:flex;gap:10px;padding:3px 0;border-bottom:1px solid var(--border)}
 .logrow:last-child{border-bottom:none}
 .logrow .t{color:var(--muted);white-space:nowrap}
 .logrow.error .m{color:var(--red)} .logrow.warn .m{color:var(--amber)}
 .flash{animation:flash .8s ease}
 @keyframes flash{0%{background:rgba(0,113,227,.10)}100%{background:transparent}}
 .center{display:flex;flex-direction:column;align-items:center;gap:14px;padding:36px 0;color:var(--muted)}
 .spinner{width:26px;height:26px;border:3px solid var(--surface2);border-top-color:var(--accent);border-radius:50%;animation:spin .8s linear infinite}
 a{color:var(--accent);text-decoration:none} a:hover{text-decoration:underline}
 .login-card{max-width:380px;margin:8vh auto 0}
</style></head>
<body>
<div class="wrap">
  <h1>vm-<span class="dot">replication</span></h1>
  <div class="sub">Migrate Linux servers to Akamai Cloud (Linode).</div>

  <!-- LOGIN -->
  <div id="login" class="card login-card hide">
    <h2>Sign in</h2>
    <label>Console password</label>
    <input id="pw" type="password" placeholder="generated at install" onkeydown="if(event.key==='Enter')login(this)">
    <div style="margin-top:14px"><button class="primary" onclick="login(this)">Sign in</button></div>
    <div id="loginErr" class="err"></div>
  </div>

  <!-- APP -->
  <div id="app" class="hide">
    <div class="bar">
      <button id="tabMig" class="tab active" onclick="nav('mig')">Migrations</button>
      <button id="tabConn" class="tab" onclick="nav('conn')">Connection test</button>
      <span style="flex:1"></span>
      <button id="refreshBtn" onclick="refresh(true)">Refresh</button>
      <span class="muted" id="updated"></span>
      <button onclick="logout()">Sign out</button>
    </div>

  <!-- VIEW: CONNECTION TEST -->
  <div id="view-conn" class="hide">
    <div class="card">
      <h2>Connection test</h2>
      <div class="muted" style="font-size:13.5px;margin-bottom:14px">
        Checks whether this replication appliance can reach a source server over the network.
        It runs an <b>ICMP ping</b> for basic reachability, then a <b>TCP probe</b> across the
        replication port range (<code style="display:inline;padding:1px 5px">5000–5100</code>, sampled every 10th port).
        Nothing is installed or changed on either side — it's a read-only diagnostic.
        <details style="margin-top:8px"><summary>How to read the results</summary><div style="font-size:13px">
          During replication the <b>source agent dials out to this appliance</b> on 5000–5100, so the source
          itself usually has nothing listening there — a probe that says <i>“connection refused”</i> still means
          the host is <b>reachable</b> and only a firewall/security-group is the open question. A <i>“timed out”</i>
          result means traffic is being <b>filtered</b> (security group / firewall) or the host is down. Use this to
          confirm the two machines can see each other before enrolling the agent.
        </div></details>
      </div>
      <div class="row">
        <div><label>Source server IP or hostname</label><input id="conn_ip" placeholder="e.g. 10.0.1.23" onkeydown="if(event.key==='Enter')runConnTest(this)"></div>
      </div>
      <div style="margin-top:14px"><button id="connBtn" class="primary" onclick="runConnTest(this)">Test connection</button></div>
      <div id="connOut" class="hide" style="margin-top:16px"></div>
    </div>
  </div>

  <!-- VIEW: MIGRATIONS -->
  <div id="view-mig">
    <div id="settings" class="card"></div>

    <div class="card">
      <h2>New migration</h2>
      <details>
        <summary>How do I find the source details?</summary>
        <div>
          <div class="muted" style="font-size:13px;margin-bottom:8px">Run this on your <b>source server</b> — it lists the hostname and every whole disk (add a row per disk):</div>
          <div style="display:flex;gap:8px;align-items:flex-start;margin-bottom:8px">
            <pre id="srcCmd" style="flex:1;margin:0">echo "Hostname : $(hostname)"; lsblk -b -d -n -o NAME,SIZE,TYPE | awk '$3=="disk"{printf "Device   : /dev/%s\nSize(GB) : %d\n", $1, ($2+1073741823)/1073741824}'</pre>
            <button onclick="copyText(document.getElementById('srcCmd').textContent,this)">Copy</button>
          </div>
          <div class="muted" style="font-size:12px">Add <b>one row per whole disk</b> (e.g. <code style="display:inline;padding:1px 5px">/dev/sda</code>). The disk with the root filesystem <code style="display:inline;padding:1px 5px">/</code> is the <b>boot disk</b> — put it first. Round sizes up.</div>
        </div>
      </details>
      <div class="row">
        <div><label>Name</label><input id="m_name" placeholder="web01"></div>
        <div><label>Source hostname</label><input id="m_host" placeholder="web01.prod"></div>
      </div>
      <label style="margin-top:12px">Source disks (first = boot disk)</label>
      <div id="disks"></div>
      <div style="margin-top:8px"><button onclick="addDisk()">+ Add disk</button></div>
      <div style="margin-top:16px"><button id="createBtn" class="primary" onclick="createMig(this)">Create migration</button></div>
      <div id="createErr" class="err"></div>
    </div>

    <h2 style="margin:6px 0 12px">Migrations</h2>
    <div id="migs"></div>
  </div>
  </div>
</div>

<script>
const $=id=>document.getElementById(id);
function esc(s){return String(s==null?'':s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function busy(btn,on){if(!btn)return;btn.classList.toggle('busy',on);if(!btn.classList.contains('primary'))btn.classList.toggle('light',on);btn.disabled=on}
async function api(method,path,body){
  const o={method,headers:{},credentials:'same-origin'};
  if(body!==undefined){o.headers['Content-Type']='application/json';o.body=JSON.stringify(body)}
  const r=await fetch(path,o);
  if(r.status===401){show('login');throw new Error('unauthorized')}
  const t=await r.text(); let j={}; try{j=t?JSON.parse(t):{}}catch(e){}
  if(!r.ok)throw new Error(j.error||r.statusText);
  return j;
}
function show(which){$('login').classList.toggle('hide',which!=='login');$('app').classList.toggle('hide',which!=='app')}
function copyText(t,btn){
  const done=()=>{if(btn){const o=btn.textContent;btn.textContent='Copied!';setTimeout(()=>{btn.textContent=o},1200)}};
  if(navigator.clipboard&&window.isSecureContext){navigator.clipboard.writeText(t).then(done).catch(()=>legacyCopy(t,done))}else legacyCopy(t,done);
}
function legacyCopy(t,done){const ta=document.createElement('textarea');ta.value=t;ta.style.position='fixed';ta.style.opacity='0';document.body.appendChild(ta);ta.focus();ta.select();try{document.execCommand('copy');done&&done()}catch(e){}document.body.removeChild(ta)}
function flash(el){if(!el)return;el.classList.remove('flash');void el.offsetWidth;el.classList.add('flash')}
function fmtBytes(n){if(!n)return '0 B';const u=['B','KiB','MiB','GiB','TiB'];let i=0;while(n>=1024&&i<u.length-1){n/=1024;i++}return n.toFixed(1)+' '+u[i]}
function fmtDur(s){if(s==null||s<0)return '—';s=Math.round(s);if(s<60)return s+'s';if(s<3600)return Math.floor(s/60)+'m '+(s%60)+'s';return Math.floor(s/3600)+'h '+Math.floor((s%3600)/60)+'m'}
function fmtTime(t){try{return new Date(t).toLocaleTimeString()}catch(e){return ''}}

async function login(btn){
  $('loginErr').textContent=''; busy(btn,true);
  try{await api('POST','/login',{password:$('pw').value});$('pw').value='';start()}
  catch(e){$('loginErr').textContent='Login failed: '+e.message}
  finally{busy(btn,false)}
}
async function logout(){try{await api('POST','/logout')}catch(e){}show('login')}

function nav(which){
  const mig=which==='mig';
  $('view-mig').classList.toggle('hide',!mig);
  $('view-conn').classList.toggle('hide',mig);
  $('tabMig').classList.toggle('active',mig);
  $('tabConn').classList.toggle('active',!mig);
}
async function runConnTest(btn){
  const ip=$('conn_ip').value.trim();const out=$('connOut');
  if(!ip){out.classList.remove('hide');out.innerHTML='<div class="resultbox bad">Enter a source IP or hostname first.</div>';return}
  busy(btn,true);
  out.classList.remove('hide');
  out.innerHTML='<div class="center"><div class="spinner"></div><div>Testing connection to '+esc(ip)+'…</div></div>';
  try{
    const r=await api('POST','/api/v1/diagnostics/connection',{ip:ip});
    const open=(r.ports||[]).filter(p=>p.open).length;
    const reachable=r.ping_ok||open>0||(r.ports||[]).some(p=>/refused/.test(p.detail));
    let h='<div class="resultbox '+(reachable?'ok':'bad')+'">'+
      (reachable?'✔ <b>'+esc(r.ip)+' is reachable from this appliance.</b>':'✘ <b>Could not reach '+esc(r.ip)+'.</b>')+'</div>';
    h+='<table style="margin-top:12px"><tr><th>Check</th><th>Result</th><th>Detail</th></tr>';
    h+='<tr><td>ICMP ping</td><td>'+(r.ping_ok?'<span class="y">✔ reply</span>':'<span class="x">✘ no reply</span>')+'</td><td class="muted">'+esc(r.ping_detail)+'</td></tr>';
    for(const p of (r.ports||[])){
      h+='<tr><td>TCP '+p.port+'</td><td>'+(p.open?'<span class="y">✔ open</span>':'<span class="muted">closed</span>')+'</td><td class="muted">'+esc(p.detail)+'</td></tr>';
    }
    h+='</table><div class="muted" style="font-size:12px;margin-top:8px">Ports 5000–5100 sampled every 10th port. The source normally has no listener there (the agent dials out to this appliance), so “connection refused” still confirms reachability.</div>';
    out.innerHTML=h;
  }catch(e){out.innerHTML='<div class="resultbox bad">Error: '+esc(e.message)+'</div>'}
  finally{busy(btn,false)}
}

async function loadSettings(){
  const st=await api('GET','/api/v1/settings');
  let h='<h2>Linode automation</h2>';
  if(st.linode_token_set){
    h+='<div style="display:flex;gap:12px;align-items:center;flex-wrap:wrap"><span><span class="y">✔</span> Linode API token stored. '+
       (st.linode_automation?('Appliance Linode '+esc(st.appliance_linode_id)+'; volumes created in its region.'):'(appliance Linode id unknown — file-fallback mode)')+'</span>'+
       '<button class="danger" onclick="removeToken(this)">Remove token</button></div>';
  }else{
    h+='<details><summary>What is this and how do I get a token?</summary><div class="muted" style="font-size:13px">'+
       'A Linode <b>Personal Access Token</b> lets the appliance create volumes, clone disks and launch instances. Stored <b>encrypted at rest</b>. '+
       'Create one at <a href="https://cloud.linode.com/profile/tokens" target="_blank" rel="noopener">cloud.linode.com/profile/tokens</a> with scopes <b>Linodes: Read/Write</b> and <b>Volumes: Read/Write</b>.'+
       '</div></details>'+
       '<div style="display:flex;gap:8px;margin-top:10px"><input id="ltok" type="password" placeholder="Linode API token"><button onclick="saveToken(this)">Save</button></div>';
  }
  $('settings').innerHTML=h;
}
async function saveToken(btn){busy(btn,true);try{await api('POST','/api/v1/settings/linode-token',{token:$('ltok').value});loadSettings()}catch(e){alert('Error: '+e.message)}finally{busy(btn,false)}}
async function removeToken(btn){if(!confirm('Remove the stored Linode API token? Provisioning/finalize will stop working until you add a new one.'))return;busy(btn,true);try{await api('DELETE','/api/v1/settings/linode-token');loadSettings()}catch(e){alert('Error: '+e.message)}finally{busy(btn,false)}}

let diskSeq=0;
function addDisk(dev,gb){
  diskSeq++;const id=diskSeq;
  const row=document.createElement('div');row.className='row';row.id='disk'+id;row.style.marginBottom='8px';
  row.innerHTML='<div><input class="d_dev" placeholder="/dev/sda'+(diskSeq===1?' (boot)':'')+'" value="'+(dev||'')+'"></div>'+
    '<div style="display:flex;gap:8px"><input class="d_size" type="number" placeholder="Size (GB)" value="'+(gb||'')+'" style="flex:1">'+
    '<button class="danger" title="Remove disk" onclick="this.closest(\'.row\').remove()">✕</button></div>';
  $('disks').appendChild(row);
}
async function createMig(btn){
  $('createErr').textContent='';
  const rows=document.querySelectorAll('#disks .row');const devices=[];
  for(const r of rows){const dev=r.querySelector('.d_dev').value.trim();const gb=parseInt(r.querySelector('.d_size').value,10);
    if(!dev)continue; if(!gb||gb<=0){$('createErr').textContent='Each disk needs a positive size (GB): '+dev;return}
    devices.push({device:dev,size_bytes:gb*1073741824});}
  if(!devices.length){$('createErr').textContent='Add at least one disk';return}
  busy(btn,true);
  // Show a loading placeholder in the migrations list while provisioning runs.
  $('migs').insertAdjacentHTML('afterbegin','<div id="creating" class="mig"><div class="center"><div class="spinner"></div><div>Creating migration & provisioning volume(s)…</div></div></div>');
  try{
    await api('POST','/api/v1/migrations',{name:$('m_name').value,source_hostname:$('m_host').value,devices:devices});
    $('m_name').value=$('m_host').value='';$('disks').innerHTML='';diskSeq=0;addDisk();
    await refresh(true);
  }catch(e){$('createErr').textContent='Error: '+e.message;const c=$('creating');if(c)c.remove();}
  finally{busy(btn,false)}
}

function stateClass(s){return ({created:'warn',awaiting_agent:'warn',replicating:'warn',ready:'ok',migrating:'warn',image_ready:'ok',launched:'ok',failed:'bad'})[s]||'muted'}
function disks(m){return m.disks||[]}
function allDone(m){const d=disks(m);return d.length>0&&d.every(x=>x.full_sync_done)}
function bytesTotal(m){return disks(m).reduce((a,d)=>a+(d.bytes_on_wire||0),0)}
function anyDiskError(m){return disks(m).map(d=>d.last_error).filter(Boolean)[0]||''}

async function startMig(id,btn){
  if(!confirm('Cut over migration #'+id+'?\n\nThis stops replication, converts the boot disk, and clones every disk into launchable image volumes.'))return;
  const launch=confirm('Also launch a new Linode instance now?\nOK = launch with all disks attached · Cancel = just create the image volumes');
  busy(btn,true);
  try{await api('POST','/api/v1/migrations/'+id+'/start',{launch_instance:launch});await refresh(true)}
  catch(e){alert('Cannot cut over: '+e.message)}finally{busy(btn,false)}
}
async function assessMig(id,btn){
  busy(btn,true);
  try{const v=await api('POST','/api/v1/migrations/'+id+'/assess');
    if(!v.assessed){const fails=(v.validations||[]).filter(c=>!c.ok).map(c=>'✘ '+c.name+' — '+c.detail).join('\n');alert('Assessment failed:\n\n'+fails);}
    await refresh(true);
  }catch(e){alert('Assessment error: '+e.message)}finally{busy(btn,false)}
}
async function stopMig(id,btn){
  if(!confirm('Stop cutover #'+id+'? The finalize run is cancelled and replication resumes; you will re-run the assessment.'))return;
  busy(btn,true);try{await api('POST','/api/v1/migrations/'+id+'/stop');await refresh(true)}catch(e){alert('Cannot stop: '+e.message)}finally{busy(btn,false)}
}
async function deleteMig(id,name,btn){
  if(!confirm('Delete migration #'+id+' ('+name+')?\n\nWARNING: deletes the replication volume(s) and ALL replicated data. Completed image volumes are kept. Remove the agent on the source separately (uninstall command shown on completed migrations).\n\nThis cannot be undone.'))return;
  busy(btn,true);try{await api('DELETE','/api/v1/migrations/'+id);await refresh(true)}catch(e){alert('Cannot delete: '+e.message)}finally{busy(btn,false)}
}
// "Check status" re-fetches and shows the latest agent connection result in a box.
async function checkStatus(id,btn){
  busy(btn,true);
  try{
    const list=await api('GET','/api/v1/migrations');const v=(list||[]).find(x=>x.migration.id===id);
    const box=$('status'+id);
    if(v){const m=v.migration;const err=anyDiskError(m);
      if(allDone(m)){box.className='resultbox ok';box.textContent='✔ All disks baselined and replicating. Last activity '+(disks(m).map(d=>d.last_sync_at).filter(Boolean).sort().pop()||'recently');}
      else if(err){box.className='resultbox bad';box.textContent='✘ Last replication attempt failed: '+err;}
      else{box.className='resultbox';box.textContent='No completed sync yet. The agent retries every 60s — run the force-retry command on the source if needed.';}
    }
    await refresh(true);
  }catch(e){const box=$('status'+id);if(box){box.className='resultbox bad';box.textContent='Error: '+e.message}}
  finally{busy(btn,false)}
}
async function toggleLog(id,btn){
  const box=$('log'+id);
  if(!box.classList.contains('hide')){box.classList.add('hide');btn.textContent='Show activity log';return}
  btn.textContent='Hide activity log';box.classList.remove('hide');box.innerHTML='<div class="muted">loading…</div>';
  try{const ev=await api('GET','/api/v1/migrations/'+id+'/events');
    box.innerHTML=ev.length?ev.map(e=>'<div class="logrow '+esc(e.level)+'"><span class="t">'+fmtTime(e.at)+'</span><span class="m">'+esc(e.message)+'</span></div>').join(''):'<div class="muted">No activity yet.</div>';
  }catch(e){box.innerHTML='<div class="err">'+esc(e.message)+'</div>'}
}

function progressLine(v,m){
  let line='<span class="muted">'+esc(v.phase||'')+'</span>';let width=0,indet=false;
  if(v.percent_done>=0){width=Math.max(2,Math.round(v.percent_done));line+=' · '+v.percent_done.toFixed(1)+'%';}
  if(v.eta_seconds>=0){line+=' · ~'+fmtDur(v.eta_seconds)+' left';}
  else if(m.state==='migrating'){line+=' · running '+fmtDur(v.elapsed_seconds);indet=true;}
  if(['image_ready','launched'].includes(m.state)){width=100;line+=' in '+fmtDur(v.elapsed_seconds);}
  return line+'<div class="prog'+(indet?' indet':'')+'"><div style="width:'+(indet?35:width)+'%"></div></div>'+
    '<div class="muted" style="font-size:12px;margin-top:3px">'+fmtBytes(bytesTotal(m))+' received</div>';
}
function diskTable(m){const d=disks(m);if(!d.length)return '';
  let h='<table><tr><th>Disk</th><th>Device</th><th>Size</th><th>Port</th><th>Baseline</th><th>Volume / note</th></tr>';
  for(const x of d){const note=x.last_error?('<span class="x">'+esc(x.last_error)+'</span>'):(x.artifact_id?esc(x.artifact_id):(x.volume_id?('vol '+x.volume_id):'file'));
    h+='<tr><td>'+(x.index===0?'boot':('data '+x.index))+'</td><td class="muted">'+esc(x.source_device)+'</td>'+
       '<td class="muted">'+fmtBytes(x.size_bytes)+'</td><td class="muted">'+x.receiver_port+'</td>'+
       '<td>'+(x.full_sync_done?'<span class="y">✔ done</span>':'<span class="muted">baselining</span>')+'</td><td>'+note+'</td></tr>';}
  return h+'</table>';
}
function migCard(v){
  const m=v.migration;const err=anyDiskError(m);
  let h='<table style="margin-bottom:4px"><tr><th>#'+m.id+' '+esc(m.name)+'</th><th>State</th><th>Source</th><th>Progress</th><th>RPO</th></tr><tr>'+
    '<td><span class="pill '+stateClass(m.state)+'">'+esc(m.state)+'</span></td>'+
    '<td class="muted">'+disks(m).length+' disk(s)<br>'+(allDone(m)?'baseline done':'baselining')+'</td>'+
    '<td class="muted">'+esc(m.source_hostname||'-')+'</td><td>'+progressLine(v,m)+'</td>'+
    '<td class="muted">'+(v.rpo_seconds?Math.round(v.rpo_seconds)+'s':'—')+'</td></tr></table>';
  if(m.last_error)h+='<div class="resultbox bad">'+esc(m.last_error)+'</div>';
  else if(err)h+='<div class="resultbox bad">Last replication attempt failed: '+esc(err)+'</div>';

  if(['image_ready','launched'].includes(m.state)){
    const arts=disks(m).map(d=>esc(d.artifact_id||('vmrep-'+m.name+'-img'))).join(', ');
    h+='<div class="banner">✔ <b>Migration completed.</b> '+disks(m).length+' image volume(s) in your Linode account ('+
       '<a href="https://cloud.linode.com/volumes" target="_blank" rel="noopener">cloud.linode.com/volumes</a>): <code style="display:inline;padding:1px 5px">'+arts+'</code>. '+
       (m.launched_linode_id?('Launched Linode '+esc(m.launched_linode_id)+' — see <a href="https://cloud.linode.com/linodes" target="_blank" rel="noopener">your Linodes</a>.')
       :'To launch: create a Linode (same region), attach these volumes (boot=sda, data=sdb…) and boot from GRUB 2.')+'</div>';
  }

  let checks='';for(const c of (v.validations||[]))checks+='<div style="font-size:13px;margin:2px 0"><span class="'+(c.ok?'y">✔':'x">✘')+'</span> '+esc(c.name)+' <span class="muted">— '+esc(c.detail)+'</span></div>';
  const allOk=(v.validations||[]).every(c=>c.ok);
  h+='<details'+(allOk?'':' open')+'><summary>Validation checks'+(allOk?' (all passing)':'')+'</summary><div>'+checks+'</div></details>';
  h+='<details><summary>Disks ('+disks(m).length+')</summary><div>'+diskTable(m)+'</div></details>';
  h+='<details><summary>Activity log</summary><div><button onclick="toggleLog('+m.id+',this)">Show activity log</button><div id="log'+m.id+'" class="log hide" style="margin-top:8px"></div></div></details>';

  if(v.enroll_cmd && !allDone(m) && m.state!=='migrating'){
    h+='<details open><summary>Enroll the source server (all '+disks(m).length+' disk(s))</summary><div>'+
       '<label>Run this on '+esc(m.source_hostname||'the source')+'</label>'+
       '<div style="display:flex;gap:8px;align-items:flex-start"><pre id="enroll'+m.id+'" style="flex:1;margin:0">'+esc(v.enroll_cmd)+'</pre>'+
       '<button onclick="copyText(document.getElementById(\'enroll'+m.id+'\').textContent,this)">Copy</button></div>'+
       '<div class="muted" style="font-size:12px;margin-top:8px">If a disk’s sync fails, no reinstall is needed — the agent retries every 60s. '+
       'Force a retry on the source with <code style="display:inline;padding:1px 5px">sudo systemctl start vmrepl-agent.service</code>, then click <b>Check status</b> below.</div>'+
       '<div class="actions"><button onclick="checkStatus('+m.id+',this)">Check status</button></div>'+
       '<div id="status'+m.id+'" class="resultbox hide"></div></div></details>';
  }
  if(v.uninstall_cmd && ['image_ready','launched'].includes(m.state)){
    h+='<details><summary>Remove the agent from the source</summary><div style="display:flex;gap:8px;align-items:flex-start">'+
       '<pre id="unin'+m.id+'" style="flex:1;margin:0">'+esc(v.uninstall_cmd)+'</pre>'+
       '<button onclick="copyText(document.getElementById(\'unin'+m.id+'\').textContent,this)">Copy</button></div></details>';
  }

  h+='<div class="actions">';
  if(!['migrating','image_ready','launched'].includes(m.state)){
    h+='<button onclick="assessMig('+m.id+',this)">Pre-migration assessment</button>';
    if(v.assessed)h+='<span class="pill ok">✔ assessment passed</span>';
    if(v.can_migrate)h+='<button class="primary"'+(v.assessed?'':' disabled title="Run the assessment first"')+' onclick="startMig('+m.id+',this)">Cutover instance</button>';
  }
  if(m.state==='migrating')h+='<button class="danger" onclick="stopMig('+m.id+',this)">Stop</button>';
  h+='<span style="flex:1"></span><button class="danger" onclick="deleteMig('+m.id+',\''+esc(m.name)+'\',this)">Delete</button></div>';
  const card=document.createElement('div');card.className='mig';card.id='mig'+m.id;card.innerHTML=h;return card;
}

// Preserve which <details> were open across refreshes so the UI doesn't collapse.
function openKeys(){const s=new Set();document.querySelectorAll('#migs details[open]').forEach((d,i)=>{const card=d.closest('.mig');if(card)s.add(card.id+':'+d.querySelector('summary').textContent)});return s}

async function refresh(animate){
  try{
    const open=openKeys();
    const list=await api('GET','/api/v1/migrations');
    const c=$('creating');if(c)c.remove();
    const migs=$('migs');
    migs.innerHTML='';
    if(!list.length){migs.innerHTML='<div class="muted" style="padding:8px">No migrations yet. Create one above.</div>';}
    else list.forEach(v=>{const card=migCard(v);migs.appendChild(card);
      card.querySelectorAll('details').forEach(d=>{if(open.has(card.id+':'+d.querySelector('summary').textContent))d.open=true});});
    if(animate)flash(migs);
    $('updated').textContent='updated '+new Date().toLocaleTimeString();
    loadSettings();
  }catch(e){/* 401 handled in api() */}
}

async function start(){show('app');if(!document.querySelector('#disks .row'))addDisk();refresh(false);}
async function init(){
  try{await api('GET','/api/v1/session');start();setInterval(()=>{if(!$('app').classList.contains('hide'))refresh(false)},5000);}
  catch(e){show('login')}
}
init();
</script>
</body></html>`
