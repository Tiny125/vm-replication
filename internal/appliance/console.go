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
 .mighead{display:flex;align-items:center;gap:8px;margin-bottom:10px}
 .chev{padding:3px 9px;font-size:13px;line-height:1;min-width:32px}
 .mig.collapsed .migbody{display:none}
 .migdivider{border:none;border-top:1px solid var(--border);margin:14px 0}
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
 button.done{background:var(--green);border-color:var(--green)}
 button.done:hover{background:#178343}
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
 .logpre{margin:0;border:1px solid var(--border);border-radius:10px;background:var(--surface2);
   padding:8px 12px;color:var(--text);white-space:pre-wrap;overflow-wrap:break-word;word-break:normal;
   font-size:12.5px;line-height:1.55;font-family:"SF Mono",ui-monospace,Menlo,Consolas,monospace}
 .logpre.scroll{max-height:62vh;overflow:auto}
 .logpre .x{color:var(--red)} .logpre .w{color:var(--amber)}
 .mini{padding:3px 9px;font-size:12px;line-height:1.2}
 .info{display:inline-flex;align-items:center;justify-content:center;width:16px;height:16px;border-radius:50%;
   background:var(--surface2);border:1px solid var(--border);color:var(--muted);font-size:10px;font-weight:700;
   font-style:normal;cursor:help;position:relative;margin-left:6px;vertical-align:middle;flex:none}
 .info:hover{background:var(--accent);color:#fff;border-color:var(--accent)}
 .info:hover::after{content:attr(data-tip);position:absolute;left:50%;bottom:150%;transform:translateX(-50%);
   background:#1d1d1f;color:#fff;padding:9px 11px;border-radius:9px;font-size:12px;font-weight:400;white-space:pre-line;
   width:max-content;max-width:280px;line-height:1.45;text-align:left;z-index:30;box-shadow:0 8px 24px rgba(0,0,0,.22)}
 .info:hover::before{content:"";position:absolute;left:50%;bottom:150%;transform:translateX(-50%) translateY(100%);
   border:5px solid transparent;border-top-color:#1d1d1f;z-index:30}
 .leg{position:relative;display:inline-flex;align-items:center;justify-content:center;width:16px;height:16px;
   border-radius:50%;background:var(--surface2);border:1px solid var(--border);color:var(--muted);font-size:10px;
   font-weight:700;font-style:normal;cursor:help;margin-left:6px;vertical-align:middle;flex:none}
 .leg:hover{background:var(--accent);color:#fff;border-color:var(--accent)}
 .legbox{display:none;position:absolute;top:150%;left:50%;transform:translateX(-50%);z-index:40;
   background:var(--surface);border:1px solid var(--border);border-radius:12px;box-shadow:0 12px 36px rgba(0,0,0,.2);
   padding:12px 14px;width:300px;text-transform:none;letter-spacing:normal;font-weight:400}
 .leg:hover .legbox{display:block}
 .legrow{display:flex;align-items:flex-start;gap:8px;margin:6px 0;font-size:12.5px;color:var(--text);white-space:normal;line-height:1.35}
 .legrow .pill{flex:none}
 .legrow .desc{color:var(--muted)}
 .modal.wide{max-width:760px}
 .flash{animation:flash .8s ease}
 @keyframes flash{0%{background:rgba(0,113,227,.10)}100%{background:transparent}}
 .center{display:flex;flex-direction:column;align-items:center;gap:14px;padding:36px 0;color:var(--muted)}
 .spinner{width:26px;height:26px;border:3px solid var(--surface2);border-top-color:var(--accent);border-radius:50%;animation:spin .8s linear infinite}
 a{color:var(--accent);text-decoration:none} a:hover{text-decoration:underline}
 .login-card{max-width:380px;margin:8vh auto 0}
 .modal-overlay{position:fixed;inset:0;background:rgba(20,20,22,.32);backdrop-filter:saturate(120%) blur(2px);
   display:flex;align-items:center;justify-content:center;z-index:100;padding:20px;animation:fadein .15s ease}
 .modal-overlay.closing{animation:fadeout .15s ease forwards}
 @keyframes fadein{from{opacity:0}to{opacity:1}}
 @keyframes fadeout{to{opacity:0}}
 .modal{background:var(--surface);border:1px solid var(--border);border-radius:16px;box-shadow:0 24px 70px rgba(0,0,0,.28);
   max-width:460px;width:100%;padding:24px 24px 20px;animation:pop .18s cubic-bezier(.2,.8,.3,1)}
 @keyframes pop{from{transform:scale(.94);opacity:.5}to{transform:scale(1);opacity:1}}
 .modal h3{font-size:17px;font-weight:600;letter-spacing:-.01em;margin:0 0 10px}
 .modal-body{font-size:14px;color:var(--text);line-height:1.55}
 .modal-body b{font-weight:600}
 .modal-body .warn{color:var(--red);font-weight:500}
 .modal-check{display:flex;align-items:flex-start;gap:9px;margin-top:16px;font-size:13.5px;color:var(--text);
   cursor:pointer;background:var(--surface2);border:1px solid var(--border);border-radius:10px;padding:11px 13px}
 .modal-check input{width:auto;margin-top:2px;cursor:pointer;accent-color:var(--accent)}
 .modal-actions{display:flex;justify-content:flex-end;gap:10px;margin-top:22px}
 .toast-wrap{position:fixed;top:18px;right:18px;z-index:200;display:flex;flex-direction:column;gap:10px;max-width:360px}
 .toast{display:flex;align-items:flex-start;gap:9px;background:var(--surface);border:1px solid var(--border);
   border-left:4px solid var(--muted);border-radius:12px;box-shadow:0 12px 36px rgba(0,0,0,.16);
   padding:12px 14px;font-size:13.5px;color:var(--text);animation:toastin .22s cubic-bezier(.2,.8,.3,1)}
 .toast.ok{border-left-color:var(--green)} .toast.ok .ic{color:var(--green)}
 .toast.bad{border-left-color:var(--red)} .toast.bad .ic{color:var(--red)}
 .toast .ic{font-weight:700;line-height:1.4}
 .toast.closing{animation:toastout .2s ease forwards}
 @keyframes toastin{from{transform:translateX(20px);opacity:0}to{transform:translateX(0);opacity:1}}
 @keyframes toastout{to{transform:translateX(20px);opacity:0}}
</style></head>
<body>
<div id="toasts" class="toast-wrap"></div>
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
          <div class="muted" style="font-size:13px;margin-bottom:8px">Run this on your <b>source server</b> — it prints the hostname, its reachable IP, and every whole disk (add a row per disk):</div>
          <div style="display:flex;gap:8px;align-items:flex-start;margin-bottom:8px">
            <pre id="srcCmd" style="flex:1;margin:0">echo "Hostname : $(hostname)"; echo "IP       : $(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')"; lsblk -b -d -n -o NAME,SIZE,TYPE | awk '$3=="disk"{printf "Device   : /dev/%s\nSize(GB) : %d\n", $1, ($2+1073741823)/1073741824}'</pre>
            <button onclick="copyText(document.getElementById('srcCmd').textContent,this)">Copy</button>
          </div>
          <div class="muted" style="font-size:12px">Use the printed <b>IP</b> below (it must pass the connection test). Add <b>one row per whole disk</b> (e.g. <code style="display:inline;padding:1px 5px">/dev/sda</code>). The disk with the root filesystem <code style="display:inline;padding:1px 5px">/</code> is the <b>boot disk</b> — put it first. Round sizes up.</div>
        </div>
      </details>
      <div class="row">
        <div><label>Name</label><input id="m_name" placeholder="web01"></div>
        <div><label>Source hostname</label><input id="m_host" placeholder="web01.prod"></div>
      </div>
      <label style="margin-top:12px">Source IP address <span class="muted">(must pass the connection test before you can create)</span></label>
      <div style="display:flex;gap:8px;align-items:flex-start">
        <input id="m_ip" placeholder="e.g. 172.236.148.63" style="flex:1" oninput="ipChanged()" onkeydown="if(event.key==='Enter')testCreateIP(this)">
        <button id="m_iptest" onclick="testCreateIP(this)">Test connection</button>
      </div>
      <div id="m_ipstatus" class="resultbox hide" style="margin-top:6px"></div>
      <label style="margin-top:12px">Source disks (first = boot disk)</label>
      <div id="disks"></div>
      <div style="margin-top:8px"><button onclick="addDisk()">+ Add disk</button></div>
      <div style="margin-top:16px;display:flex;align-items:center;gap:2px">
        <button id="createBtn" class="primary" onclick="createMig(this)">Create migration</button>
        <span class="info" data-tip="Registers this source server and its disks, provisions one replication volume per disk on the appliance, and generates the one-line agent enrollment command. No data is copied until you run that command on the source.">i</span>
      </div>
      <div id="createErr" class="err"></div>
    </div>

    <div style="display:flex;align-items:center;gap:12px;margin:6px 0 12px">
      <h2 style="margin:0">Migrations</h2>
    </div>
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
function fmtTime(t){try{return new Date(t).toLocaleTimeString([],{hour12:false})}catch(e){return ''}}

// uiDialog renders a themed modal in place of the browser's native confirm()/
// alert() boxes. Resolves to false on cancel/Escape/backdrop; on confirm it
// resolves to true, or to {checked:bool} when opts.checkbox is set.
function uiDialog(opts){
  return new Promise(resolve=>{
    const ov=document.createElement('div');ov.className='modal-overlay';
    const check=opts.checkbox?'<label class="modal-check"><input type="checkbox" id="__mck"'+(opts.checkbox.checked?' checked':'')+'><span>'+esc(opts.checkbox.label)+'</span></label>':'';
    const cancelBtn=opts.cancel===false?'':'<button class="modal-cancel">'+esc(opts.cancelText||'Cancel')+'</button>';
    ov.innerHTML='<div class="modal'+(opts.wide?' wide':'')+'" role="dialog" aria-modal="true">'+
      '<h3>'+esc(opts.title||'')+'</h3>'+
      '<div class="modal-body">'+(opts.html||'')+'</div>'+check+
      '<div class="modal-actions">'+cancelBtn+
      '<button class="modal-ok '+(opts.okDanger?'danger':'primary')+'">'+esc(opts.okText||'OK')+'</button></div></div>';
    document.body.appendChild(ov);
    const ok=ov.querySelector('.modal-ok'),cancel=ov.querySelector('.modal-cancel');
    let done=false;
    const close=val=>{if(done)return;done=true;document.removeEventListener('keydown',onKey);ov.classList.add('closing');setTimeout(()=>ov.remove(),150);resolve(val)};
    const confirm=()=>close(opts.checkbox?{checked:ov.querySelector('#__mck').checked}:true);
    function onKey(e){if(e.key==='Escape')close(false);else if(e.key==='Enter'){e.preventDefault();confirm()}}
    ok.onclick=confirm;
    if(cancel)cancel.onclick=()=>close(false);
    ov.onclick=e=>{if(e.target===ov)close(false)};
    document.addEventListener('keydown',onKey);
    setTimeout(()=>ok.focus(),0);
  });
}
function confirmModal(opts){return uiDialog(opts)}
function alertModal(opts){return uiDialog({title:opts.title,html:opts.html,okText:opts.okText||'OK',okDanger:opts.danger,cancel:false})}

// toast shows a small auto-dismissing notification (kind: 'ok' | 'bad').
function toast(msg,kind){
  const el=document.createElement('div');el.className='toast '+(kind||'');
  el.innerHTML='<span class="ic">'+(kind==='bad'?'✕':'✓')+'</span><span>'+esc(msg)+'</span>';
  $('toasts').appendChild(el);
  setTimeout(()=>{el.classList.add('closing');setTimeout(()=>el.remove(),200)},3800);
}

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
async function saveToken(btn){busy(btn,true);try{await api('POST','/api/v1/settings/linode-token',{token:$('ltok').value});loadSettings()}catch(e){alertModal({title:'Could not save token',html:esc(e.message),danger:true})}finally{busy(btn,false)}}
async function removeToken(btn){
  if(!await confirmModal({title:'Remove Linode API token?',html:'Provisioning and finalize will stop working until you add a new one.',okText:'Remove token',okDanger:true}))return;
  busy(btn,true);try{await api('DELETE','/api/v1/settings/linode-token');loadSettings()}catch(e){alertModal({title:'Error',html:esc(e.message),danger:true})}finally{busy(btn,false)}}

let diskSeq=0;
function addDisk(dev,gb){
  diskSeq++;const id=diskSeq;
  const row=document.createElement('div');row.className='row';row.id='disk'+id;row.style.marginBottom='8px';
  row.innerHTML='<div><input class="d_dev" placeholder="/dev/sda'+(diskSeq===1?' (boot)':'')+'" value="'+(dev||'')+'"></div>'+
    '<div style="display:flex;gap:8px"><input class="d_size" type="number" placeholder="Size (GB)" value="'+(gb||'')+'" style="flex:1">'+
    '<button class="danger" title="Remove disk" onclick="this.closest(\'.row\').remove()">✕</button></div>';
  $('disks').appendChild(row);
}
// IP addresses that passed the in-form connection test; only these may be used
// to create a migration.
const testedOkIPs=new Set();
function ipChanged(){$('m_ipstatus').classList.add('hide');}
async function testCreateIP(btn){
  const ip=$('m_ip').value.trim();const s=$('m_ipstatus');
  s.classList.remove('hide');
  if(!ip){s.className='resultbox bad';s.textContent='Enter the source IP address first.';return}
  busy(btn,true);
  s.className='resultbox';s.textContent='Testing connection to '+ip+'…';
  try{
    const r=await api('POST','/api/v1/diagnostics/connection',{ip:ip});
    const open=(r.ports||[]).filter(p=>p.open).length;
    const reachable=r.ping_ok||open>0||(r.ports||[]).some(p=>/refused/.test(p.detail));
    if(reachable){testedOkIPs.add(ip);s.className='resultbox ok';s.textContent='✔ '+ip+' is reachable — you can create the migration.';}
    else{testedOkIPs.delete(ip);s.className='resultbox bad';s.textContent='✘ '+ip+' is not reachable. Fix connectivity (see the Connection test tab) before creating.';}
  }catch(e){s.className='resultbox bad';s.textContent='Error: '+e.message}
  finally{busy(btn,false)}
}
async function createMig(btn){
  $('createErr').textContent='';
  const name=$('m_name').value.trim(),host=$('m_host').value.trim(),ip=$('m_ip').value.trim();
  if(!name){$('createErr').textContent='Enter a migration name.';return}
  if(!host){$('createErr').textContent='Enter the source hostname.';return}
  if(!ip){$('createErr').textContent='Enter the source IP address.';return}
  if(!testedOkIPs.has(ip)){$('createErr').textContent='Run and pass the connection test for '+ip+' first (click “Test connection”).';return}
  const rows=document.querySelectorAll('#disks .row');const devices=[];
  for(const r of rows){const dev=r.querySelector('.d_dev').value.trim();const gb=parseInt(r.querySelector('.d_size').value,10);
    if(!dev)continue; if(!gb||gb<=0){$('createErr').textContent='Each disk needs a positive size (GB): '+dev;return}
    devices.push({device:dev,size_bytes:gb*1073741824});}
  if(!devices.length){$('createErr').textContent='Add at least one disk';return}
  busy(btn,true);
  // Show a loading placeholder at the BOTTOM of the list while provisioning runs
  // (new migrations are appended, newest last).
  $('migs').insertAdjacentHTML('beforeend','<div id="creating" class="mig"><div class="center"><div class="spinner"></div><div>Creating migration & provisioning volume(s)…</div></div></div>');
  $('creating').scrollIntoView({behavior:'smooth',block:'center'});
  try{
    await api('POST','/api/v1/migrations',{name:name,source_hostname:host,source_ip:ip,devices:devices});
    $('m_name').value=$('m_host').value=$('m_ip').value='';$('m_ipstatus').classList.add('hide');
    $('disks').innerHTML='';diskSeq=0;addDisk();
    await refresh(true);
    const last=$('migs').lastElementChild;if(last)last.scrollIntoView({behavior:'smooth',block:'center'});
    toast('Migration "'+name+'" created — enroll the source agent to start replicating','ok');
  }catch(e){$('createErr').textContent='Error: '+e.message;const c=$('creating');if(c)c.remove();toast('Create failed: '+e.message,'bad');}
  finally{busy(btn,false)}
}

function stateClass(s){return ({created:'warn',awaiting_agent:'warn',replicating:'warn',ready:'ok',migrating:'warn',image_ready:'ok',launched:'ok',failed:'bad'})[s]||'muted'}
function disks(m){return m.disks||[]}
function allDone(m){const d=disks(m);return d.length>0&&d.every(x=>x.full_sync_done)}
function bytesTotal(m){return disks(m).reduce((a,d)=>a+(d.bytes_on_wire||0),0)}
function anyDiskError(m){return disks(m).map(d=>d.last_error).filter(Boolean)[0]||''}

async function startMig(id,btn){
  const r=await confirmModal({
    title:'Cut over migration #'+id+'?',
    html:'<div class="warn" style="margin-bottom:8px">After cutover the source agent <b>stops replicating</b> — the migrated copy is frozen at this point.</div>'+
      'This converts the boot disk and clones every disk into launchable <b>&lt;name&gt;-cutover</b> volumes. This is the final step.',
    okText:'Cut over',
    checkbox:{label:'Also launch a new <name>-cutover Linode now (boot=sda, data=sdb…). Leave unchecked to just create the cutover volumes.',checked:true}
  });
  if(!r)return;
  busy(btn,true);
  try{await api('POST','/api/v1/migrations/'+id+'/start',{launch_instance:r.checked});await refreshMig(id)}
  catch(e){alertModal({title:'Cannot cut over',html:esc(e.message),danger:true})}finally{busy(btn,false)}
}
async function stopMig(id,btn){
  if(!await confirmModal({title:'Stop cutover #'+id+'?',html:'The finalize run is cancelled and replication resumes.',okText:'Stop cutover',okDanger:true}))return;
  busy(btn,true);try{await api('POST','/api/v1/migrations/'+id+'/stop');await refreshMig(id)}catch(e){alertModal({title:'Cannot stop',html:esc(e.message),danger:true})}finally{busy(btn,false)}
}
async function deleteMig(id,name,btn){
  if(!await confirmModal({title:'Delete migration #'+id+'?',
    html:'<b>'+esc(name)+'</b><div style="margin-top:8px" class="warn">This deletes any <name>-cutover image volume(s) and the launched cutover Linode. It cannot be undone.</div>'+
      '<div class="muted" style="margin-top:8px;font-size:13px">The <b>vrep-'+esc(name)+' replication volume is detached but kept</b> in your Linode account so you can still reference it. After deletion this card stays briefly with the command to remove the agent from the source — dismiss it once that’s done.</div>',
    okText:'Delete',okDanger:true}))return;
  busy(btn,true);
  try{
    await api('DELETE','/api/v1/migrations/'+id);
    const meta=migMeta[id]||{};
    pendingCleanup[id]={name:name,source:meta.source||'',cmd:meta.uninstall||''};
    const cc=cleanupCard(id);const old=$('mig'+id);
    if(old&&cc)old.replaceWith(cc);else if(cc)$('migs').appendChild(cc);
    toast('Migration #'+id+' ('+name+') deleted — remove the source agent','ok');
  }
  catch(e){toast('Delete failed: '+e.message,'bad')}finally{busy(btn,false)}
}
// completeMig is the green "migration complete" action: it shows the command to
// remove the replication agent from the source — the final step of the cycle.
function completeMig(id){
  const meta=migMeta[id]||{};const cmd=meta.uninstall||'';
  const body='<div style="font-size:13.5px;margin-bottom:10px">Your server is migrated and launched on Linode. To finish, remove the replication agent from <b>'+esc(meta.source||'the source server')+'</b>:</div>'+
    (cmd?('<div style="display:flex;gap:8px;align-items:flex-start"><pre id="donecmd'+id+'" style="flex:1;margin:0">'+esc(cmd)+'</pre>'+
      '<button onclick="copyText(document.getElementById(\'donecmd'+id+'\').textContent,this)">Copy</button></div>')
      :'<div class="muted">Run your uninstall command on the source to remove the agent.</div>')+
    '<div class="muted" style="font-size:12px;margin-top:10px">After removing the agent you can Delete this migration to clean up the appliance’s replication volumes (the launched cutover Linode and its volumes are yours to keep).</div>';
  uiDialog({title:'Migration complete — remove source agent',html:body,wide:true,cancel:false,okText:'Done'});
}
// "Check status" re-fetches THIS migration and shows the latest result in a box.
async function checkStatus(id,btn){
  busy(btn,true);
  try{
    const v=await api('GET','/api/v1/migrations/'+id);
    const card=replaceCard(id,v);
    const m=v.migration;const err=anyDiskError(m);const box=card.querySelector('#status'+id)||$('status'+id);
    if(box){
      box.classList.remove('hide');
      if(allDone(m)){box.className='resultbox ok';box.textContent='✔ All disks baselined and replicating.';}
      else if(err){box.className='resultbox bad';box.textContent='✘ Last replication attempt failed: '+err;}
      else{box.className='resultbox';box.textContent='No completed sync yet. The agent retries every 60s — run the force-retry command on the source if needed.';}
    }
    flash(card);
  }catch(e){const box=$('status'+id);if(box){box.className='resultbox bad';box.textContent='Error: '+e.message}}
  finally{busy(btn,false)}
}
// Cache the rendered (latest-5) activity-log lines per migration so the 5s
// auto-poll (which rebuilds the cards) can restore the log without a flicker.
const logCache={};
function ensureLog(id){if(logCache[id]===undefined)loadLog(id)}
// logLines renders events newest-first as plain text lines inside a <pre>
// ("HH:MM:SS  message"). A <pre> lays lines out top-to-bottom and wraps long
// ones, so entries can never overlap. With limit set, only the latest N show.
function logLines(ev,limit){
  let rows=ev||[];
  if(limit&&rows.length>limit)rows=rows.slice(0,limit);
  if(!rows.length)return 'No activity yet.';
  return rows.map(e=>{
    const line=esc(fmtTime(e.at)+'  '+e.message);
    if(e.level==='error')return '<span class="x">'+line+'</span>';
    if(e.level==='warn')return '<span class="w">'+line+'</span>';
    return line;
  }).join('\n');
}
// loadLog fills the inline <pre> with the latest 5 entries (no scroll).
async function loadLog(id){
  const box=$('log'+id);if(!box)return;
  try{const ev=await api('GET','/api/v1/migrations/'+id+'/events');
    box.innerHTML=logLines(ev,5);
    logCache[id]={html:box.innerHTML};
  }catch(e){box.innerHTML=esc(e.message)}
}
// showLogModal opens the FULL activity log in a large, scrollable <pre> modal.
async function showLogModal(id){
  let body='loading…';
  try{const ev=await api('GET','/api/v1/migrations/'+id+'/events');body=logLines(ev);}
  catch(e){body=esc(e.message);}
  uiDialog({title:'Activity log — migration #'+id,html:'<pre class="logpre scroll">'+body+'</pre>',wide:true,cancel:false,okText:'Close'});
}

// syncPct estimates initial-full-sync completion (0–100). Prefers the live
// block-level percentage reported by the receiver; falls back to bytes received
// vs total source size when no session is active.
function syncPct(v,m){
  const ds=disks(m);
  if(ds.length&&ds.every(d=>d.full_sync_done))return 100;
  if(v.percent_done>=0)return Math.max(0,Math.min(99.9,v.percent_done));
  const tot=ds.reduce((a,d)=>a+(d.size_bytes||0),0);
  if(tot>0)return Math.max(0,Math.min(99,bytesTotal(m)/tot*100));
  return 0;
}
function progBar(width,indet){
  return '<div class="prog'+(indet?' indet':'')+'"><div style="width:'+(indet?35:Math.round(width))+'%"></div></div>';
}
function progressLine(v,m){
  const st=m.state;let label,bar;
  if(st==='image_ready'||st==='launched'){label='completed in '+fmtDur(v.elapsed_seconds);bar=progBar(100,false);}
  else if(st==='failed'){label='failed';bar=progBar(0,false);}
  else if(st==='migrating'){label='finalizing · running '+fmtDur(v.elapsed_seconds);bar=progBar(0,true);}
  else{
    const allBase=disks(m).length>0&&disks(m).every(d=>d.full_sync_done);
    if(allBase){label='initial sync completed · 100%';bar=progBar(100,false);}
    else{
      const pct=syncPct(v,m);
      label=(st==='created'||st==='awaiting_agent'?'waiting for agent':'initial sync')+' · '+pct.toFixed(1)+'%';
      // Live throughput: bytes copied so far (percent of total source size)
      // over the elapsed sync time. Shown as e.g. "42.3 MiB/s".
      const tot=disks(m).reduce((a,d)=>a+(d.size_bytes||0),0);
      if(pct>0&&v.elapsed_seconds>0&&tot>0)label+=' · '+fmtBytes(pct/100*tot/v.elapsed_seconds)+'/s';
      if(v.eta_seconds>=0)label+=' · ~'+fmtDur(v.eta_seconds)+' left';
      bar=progBar(pct,false);
    }
  }
  return '<span class="muted">'+esc(label)+'</span>'+bar+
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
function infoIcon(tip){return '<span class="info" data-tip="'+esc(tip)+'">i</span>'}
function stateLabel(s){return ({created:'created',awaiting_agent:'waiting for agent',replicating:'replicating',ready:'ready to cut over',migrating:'finalizing',image_ready:'image ready',launched:'launched',failed:'failed'})[s]||s}
// statusLegend renders a hover legend that shows each status as its ACTUAL
// colored pill (so you can match what you see in the table to its meaning).
const STATE_DESCS=[['awaiting_agent','enrolled; the agent has not connected yet'],
  ['replicating','agent connected; copying the baseline, then ongoing changes'],
  ['ready','baseline done and lag is low — safe to cut over'],
  ['migrating','converting the boot disk and cloning volumes'],
  ['image_ready','image volume(s) ready to launch'],
  ['launched','a new Linode was launched from the image'],
  ['failed','something went wrong — see the error shown on the card']];
function statusLegend(){
  let h='<span class="leg">i<span class="legbox"><div style="font-weight:600;font-size:12px;margin-bottom:6px">Source &rarr; Appliance status</div>';
  for(const [s,d] of STATE_DESCS)h+='<div class="legrow"><span class="pill '+stateClass(s)+'">'+esc(stateLabel(s))+'</span><span class="desc">'+esc(d)+'</span></div>';
  return h+'</span></span>';
}
// Per-migration UI state that must survive the 5s rebuild.
const collapsedMigs=new Set();   // migration ids the user collapsed
const seenMigs=new Set();        // migrations rendered at least once (for first-time defaults)
const migMeta={};                // id -> {uninstall, source, name} captured at render time
const pendingCleanup={};         // id -> {name, source, cmd} for deleted migrations awaiting agent removal
function toggleCollapse(id,btn){
  const card=$('mig'+id);if(!card)return;
  const now=card.classList.toggle('collapsed');
  if(now)collapsedMigs.add(id);else collapsedMigs.delete(id);
  btn.textContent=now?'▸':'▾';btn.title=now?'Expand':'Collapse';
}
// After a migration is deleted we keep a lightweight reminder card holding the
// command to remove the agent from the source, plus a "Done" tick to dismiss it.
function cleanupCard(id){
  const p=pendingCleanup[id];if(!p)return null;
  const card=document.createElement('div');card.className='mig';card.id='mig'+id;
  card.innerHTML=
    '<div class="banner" style="border-color:#cdd6e8;background:#f3f6fc;color:#22408a">✔ <b>Migration #'+id+' ('+esc(p.name)+') deleted.</b> The replication volume and data were removed. One last step: remove the replication agent from your source server.</div>'+
    (p.cmd?('<label>Run this on '+esc(p.source||'the source server')+' to remove the agent</label>'+
      '<div style="display:flex;gap:8px;align-items:flex-start"><pre id="uclean'+id+'" style="flex:1;margin:0">'+esc(p.cmd)+'</pre>'+
      '<button onclick="copyText(document.getElementById(\'uclean'+id+'\').textContent,this)">Copy</button></div>')
      :'<div class="muted" style="font-size:13px">Remove the agent on the source: <code style="display:inline;padding:1px 5px">curl -fsSL .../install/uninstall.sh | sudo bash</code></div>')+
    '<div class="actions"><button class="primary" onclick="dismissCleanup('+id+')">✓ Done — agent removed</button>'+
    '<span class="muted" style="font-size:12px">Reminder only — the migration is already deleted. Dismiss when the agent is gone.</span></div>';
  return card;
}
function dismissCleanup(id){delete pendingCleanup[id];const c=$('mig'+id);if(c)c.remove();}
function migCard(v){
  const m=v.migration;const err=anyDiskError(m);
  migMeta[m.id]={uninstall:v.uninstall_cmd||'',source:m.source_hostname||'',name:m.name};
  const collapsed=collapsedMigs.has(m.id);
  const firstSeen=!seenMigs.has(m.id);seenMigs.add(m.id);

  // Header: collapse chevron + per-migration refresh (acts on this card only).
  let h='<div class="mighead">'+
    '<button class="chev" title="'+(collapsed?'Expand':'Collapse')+'" onclick="toggleCollapse('+m.id+',this)">'+(collapsed?'▸':'▾')+'</button>'+
    '<span class="muted" style="font-size:12px;flex:1">'+(collapsed?'collapsed — click ▸ to expand':'')+'</span>'+
    '<button class="mini" title="Refresh this migration" onclick="refreshMig('+m.id+',this)">↻ Refresh</button></div>';

  h+='<table style="margin-bottom:4px"><tr><th>Migration</th><th>Source &rarr; Appliance'+statusLegend()+'</th><th>Disks</th><th>Progress</th><th>RPO</th></tr><tr>'+
    '<td><b>#'+m.id+'</b> '+esc(m.name)+'<br><span class="muted">'+esc(m.source_ip||m.source_hostname||'-')+'</span></td>'+
    '<td><span class="pill '+stateClass(m.state)+'">'+esc(stateLabel(m.state))+'</span></td>'+
    '<td class="muted">'+disks(m).length+' disk(s)<br>'+(allDone(m)?'baseline done':'baselining')+'</td>'+
    '<td>'+progressLine(v,m)+'</td>'+
    '<td class="muted">'+(v.rpo_seconds?Math.round(v.rpo_seconds)+'s':'—')+'</td></tr></table>';

  // Everything below the status table is hidden when the card is collapsed.
  let b='';
  if(m.last_error)b+='<div class="resultbox bad">'+esc(m.last_error)+'</div>';
  else if(err)b+='<div class="resultbox bad">Last replication attempt failed: '+esc(err)+'</div>';

  if(['image_ready','launched'].includes(m.state)){
    const arts=disks(m).map(d=>esc(d.artifact_id||('vmrep-'+m.name+'-img'))).join(', ');
    b+='<div class="banner">✔ <b>Migration completed.</b> '+disks(m).length+' image volume(s) in your Linode account ('+
       '<a href="https://cloud.linode.com/volumes" target="_blank" rel="noopener">cloud.linode.com/volumes</a>): <code style="display:inline;padding:1px 5px">'+arts+'</code>. '+
       (m.launched_linode_id?('Launched Linode '+esc(m.launched_linode_id)+' — see <a href="https://cloud.linode.com/linodes" target="_blank" rel="noopener">your Linodes</a>.')
       :'To launch manually: create a Linode (same region), attach these volumes (boot disk = <b>sda</b>, data = sdb…), then add a config. If the boot disk has a <b>partition table + GRUB</b>, use kernel <code style="display:inline;padding:1px 5px">GRUB 2</code>; if it is a <b>partitionless whole-disk filesystem</b>, use a <b>Linode kernel</b> (e.g. “Latest 64-bit”) with root <code style="display:inline;padding:1px 5px">/dev/sda</code>. Then boot. The “Cutover” launch option picks the right kernel for you automatically.')+'</div>';
  }

  // Two groups: pre-migration (environment readiness while replicating) and
  // migration (the cutover gate: initial full sync).
  const checkRow=c=>'<div style="font-size:13px;margin:2px 0"><span class="'+(c.ok?'y">✔':'x">✘')+'</span> '+esc(c.name)+' <span class="muted">— '+esc(c.detail)+'</span></div>';
  const subHead=t=>'<div class="muted" style="font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:.04em;margin:10px 0 4px">'+t+'</div>';
  const pre=(v.validations||[]).filter(c=>c.group!=='migration'),mig=(v.validations||[]).filter(c=>c.group==='migration');
  let checks=subHead('Pre-migration validation checks')+pre.map(checkRow).join('')+
             subHead('Migration validation check')+mig.map(checkRow).join('');
  const allOk=(v.validations||[]).every(c=>c.ok);
  b+='<details'+(allOk?'':' open')+'><summary>Validation checks'+(allOk?' (all passing)':'')+'</summary><div>'+
     '<div class="muted" style="font-size:12px;margin-bottom:6px">Pre-migration checks track readiness while replicating (informational after cutover). The migration check — <b>initial full sync complete</b> — is what allows cutover.</div>'+checks+'</div></details>';
  b+='<details><summary>Disks ('+disks(m).length+')</summary><div>'+diskTable(m)+'</div></details>';
  const cachedLog=logCache[m.id];
  b+='<details ontoggle="if(this.open)ensureLog('+m.id+')"><summary>Activity log</summary><div>'+
     '<div style="display:flex;align-items:center;gap:8px;margin-bottom:6px"><span class="muted" style="font-size:12px;flex:1">Latest 5 entries — click Expand for the full history</span>'+
     '<button class="mini" onclick="showLogModal('+m.id+')" title="Open full log">⤢ Expand</button></div>'+
     '<pre id="log'+m.id+'" class="logpre">'+(cachedLog?cachedLog.html:'loading…')+'</pre></div></details>';

  if(v.enroll_cmd && !allDone(m) && m.state!=='migrating'){
    const certErr=/certificate|tls|x509/i.test(err||'');
    // Default-open on first sight; afterwards the open/closed state is preserved
    // across refreshes (openKeys), so closing it makes it stay closed.
    b+='<details'+(firstSeen?' open':'')+'><summary>Enroll the source server (all '+disks(m).length+' disk(s))</summary><div>';
    if(certErr)b+='<div class="resultbox bad" style="margin-bottom:8px">The agent could not complete the TLS handshake — this usually means it was installed against an <b>older appliance certificate</b>. A retry will not fix it: <b>re-run the command below</b> to reinstall the agent with the current certificates.</div>';
    b+='<label>Run this on '+esc(m.source_hostname||'the source')+'</label>'+
       '<div style="display:flex;gap:8px;align-items:flex-start"><pre id="enroll'+m.id+'" style="flex:1;margin:0">'+esc(v.enroll_cmd)+'</pre>'+
       '<button onclick="copyText(document.getElementById(\'enroll'+m.id+'\').textContent,this)">Copy</button></div>'+
       '<div class="muted" style="font-size:12px;margin-top:8px">If a disk’s sync fails, no reinstall is needed — the agent retries every 60s. '+
       'Force a retry on the source with <code style="display:inline;padding:1px 5px">sudo systemctl start vmrepl-agent.service</code>, then click <b>Check status</b> below.</div>'+
       '<div class="actions"><button onclick="checkStatus('+m.id+',this)">Check status</button></div>'+
       '<div id="status'+m.id+'" class="resultbox hide"></div>'+
       '<hr class="migdivider"></div></details>';
  }
  if(v.uninstall_cmd && ['image_ready','launched'].includes(m.state)){
    b+='<details><summary>Remove the agent from the source</summary><div style="display:flex;gap:8px;align-items:flex-start">'+
       '<pre id="unin'+m.id+'" style="flex:1;margin:0">'+esc(v.uninstall_cmd)+'</pre>'+
       '<button onclick="copyText(document.getElementById(\'unin'+m.id+'\').textContent,this)">Copy</button></div></details>';
  }

  b+='<div class="actions">';
  if(['image_ready','launched'].includes(m.state)){
    // Done: green button leads to the agent-removal notice — the intended way
    // to finish the migration cycle.
    b+='<button class="primary done" onclick="completeMig('+m.id+')">✓ Migration complete — remove source agent</button>';
  }else if(m.state==='migrating'){
    b+='<button class="danger" onclick="stopMig('+m.id+',this)">Stop</button>';
  }else if(m.state==='failed' && allDone(m)){
    // A cutover failed but the data is fully replicated — retry re-runs it.
    b+='<button class="primary" onclick="startMig('+m.id+',this)">Retry cutover</button>'+
      infoIcon('Re-runs the cutover on the data already replicated to this appliance. It first removes any half-built <name>-cutover instance/volumes from the failed attempt, then launches fresh — no re-replication of the source is needed.');
  }else{
    // Readiness is auto-computed: the Cutover button enables itself once the
    // initial full sync is complete (no manual assessment).
    const ready=v.can_migrate;
    b+='<button class="primary"'+(ready?'':' disabled title="The initial full sync must complete on all disks first"')+' onclick="startMig('+m.id+',this)">Cutover instance</button>'+
      infoIcon('Cuts over to Linode: stops replication, converts the boot disk, clones every disk into <name>-cutover volumes, and (optionally) launches a <name>-cutover Linode. Enables automatically once the initial full sync completes.');
  }
  b+='<span style="flex:1"></span><button class="danger" onclick="deleteMig('+m.id+',\''+esc(m.name)+'\',this)">Delete</button></div>';

  h+='<div class="migbody">'+b+'</div>';
  const card=document.createElement('div');card.className='mig'+(collapsed?' collapsed':'');card.id='mig'+m.id;card.innerHTML=h;return card;
}

// replaceCard swaps just this migration's card in place, preserving which
// <details> were open and the activity-log scroll position.
function replaceCard(id,v){
  const old=$('mig'+id);const open=cardOpenKeys(old);
  const card=migCard(v);
  if(old)old.replaceWith(card);
  card.querySelectorAll('details').forEach(d=>{if(open.has(d.querySelector('summary').textContent))d.open=true});
  if(card.querySelector('details[open] pre[id^="log"]'))loadLog(id); // refetch, not just cached
  return card;
}
// refreshMig re-fetches and re-renders ONLY this migration's card.
async function refreshMig(id,btn){
  busy(btn,true);
  try{const v=await api('GET','/api/v1/migrations/'+id);flash(replaceCard(id,v));}
  catch(e){toast('Refresh failed: '+e.message,'bad')}
  finally{busy(btn,false)}
}
function cardOpenKeys(card){const s=new Set();if(card)card.querySelectorAll('details[open]').forEach(d=>s.add(d.querySelector('summary').textContent));return s}

// Preserve which <details> were open across refreshes so the UI doesn't collapse.
function openKeys(){const s=new Set();document.querySelectorAll('#migs details[open]').forEach((d,i)=>{const card=d.closest('.mig');if(card)s.add(card.id+':'+d.querySelector('summary').textContent)});return s}

async function refresh(animate){
  try{
    const open=openKeys();
    const list=await api('GET','/api/v1/migrations');
    list.sort((a,b)=>a.migration.id-b.migration.id); // oldest first; newest at the bottom
    const c=$('creating');if(c)c.remove();
    const migs=$('migs');
    migs.innerHTML='';
    list.forEach(v=>{const card=migCard(v);migs.appendChild(card);
      card.querySelectorAll('details').forEach(d=>{if(open.has(card.id+':'+d.querySelector('summary').textContent))d.open=true});});
    // Keep reminder cards for migrations that were deleted but whose agent
    // cleanup hasn't been dismissed yet (they're no longer in the list).
    Object.keys(pendingCleanup).forEach(id=>{if(!$('mig'+id)){const cc=cleanupCard(id);if(cc)migs.appendChild(cc);}});
    if(!migs.children.length){migs.innerHTML='<div class="muted" style="padding:8px">No migrations yet. Create one above.</div>';}
    // Re-fetch every OPEN activity log so new entries appear automatically
    // (the cached copy is only a flicker-free placeholder until this lands).
    document.querySelectorAll('#migs details[open] pre[id^="log"]').forEach(p=>loadLog(parseInt(p.id.slice(3),10)));
    if(animate)flash(migs);
    loadSettings();
  }catch(e){/* 401 handled in api() */}
}

async function start(){show('app');if(!document.querySelector('#disks .row'))addDisk();refresh(false);}
async function init(){
  try{await api('GET','/api/v1/session');start();setInterval(()=>{if(!$('app').classList.contains('hide'))refresh(false)},10000);}
  catch(e){show('login')}
}
init();
</script>
</body></html>`
