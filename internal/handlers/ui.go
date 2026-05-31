package handlers

import (
	"net/http"
)

// HandleUI serves the log viewer web interface.
func (h *Handler) HandleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(logViewerHTML))
}

const logViewerHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Service Log Viewer</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  :root {
    --bg:      #0d1117; --surface: #161b22; --border: #30363d;
    --text:    #e6edf3; --muted:   #8b949e; --accent:  #58a6ff;
    --green:   #3fb950; --yellow:  #d29922; --red:     #f85149;
    --orange:  #f0883e;
    --purple:  #bc8cff;
  }
  html, body { height: 100%; background: var(--bg); color: var(--text); font-family: 'Inter', sans-serif; font-size: 14px; }

  /* ── Tabs ── */
  .tabs { display: flex; gap: 2px; padding: 8px 16px 0; background: var(--surface); border-bottom: 1px solid var(--border); }
  .tab {
    padding: 8px 18px; cursor: pointer; font-size: 13px; font-weight: 500;
    color: var(--muted); border-radius: 6px 6px 0 0; border: 1px solid transparent;
    border-bottom: none; transition: all .15s;
  }
  .tab:hover { color: var(--text); background: rgba(255,255,255,.04); }
  .tab.active { color: var(--text); background: var(--bg); border-color: var(--border); }

  /* ── Page ── */
  .page { display: none; height: calc(100vh - 42px); }
  .page.active { display: flex; }

  /* ── Layout ── */
  .layout { display: flex; width: 100%; overflow: hidden; }
  .sidebar {
    width: 220px; flex-shrink: 0; background: var(--surface);
    border-right: 1px solid var(--border); display: flex; flex-direction: column; padding: 16px 0;
  }
  .sidebar-title { font-size: 11px; font-weight: 600; letter-spacing: .08em; color: var(--muted); text-transform: uppercase; padding: 0 16px 12px; }
  .file-item {
    padding: 8px 16px; cursor: pointer; font-family: 'JetBrains Mono', monospace; font-size: 12px;
    color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
    border-left: 2px solid transparent; transition: all .15s;
  }
  .file-item:hover { color: var(--text); background: rgba(88,166,255,.06); }
  .file-item.active { color: var(--accent); border-left-color: var(--accent); background: rgba(88,166,255,.08); }

  .main { flex: 1; display: flex; flex-direction: column; overflow: hidden; }
  .header {
    padding: 10px 20px; border-bottom: 1px solid var(--border); background: var(--surface);
    display: flex; align-items: center; gap: 12px;
  }
  .header-title { font-weight: 600; font-size: 15px; flex: 1; }
  .header-file { font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--muted); }
  .badge { padding: 2px 8px; border-radius: 20px; font-size: 11px; font-weight: 500; display: flex; align-items: center; gap: 5px; }
  .badge.live    { background: rgba(63,185,80,.15);  color: var(--green);  border: 1px solid rgba(63,185,80,.3); }
  .badge.offline { background: rgba(248,81,73,.15);  color: var(--red);    border: 1px solid rgba(248,81,73,.3); }
  .badge.waiting { background: rgba(210,153,34,.15); color: var(--yellow); border: 1px solid rgba(210,153,34,.3); }
  .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; animation: pulse 1.5s infinite; }
  @keyframes pulse { 0%,100%{opacity:1}50%{opacity:.3} }
  .badge.offline .dot, .badge.waiting .dot { animation: none; }
  .btn { padding: 5px 12px; border-radius: 6px; border: 1px solid var(--border); background: transparent; color: var(--text); font-size: 12px; cursor: pointer; transition: all .15s; font-family: inherit; }
  .btn:hover { background: rgba(255,255,255,.06); border-color: var(--accent); color: var(--accent); }

  .log-wrap { flex: 1; overflow-y: auto; padding: 12px 0; }
  .log-line { display: flex; font-family: 'JetBrains Mono', monospace; font-size: 12.5px; line-height: 1.65; padding: 1px 20px; transition: background .1s; }
  .log-line:hover { background: rgba(255,255,255,.03); }
  .log-num { color: var(--border); min-width: 40px; user-select: none; text-align: right; padding-right: 16px; flex-shrink: 0; }
  .log-text { flex: 1; white-space: pre-wrap; word-break: break-all; }
  .log-text.lvl-ok    { color: #3fb950; }
  .log-text.lvl-warn  { color: #d29922; }
  .log-text.lvl-err   { color: #f85149; }
  .log-text.lvl-info  { color: var(--text); }
  .log-text.lvl-muted { color: var(--muted); }

  .footer { border-top: 1px solid var(--border); padding: 6px 20px; font-size: 11px; color: var(--muted); display: flex; gap: 16px; align-items: center; }

  /* ── Hetzner Panel ── */
  .htz-page { width: 100%; padding: 24px; overflow-y: auto; }
  .htz-header { display: flex; align-items: center; gap: 12px; margin-bottom: 20px; }
  .htz-title { font-size: 18px; font-weight: 600; }
  .server-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(340px, 1fr)); gap: 16px; }
  .server-card {
    background: var(--surface); border: 1px solid var(--border); border-radius: 10px;
    padding: 18px; display: flex; flex-direction: column; gap: 12px;
  }
  .server-card-head { display: flex; align-items: center; gap: 10px; }
  .server-name { font-weight: 600; font-size: 14px; flex: 1; font-family: 'JetBrains Mono', monospace; }
  .status-pill { padding: 2px 10px; border-radius: 20px; font-size: 11px; font-weight: 500; }
  .status-running      { background: rgba(63,185,80,.15);  color: var(--green);  border: 1px solid rgba(63,185,80,.3); }
  .status-initializing { background: rgba(210,153,34,.15); color: var(--yellow); border: 1px solid rgba(210,153,34,.3); }
  .status-off          { background: rgba(248,81,73,.15);  color: var(--red);    border: 1px solid rgba(248,81,73,.3); }
  .server-meta { font-size: 12px; color: var(--muted); display: flex; flex-direction: column; gap: 4px; }
  .server-meta span { display: flex; gap: 8px; }
  .server-meta .label { min-width: 56px; }
  .server-actions { display: flex; gap: 8px; }
  .btn-sm { padding: 4px 10px; font-size: 12px; border-radius: 6px; border: 1px solid var(--border); background: transparent; color: var(--text); cursor: pointer; font-family: inherit; transition: all .15s; }
  .btn-sm:hover { background: rgba(88,166,255,.1); border-color: var(--accent); color: var(--accent); }

  /* ── Worker Panel ── */
  .worker-stats { display: flex; gap: 16px; margin-bottom: 20px; flex-wrap: wrap; }
  .worker-stat {
    background: var(--surface); border: 1px solid var(--border); border-radius: 10px;
    padding: 14px 20px; display: flex; flex-direction: column; gap: 4px; min-width: 120px;
  }
  .worker-stat-label { font-size: 11px; color: var(--muted); text-transform: uppercase; letter-spacing: .06em; }
  .worker-stat-value { font-size: 22px; font-weight: 600; }
  .worker-card-disabled { opacity: .5; }
  .progress-bar { height: 6px; background: var(--border); border-radius: 3px; overflow: hidden; margin-top: 2px; }
  .progress-fill { height: 100%; border-radius: 3px; transition: width .3s; }
  .progress-fill.green  { background: var(--green); }
  .progress-fill.yellow { background: var(--yellow); }
  .progress-fill.red    { background: var(--red); }
  .worker-tag { display: inline-flex; align-items: center; gap: 4px; padding: 2px 8px; border-radius: 4px; font-size: 11px; font-weight: 500; }
  .worker-tag.enabled  { background: rgba(63,185,80,.12); color: var(--green); }
  .worker-tag.disabled { background: rgba(248,81,73,.12); color: var(--red); }
  .worker-tag.type-download  { background: rgba(88,166,255,.12); color: var(--accent); }
  .worker-tag.type-transcode { background: rgba(188,140,255,.12); color: var(--purple); }

  /* ── Filter Bar ── */
  .filter-bar { display: flex; gap: 8px; margin-bottom: 16px; align-items: center; }
  .btn-filter {
    padding: 6px 14px; border-radius: 20px; border: 1px solid var(--border);
    background: transparent; color: var(--muted); font-size: 12px; cursor: pointer;
    transition: all .15s; font-family: inherit; font-weight: 500;
  }
  .btn-filter:hover { background: rgba(255,255,255,.04); color: var(--text); border-color: var(--muted); }
  .btn-filter.active { background: rgba(88,166,255,.1); color: var(--accent); border-color: var(--accent); }

  /* Log modal */
  .modal-overlay { position: fixed; inset: 0; background: rgba(0,0,0,.6); z-index: 100; display: none; align-items: center; justify-content: center; }
  .modal-overlay.open { display: flex; }
  .modal { background: var(--surface); border: 1px solid var(--border); border-radius: 12px; width: 90vw; max-width: 900px; max-height: 85vh; display: flex; flex-direction: column; }
  .modal-header { padding: 14px 20px; border-bottom: 1px solid var(--border); display: flex; align-items: center; gap: 10px; }
  .modal-title { font-weight: 600; flex: 1; font-family: 'JetBrains Mono', monospace; font-size: 13px; }
  .modal-close { cursor: pointer; color: var(--muted); font-size: 18px; background: none; border: none; color: var(--muted); }
  .modal-close:hover { color: var(--text); }
  .modal-body { flex: 1; overflow-y: auto; padding: 16px 20px; font-family: 'JetBrains Mono', monospace; font-size: 12px; line-height: 1.7; white-space: pre-wrap; word-break: break-all; color: var(--text); }
  .modal-loading { color: var(--muted); text-align: center; padding: 40px; }

  .empty-state { color: var(--muted); text-align: center; padding: 80px 20px; font-size: 14px; }

  ::-webkit-scrollbar { width: 6px; }
  ::-webkit-scrollbar-track { background: transparent; }
  ::-webkit-scrollbar-thumb { background: var(--border); border-radius: 3px; }
  ::-webkit-scrollbar-thumb:hover { background: var(--muted); }
</style>
</head>
<body>

<!-- Tabs -->
<div class="tabs">
  <div class="tab active" onclick="switchTab('logs')">📋 Logs</div>
  <div class="tab" onclick="switchTab('workers')">👷 Workers</div>
  <div class="tab" onclick="switchTab('hetzner')">🖥️ Hetzner</div>
</div>

<!-- ── Logs Page ── -->
<div class="page active" id="page-logs">
  <div class="layout">
    <div class="sidebar">
      <div class="sidebar-title">Log Files</div>
      <div id="file-list"></div>
    </div>
    <div class="main">
      <div class="header">
        <div class="header-title">📋 Log Viewer</div>
        <div class="header-file" id="current-file">—</div>
        <div class="badge waiting" id="ws-badge"><span class="dot"></span><span id="ws-label">Connecting...</span></div>
        <button class="btn" onclick="reconnect()">↺ Reconnect</button>
      </div>
      <div class="log-wrap" id="log-wrap">
        <div style="padding:40px 20px;color:var(--muted);font-family:'JetBrains Mono',monospace;font-size:13px;">Select a log file from the sidebar...</div>
      </div>
      <div class="footer">
        <span id="line-count">—</span>
        <span id="last-update">—</span>
      </div>
    </div>
  </div>
</div>

<!-- ── Workers Page ── -->
<div class="page" id="page-workers">
  <div class="htz-page">
    <div class="htz-header">
      <div class="htz-title">👷 Workers</div>
      <button class="btn" onclick="loadWorkers()">↺ Refresh</button>
    </div>
    <div class="filter-bar">
      <button class="btn-filter active" data-filter="all" onclick="setWorkerFilter('all')">All Workers</button>
      <button class="btn-filter" data-filter="download" onclick="setWorkerFilter('download')">⬇️ Downloads</button>
      <button class="btn-filter" data-filter="transcode" onclick="setWorkerFilter('transcode')">⚙️ Transcodes</button>
    </div>
    <div id="worker-stats" class="worker-stats"></div>
    <div id="worker-grid" class="server-grid">
      <div class="empty-state">Loading...</div>
    </div>
  </div>
</div>

<!-- ── Hetzner Page ── -->
<div class="page" id="page-hetzner">
  <div class="htz-page">
    <div class="htz-header">
      <div class="htz-title">🖥️ Hetzner Servers</div>
      <button class="btn" onclick="loadHetznerServers()">↺ Refresh</button>
    </div>
    <div id="server-grid" class="server-grid">
      <div class="empty-state">Loading...</div>
    </div>
  </div>
</div>

<!-- Log Modal -->
<div class="modal-overlay" id="log-modal" onclick="closeModal(event)">
  <div class="modal">
    <div class="modal-header">
      <div class="modal-title" id="modal-title">cloud-init-output.log</div>
      <button class="modal-close" onclick="document.getElementById('log-modal').classList.remove('open')">✕</button>
    </div>
    <div class="modal-body" id="modal-body"><div class="modal-loading">Loading...</div></div>
  </div>
</div>

<script>
// ── Tab switching ───────────────────────────────────────────────────────
function switchTab(tab) {
  const tabs = ['logs','workers','hetzner'];
  document.querySelectorAll('.tab').forEach((el, i) => el.classList.toggle('active', tabs[i] === tab));
  document.querySelectorAll('.page').forEach(el => el.classList.remove('active'));
  document.getElementById('page-' + tab).classList.add('active');
  if (tab === 'workers') loadWorkers();
  if (tab === 'hetzner') loadHetznerServers();
}

// ── WebSocket (Logs tab) ────────────────────────────────────────────────
let ws = null, currentRoom = null;
function connect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(proto + '//' + location.host + '/ws');
  ws.onopen = () => { setStatus('live', 'Live'); if (currentRoom) subscribe(currentRoom); };
  ws.onclose = () => { setStatus('offline', 'Disconnected'); setTimeout(connect, 3000); };
  ws.onerror = () => setStatus('offline', 'Error');
  ws.onmessage = (e) => {
    const msg = JSON.parse(e.data);
    if (msg.type === 'files') renderFileList(msg.files || []);
    if (msg.type === 'log' && msg.room === currentRoom) renderLog(msg.lines || [], msg.total || 0);
  };
}
function reconnect() { if (ws) ws.close(); }
function subscribe(filename) {
  currentRoom = filename;
  document.getElementById('current-file').textContent = filename;
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'subscribe', room: filename }));
}
function setStatus(cls, label) {
  document.getElementById('ws-badge').className = 'badge ' + cls;
  document.getElementById('ws-label').textContent = label;
}
function renderFileList(files) {
  const list = document.getElementById('file-list');
  const prev = currentRoom;
  list.innerHTML = '';
  (files || []).forEach((f, i) => {
    const el = document.createElement('div');
    el.className = 'file-item' + (currentRoom === f.name ? ' active' : '');
    el.textContent = f.name;
    el.onclick = () => {
      document.querySelectorAll('.file-item').forEach(x => x.classList.remove('active'));
      el.classList.add('active');
      subscribe(f.name);
    };
    list.appendChild(el);
    if (i === 0 && !prev) subscribe(f.name);
  });
}
function renderLog(lines, total) {
  if (!lines.length) return;
  const wrap = document.getElementById('log-wrap');
  const atTop = wrap.scrollTop < 20;
  wrap.innerHTML = lines.map((line, i) =>
    '<div class="log-line"><span class="log-num">' + (total-i) + '</span><span class="log-text ' + classify(line) + '">' + escHtml(line) + '</span></div>'
  ).join('');
  if (atTop) wrap.scrollTop = 0;
  document.getElementById('line-count').textContent = total + ' total, showing ' + lines.length;
  document.getElementById('last-update').textContent = 'Updated ' + new Date().toLocaleTimeString();
}
function classify(line) {
  if (/✅|→ active|connected|ensured|successful/i.test(line)) return 'lvl-ok';
  if (/⚠️|warn|failed|skip/i.test(line)) return 'lvl-warn';
  if (/❌|error|fatal/i.test(line)) return 'lvl-err';
  if (/⏳|pending|mismatch/i.test(line)) return 'lvl-muted';
  return 'lvl-info';
}
function escHtml(s) { return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

// ── Hetzner tab ─────────────────────────────────────────────────────────
async function loadHetznerServers() {
  const grid = document.getElementById('server-grid');
  grid.innerHTML = '<div class="empty-state">Loading...</div>';
  try {
    const res = await fetch('/hetzner/servers');
    const data = await res.json();
    const servers = data.servers || [];
    if (!servers.length) {
      grid.innerHTML = '<div class="empty-state">No managed servers running</div>';
      return;
    }
    grid.innerHTML = servers.map(s => {
      const statusCls = s.status === 'running' ? 'status-running' : s.status === 'initializing' ? 'status-initializing' : 'status-off';
      const created = new Date(s.created).toLocaleString();
      return ` + "`" + `
        <div class="server-card">
          <div class="server-card-head">
            <div class="server-name">${escHtml(s.name)}</div>
            <div class="status-pill ${statusCls}">${s.status}</div>
          </div>
          <div class="server-meta">
            <span><span class="label">IP</span><strong>${s.ip || '—'}</strong></span>
            <span><span class="label">ID</span>${s.id}</span>
            <span><span class="label">Created</span>${created}</span>
          </div>
          <div class="server-actions">
            <button class="btn-sm" onclick="openLog('${s.ip}', '${escHtml(s.name)}')">📋 View Install Log</button>
            <button class="btn-sm" onclick="copySSH('${s.ip}')">🔑 Copy SSH</button>
          </div>
        </div>` + "`" + `
    }).join('');
  } catch(e) {
    grid.innerHTML = '<div class="empty-state" style="color:var(--red)">Failed to load: ' + e.message + '</div>';
  }
}

async function openLog(ip, name) {
  const modal = document.getElementById('log-modal');
  const body = document.getElementById('modal-body');
  document.getElementById('modal-title').textContent = name + ' — cloud-init-output.log';
  body.innerHTML = '<div class="modal-loading">Connecting via SSH...</div>';
  modal.classList.add('open');
  try {
    const res = await fetch('/hetzner/log?ip=' + encodeURIComponent(ip));
    if (!res.ok) {
      const err = await res.json().catch(() => ({error: res.statusText}));
      body.textContent = '❌ ' + (err.error || res.statusText);
      return;
    }
    const text = await res.text();
    body.textContent = text || '(empty log)';
    body.scrollTop = body.scrollHeight; // scroll to bottom (newest)
  } catch(e) {
    body.textContent = '❌ ' + e.message;
  }
}

function copySSH(ip) {
  navigator.clipboard.writeText('ssh root@' + ip).then(() => {
    alert('Copied: ssh root@' + ip);
  });
}

function closeModal(e) {
  if (e.target === document.getElementById('log-modal')) {
    document.getElementById('log-modal').classList.remove('open');
  }
}

function fmtSize(b) {
  if (!b) return '0 B';
  if (b < 1024) return b + ' B';
  if (b < 1024*1024) return (b/1024).toFixed(1) + ' KB';
  if (b < 1024*1024*1024) return (b/1024/1024).toFixed(1) + ' MB';
  return (b/1024/1024/1024).toFixed(1) + ' GB';
}

function fmtAgo(iso) {
  const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.floor(s/60) + 'm ago';
  return Math.floor(s/3600) + 'h ago';
}

function pctColor(pct) {
  if (pct >= 90) return 'red';
  if (pct >= 70) return 'yellow';
  return 'green';
}

// ── Workers tab ─────────────────────────────────────────────────────────
let cachedWorkers = [];
let currentWorkerFilter = 'all';

async function loadWorkers() {
  const grid = document.getElementById('worker-grid');
  grid.innerHTML = '<div class="empty-state">Loading...</div>';
  try {
    const res = await fetch('/workers');
    const data = await res.json();
    cachedWorkers = data.workers || [];
    renderWorkers();
  } catch(e) {
    grid.innerHTML = '<div class="empty-state" style="color:var(--red)">Failed to load: ' + e.message + '</div>';
  }
}

function setWorkerFilter(filter) {
  currentWorkerFilter = filter;
  document.querySelectorAll('.btn-filter').forEach(btn => {
    btn.classList.toggle('active', btn.getAttribute('data-filter') === filter);
  });
  renderWorkers();
}

function renderWorkers() {
  const grid = document.getElementById('worker-grid');
  const stats = document.getElementById('worker-stats');
  
  if (!cachedWorkers.length) {
    grid.innerHTML = '<div class="empty-state">No workers registered</div>';
    stats.innerHTML = '';
    return;
  }

  // Calculate stats using ALL workers
  const online = cachedWorkers.filter(w => w.isOnline).length;
  const busy = cachedWorkers.filter(w => w.isOnline && w.status === 'busy').length;
  const idle = cachedWorkers.filter(w => w.isOnline && w.status === 'idle').length;
  const offlineN = cachedWorkers.filter(w => !w.isOnline).length;
  
  const downloadCount = cachedWorkers.filter(w => w.type === 'download' || !w.type).length;
  const transcodeCount = cachedWorkers.filter(w => w.type === 'transcode').length;

  stats.innerHTML =
    '<div class="worker-stat"><div class="worker-stat-label">Total</div><div class="worker-stat-value">' + cachedWorkers.length + '</div></div>' +
    '<div class="worker-stat"><div class="worker-stat-label">Online</div><div class="worker-stat-value" style="color:var(--green)">' + online + '</div></div>' +
    '<div class="worker-stat"><div class="worker-stat-label">Busy</div><div class="worker-stat-value" style="color:var(--yellow)">' + busy + '</div></div>' +
    '<div class="worker-stat"><div class="worker-stat-label">Idle</div><div class="worker-stat-value" style="color:var(--accent)">' + idle + '</div></div>' +
    '<div class="worker-stat"><div class="worker-stat-label">Offline</div><div class="worker-stat-value" style="color:var(--red)">' + offlineN + '</div></div>' +
    '<div class="worker-stat"><div class="worker-stat-label">Downloads</div><div class="worker-stat-value" style="color:var(--accent)">' + downloadCount + '</div></div>' +
    '<div class="worker-stat"><div class="worker-stat-label">Transcodes</div><div class="worker-stat-value" style="color:var(--purple)">' + transcodeCount + '</div></div>';

  // Filter workers to display
  let filtered = cachedWorkers;
  if (currentWorkerFilter !== 'all') {
    filtered = cachedWorkers.filter(w => w.type === currentWorkerFilter || (!w.type && currentWorkerFilter === 'download'));
  }

  if (!filtered.length) {
    grid.innerHTML = '<div class="empty-state">No workers match the selected filter</div>';
    return;
  }

  // Render cards
  grid.innerHTML = filtered.map(w => {
    const statusCls = w.isOnline ? (w.status === 'busy' ? 'status-initializing' : 'status-running') : 'status-off';
    const statusLabel = w.isOnline ? w.status : 'offline';
    
    const enableTag = w.enable
      ? '<span class="worker-tag enabled">✓ Enabled</span>'
      : '<span class="worker-tag disabled">✗ Disabled</span>';
      
    const typeTag = w.type === 'transcode'
      ? '<span class="worker-tag type-transcode">⚙️ Transcode</span>'
      : '<span class="worker-tag type-download">⬇️ Download</span>';
      
    const cardCls = 'server-card' + (!w.enable ? ' worker-card-disabled' : '');

    let sysHtml = '';
    if (w.system) {
      const s = w.system;
      const diskPct = s.diskTotal > 0 ? Math.round(s.diskUsed / s.diskTotal * 100) : 0;
      const memPct = s.memTotal > 0 ? Math.round(s.memUsed / s.memTotal * 100) : 0;
      const cpuPct = typeof s.cpuPercent === 'number' ? Math.round(s.cpuPercent) : 0;
      sysHtml =
        '<div style="display:flex;flex-direction:column;gap:6px;margin-top:4px">' +
          '<div style="font-size:11px;color:var(--muted)">Disk ' + fmtSize(s.diskUsed) + ' / ' + fmtSize(s.diskTotal) + ' (' + diskPct + '%)' +
            '<div class="progress-bar"><div class="progress-fill ' + pctColor(diskPct) + '" style="width:' + diskPct + '%"></div></div>' +
          '</div>' +
          '<div style="font-size:11px;color:var(--muted)">Memory ' + fmtSize(s.memUsed) + ' / ' + fmtSize(s.memTotal) + ' (' + memPct + '%)' +
            '<div class="progress-bar"><div class="progress-fill ' + pctColor(memPct) + '" style="width:' + memPct + '%"></div></div>' +
          '</div>' +
          '<div style="font-size:11px;color:var(--muted)">CPU ' + cpuPct + '%' +
            '<div class="progress-bar"><div class="progress-fill ' + pctColor(cpuPct) + '" style="width:' + cpuPct + '%"></div></div>' +
          '</div>' +
        '</div>';
    }

    return '<div class="' + cardCls + '">' +
      '<div class="server-card-head">' +
        '<div class="server-name">' + escHtml(w.workerId) + '</div>' +
        '<div class="status-pill ' + statusCls + '">' + statusLabel + '</div>' +
        typeTag +
        enableTag +
      '</div>' +
      '<div class="server-meta">' +
        '<span><span class="label">Host</span><strong>' + escHtml(w.hostname) + '</strong></span>' +
        '<span><span class="label">IP</span>' + (w.ip || '—') + '</span>' +
        '<span><span class="label">PID</span>' + w.pid + '</span>' +
        '<span><span class="label">Jobs</span>' + w.activeJobs + ' / ' + w.maxJobs + '</span>' +
        '<span><span class="label">Heartbeat</span>' + fmtAgo(w.heartbeatAt) + '</span>' +
      '</div>' +
      sysHtml +
    '</div>';
  }).join('');
}

connect();
</script>
</body>
</html>`
