package controlplane

import "net/http"

// handleDashboard serves a self-contained single-page dashboard. It prompts for
// the API token once (stored in localStorage) and polls /api/v1/status to show
// each job's replication lag (RPO), last sync, and throughput.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>vm-replication · control plane</title>
<style>
  :root{--bg:#0a0c10;--surface:#111318;--border:#2a2d36;--accent:#00e5ff;--green:#22c55e;--yellow:#eab308;--red:#ef4444;--text:#e2e8f0;--muted:#64748b}
  *{margin:0;padding:0;box-sizing:border-box}
  body{background:var(--bg);color:var(--text);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;padding:32px}
  h1{font-size:22px;font-weight:700;letter-spacing:-.02em}
  h1 span{color:var(--accent)}
  .sub{color:var(--muted);font-size:12px;margin:6px 0 24px}
  .bar{display:flex;gap:12px;align-items:center;margin-bottom:20px;flex-wrap:wrap}
  button{background:var(--surface);color:var(--text);border:1px solid var(--border);padding:7px 12px;border-radius:6px;cursor:pointer;font:inherit;font-size:12px}
  button:hover{border-color:var(--accent)}
  table{width:100%;border-collapse:collapse;font-size:13px}
  th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--border)}
  th{color:var(--muted);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:.08em}
  tr:hover td{background:#0e1116}
  .pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:11px;border:1px solid}
  .ok{color:var(--green);border-color:rgba(34,197,94,.4)}
  .warn{color:var(--yellow);border-color:rgba(234,179,8,.4)}
  .bad{color:var(--red);border-color:rgba(239,68,68,.4)}
  .muted{color:var(--muted)}
  .dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
  .empty{color:var(--muted);padding:40px;text-align:center;border:1px dashed var(--border);border-radius:8px;margin-top:12px}
  code{color:var(--accent)}
</style></head>
<body>
  <h1>vm-<span>replication</span> · control plane</h1>
  <div class="sub">Continuous block-level Linux→Linux replication to Akamai Cloud (Linode)</div>
  <div class="bar">
    <button onclick="setToken()">Set API token</button>
    <button onclick="refresh()">Refresh now</button>
    <span class="muted" id="updated"></span>
  </div>
  <div id="content"><div class="empty">Loading…</div></div>

<script>
const tokenKey='vmrepl_token';
function token(){return localStorage.getItem(tokenKey)||''}
function setToken(){const t=prompt('Control plane API token:',token());if(t!==null){localStorage.setItem(tokenKey,t);refresh()}}
function fmtAge(s){if(s<0)s=0;if(s<60)return s.toFixed(0)+'s';if(s<3600)return (s/60).toFixed(1)+'m';if(s<86400)return (s/3600).toFixed(1)+'h';return (s/86400).toFixed(1)+'d'}
function fmtBytes(n){if(!n)return '0 B';const u=['B','KiB','MiB','GiB','TiB'];let i=0;while(n>=1024&&i<u.length-1){n/=1024;i++}return n.toFixed(1)+' '+u[i]}
function rpoClass(st){if(st.rpo_breached)return 'bad';if(st.last_ok_sync&&st.rpo_seconds>(st.job.rpo_target_seconds||60)*0.7)return 'warn';return 'ok'}
function stateClass(s){return s==='active'?'ok':s==='failed'?'bad':s==='done'?'ok':'warn'}

async function refresh(){
  try{
    const res=await fetch('/api/v1/status',{headers:{'Authorization':'Bearer '+token()}});
    if(res.status===401){document.getElementById('content').innerHTML='<div class="empty">Unauthorized — click <b>Set API token</b>.</div>';return}
    const data=await res.json();
    render(data);
    document.getElementById('updated').textContent='updated '+new Date().toLocaleTimeString();
  }catch(e){document.getElementById('content').innerHTML='<div class="empty">Error: '+e+'</div>'}
}
function render(rows){
  if(!rows||!rows.length){document.getElementById('content').innerHTML='<div class="empty">No jobs yet. Register servers and create a job via the API or <code>replctl</code>.</div>';return}
  let h='<table><thead><tr><th>Job</th><th>State</th><th>Source → Target</th><th>RPO (lag)</th><th>Last sync</th><th>Δ blocks</th><th>On wire</th><th>Syncs</th></tr></thead><tbody>';
  for(const st of rows){
    const j=st.job;
    const src=st.source?st.source.name:(j.source_server_id||'—');
    const tgt=st.target?st.target.name:(j.target_addr||'—');
    const last=st.last_sync;
    const lastTxt=last?(new Date(last.finished_at).toLocaleString()+' ('+last.mode+')'):'never';
    h+='<tr>'+
      '<td><b>'+esc(j.name)+'</b></td>'+
      '<td><span class="pill '+stateClass(j.state)+'">'+j.state+'</span></td>'+
      '<td class="muted">'+esc(String(src))+' → '+esc(String(tgt))+'</td>'+
      '<td><span class="pill '+rpoClass(st)+'">'+(st.last_ok_sync?fmtAge(st.rpo_seconds):'—')+(j.rpo_target_seconds?(' / '+j.rpo_target_seconds+'s'):'')+'</span></td>'+
      '<td class="muted">'+lastTxt+(last&&!last.ok?' <span class="bad">FAILED</span>':'')+'</td>'+
      '<td>'+(last?last.changed_blocks+'/'+last.total_blocks:'—')+'</td>'+
      '<td>'+(last?fmtBytes(last.bytes_on_wire):'—')+'</td>'+
      '<td>'+st.total_syncs+'</td>'+
      '</tr>';
  }
  h+='</tbody></table>';
  document.getElementById('content').innerHTML=h;
}
function esc(s){return String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
refresh();setInterval(refresh,5000);
</script>
</body></html>`
