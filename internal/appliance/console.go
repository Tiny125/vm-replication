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
      <label style="margin-top:12px">Source IP address</label>
      <input id="m_ip" placeholder="e.g. 172.236.148.63">
      <label style="margin-top:12px">Source disks (first = boot disk)</label>
      <div id="disks"></div>
      <div style="margin-top:8px"><button onclick="addDisk()">+ Add disk</button></div>

      <label style="margin-top:14px">Boot target</label>
      <div class="row">
        <div><select id="m_boot" onchange="bootTargetChanged()">
          <option value="volume">Separate Block Storage volume (default)</option>
          <option value="disk">Linode local disk (NVMe)</option>
        </select></div>
        <div><select id="m_planclass" onchange="reloadPlanOptions()">
          <option value="shared">Shared CPU</option>
          <option value="dedicated">Dedicated CPU</option>
        </select></div>
      </div>
      <label style="margin-top:10px">Linode plan</label>
      <select id="m_plan" onchange="updatePlanInfo()"></select>
      <div id="m_plan_help" class="muted" style="font-size:12px;margin-top:6px"></div>
      <div id="m_boot_help" class="muted" style="font-size:12px;margin-top:6px"></div>

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
    // Optional text inputs (opts.fields:[{id,label,type,placeholder}]); their
    // trimmed values are returned on confirm, keyed by id.
    const fields=(opts.fields||[]).map(f=>
      '<div style="margin-top:10px"><label style="display:block;margin-bottom:4px;font-size:13px">'+esc(f.label)+'</label>'+
      '<input id="__f_'+f.id+'" type="'+(f.type||'text')+'" style="width:100%" placeholder="'+esc(f.placeholder||'')+'"></div>').join('');
    const check=opts.checkbox?'<label class="modal-check"><input type="checkbox" id="__mck"'+(opts.checkbox.checked?' checked':'')+'><span>'+esc(opts.checkbox.label)+'</span></label>':'';
    // Optional multiple checkboxes (opts.checkboxes:[{id,label,checked}]); each is
    // returned as out[id]=bool on confirm.
    const checks=(opts.checkboxes||[]).map(c=>'<label class="modal-check"><input type="checkbox" id="__c_'+c.id+'"'+(c.checked?' checked':'')+'><span>'+esc(c.label)+'</span></label>').join('');
    const cancelBtn=opts.cancel===false?'':'<button class="modal-cancel">'+esc(opts.cancelText||'Cancel')+'</button>';
    ov.innerHTML='<div class="modal'+(opts.wide?' wide':'')+'" role="dialog" aria-modal="true">'+
      '<h3>'+esc(opts.title||'')+'</h3>'+
      '<div class="modal-body">'+(opts.html||'')+'</div>'+fields+check+checks+
      '<div class="modal-actions">'+cancelBtn+
      '<button class="modal-ok '+(opts.okDanger?'danger':'primary')+'">'+esc(opts.okText||'OK')+'</button></div></div>';
    document.body.appendChild(ov);
    const ok=ov.querySelector('.modal-ok'),cancel=ov.querySelector('.modal-cancel');
    let done=false;
    const close=val=>{if(done)return;done=true;document.removeEventListener('keydown',onKey);ov.classList.add('closing');setTimeout(()=>ov.remove(),150);resolve(val)};
    const confirm=()=>{
      const hasOut=opts.checkbox||(opts.checkboxes&&opts.checkboxes.length)||(opts.fields&&opts.fields.length);
      if(!hasOut)return close(true);
      const out={};
      if(opts.checkbox)out.checked=ov.querySelector('#__mck').checked;
      (opts.checkboxes||[]).forEach(c=>out[c.id]=ov.querySelector('#__c_'+c.id).checked);
      (opts.fields||[]).forEach(f=>out[f.id]=ov.querySelector('#__f_'+f.id).value.trim());
      close(out);
    };
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
    h+='<div style="display:flex;gap:12px;align-items:center;flex-wrap:wrap"><span><span class="y">✔</span> Linode API token validated &amp; stored.'+
       (st.linode_account?(' Account: <b>'+esc(st.linode_account)+'</b>.'):'')+'<br>'+
       (st.linode_automation?('Appliance Linode '+esc(st.appliance_linode_id)+'; volumes created in its region.'):'(appliance Linode id unknown — file-fallback mode)')+'<br>'+
       (st.audit_ready?('<span class="y">✔</span> Audit log bucket <b>'+esc(st.audit_bucket)+'</b>'+(st.audit_region?(' in region <b>'+esc(st.audit_region)+'</b>'):'')+' — console &amp; per-migration logs upload to Object Storage (browse in Cloud Manager).')
        :(st.audit_error?('<span class="x">✘</span> Audit log bucket not created: '+esc(st.audit_error)):'<span class="muted">Audit log bucket: provisioning…</span>'))+'</span>'+
       '<button onclick="reprovisionAuditBucket(this)">Re-create audit bucket</button>'+
       (st.audit_ready?'<button class="danger" onclick="deleteAuditBucket(this)">Delete audit bucket</button>':'')+
       '<button class="danger" onclick="removeToken(this)">Remove token</button></div>'+
       '<div class="muted" style="margin-top:8px;font-size:12px">“Re-create” makes <code style="display:inline;padding:1px 5px">vmrep-audit-'+esc(st.appliance_linode_id||'&lt;id&gt;')+'</code> if it doesn’t exist (and tells you if it already does). “Delete audit bucket” empties and removes it with all logs — only when <b>no migration is active</b> and after you enter the console password. The token can only be removed once <b>no migrations exist</b>.</div>';
  }else{
    h+='<details><summary>What is this and how do I get a token?</summary><div class="muted" style="font-size:13px">'+
       'A Linode <b>Personal Access Token</b> lets the appliance create volumes, clone disks and launch instances. Stored <b>encrypted at rest</b>. '+
       'Create one at <a href="https://cloud.linode.com/profile/tokens" target="_blank" rel="noopener">cloud.linode.com/profile/tokens</a> with scopes <b>Linodes: Read/Write</b>, <b>Volumes: Read/Write</b>, <b>Images: Read/Write</b> and <b>Object Storage: Read/Write</b> (Images is used by the local-disk boot method; Object Storage for the audit logs).'+
       '</div></details>'+
       '<div style="display:flex;gap:8px;margin-top:10px"><input id="ltok" type="password" placeholder="Linode API token"><button onclick="saveToken(this)">Save</button></div>';
  }
  $('settings').innerHTML=h;
}
async function saveToken(btn){
  busy(btn,true);
  try{
    const r=await api('POST','/api/v1/settings/linode-token',{token:$('ltok').value});
    toast('Linode token validated'+(r&&r.linode_account?(' — account '+r.linode_account):''),'ok');
    loadSettings();
  }catch(e){alertModal({title:'Token rejected',html:esc(e.message),danger:true})}
  finally{busy(btn,false)}
}
async function removeToken(btn){
  if(!await confirmModal({title:'⚠ Remove Linode API token?',
    html:'<div class="warn">Provisioning, cloning and launching will <b>stop working</b> until you add a valid token again.</div>'+
      '<div class="muted" style="margin-top:8px;font-size:13px">Only allowed when <b>no migrations exist</b> — otherwise removal is refused, because deleting a migration needs the token to remove its Linode volumes (removing it first would orphan them). This does not delete anything in your Linode account.</div>',
    okText:'Remove token',okDanger:true}))return;
  busy(btn,true);try{await api('DELETE','/api/v1/settings/linode-token');toast('Linode token removed','ok');loadSettings()}catch(e){alertModal({title:'Error',html:esc(e.message),danger:true})}finally{busy(btn,false)}}
async function reprovisionAuditBucket(btn){
  busy(btn,true);
  try{
    const r=await api('POST','/api/v1/settings/audit-bucket',{});
    if(r.already_exists){toast('Audit bucket already exists: '+r.audit_bucket,'ok');}
    else{toast('Audit bucket created: '+r.audit_bucket+(r.audit_region?(' ('+r.audit_region+')'):''),'ok');}
    loadSettings();
  }catch(e){alertModal({title:'Could not create audit bucket',html:esc(e.message),danger:true})}finally{busy(btn,false)}}
async function deleteAuditBucket(btn){
  const r=await confirmModal({
    title:'⚠ Delete the audit-log bucket?',
    html:'<div class="warn"><b>This permanently deletes the Object Storage bucket and ALL audit logs</b> — the console “main” log and every per-migration log. It cannot be undone.</div>'+
      '<div class="muted" style="margin-top:8px;font-size:13px">Only allowed when <b>no migration is active</b> (created or running). Enter your <b>console password</b> to confirm. You can re-create an empty bucket afterwards.</div>',
    okText:'Delete bucket',okDanger:true,
    fields:[{id:'pw',label:'Console password',type:'password',placeholder:'your console login password'}]
  });
  if(!r)return;
  if(!r.pw){alertModal({title:'Password required',html:'Enter your console password to delete the bucket.',danger:true});return}
  busy(btn,true);
  try{await api('DELETE','/api/v1/settings/audit-bucket',{password:r.pw});toast('Audit bucket and its logs deleted','ok');loadSettings();}
  catch(e){alertModal({title:'Could not delete audit bucket',html:esc(e.message),danger:true})}finally{busy(btn,false)}}

let diskSeq=0;
function addDisk(dev,gb){
  diskSeq++;const id=diskSeq;
  const row=document.createElement('div');row.className='row';row.id='disk'+id;row.style.marginBottom='8px';
  row.innerHTML='<div><input class="d_dev" placeholder="/dev/sda'+(diskSeq===1?' (boot)':'')+'" value="'+(dev||'')+'"></div>'+
    '<div style="display:flex;gap:8px"><input class="d_size" type="number" placeholder="Size (GB)" value="'+(gb||'')+'" style="flex:1" oninput="diskSizeChanged()">'+
    '<button class="danger" title="Remove disk" onclick="this.closest(\'.row\').remove();diskSizeChanged()">✕</button></div>';
  $('disks').appendChild(row);
}

// ---- boot target: separate volume (default) vs Linode local disk ----
let _plansCache=null;
async function loadPlans(){
  if(_plansCache)return _plansCache;
  const r=await api('GET','/api/v1/linode/plans');
  _plansCache=(r&&r.plans)||[];
  return _plansCache;
}
function validHostname(h){return !!h&&h.length<=253&&/^[A-Za-z0-9.-]+$/.test(h)&&h[0]!=='-';}
function totalDiskGB(){let g=0;document.querySelectorAll('#disks .d_size').forEach(i=>{const v=parseInt(i.value,10);if(v>0)g+=v;});return g;}
function diskSizeChanged(){reloadPlanOptions();}
function bootTargetChanged(){
  const disk=$('m_boot').value==='disk';
  $('m_boot_help').innerHTML=disk
    ? 'Boots from the Linode’s <b>local NVMe disk</b> — faster, and no separate volume cost. Pick a plan whose disk fits your data; region follows the appliance.'
    : 'Boots from a <b>Block Storage volume</b> sized to your data and attached to the chosen plan. The volume is billed separately (~$0.10/GB-month) on top of the plan.';
  reloadPlanOptions();
}
// Populate the plan dropdown from the chosen class + boot target. In disk mode
// only plans whose local disk fits the data are offered; the default selection
// is the closest fit. The user's explicit choice is preserved across refreshes.
async function reloadPlanOptions(){
  const sel=$('m_plan'); if(!sel)return;
  const cls=$('m_planclass').value, disk=$('m_boot').value==='disk', gb=totalDiskGB();
  let plans;
  try{plans=await loadPlans();}
  catch(e){sel.innerHTML='<option value="">add a Linode token in Settings to load plans</option>';$('m_plan_help').innerHTML='';return;}
  let list=plans.filter(p=>p.class===cls);
  if(disk)list=list.filter(p=>p.disk_gb>=gb);
  list.sort((a,b)=>a.disk_gb-b.disk_gb||a.price_monthly-b.price_monthly);
  if(!list.length){sel.innerHTML='<option value="">'+(disk?('no '+cls+' plan has a disk ≥ '+gb+' GB'):('no '+cls+' plans available'))+'</option>';updatePlanInfo();return;}
  const def=(list.find(p=>p.disk_gb>=gb)||list[0]).id, prev=sel.value;
  sel.innerHTML=list.map(p=>'<option value="'+p.id+'">'+esc(p.label)+' — '+p.vcpus+' vCPU, '+(p.memory_mb/1024)+' GB, '+p.disk_gb+' GB disk ($'+p.price_monthly+'/mo)</option>').join('');
  sel.value=(prev&&list.some(p=>p.id===prev))?prev:def;
  updatePlanInfo();
}
function updatePlanInfo(){
  const sel=$('m_plan'), help=$('m_plan_help'); if(!sel||!help)return;
  const p=(_plansCache||[]).find(x=>x.id===sel.value);
  if(!p){help.innerHTML='';return;}
  let h='Instance: <b>'+esc(p.label)+'</b> — '+p.vcpus+' vCPU, '+(p.memory_mb/1024)+' GB RAM (~$'+p.price_monthly+'/mo).';
  if($('m_boot').value!=='disk'){
    const gb=totalDiskGB(), vol=gb*0.10;
    h+=' Block Storage volume'+(gb>0?(' ('+gb+' GB)'):'')+': ~$'+vol.toFixed(2)+'/mo'+(gb>0?'':' — enter disk size(s)')+'. <b>Est. total ~$'+(p.price_monthly+vol).toFixed(2)+'/mo.</b>';
  }
  help.innerHTML=h;
}
// IP addresses that passed the in-form connection test; only these may be used
// to create a migration.
// validIP checks the source IP is a well-formed IPv4 (or loose IPv6) address.
// Reachability is no longer tested at create time — enroll the agent and watch
// the connection validate in the migration card instead.
function validIP(s){
  const m=/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
  if(m)return m.slice(1).every(o=>+o<=255);
  return /^[0-9A-Fa-f:]+$/.test(s)&&s.includes(':'); // loose IPv6
}
async function createMig(btn){
  $('createErr').textContent='';
  const name=$('m_name').value.trim(),host=$('m_host').value.trim(),ip=$('m_ip').value.trim();
  if(!name){$('createErr').textContent='Enter a migration name.';return}
  if(!host){$('createErr').textContent='Enter the source hostname.';return}
  if(!validHostname(host)){$('createErr').textContent='“'+host+'” is not a valid hostname (letters, digits, dots and hyphens only — no spaces). Fix it and try again.';return}
  if(!ip){$('createErr').textContent='Enter the source IP address.';return}
  if(!validIP(ip)){$('createErr').textContent='“'+ip+'” is not a valid IP address (e.g. 172.236.148.63).';return}
  const rows=document.querySelectorAll('#disks .row');const devices=[];
  for(const r of rows){const dev=r.querySelector('.d_dev').value.trim();const gbRaw=r.querySelector('.d_size').value.trim();const gb=parseInt(gbRaw,10);
    if(!dev||!gbRaw){$('createErr').textContent='Fill in every disk row (device path and size) or remove the empty one before creating.';return}
    if(!gb||gb<=0){$('createErr').textContent='Each disk needs a positive size (GB): '+dev;return}
    devices.push({device:dev,size_bytes:gb*1073741824});}
  if(!devices.length){$('createErr').textContent='Add at least one disk';return}
  const bootTarget=$('m_boot').value, planClass=$('m_planclass').value, planType=$('m_plan').value;
  busy(btn,true);
  // Show a loading placeholder at the BOTTOM of the list while provisioning runs
  // (new migrations are appended, newest last).
  $('migs').insertAdjacentHTML('beforeend','<div id="creating" class="mig"><div class="center"><div class="spinner"></div><div>Creating migration & provisioning volume(s)…</div></div></div>');
  $('creating').scrollIntoView({behavior:'smooth',block:'center'});
  try{
    await api('POST','/api/v1/migrations',{name:name,source_hostname:host,source_ip:ip,devices:devices,boot_target:bootTarget,plan_class:planClass,linode_type:planType});
    $('m_name').value=$('m_host').value=$('m_ip').value='';
    $('disks').innerHTML='';diskSeq=0;addDisk();$('m_boot').value='volume';bootTargetChanged();
    await refresh(true);
    const last=$('migs').lastElementChild;if(last)last.scrollIntoView({behavior:'smooth',block:'center'});
    toast('Migration "'+name+'" created — enroll the source agent to start replicating','ok');
  }catch(e){$('createErr').textContent='Error: '+e.message;const c=$('creating');if(c)c.remove();toast('Create failed: '+e.message,'bad');}
  finally{busy(btn,false)}
}

function stateClass(s){return ({created:'warn',awaiting_agent:'warn',replicating:'warn',ready:'ok',awaiting_cutover:'warn',migrating:'warn',image_ready:'ok',launched:'ok',failed:'bad'})[s]||'muted'}
function disks(m){return m.disks||[]}
function allDone(m){const d=disks(m);return d.length>0&&d.every(x=>x.full_sync_done)}
function bytesTotal(m){return disks(m).reduce((a,d)=>a+(d.bytes_on_wire||0),0)}
function anyDiskError(m){return disks(m).map(d=>d.last_error).filter(Boolean)[0]||''}

async function startMig(id,btn){
  const meta=migMeta[id]||{};
  const disk=meta.boot_target==='disk';
  const planNote=meta.linode_type?(' on plan <b>'+esc(meta.linode_type)+'</b>'):'';
  const access='<div class="muted" style="font-size:12px;margin-top:10px">Migrated disks keep the <b>source</b>’s logins, and cloud images usually leave root locked — so set a root password (and/or SSH key) below to reach the new instance via the Lish console without rescue mode.</div>';
  // Guided cutover, two steps with a power-off in between (same for volume- and
  // disk-boot, for consistency). Step 1 STOPS replication and freezes the current
  // replicated copy as the image — crash-consistent (like a power-loss), repaired
  // with fsck on convert; it never attempts a read-only remount, so it can't get
  // stuck on a busy root. Step 2 (after you power off the source) launches.
  const how='<div style="margin-bottom:8px"><b>Step 1 of 2.</b> This <b>stops replication</b> and freezes the current replicated copy as the image to launch'+(disk?(' — step 2 then creates a new Linode'+planNote+' booting from its local disk.'):(meta.linode_type?(' — step 2 then launches a new Linode'+planNote+'.'):' and clones every disk into launchable volumes.'))+' Next you’ll <b>power off the source</b>, then launch.</div>';
  const prep='<div class="muted" style="font-size:12px;margin-top:8px"><b>Before you click:</b> stop the source’s databases/heavy writers and let the <b>RPO lag drop to ~0</b> so the frozen copy is current. The image is crash-consistent and repaired with fsck on convert — no LVM or read-only remount needed.</div>';
  const opts={
    title:'Cut over migration #'+id+' — step 1: stop replication & freeze the image',
    okText:'Stop replication & continue',
    html:how+access+prep,
    fields:[
      {id:'root_pw',label:'Root password for the migrated instance (optional)',type:'password',placeholder:'leave blank to keep the source’s credentials'},
      {id:'ssh_key',label:'SSH public key for root (optional)',type:'text',placeholder:'ssh-ed25519 AAAA… you@host'}
    ]
  };
  const r=await confirmModal(opts);
  if(!r)return;
  busy(btn,true);
  // Always guided + skip the read-only snapshot: freeze the current crash-consistent
  // data, pause for the operator to power off the source, then launch. Always launch.
  try{await api('POST','/api/v1/migrations/'+id+'/start',{launch_instance:true,root_password:r.root_pw||'',ssh_authorized_key:r.ssh_key||'',skip_snapshot:true,guided_shutdown:true});await refreshMig(id)}
  catch(e){alertModal({title:'Cannot start cutover',html:esc(e.message),danger:true})}finally{busy(btn,false)}
}
async function completeCutover(id,btn){
  if(!await confirmModal({title:'Source powered off — launch the migrated instance?',html:'Confirm the <b>source server is shut down</b> (both machines must not run at once). This converts the frozen image, clones the disk(s), and launches the new instance. This is the final step.',okText:'Launch instance'}))return;
  busy(btn,true);try{await api('POST','/api/v1/migrations/'+id+'/complete',{});await refreshMig(id)}catch(e){alertModal({title:'Cannot complete',html:esc(e.message),danger:true})}finally{busy(btn,false)}
}
async function stopMig(id,btn){
  if(!await confirmModal({title:'Stop cutover #'+id+'?',html:'The finalize run is cancelled and replication resumes.',okText:'Stop cutover',okDanger:true}))return;
  busy(btn,true);try{await api('POST','/api/v1/migrations/'+id+'/stop');await refreshMig(id)}catch(e){alertModal({title:'Cannot stop',html:esc(e.message),danger:true})}finally{busy(btn,false)}
}
// startReplication starts (or, when resume=true, resumes) replication.
async function startReplication(id,btn,resume){
  const opts=resume
    ?{title:'Resume replication for #'+id+'?',html:'Replication continues with an <b>incremental delta sync</b> — only the blocks that changed during the pause are sent, not a full re-copy.',okText:'Resume replication'}
    :{title:'Start replication for #'+id+'?',html:'The agent connection is validated. This begins the <b>initial full sync</b> from the source; the agent streams every block, then keeps the copy current. You can cut over once the baseline completes.',okText:'Start replication'};
  if(!await confirmModal(opts))return;
  busy(btn,true);
  try{await api('POST','/api/v1/migrations/'+id+'/replicate',{});await refreshMig(id)}
  catch(e){alertModal({title:'Cannot '+(resume?'resume':'start')+' replication',html:esc(e.message),danger:true})}finally{busy(btn,false)}
}
// pauseReplication stops replication after any in-flight pass; data is kept and a
// later resume continues with an incremental delta (no full re-copy).
async function pauseReplication(id,btn){
  if(!await confirmModal({title:'Pause replication for #'+id+'?',
    html:'<div class="warn"><b>Replication will stop.</b></div>The agent stops sending data once any pass already in flight finishes. Already-replicated data and the change tracking are kept, so <b>Resume</b> later continues with an <b>incremental delta sync</b> — no full re-copy. (Cutover needs replication running and up to date, so resume before cutting over.)',
    okText:'Pause replication',okDanger:true}))return;
  busy(btn,true);
  try{await api('POST','/api/v1/migrations/'+id+'/pause',{});await refreshMig(id)}
  catch(e){alertModal({title:'Cannot pause replication',html:esc(e.message),danger:true})}finally{busy(btn,false)}
}
// connStatus renders the connection / replication indicator in the enroll panel:
// a green tick once every disk's agent has handshaked, a paused/running note, a
// soft "connection failed" after the post-install grace, or a waiting note. The
// Start/Pause/Resume buttons live in the action row, not here.
function connStatus(v,m){
  if(v.replication_active)
    return '<div class="resultbox ok" style="margin-top:10px">✔ Replication running — the agent is streaming changes.</div>';
  if(v.replication_paused)
    return '<div class="resultbox" style="margin-top:10px"><b>⏸ Replication paused.</b> Already-replicated data is kept; use <b>Resume replication</b> to continue with a delta sync.</div>';
  if(v.agent_connected)
    return '<div class="resultbox ok" style="margin-top:10px"><b>✔ Agent connected</b> on all disks — connection validated. Use <b>Start replication</b> to begin the initial full sync.</div>';
  if(v.connection_failed)
    return '<div class="resultbox bad" style="margin-top:10px"><b>✘ Connection failed.</b> The agent hasn’t checked in. Confirm the install command ran on <b>'+esc(m.source_hostname||'the source')+'</b>, and that the appliance’s receiver ports (TCP 5000–5100) are reachable. The agent retries every 60s, so this clears once it connects.</div>';
  return '<div class="resultbox" style="margin-top:10px">Waiting for the agent to connect (usually within ~60s of running the command above)…</div>';
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
// Cache the rendered (latest-5) activity-log lines per migration so the 5s
// auto-poll (which rebuilds the cards) can restore the log without a flicker.
const logCache={};
function ensureLog(id){if(logCache[id]===undefined)loadLog(id)}
// logLines renders events as plain text lines inside a <pre> ("HH:MM:SS
// message"), oldest first so the LATEST entry is at the bottom. Events arrive
// newest-first; with a limit we keep the latest N, then reverse for display.
function logLines(ev,limit){
  let rows=ev||[];
  if(limit&&rows.length>limit)rows=rows.slice(0,limit);
  rows=rows.slice().reverse(); // oldest → newest (latest at the bottom)
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

// syncPct is the TRUE initial-full-sync completion (0–100), or -1 when unknown.
// It uses only the live block-level percentage reported by the receiver during
// an active full-sync session. It does NOT derive a percentage from cumulative
// bytes-transferred, which counts re-sent/changed blocks and can exceed the disk
// size (that produced a misleading "99%" that never finished). When no session
// is reporting and the baseline isn't done, the progress is genuinely unknown.
function syncPct(v,m){
  const ds=disks(m);
  if(ds.length&&ds.every(d=>d.full_sync_done))return 100;
  if(v.percent_done>=0)return Math.max(0,Math.min(99.9,v.percent_done));
  return -1; // no live session reporting; unknown
}
function progBar(width,indet){
  return '<div class="prog'+(indet?' indet':'')+'"><div style="width:'+(indet?35:Math.round(width))+'%"></div></div>';
}
// replSpeed estimates copy throughput (bytes/sec). It prefers the backend's
// LIVE in-session rate (accurate while a full-sync session is actively
// transferring), and otherwise falls back to the change in total bytes received
// between polls, smoothed with an EMA (covers reconnecting/short sessions where
// bytes only update at session completion). Returns -1 until measurable.
const speedSamples={}; // id -> {bytes, t, ema}
function replSpeed(v,m){
  // Live in-session rate: bytes written this session / session elapsed.
  const tot=disks(m).reduce((a,d)=>a+(d.size_bytes||0),0);
  if(v.percent_done>=0 && v.elapsed_seconds>0 && tot>0) return v.percent_done/100*tot/v.elapsed_seconds;
  // Fallback: smoothed delta of total bytes received between polls.
  const now=Date.now(), bytes=bytesTotal(m), s=speedSamples[m.id];
  if(!s||bytes<s.bytes){speedSamples[m.id]={bytes,t:now,ema:-1};return -1;}
  const dt=(now-s.t)/1000;
  if(dt<4)return s.ema;            // don't resample faster than ~the poll interval
  const inst=Math.max(0,(bytes-s.bytes)/dt);
  s.ema = s.ema<0 ? inst : 0.5*inst+0.5*s.ema;
  s.bytes=bytes; s.t=now;
  return s.ema;
}
function progressLine(v,m){
  const st=m.state;let label,bar;
  if(st==='image_ready'||st==='launched'){label='completed in '+fmtDur(v.elapsed_seconds);bar=progBar(100,false);}
  else if(st==='failed'){label='failed';bar=progBar(0,false);}
  else if(st==='migrating'){label='finalizing · running '+liveDur(m.migrate_started,v.elapsed_seconds);bar=progBar(0,true);}
  else if(st==='awaiting_cutover'){label='step 1 done — power off the source, then Launch instance';bar=progBar(100,false);}
  else{
    const allBase=disks(m).length>0&&disks(m).every(d=>d.full_sync_done);
    if(allBase){label='initial sync completed · 100%';bar=progBar(100,false);}
    else{
      const pct=syncPct(v,m);
      if(pct<0){
        // No live session reporting and not yet baselined — show motion, not a
        // fabricated percentage.
        label=(st==='created'||st==='awaiting_agent'?'waiting for agent':'initial sync in progress');
        bar=progBar(0,true); // indeterminate
      }else{
        label='initial sync · '+pct.toFixed(1)+'%';
        if(v.eta_seconds>=0)label+=' · ~'+fmtDur(v.eta_seconds)+' left';
        bar=progBar(pct,false);
      }
    }
  }
  // Throughput line: total bytes transferred over the wire (counts re-sent and
  // changed blocks, so it can exceed the disk size) + current copy speed.
  const bps=replSpeed(v,m);
  // During the live initial sync, bytes_on_wire only updates when a pass finishes,
  // so estimate transferred from the live percent instead of showing 0 B.
  const totSz=disks(m).reduce((a,d)=>a+(d.size_bytes||0),0);
  let recv=fmtBytes(v.percent_done>=0&&totSz>0 ? v.percent_done/100*totSz : bytesTotal(m))+' transferred';
  if(bps>=0)recv+=' · '+fmtBytes(bps)+'/s';
  // label is built only from literals/numbers (no user data), so it's safe to
  // render as HTML — needed for the live-ticking elapsed span.
  return '<span class="muted">'+label+'</span>'+bar+
    '<div class="muted" style="font-size:12px;margin-top:3px">'+recv+'</div>';
}
// liveDur renders the elapsed time as a span that the 1s ticker keeps current
// between server refreshes, so the timer counts up smoothly.
function liveDur(sinceISO,fallbackSecs){
  const t=Date.parse(sinceISO||'');
  if(isNaN(t)||t<=0)return fmtDur(fallbackSecs);
  return '<span class="livedur" data-since="'+t+'">'+fmtDur((Date.now()-t)/1000)+'</span>';
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
function stateLabel(s){return ({created:'created',awaiting_agent:'waiting for agent',replicating:'replicating',ready:'ready to cut over',awaiting_cutover:'power off source, then launch',migrating:'finalizing',image_ready:'image ready',launched:'launched',failed:'failed'})[s]||s}
// pillFor renders the header status pill. Before replication starts (state still
// awaiting_agent/created) it reflects the gated-start connection signals so the
// operator sees connect → start at a glance; otherwise it uses the migration state.
function pillFor(v,m){
  // Paused takes precedence at any replication-phase state.
  if(v.replication_paused && ['created','awaiting_agent','replicating','ready'].includes(m.state))
    return '<span class="pill warn">paused</span>';
  if((m.state==='awaiting_agent'||m.state==='created') && !v.replication_started){
    if(v.agent_connected)return '<span class="pill ok">agent connected</span>';
    if(v.connection_failed)return '<span class="pill bad">connection failed</span>';
    return '<span class="pill warn">waiting for agent</span>';
  }
  if((m.state==='awaiting_agent'||m.state==='created') && v.replication_started)
    return '<span class="pill warn">starting replication</span>';
  return '<span class="pill '+stateClass(m.state)+'">'+esc(stateLabel(m.state))+'</span>';
}
// statusLegend renders a hover legend that shows each status as its ACTUAL
// colored pill (so you can match what you see in the table to its meaning).
const STATE_DESCS=[['awaiting_agent','enrolled; the agent has not connected yet'],
  ['replicating','agent connected; copying the baseline, then ongoing changes'],
  ['ready','baseline done and lag is low — safe to cut over'],
  ['awaiting_cutover','guided cutover: replication stopped & image frozen — power off the source, then Launch instance'],
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
  migMeta[m.id]={uninstall:v.uninstall_cmd||'',source:m.source_hostname||'',name:m.name,boot_target:m.boot_target,plan_class:m.plan_class,linode_type:m.linode_type};
  const collapsed=collapsedMigs.has(m.id);
  const firstSeen=!seenMigs.has(m.id);seenMigs.add(m.id);

  // Header: collapse chevron + per-migration refresh (acts on this card only).
  let h='<div class="mighead">'+
    '<button class="chev" title="'+(collapsed?'Expand':'Collapse')+'" onclick="toggleCollapse('+m.id+',this)">'+(collapsed?'▸':'▾')+'</button>'+
    '<span class="muted" style="font-size:12px;flex:1">'+(collapsed?'collapsed — click ▸ to expand':'')+'</span>'+
    '<button class="mini" title="Refresh this migration" onclick="refreshMig('+m.id+',this)">↻ Refresh</button></div>';

  // Boot-mode header banner: distinct colours so the mode is obvious at a glance
  // (green = Linode local disk, blue = separate Block Storage volume).
  h+=(m.boot_target==='disk')
    ? '<div class="banner" style="border-color:#cde8d8;background:#f1faf4;color:#0f5c30;margin:0 0 10px"><b>Boot: Linode local disk</b>'+((m.linode_type)?(' — '+esc((m.plan_class||'')+' plan '+m.linode_type)):'')+'</div>'
    : '<div class="banner" style="border-color:#cdd6e8;background:#f3f6fc;color:#22408a;margin:0 0 10px"><b>Boot: separate Block Storage volume</b>'+((m.linode_type)?(' — plan '+esc(m.linode_type)):'')+'</div>';

  h+='<table style="margin-bottom:4px"><tr><th>Migration</th><th>Source &rarr; Appliance'+statusLegend()+'</th><th>Disks</th><th>Progress</th><th>RPO</th></tr><tr>'+
    '<td><b>#'+m.id+'</b> '+esc(m.name)+'<br><span class="muted">'+esc(m.source_ip||m.source_hostname||'-')+'</span></td>'+
    '<td>'+pillFor(v,m)+'</td>'+
    '<td class="muted">'+disks(m).length+' disk(s)<br>'+(allDone(m)?'baseline done':'baselining')+'</td>'+
    '<td id="prog'+m.id+'">'+progressLine(v,m)+'</td>'+
    '<td class="muted">'+(v.rpo_seconds?Math.round(v.rpo_seconds)+'s':'—')+'</td></tr></table>';

  // Everything below the status table is hidden when the card is collapsed.
  let b='';
  if(m.last_error)b+='<div class="resultbox bad">'+esc(m.last_error)+'</div>';
  else if(err)b+='<div class="resultbox bad">Last replication attempt failed: '+esc(err)+'</div>';

  if(['image_ready','launched'].includes(m.state) && m.boot_target==='disk'){
    b+='<div class="banner">✔ <b>Migration completed.</b> '+
       (m.launched_linode_id?('Launched Linode '+esc(m.launched_linode_id)+' booting from its <b>local disk</b> ('+esc(m.linode_type||'plan')+') — see <a href="https://cloud.linode.com/linodes" target="_blank" rel="noopener">your Linodes</a> and connect via Lish. No separate volume is kept.')
       :'The image is ready to boot from the instance’s local disk.')+'</div>';
  } else if(['image_ready','launched'].includes(m.state)){
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
       connStatus(v,m)+
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
  }else if(m.state==='awaiting_cutover'){
    // Guided cutover: phase 1 captured a consistent image and froze it (receivers
    // stopped). Now the operator powers off the source and confirms.
    b+='<div class="warn" style="font-size:12px;margin-bottom:6px">✓ <b>Step 1 done</b> — replication is stopped and the current copy is frozen as the image. <b>Step 2:</b> <b>power off the source server now</b> (so the old and new machines aren’t both running), then click <b>Launch instance</b>.</div>';
    b+='<button class="primary" onclick="completeCutover('+m.id+',this)">Launch instance</button>'+
      infoIcon('Converts the frozen image, clones the disk(s), and launches the new Linode. Power off the source first. Final step.')+
      '<button class="danger" onclick="stopMig('+m.id+',this)">Cancel</button>';
  }else if(m.state==='failed' && allDone(m)){
    // A cutover failed but the data is fully replicated — retry re-runs it.
    b+='<button class="primary" onclick="startMig('+m.id+',this)">Retry cutover</button>'+
      infoIcon('Re-runs the cutover on the data already replicated to this appliance. It first removes any half-built <name>-cutover instance/volumes from the failed attempt, then launches fresh — no re-replication of the source is needed.');
  }else{
    // Replication controls (start / pause / resume) precede the cutover button,
    // but only during the replication phase.
    const ctrl=['created','awaiting_agent','replicating','ready'].includes(m.state);
    if(ctrl && !v.replication_started){
      b+='<button class="primary"'+(v.can_replicate?'':' disabled title="Waiting for the agent connection to be validated"')+' onclick="startReplication('+m.id+',this,false)">Start replication</button>'+
        infoIcon('Replication does not start automatically. Once the agent connection shows a green tick, this begins the initial full sync.');
    }else if(ctrl && v.replication_active){
      b+='<button class="danger" onclick="pauseReplication('+m.id+',this)">Pause replication</button>'+
        infoIcon('Stops sending data after any in-flight pass finishes. Already-replicated data is kept; resume continues with an incremental delta — no full re-copy.');
    }else if(ctrl && v.replication_paused){
      b+='<button class="primary"'+(v.can_replicate?'':' disabled title="Waiting for the agent connection"')+' onclick="startReplication('+m.id+',this,true)">Resume replication</button>'+
        infoIcon('Continues replication with an incremental delta sync — only the blocks changed during the pause are sent.');
    }
    // Readiness is auto-computed: the Cutover button enables itself once the
    // initial full sync is complete (no manual assessment).
    const ready=v.can_migrate;
    b+='<button class="primary"'+(ready?'':' disabled title="The initial full sync must complete on all disks first"')+' onclick="startMig('+m.id+',this)">Cutover instance</button>'+
      infoIcon('Cuts over to Linode: stops replication, converts the boot disk, clones every disk into <name>-cutover volumes, and (optionally) launches a <name>-cutover Linode. Enables automatically once the initial full sync completes.');
  }
  b+='<span style="flex:1"></span><button class="danger" onclick="deleteMig('+m.id+',\''+esc(m.name)+'\',this)">Delete</button></div>';

  h+='<div class="migbody">'+b+'</div>';
  const card=document.createElement('div');card.className='mig'+(collapsed?' collapsed':'');card.id='mig'+m.id;card.innerHTML=h;
  // Mark cards whose progress is actively moving (initial sync streaming) so the
  // 1s poller can update just those in place for a live progress bar.
  card.dataset.live=(v.percent_done>=0)?'1':'';
  return card;
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

async function start(){show('app');if(!document.querySelector('#disks .row'))addDisk();bootTargetChanged();refresh(false);}
async function init(){
  try{await api('GET','/api/v1/session');start();
    setInterval(()=>{if(!$('app').classList.contains('hide'))refresh(false)},5000);
    // Tick visible elapsed timers every second between server refreshes.
    setInterval(()=>{document.querySelectorAll('.livedur[data-since]').forEach(s=>{const t=+s.dataset.since;if(t)s.textContent=fmtDur((Date.now()-t)/1000)})},1000);
    // Live progress: every second, update just the progress cell of migrations
    // whose initial sync is actively streaming (data-live) — so the bar, %, ETA,
    // transferred and speed move smoothly without waiting for the 5s full refresh.
    // Surgical: one GET per live migration, replacing only the progress <td> — no
    // card rebuild, no log/settings reload, so it never disrupts the rest of the UI.
    setInterval(()=>{
      if($('app').classList.contains('hide'))return;
      document.querySelectorAll('#migs .mig[data-live="1"]').forEach(card=>{
        const id=card.id.slice(3);
        api('GET','/api/v1/migrations/'+id).then(v=>{
          const td=card.querySelector('#prog'+id);
          if(td)td.innerHTML=progressLine(v,v.migration);
          card.dataset.live=(v.percent_done>=0)?'1':'';
        }).catch(()=>{});
      });
    },1000);}
  catch(e){show('login')}
}
init();
</script>
</body></html>`
