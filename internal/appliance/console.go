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
<title>vm-replication · migration console</title>
<style>
 :root{--bg:#0a0c10;--surface:#111318;--surface2:#171a21;--border:#2a2d36;--accent:#00e5ff;--green:#22c55e;--yellow:#eab308;--red:#ef4444;--text:#e2e8f0;--muted:#64748b}
 *{margin:0;padding:0;box-sizing:border-box}
 body{background:var(--bg);color:var(--text);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;padding:28px;line-height:1.5}
 h1{font-size:20px}h1 span{color:var(--accent)} h2{font-size:14px;margin:0 0 10px;color:var(--muted);text-transform:uppercase;letter-spacing:.08em}
 .sub{color:var(--muted);font-size:12px;margin:6px 0 22px}
 .card{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:18px;margin-bottom:16px}
 input,button,select{font:inherit;font-size:13px;background:var(--surface2);color:var(--text);border:1px solid var(--border);border-radius:6px;padding:8px 10px}
 input{width:100%}
 button{cursor:pointer} button:hover{border-color:var(--accent)} button.primary{background:var(--accent);color:#001014;border-color:var(--accent);font-weight:700}
 label{display:block;font-size:11px;color:var(--muted);margin:8px 0 4px;text-transform:uppercase;letter-spacing:.05em}
 .row{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:10px}
 table{width:100%;border-collapse:collapse;font-size:12px} th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--border);vertical-align:top}
 th{color:var(--muted);font-size:10px;text-transform:uppercase}
 .pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:11px;border:1px solid}
 .ok{color:var(--green);border-color:rgba(34,197,94,.4)} .warn{color:var(--yellow);border-color:rgba(234,179,8,.4)} .bad{color:var(--red);border-color:rgba(239,68,68,.4)} .muted{color:var(--muted)}
 code,pre{background:#0e1116;border:1px solid var(--border);border-radius:6px;padding:8px;display:block;white-space:pre-wrap;word-break:break-all;font-size:12px;color:var(--accent)}
 .check{font-size:12px;margin:2px 0} .check .x{color:var(--red)} .check .y{color:var(--green)}
 .bar{display:flex;gap:10px;align-items:center;margin-bottom:18px;flex-wrap:wrap}
 .hide{display:none}
 .err{color:var(--red);font-size:12px;margin-top:8px}
 .prog{height:6px;background:#0e1116;border-radius:4px;overflow:hidden;margin-top:4px} .prog>div{height:100%;background:var(--accent)}
 details{margin:6px 0} details>summary{cursor:pointer;color:var(--accent);font-size:12px;user-select:none;list-style:none}
 details>summary::before{content:'▸ '} details[open]>summary::before{content:'▾ '}
 details>div{margin-top:8px}
 button.danger{color:var(--red);border-color:rgba(239,68,68,.4)} button.danger:hover{border-color:var(--red)}
 .banner{border:1px solid rgba(34,197,94,.4);border-radius:8px;padding:10px 14px;margin:8px 0;color:var(--green);font-size:13px}
 .banner a{color:var(--accent)}
 .actions{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-top:10px}
</style></head>
<body>
 <h1>vm-<span>replication</span> · migration console</h1>
 <div class="sub">Migrate Linux servers to Akamai Cloud (Linode), block by block.</div>

 <!-- LOGIN -->
 <div id="login" class="card hide" style="max-width:380px">
   <h2>Sign in</h2>
   <label>Console password</label>
   <input id="pw" type="password" placeholder="generated at install" onkeydown="if(event.key==='Enter')login()">
   <div style="margin-top:12px"><button class="primary" onclick="login()">Sign in</button></div>
   <div id="loginErr" class="err"></div>
 </div>

 <!-- APP -->
 <div id="app" class="hide">
   <div class="bar">
     <button onclick="refresh()">Refresh</button>
     <span class="muted" id="updated"></span>
     <span style="flex:1"></span>
     <button onclick="logout()">Sign out</button>
   </div>

   <div id="settings" class="card"></div>

   <div class="card">
     <h2>New migration</h2>
     <details>
       <summary>How do I find the source details?</summary>
       <div>
         <div class="muted" style="font-size:12px;margin-bottom:8px">
           Run this on your <b>source server</b> — it prints all four values:
         </div>
         <div style="display:flex;gap:8px;align-items:flex-start;margin-bottom:6px">
           <pre id="srcCmd" style="flex:1;margin:0">echo "Hostname : $(hostname)"; lsblk -b -d -n -o NAME,SIZE,TYPE | awk '$3=="disk"{printf "Device   : /dev/%s\nSize(GB) : %d\n", $1, ($2+1073741823)/1073741824}'</pre>
           <button onclick="copyText(document.getElementById('srcCmd').textContent,this)">Copy</button>
         </div>
         <div class="muted" style="font-size:11px;margin-bottom:6px">
           Enter the <b>whole disk</b> (e.g. <code style="display:inline;padding:1px 4px">/dev/sda</code>), not a partition — pick the
           disk whose partitions include the root filesystem (<code style="display:inline;padding:1px 4px">/</code>). Always round the size <b>up</b>.
         </div>
       </div>
     </details>
     <div class="row">
       <div><label>Name</label><input id="m_name" placeholder="web01"></div>
       <div><label>Source hostname</label><input id="m_host" placeholder="web01.prod"></div>
       <div><label>Source device</label><input id="m_dev" placeholder="/dev/sda"></div>
       <div><label>Disk size (GB)</label><input id="m_size" type="number" placeholder="80"></div>
     </div>
     <div style="margin-top:12px"><button class="primary" onclick="createMig()">Create migration</button></div>
     <div id="createErr" class="err"></div>
   </div>

   <div class="card">
     <h2>Migrations</h2>
     <div id="migs"></div>
   </div>
 </div>

<script>
const $=id=>document.getElementById(id);
function esc(s){return String(s==null?'':s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function copyText(t,btn){
  const done=()=>{if(btn){const o=btn.textContent;btn.textContent='Copied!';setTimeout(()=>{btn.textContent=o},1200)}};
  if(navigator.clipboard&&window.isSecureContext){navigator.clipboard.writeText(t).then(done).catch(()=>legacyCopy(t,done))}
  else legacyCopy(t,done);
}
function legacyCopy(t,done){const ta=document.createElement('textarea');ta.value=t;ta.style.position='fixed';ta.style.opacity='0';document.body.appendChild(ta);ta.focus();ta.select();try{document.execCommand('copy');done&&done()}catch(e){}document.body.removeChild(ta)}
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

async function login(){
  $('loginErr').textContent='';
  try{await api('POST','/login',{password:$('pw').value});$('pw').value='';start()}
  catch(e){$('loginErr').textContent='Login failed: '+e.message}
}
async function logout(){try{await api('POST','/logout')}catch(e){}show('login')}

async function loadSettings(){
  const st=await api('GET','/api/v1/settings');
  let h='<h2>Linode automation</h2>';
  if(st.linode_token_set){
    h+='<div style="display:flex;gap:10px;align-items:center;flex-wrap:wrap">'+
       '<span class="check"><span class="y">✔</span> Linode API token stored. '+
       (st.linode_automation?('Appliance Linode id '+esc(st.appliance_linode_id)+', volumes are created in the appliance’s own region.'):'(appliance Linode id unknown — file-fallback mode)')+'</span>'+
       '<button class="danger" onclick="removeToken()">Remove token</button></div>';
  }else{
    h+='<details><summary>What is this and how do I get a token?</summary><div>'+
       '<div class="muted" style="font-size:12px;margin-bottom:8px">'+
       'A Linode <b>Personal Access Token</b> lets this appliance provision Block Storage volumes, '+
       'clone the migrated disk, and launch new instances on your behalf. Without it the tool runs in '+
       'file-fallback mode (no Linode provisioning). The token is stored <b>encrypted at rest</b> on this server '+
       'and is only sent to api.linode.com.'+
       '</div>'+
       '<div class="muted" style="font-size:12px;margin-bottom:8px">'+
       '<b>How to get one:</b> open <a href="https://cloud.linode.com/profile/tokens" target="_blank" rel="noopener" style="color:var(--accent)">cloud.linode.com/profile/tokens</a> '+
       '&rarr; <i>Create a Personal Access Token</i>. Set all scopes to <b>None</b> except '+
       '<b>Linodes: Read/Write</b> and <b>Volumes: Read/Write</b>, then create and copy the token (shown once).'+
       '</div></div></details>'+
       '<div style="display:flex;gap:8px;margin-top:8px"><input id="ltok" type="password" placeholder="Linode API token"><button onclick="saveToken()">Save</button></div>';
  }
  $('settings').innerHTML=h;
}
async function saveToken(){try{await api('POST','/api/v1/settings/linode-token',{token:$('ltok').value});loadSettings()}catch(e){alert('Error: '+e.message)}}
async function removeToken(){
  if(!confirm('Remove the stored Linode API token?\n\nVolume provisioning and finalize will stop working until you save a new token. Existing migrations and volumes are not affected.'))return;
  try{await api('DELETE','/api/v1/settings/linode-token');loadSettings()}catch(e){alert('Error: '+e.message)}
}

async function createMig(){
  $('createErr').textContent='';
  const gb=parseInt($('m_size').value,10);
  if(!gb||gb<=0){$('createErr').textContent='Enter a valid disk size in GB';return}
  try{
    await api('POST','/api/v1/migrations',{name:$('m_name').value,source_hostname:$('m_host').value,source_device:$('m_dev').value,source_disk_size:gb*1073741824});
    $('m_name').value=$('m_host').value=$('m_dev').value=$('m_size').value='';
    refresh();
  }catch(e){$('createErr').textContent='Error: '+e.message}
}

function stateClass(s){return ({created:'warn',awaiting_agent:'warn',replicating:'warn',ready:'ok',migrating:'warn',image_ready:'ok',launched:'ok',failed:'bad'})[s]||'muted'}
function fmtBytes(n){if(!n)return '0 B';const u=['B','KiB','MiB','GiB','TiB'];let i=0;while(n>=1024&&i<u.length-1){n/=1024;i++}return n.toFixed(1)+' '+u[i]}

async function startMig(id){
  if(!confirm('Start migration #'+id+'? This stops replication, converts the disk, and creates the launchable image.'))return;
  const launch=confirm('Also launch a new Linode instance from the migrated image now? (OK = launch, Cancel = just create the image)');
  try{await api('POST','/api/v1/migrations/'+id+'/start',{launch_instance:launch});refresh()}
  catch(e){alert('Cannot start: '+e.message)}
}

async function assessMig(id){
  try{
    const v=await api('POST','/api/v1/migrations/'+id+'/assess');
    if(v.assessed){refresh()}
    else{
      const fails=(v.validations||[]).filter(c=>!c.ok).map(c=>'✘ '+c.name+' — '+c.detail).join('\n');
      alert('Assessment failed:\n\n'+fails);
      refresh();
    }
  }catch(e){alert('Assessment error: '+e.message)}
}

async function stopMig(id){
  if(!confirm('Stop migration #'+id+'?\n\nThe finalize run is cancelled and replication resumes. You will need to re-run the assessment before starting again.'))return;
  try{await api('POST','/api/v1/migrations/'+id+'/stop');refresh()}
  catch(e){alert('Cannot stop: '+e.message)}
}

async function deleteMig(id,name){
  if(!confirm('Delete migration #'+id+' ('+name+')?\n\nWARNING: this stops its receiver and deletes the replication volume with ALL replicated data for this migration. A completed artifact volume (vmrepl-img-'+id+') is kept. The agent on the source keeps running until you remove it there (systemctl disable --now vmrepl-agent.timer).\n\nThis cannot be undone.'))return;
  try{await api('DELETE','/api/v1/migrations/'+id);refresh()}
  catch(e){alert('Cannot delete: '+e.message)}
}

function fmtDur(s){if(s==null||s<0)return '—';s=Math.round(s);if(s<60)return s+'s';if(s<3600)return Math.floor(s/60)+'m '+(s%60)+'s';return Math.floor(s/3600)+'h '+Math.floor((s%3600)/60)+'m'}

function progressLine(v,m){
  // Live phase + percent + ETA; the page polls every 5s so this self-refreshes.
  let line='<span class="muted">'+esc(v.phase||'')+'</span>';
  let width=0;
  if(v.percent_done>=0){width=Math.max(2,Math.round(v.percent_done));line+=' · '+v.percent_done.toFixed(1)+'%';}
  if(v.eta_seconds>=0){line+=' · est. '+fmtDur(v.eta_seconds)+' remaining';}
  else if(m.state==='migrating'){line+=' · running '+fmtDur(v.elapsed_seconds);width=2;}
  if(['image_ready','launched'].includes(m.state)){width=100;line+=' in '+fmtDur(v.elapsed_seconds);}
  return line+'<div class="prog"><div style="width:'+width+'%"></div></div><span class="muted">'+fmtBytes(m.bytes_on_wire)+' received</span>';
}

function migCard(v){
  const m=v.migration;
  let h='<table style="margin-bottom:6px"><tr>'+
    '<th>#'+m.id+' '+esc(m.name)+'</th><th>State</th><th>Source</th><th>Progress</th><th>RPO</th></tr><tr>'+
    '<td><span class="pill '+stateClass(m.state)+'">'+esc(m.state)+'</span>'+(m.last_error?'<div class="err">'+esc(m.last_error)+'</div>':'')+'</td>'+
    '<td class="muted">'+(m.full_sync_done?'baseline done':'baselining')+'</td>'+
    '<td class="muted">'+esc(m.source_hostname||'-')+'<br>'+esc(m.source_device)+'</td>'+
    '<td>'+progressLine(v,m)+'</td>'+
    '<td class="muted">'+(v.rpo_seconds?Math.round(v.rpo_seconds)+'s':'—')+'</td>'+
    '</tr></table>';

  // Completed banner with where to find the artifact in the Linode account.
  if(['image_ready','launched'].includes(m.state)){
    h+='<div class="banner">✔ <b>Migration completed.</b> The migrated disk image is the volume '+
       '<code style="display:inline;padding:1px 4px">vmrepl-img-'+m.id+'</code> ('+esc(m.image_id||'')+') in your Linode account — see '+
       '<a href="https://cloud.linode.com/volumes" target="_blank" rel="noopener">cloud.linode.com/volumes</a>. '+
       (m.launched_linode_id?('A new instance (Linode '+esc(m.launched_linode_id)+') was launched from it — see <a href="https://cloud.linode.com/linodes" target="_blank" rel="noopener">your Linodes</a>.')
       :'Attach it to a new Linode and boot from it (GRUB 2) to launch the migrated server.')+'</div>';
    if(v.uninstall_cmd){
      h+='<details><summary>Remove the agent from the source server</summary><div>'+
         '<div class="muted" style="font-size:12px;margin-bottom:6px">Replication is done — run this on '+esc(m.source_hostname||'the source')+' to remove the agent, its timer, certificates, and checkpoint in one go:</div>'+
         '<div style="display:flex;gap:8px;align-items:flex-start"><pre id="unin'+m.id+'" style="flex:1;margin:0">'+esc(v.uninstall_cmd)+'</pre>'+
         '<button onclick="copyText(document.getElementById(\'unin'+m.id+'\').textContent,this)">Copy</button></div></div></details>';
    }
  }

  // Validations + enrollment, collapsed once the baseline is replicating fine.
  let checks='';
  for(const c of (v.validations||[])){checks+='<div class="check"><span class="'+(c.ok?'y">✔':'x">✘')+'</span> '+esc(c.name)+' <span class="muted">— '+esc(c.detail)+'</span></div>'}
  const allOk=(v.validations||[]).every(c=>c.ok);
  h+='<details'+(allOk?'':' open')+'><summary>Validation checks'+(allOk?' (all passing)':'')+'</summary><div>'+checks+'</div></details>';
  if(v.enroll_cmd && !m.full_sync_done && m.state!=='migrating'){
    h+='<details open><summary>Enroll the source server</summary><div>'+
       '<label>Run this on '+esc(m.source_hostname||m.source_device)+'</label>'+
       '<div style="display:flex;gap:8px;align-items:flex-start"><pre id="enroll'+m.id+'" style="flex:1;margin:0">'+esc(v.enroll_cmd)+'</pre>'+
       '<button onclick="copyText(document.getElementById(\'enroll'+m.id+'\').textContent,this)">Copy</button></div>'+
       '<div class="muted" style="font-size:11px;margin-top:6px">Already enrolled but the first sync failed? No reinstall needed — the agent retries every 60s. '+
       'Fix the cause (most often: open TCP '+esc(m.receiver_port)+' on this server’s firewall, including any Linode Cloud Firewall), '+
       'or force a retry on the source: <code style="display:inline;padding:1px 4px">sudo systemctl start vmrepl-agent.service</code></div>'+
       '</div></details>';
  }

  // Actions: assess -> start; stop while running; delete always.
  h+='<div class="actions">';
  if(!['migrating','image_ready','launched'].includes(m.state)){
    h+='<button onclick="assessMig('+m.id+')">Pre-migration assessment</button>';
    if(v.assessed){h+='<span class="pill ok">✔ assessment successful</span>';}
    if(v.can_migrate){
      h+='<button class="primary"'+(v.assessed?'':' disabled title="Run the pre-migration assessment first"')+' onclick="startMig('+m.id+')">Start migration</button>';
    }
  }
  if(m.state==='migrating'){h+='<button class="danger" onclick="stopMig('+m.id+')">Stop</button>';}
  h+='<span style="flex:1"></span><button class="danger" onclick="deleteMig('+m.id+',\''+esc(m.name)+'\')">Delete</button>';
  h+='</div>';
  return '<div class="card" style="background:var(--surface2)">'+h+'</div>';
}

async function refresh(){
  try{
    const list=await api('GET','/api/v1/migrations');
    $('migs').innerHTML=list.length?list.map(migCard).join(''):'<div class="muted">No migrations yet. Create one above.</div>';
    $('updated').textContent='updated '+new Date().toLocaleTimeString();
    loadSettings();
  }catch(e){/* 401 handled in api() */}
}

async function start(){show('app');refresh();}
async function init(){
  try{await api('GET','/api/v1/session');start();setInterval(()=>{if(!$('app').classList.contains('hide'))refresh()},5000);}
  catch(e){show('login')}
}
init();
</script>
</body></html>`
