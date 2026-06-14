/* websh — mobile shell terminal PWA */
'use strict';

const $ = (s) => document.querySelector(s);
const el = (tag, cls, txt) => { const e = document.createElement(tag); if (cls) e.className = cls; if (txt != null) e.textContent = txt; return e; };

// 应用挂载路径（支持子路径部署），始终以 / 结尾。
const BASE = (() => {
  try { return new URL('.', document.currentScript.src).pathname; } catch { return '/'; }
})();

function toast(msg, ms = 2200) {
  const t = $('#toast'); t.textContent = msg; t.classList.add('show');
  clearTimeout(toast._t); toast._t = setTimeout(() => t.classList.remove('show'), ms);
}

async function api(path, opts = {}) {
  const url = /^https?:/.test(path) ? path : BASE + path.replace(/^\//, '');
  const r = await fetch(url, { credentials: 'same-origin', ...opts });
  let data = null;
  try { data = await r.json(); } catch (_) {}
  if (!r.ok) {
    const e = new Error((data && data.detail) || ('HTTP ' + r.status)); e.status = r.status; throw e;
  }
  return data;
}

const FONT_KEY = 'websh.fontSize';
let fontSize = parseInt(localStorage.getItem(FONT_KEY) || '14', 10) || 14;

const state = { user: null, live: [], quickconnects: [], tabs: [], terms: new Map(), active: null };

// PWA update handling: a freshly-installed service worker waits until the user
// taps the update banner, then activates and the page reloads.
let waitingWorker = null;
function offerUpdate(worker) { waitingWorker = worker; $('#updateBar').classList.remove('hidden'); }

// ---- 视图切换 --------------------------------------------------------------
function show(view) {
  for (const id of ['login', 'srvHeader', 'notifyBar', 'sessions', 'workHeader', 'work']) $('#' + id).classList.add('hidden');
  if (view === 'login') $('#login').classList.remove('hidden');
  if (view === 'sessions') { $('#srvHeader').classList.remove('hidden'); $('#sessions').classList.remove('hidden'); updateNotifyBar(); }
  if (view === 'work') { $('#workHeader').classList.remove('hidden'); $('#work').classList.remove('hidden'); }
}

// ---- 登录 ------------------------------------------------------------------
async function doLogin() {
  const username = $('#username').value.trim();
  const otp = $('#otp').value.trim();
  const password = $('#password').value;
  $('#loginErr').textContent = '';
  if (!username || !otp || !password) { $('#loginErr').textContent = '请填写用户名、验证码和密码'; return; }
  $('#loginBtn').disabled = true;
  try {
    const r = await api('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username, otp, password }) });
    state.user = r.user;
    await enterSessions();
  } catch (e) {
    $('#loginErr').textContent = e.message;
  } finally { $('#loginBtn').disabled = false; }
}

async function logout() {
  try { await api('/api/logout', { method: 'POST' }); } catch {}
  state.terms.forEach(rec => { rec.closing = true; try { rec.ws && rec.ws.close(); } catch {} try { rec.term.dispose(); } catch {} });
  state.terms.clear(); state.tabs = []; $('#pane').innerHTML = '';
  state.user = null; show('login');
}

// ---- 会话列表 --------------------------------------------------------------
async function enterSessions() {
  show('sessions');
  const wrap = $('#sessions'); wrap.innerHTML = ''; wrap.appendChild(el('div', 'mut', '加载中…'));
  try {
    const r = await api('/api/sessions');
    state.live = r.live || [];
    state.quickconnects = r.quickconnects || [];
  } catch (e) {
    wrap.innerHTML = ''; wrap.appendChild(el('div', 'err', '加载会话失败：' + e.message)); return;
  }
  renderSessions();
}

function srvCard(icon, name, desc, onclick) {
  const card = el('div', 'srv');
  card.appendChild(el('span', 'ic', icon));
  const meta = el('div', 'meta');
  meta.appendChild(el('div', 'name', name));
  if (desc) meta.appendChild(el('div', 'desc', desc));
  card.appendChild(meta);
  card.onclick = onclick;
  return card;
}

function renderSessions() {
  const wrap = $('#sessions'); wrap.innerHTML = '';

  // 新建 bash
  wrap.appendChild(srvCard('➕', '新建 Bash', '本机新终端', () => newBash()));

  // 配置里的 SSH 远端
  for (const q of state.quickconnects) {
    wrap.appendChild(srvCard('🌐', '＋ ' + (q.name || q.id), 'ssh · ' + (q.host || ''),
      () => openSession({ id: q.id, name: q.name || q.id, type: q.type })));
  }

  // 运行中的 tmux 会话
  if (state.live.length) wrap.appendChild(el('div', 'mut', '运行中的会话'));
  for (const s of state.live) {
    const card = srvCard(s.type === 'ssh' ? '🌐' : '🖥️', s.label,
      (s.type === 'ssh' ? 'ssh' : 'bash') + ' · id ' + s.id + (s.attached ? ' · 已连接' : ''),
      () => openSession({ id: s.id, name: s.label, type: s.type }));
    const ren = el('button', 'btn-sm', '✎'); ren.onclick = (e) => { e.stopPropagation(); renameSession(s.id, s.label); };
    const del = el('button', 'btn-sm', '🗑'); del.onclick = (e) => { e.stopPropagation(); deleteSession(s.id, s.label); };
    card.appendChild(ren); card.appendChild(del);
    wrap.appendChild(card);
  }
}

async function newBash() {
  try {
    const r = await api('/api/sessions/new', { method: 'POST' });
    openSession({ id: r.id, name: r.id, type: 'bash' });
  } catch (e) { toast('新建失败：' + e.message); }
}

async function renameSession(id, current) {
  const name = prompt('会话名称', current === id ? '' : current);
  if (name === null) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}/rename`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: name.trim() }) });
    const t = state.tabs.find(x => x.id === id);
    if (t) { t.name = name.trim() || id; renderTabbar(); if (state.active === t) $('#workTitle').textContent = t.name; }
    if (!$('#sessions').classList.contains('hidden')) enterSessions();
  } catch (e) { toast('改名失败：' + e.message); }
}

async function deleteSession(id, label) {
  if (!confirm(`删除会话「${label}」？会终止其中运行的程序。`)) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' });
  } catch (e) { toast('删除失败：' + e.message); return; }
  const t = state.tabs.find(x => x.id === id);
  if (t) closeTab(t);
  enterSessions();
}

// ---- 通知开关 --------------------------------------------------------------
function updateNotifyBar() {
  const bar = $('#notifyBar');
  const supported = ('serviceWorker' in navigator) && ('PushManager' in window) && ('Notification' in window);
  if (supported && Notification.permission !== 'granted') bar.classList.remove('hidden');
  else bar.classList.add('hidden');
}

function urlB64ToUint8Array(b64) {
  const pad = '='.repeat((4 - b64.length % 4) % 4);
  const base64 = (b64 + pad).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

async function enablePush() {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) { toast('此浏览器不支持推送'); return; }
  try {
    const perm = await Notification.requestPermission();
    if (perm !== 'granted') { toast('通知权限被拒绝'); return; }
    const reg = await navigator.serviceWorker.ready;
    const { key } = await api('/api/push/vapid-public-key');
    let sub = await reg.pushManager.getSubscription();
    if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: urlB64ToUint8Array(key) });
    await api('/api/push/subscribe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sub) });
    toast('通知已开启');
    updateNotifyBar();
  } catch (e) { toast('开启通知失败：' + e.message); }
}

// ---- 打开会话 → 终端标签 ---------------------------------------------------
function openSession(s) {
  let t = state.tabs.find(x => x.id === s.id);
  if (!t) {
    t = { id: s.id, name: s.name || s.id, type: s.type };
    state.tabs.push(t);
  }
  show('work');
  renderTabbar();
  setActive(t);
}

function renderTabbar() {
  const bar = $('#tabbar'); bar.innerHTML = '';
  const add = el('div', 'tab tabadd', '＋'); add.title = '新建 Bash'; add.onclick = () => newBash(); bar.appendChild(add);
  for (const t of state.tabs) {
    const tab = el('div', 'tab' + (t === state.active ? ' active' : ''));
    const d = el('span', 'dot' + (t._status ? ' ' + t._status : '')); tab.appendChild(d);
    tab.appendChild(el('span', null, t.name));
    const x = el('span', 'tabx', '×');
    x.onclick = (ev) => { ev.stopPropagation(); closeTab(t); };
    tab.appendChild(x);
    tab.onclick = () => setActive(t);
    t._tabEl = tab;
    bar.appendChild(tab);
  }
}

function closeTab(t) {
  const rec = state.terms.get(t.id);
  if (rec) { rec.closing = true; clearTimeout(rec.reconnectTimer); try { rec.ws && rec.ws.close(); } catch {} try { rec.term.dispose(); } catch {} state.terms.delete(t.id); }
  const view = $('#pane').querySelector(`.view[data-term="${CSS.escape(t.id)}"]`);
  if (view) view.remove();
  const idx = state.tabs.indexOf(t);
  state.tabs = state.tabs.filter(x => x !== t);
  if (!state.tabs.length) { enterSessions(); return; }
  if (state.active === t) setActive(state.tabs[Math.max(0, idx - 1)] || state.tabs[0]);
  else renderTabbar();
}

function setActive(t) {
  state.active = t;
  t._wait = false;
  $('#workTitle').textContent = t.name;
  renderTabbar();
  const pane = $('#pane');
  for (const v of pane.querySelectorAll('.view')) v.classList.add('hidden');
  let v = pane.querySelector(`.view[data-term="${CSS.escape(t.id)}"]`);
  if (!v) { v = buildTermView(t); pane.appendChild(v); }
  v.classList.remove('hidden');
  const rec = state.terms.get(t.id);
  if (rec) setTimeout(() => { try { rec.fit.fit(); rec.term.focus(); sendResize(rec); } catch {} }, 30);
}

// ============================================================================
// 终端
// ============================================================================
const KEYMODE_KEY = 'websh.keymode';
let defaultMode = localStorage.getItem(KEYMODE_KEY) || 'sentence';

function toggleMode(rec) {
  rec.mode = rec.mode === 'char' ? 'sentence' : 'char';
  defaultMode = rec.mode;
  try { localStorage.setItem(KEYMODE_KEY, rec.mode); } catch {}
  renderKeys(rec);
  if (rec.mode === 'char') rec.term.focus();
  setTimeout(() => { try { rec.fit.fit(); sendResize(rec); } catch {} }, 30);
}

function renderKeys(rec) {
  const qk = rec.qk; qk.innerHTML = '';
  const mk = (label, fn, cls) => { const b = el('button', cls || null, label); b.onclick = fn; qk.appendChild(b); return b; };
  const ctl = (c) => () => { sendData(rec, c); rec.term.focus(); };

  mk(rec.mode === 'char' ? '⌨️ 单字' : '✍️ 整句', () => toggleMode(rec), 'modebtn');

  if (rec.mode === 'char') {
    rec.cmdbar.style.display = 'none';
    mk('Esc', ctl('\x1b')); mk('Tab', ctl('\t')); mk('⇧Tab', ctl('\x1b[Z'));
    mk('↑', ctl('\x1b[A')); mk('↓', ctl('\x1b[B')); mk('←', ctl('\x1b[D')); mk('→', ctl('\x1b[C'));
    mk('Home', ctl('\x1b[H')); mk('End', ctl('\x1b[F')); mk('PgUp', ctl('\x1b[5~')); mk('PgDn', ctl('\x1b[6~'));
    mk('⌫', ctl('\x7f')); mk('Del', ctl('\x1b[3~'));
    mk('^A', ctl('\x01')); mk('^E', ctl('\x05')); mk('^C', ctl('\x03')); mk('^D', ctl('\x04'));
    mk('^B', ctl('\x02')); mk('^K', ctl('\x0b')); mk('^U', ctl('\x15')); mk('^W', ctl('\x17')); mk('^R', ctl('\x12'));
    mk('^Z', ctl('\x1a')); mk('^L', ctl('\x0c')); mk('^T', ctl('\x14'));
    mk('清屏', () => rec.term.clear());
  } else {
    rec.cmdbar.style.display = '';
    mk('Esc', ctl('\x1b')); mk('⇧Tab', ctl('\x1b[Z')); mk('^C', ctl('\x03')); mk('清屏', () => rec.term.clear());
  }
  mk('📋', () => openCopyPanel(rec)); mk('复制屏', () => copyText(termText(rec.term))); mk('🔄', () => { rec.retries = 0; connectTerm(rec); });
}

function buildTermView(t) {
  const v = el('div', 'view'); v.dataset.term = t.id;
  const wrap = el('div', 'term-wrap'); v.appendChild(wrap);
  const qk = el('div', 'termbar'); v.appendChild(qk);
  const cb = el('div', 'cmdbar');
  const input = el('input'); input.placeholder = '输入命令，回车发送'; input.autocapitalize = 'off'; input.autocomplete = 'off'; input.spellcheck = false;
  const send = el('button', null, '发送');
  cb.appendChild(input); cb.appendChild(send); v.appendChild(cb);

  const term = new Terminal({
    fontSize, cursorBlink: true, scrollback: 5000,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    theme: { background: '#000000', selectionBackground: '#3a4a63' },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(wrap);
  setTimeout(() => { try { fit.fit(); } catch {} }, 30);

  const rec = { term, fit, ws: null, tab: t, view: v, qk, cmdbar: cb, mode: defaultMode, retries: 0, closing: false };
  state.terms.set(t.id, rec);

  attachPinch(wrap, rec);
  wrap.addEventListener('click', () => { if (rec.mode === 'char') term.focus(); });
  term.onData(d => sendData(rec, d));

  const sendCmd = () => { sendData(rec, input.value + '\r'); input.value = ''; };
  send.onclick = sendCmd;
  input.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); sendCmd(); } });

  renderKeys(rec);
  connectTerm(rec);
  window.addEventListener('resize', () => { if (state.active === t) setTimeout(() => { try { fit.fit(); sendResize(rec); } catch {} }, 50); });
  return v;
}

// Terminal input goes as BINARY frames (raw PTY bytes); control messages
// (resize/presence/pong) go as JSON text frames. The server distinguishes the
// two by frame type, so keystrokes must not be sent as text.
const ENC = new TextEncoder();
function sendData(rec, d) { if (rec.ws && rec.ws.readyState === 1) rec.ws.send(ENC.encode(d)); }

function termStatus(rec, st) {
  rec.tab._status = st;
  const d = rec.tab._tabEl && rec.tab._tabEl.querySelector('.dot');
  if (d) d.className = 'dot' + (st ? ' ' + st : '');
}

function wsURL(t) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}${BASE}ws/term/${encodeURIComponent(t.id)}`;
}

function connectTerm(rec) {
  if (rec.closing) return;
  const t = rec.tab;
  clearTimeout(rec.reconnectTimer);
  // Detach the previous socket's handlers before closing it, so its (intentional)
  // close does NOT trigger a reconnect — otherwise every reconnect schedules
  // another one and we loop forever.
  if (rec.ws) {
    const old = rec.ws;
    old.onopen = old.onmessage = old.onerror = old.onclose = null;
    try { if (old.readyState <= 1) old.close(); } catch {}
  }
  termStatus(rec, 'wait');
  rec.term.write('\r\n\x1b[90m[连接中…]\x1b[0m\r\n');
  const ws = new WebSocket(wsURL(t));
  ws.binaryType = 'arraybuffer';
  rec.ws = ws;
  ws.onopen = () => { if (rec.ws !== ws) return; rec.retries = 0; termStatus(rec, 'on'); setTimeout(() => { sendResize(rec); sendPresence(rec); }, 60); };
  ws.onmessage = (ev) => {
    if (rec.ws !== ws) return;
    if (typeof ev.data === 'string') { handleCtl(rec, ev.data); return; }
    rec.term.write(new Uint8Array(ev.data));
  };
  ws.onerror = () => { if (rec.ws === ws) termStatus(rec, 'err'); };
  ws.onclose = (e) => {
    if (rec.closing || rec.ws !== ws) return;   // ignore stale/replaced sockets
    if (e.code === 4401) { termStatus(rec, 'err'); rec.term.write('\r\n\x1b[91m会话失效，请重新登录\x1b[0m\r\n'); return; }
    scheduleReconnect(rec);
  };
}

function scheduleReconnect(rec) {
  if (rec.closing) return;
  rec.retries = (rec.retries || 0) + 1;
  const delay = Math.min(15000, 500 * Math.pow(2, rec.retries - 1)) + Math.random() * 400;
  termStatus(rec, 'err');
  clearTimeout(rec.reconnectTimer);
  rec.reconnectTimer = setTimeout(() => connectTerm(rec), delay);
}

function handleCtl(rec, txt) {
  let m; try { m = JSON.parse(txt); } catch { return; }
  if (!m) return;
  if (m.type === 'ping') { try { rec.ws.send(JSON.stringify({ type: 'pong' })); } catch {} return; }
  if (m.type === 'attention') {
    if (state.active === rec.tab && !document.hidden) { toast(m.message || '终端有新消息'); }
    else { rec.tab._wait = true; const d = rec.tab._tabEl && rec.tab._tabEl.querySelector('.dot'); if (d && rec.tab._status === 'on') d.className = 'dot wait'; }
  }
}

// ---- 前台/后台上报 ---------------------------------------------------------
function presenceState() { return document.hidden ? 'background' : 'foreground'; }
function sendPresence(rec) {
  if (rec.ws && rec.ws.readyState === 1) { try { rec.ws.send(JSON.stringify({ type: 'presence', state: presenceState() })); } catch {} }
}
function broadcastPresence() { state.terms.forEach(sendPresence); }
function reconnectStale() {
  state.terms.forEach(rec => { if (!rec.closing && (!rec.ws || rec.ws.readyState > 1)) { rec.retries = 0; connectTerm(rec); } });
}

// ---- 文本/复制/缩放 --------------------------------------------------------
function termText(term) {
  const buf = term.buffer.active, lines = [];
  for (let i = 0; i < buf.length; i++) { const line = buf.getLine(i); lines.push(line ? line.translateToString(true) : ''); }
  return lines.join('\n').replace(/\s+$/, '') + '\n';
}

function openCopyPanel(rec) {
  const ov = el('div', 'copyov');
  const bar = el('div', 'copybar');
  const close = el('button', 'ghost', '✕ 关闭');
  const all = el('button', 'ghost', '复制全部');
  const tip = el('span', 'grow mut', '长按选择 → 系统「复制」；或点「复制全部」');
  bar.appendChild(close); bar.appendChild(all); bar.appendChild(tip);
  const ta = el('textarea', 'copyta'); ta.value = termText(rec.term); ta.readOnly = true; ta.spellcheck = false;
  ov.appendChild(bar); ov.appendChild(ta); document.body.appendChild(ov);
  close.onclick = () => ov.remove();
  all.onclick = () => copyText(ta.value);
  setTimeout(() => { ta.scrollTop = ta.scrollHeight; }, 0);
}

function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(() => toast('已复制'), () => fallbackCopy(text));
  else fallbackCopy(text);
}
function fallbackCopy(text) {
  const ta = document.createElement('textarea'); ta.value = text; ta.style.cssText = 'position:fixed;opacity:0;';
  document.body.appendChild(ta); ta.select();
  try { document.execCommand('copy'); toast('已复制'); } catch { toast('复制失败'); }
  ta.remove();
}

function sendResize(rec) {
  if (rec.ws && rec.ws.readyState === 1) { try { rec.ws.send(JSON.stringify({ type: 'resize', cols: rec.term.cols, rows: rec.term.rows })); } catch {} }
}

function attachPinch(elm, rec) {
  let startDist = 0, startFont = fontSize;
  const dist = (e) => Math.hypot(e.touches[0].clientX - e.touches[1].clientX, e.touches[0].clientY - e.touches[1].clientY);
  elm.addEventListener('touchstart', e => { if (e.touches.length === 2) { startDist = dist(e); startFont = rec.term.options.fontSize; } }, { passive: true });
  elm.addEventListener('touchmove', e => {
    if (e.touches.length === 2 && startDist) {
      const f = Math.max(8, Math.min(28, Math.round(startFont * dist(e) / startDist)));
      if (f !== rec.term.options.fontSize) { rec.term.options.fontSize = f; try { rec.fit.fit(); sendResize(rec); } catch {} }
    }
  }, { passive: true });
}

function changeFont(delta) {
  fontSize = Math.max(8, Math.min(28, fontSize + delta));
  localStorage.setItem(FONT_KEY, String(fontSize));
  const rec = state.active && state.terms.get(state.active.id);
  if (rec) { rec.term.options.fontSize = fontSize; try { rec.fit.fit(); sendResize(rec); } catch {} }
  else toast('字号 ' + fontSize);
}

// ---- 启动 ------------------------------------------------------------------
function bind() {
  $('#loginBtn').onclick = doLogin;
  $('#otp').addEventListener('keydown', e => { if (e.key === 'Enter') doLogin(); });
  $('#logoutBtn').onclick = logout;
  $('#notifyBtn').onclick = enablePush;
  $('#backBtn').onclick = () => enterSessions();
  $('#workTitle').onclick = () => { const t = state.active; if (t) renameSession(t.id, t.name); };
  $('#fontInc').onclick = () => changeFont(1);
  $('#fontDec').onclick = () => changeFont(-1);

  document.addEventListener('visibilitychange', () => { broadcastPresence(); if (!document.hidden) reconnectStale(); });
  window.addEventListener('focus', broadcastPresence);
  window.addEventListener('blur', broadcastPresence);
  window.addEventListener('online', reconnectStale);

  $('#updateBtn').onclick = () => { $('#updateBar').classList.add('hidden'); if (waitingWorker) waitingWorker.postMessage({ type: 'SKIP_WAITING' }); };

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register(BASE + 'service-worker.js', { scope: BASE }).then(reg => {
      if (reg.waiting && navigator.serviceWorker.controller) offerUpdate(reg.waiting);
      reg.addEventListener('updatefound', () => {
        const nw = reg.installing;
        if (nw) nw.addEventListener('statechange', () => {
          if (nw.state === 'installed' && navigator.serviceWorker.controller) offerUpdate(nw);
        });
      });
      // Re-check for a new version whenever the app comes to the foreground.
      document.addEventListener('visibilitychange', () => { if (!document.hidden) reg.update().catch(() => {}); });
    }).catch(() => {});
    // When the new SW takes over (after the user taps update), reload once.
    let swReloaded = false;
    navigator.serviceWorker.addEventListener('controllerchange', () => {
      if (swReloaded) return; swReloaded = true; location.reload();
    });
    navigator.serviceWorker.addEventListener('message', (ev) => {
      const tabId = ev.data && ev.data.tabId;
      if (tabId) { const t = state.tabs.find(x => x.id === tabId); if (t) setActive(t); }
    });
  }
}

async function boot() {
  bind();
  try { const r = await api('/api/me'); state.user = r.user; await enterSessions(); }
  catch { show('login'); }
}
boot();
