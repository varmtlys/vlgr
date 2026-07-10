package client

// dashboardHTML is the self-contained inspector UI (no external assets, so it
// works offline and under a strict CSP).
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>vlgr inspector</title>
<style>
  :root {
    --bg: #ffffff; --fg: #1a1a1a; --muted: #6b7280; --line: #e5e7eb;
    --row: #f9fafb; --accent: #2e9bff; --code: #f3f4f6;
    --ok: #16a34a; --warn: #d97706; --err: #dc2626;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0f1115; --fg: #e6e6e6; --muted: #9aa0a6; --line: #232733;
      --row: #171a21; --accent: #2e9bff; --code: #12141a;
      --ok: #4ade80; --warn: #fbbf24; --err: #f87171;
    }
  }
  * { box-sizing: border-box; }
  body { margin: 0; font: 14px/1.5 system-ui, sans-serif; background: var(--bg); color: var(--fg); }
  header { padding: 12px 16px; border-bottom: 1px solid var(--line); display: flex; align-items: center; gap: 12px; }
  header h1 { font-size: 15px; margin: 0; font-weight: 600; }
  header .dot { width: 9px; height: 9px; border-radius: 50%; background: var(--ok); }
  header .count { color: var(--muted); font-size: 13px; }
  .wrap { display: flex; height: calc(100vh - 49px); }
  .list { width: 48%; overflow-y: auto; border-right: 1px solid var(--line); }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 7px 10px; border-bottom: 1px solid var(--line); white-space: nowrap; }
  th { position: sticky; top: 0; background: var(--bg); color: var(--muted); font-weight: 500; font-size: 12px; }
  tbody tr { cursor: pointer; }
  tbody tr:nth-child(even) { background: var(--row); }
  tbody tr.sel { background: color-mix(in srgb, var(--accent) 18%, transparent); }
  td.path { max-width: 0; width: 100%; overflow: hidden; text-overflow: ellipsis; }
  .m { font-weight: 600; font-family: ui-monospace, monospace; }
  .s2 { color: var(--ok); } .s3 { color: var(--accent); } .s4 { color: var(--warn); } .s5 { color: var(--err); }
  .tag { font-size: 10px; padding: 1px 5px; border-radius: 4px; background: var(--code); color: var(--muted); margin-left: 6px; }
  .detail { flex: 1; overflow-y: auto; padding: 16px; }
  .detail.empty { color: var(--muted); display: flex; align-items: center; justify-content: center; }
  .detail h2 { font-size: 14px; margin: 18px 0 8px; }
  .detail h2:first-child { margin-top: 0; }
  pre { background: var(--code); padding: 10px; border-radius: 6px; overflow-x: auto; font: 12px/1.5 ui-monospace, monospace; white-space: pre-wrap; word-break: break-word; }
  .kv { font: 12px/1.6 ui-monospace, monospace; }
  .kv b { color: var(--muted); font-weight: 500; }
  button { font: inherit; background: var(--accent); color: #fff; border: 0; padding: 6px 14px; border-radius: 6px; cursor: pointer; }
  button:disabled { opacity: .5; cursor: not-allowed; }
  .meta { color: var(--muted); font-size: 12px; margin-bottom: 12px; }
</style>
</head>
<body>
<header>
  <span class="dot"></span>
  <h1>vlgr inspector</h1>
  <span class="count" id="count">0 requests</span>
</header>
<div class="wrap">
  <div class="list">
    <table>
      <thead><tr><th>Time</th><th>Method</th><th>Path</th><th>Status</th><th>ms</th></tr></thead>
      <tbody id="rows"></tbody>
    </table>
  </div>
  <div class="detail empty" id="detail">Select a request to inspect it.</div>
</div>
<script>
const rows = document.getElementById('rows');
const detail = document.getElementById('detail');
const countEl = document.getElementById('count');
let items = [];
let selected = null;

function statusClass(s) { return s >= 500 ? 's5' : s >= 400 ? 's4' : s >= 300 ? 's3' : s >= 200 ? 's2' : ''; }
function esc(s) { return (s ?? '').replace(/[&<>]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;'}[c])); }

function render() {
  countEl.textContent = items.length + ' request' + (items.length === 1 ? '' : 's');
  rows.innerHTML = items.map(e => ` + "`" + `
    <tr data-id="${e.id}" class="${e.id === selected ? 'sel' : ''}">
      <td>${e.time}</td>
      <td class="m">${esc(e.method)}</td>
      <td class="path">${esc(e.path)}${e.replayed ? '<span class="tag">replay</span>' : ''}</td>
      <td class="${statusClass(e.status)}">${e.status}</td>
      <td>${e.durationMs}</td>
    </tr>` + "`" + `).join('');
}

rows.addEventListener('click', ev => {
  const tr = ev.target.closest('tr');
  if (tr) showDetail(parseInt(tr.dataset.id, 10));
});

function headersHTML(h) {
  if (!h) return '<div class="kv"><i>none</i></div>';
  return '<div class="kv">' + Object.entries(h).map(([k, vs]) =>
    vs.map(v => '<div><b>' + esc(k) + ':</b> ' + esc(v) + '</div>').join('')).join('') + '</div>';
}

async function showDetail(id) {
  selected = id;
  render();
  const e = await (await fetch('/api/request/' + id)).json();
  const canReplay = !e.reqStreamed;
  detail.className = 'detail';
  detail.innerHTML = ` + "`" + `
    <div class="meta">${esc(e.method)} ${esc(e.host)}${esc(e.path)} → localhost:${e.localPort} · ${e.status} · ${e.durationMs} ms · ${e.time}</div>
    <button id="replay" ${canReplay ? '' : 'disabled title="streamed body"'}>Replay to local</button>
    <h2>Request headers</h2>${headersHTML(e.reqHeaders)}
    ${e.reqBody ? '<h2>Request body</h2><pre>' + esc(e.reqBody) + '</pre>' : ''}
    <h2>Response headers</h2>${headersHTML(e.respHeaders)}
    ${e.respBody ? '<h2>Response body</h2><pre>' + esc(e.respBody) + '</pre>' : ''}
  ` + "`" + `;
  const btn = document.getElementById('replay');
  if (btn && canReplay) btn.onclick = async () => {
    btn.disabled = true; btn.textContent = 'Replaying…';
    await fetch('/api/replay/' + id, { method: 'POST' });
    btn.textContent = 'Replayed';
  };
}

async function load() {
  items = await (await fetch('/api/requests')).json();
  render();
}
load();

const es = new EventSource('/api/stream');
es.onmessage = ev => {
  const e = JSON.parse(ev.data);
  items.unshift(e);
  if (items.length > 200) items.pop();
  render();
};
</script>
</body>
</html>`
