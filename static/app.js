/* websh — single-terminal PWA: one websocket, one tmux client, switch sessions */
'use strict';

const $ = (s) => document.querySelector(s);
const el = (tag, cls, txt) => { const e = document.createElement(tag); if (cls) e.className = cls; if (txt != null) e.textContent = txt; return e; };

const BASE = (() => { try { return new URL('.', document.currentScript.src).pathname; } catch { return '/'; } })();

function toast(msg, ms = 2200) {
  const t = $('#toast'); t.textContent = msg; t.classList.add('show');
  clearTimeout(toast._t); toast._t = setTimeout(() => t.classList.remove('show'), ms);
}

async function api(path, opts = {}) {
  const url = /^https?:/.test(path) ? path : BASE + path.replace(/^\//, '');
  const r = await fetch(url, { credentials: 'same-origin', ...opts });
  let data = null;
  try { data = await r.json(); } catch (_) {}
  if (!r.ok) { const e = new Error((data && data.detail) || ('HTTP ' + r.status)); e.status = r.status; throw e; }
  return data;
}

const FONT_KEY = 'websh.fontSize';
let fontSize = parseInt(localStorage.getItem(FONT_KEY) || '14', 10) || 14;
const KEYMODE_KEY = 'websh.keymode';

const state = { user: null, current: '', live: [], remotes: [] };
let T = null; // the single terminal client: {term, fit, ws, mode, retries, closing, reconnectTimer, qk, cmdbar}
const ENC = new TextEncoder();

function show(view) {
  for (const id of ['login', 'workHeader', 'work']) $('#' + id).classList.add('hidden');
  if (view === 'login') $('#login').classList.remove('hidden');
  if (view === 'work') { $('#workHeader').classList.remove('hidden'); $('#work').classList.remove('hidden'); }
}

// ---- 登录 ------------------------------------------------------------------
async function doLogin() {
  const username = $('#username').value.trim();
  const otp = $('#otp').value.trim();
  const password = $('#password').value;
  $('#loginErr').textContent = '';
  if (!username || !otp || !password) { $('#loginErr').textContent = '请填写用户名、密码和验证码'; return; }
  $('#loginBtn').disabled = true;
  try {
    const r = await api('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username, otp, password }) });
    state.user = r.user;
    enterApp();
  } catch (e) { $('#loginErr').textContent = e.message; }
  finally { $('#loginBtn').disabled = false; }
}

async function logout() {
  if (T) { T.closing = true; clearTimeout(T.reconnectTimer); try { T.ws && T.ws.close(); } catch {} }
  try { await api('/api/logout', { method: 'POST' }); } catch {}
  closeDrawer(); state.user = null; show('login');
}

function enterApp() {
  show('work');
  if (!T) buildTerminal();
  if (!T.ws || T.ws.readyState > 1) { T.retries = 0; connect(); }
  setTitle();
}

function setTitle() { $('#workTitle').textContent = state.current || 'websh'; }

// ============================================================================
// 单终端
// ============================================================================
function buildTerminal() {
  const v = el('div', 'view');
  const wrap = el('div', 'term-wrap'); v.appendChild(wrap);
  const qk = el('div', 'termbar'); v.appendChild(qk);
  const cb = el('div', 'cmdbar');
  const input = el('input'); input.placeholder = '输入命令，回车发送'; input.autocapitalize = 'off'; input.autocomplete = 'off'; input.spellcheck = false;
  const send = el('button', null, '发送');
  cb.appendChild(input); cb.appendChild(send); v.appendChild(cb);
  $('#pane').appendChild(v);

  const term = new Terminal({
    fontSize, cursorBlink: true, scrollback: 5000,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    theme: { background: '#000000', selectionBackground: '#3a4a63' },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit); term.open(wrap);
  setTimeout(() => { try { fit.fit(); } catch {} }, 30);

  T = { term, fit, ws: null, mode: localStorage.getItem(KEYMODE_KEY) || 'sentence', retries: 0, closing: false, qk, cmdbar: cb };

  attachPinch(wrap);
  wrap.addEventListener('click', () => { if (T.mode === 'char') term.focus(); });
  term.onData(d => sendData(d));
  const sendCmd = () => { sendData(input.value + '\r'); input.value = ''; };
  send.onclick = sendCmd;
  input.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); sendCmd(); } });

  renderKeys();
  window.addEventListener('resize', () => setTimeout(() => { try { fit.fit(); sendResize(); } catch {} }, 50));
}

function sendData(d) { if (T && T.ws && T.ws.readyState === 1) T.ws.send(ENC.encode(d)); }
function wsSend(obj) { if (T && T.ws && T.ws.readyState === 1) { try { T.ws.send(JSON.stringify(obj)); } catch {} } }

function toggleMode() {
  T.mode = T.mode === 'char' ? 'sentence' : 'char';
  try { localStorage.setItem(KEYMODE_KEY, T.mode); } catch {}
  renderKeys();
  if (T.mode === 'char') T.term.focus();
  setTimeout(() => { try { T.fit.fit(); sendResize(); } catch {} }, 30);
}

function renderKeys() {
  const qk = T.qk; qk.innerHTML = '';
  const mk = (label, fn, cls) => { const b = el('button', cls || null, label); b.onclick = fn; qk.appendChild(b); return b; };
  const ctl = (c) => () => { sendData(c); T.term.focus(); };
  mk(T.mode === 'char' ? '⌨️ 单字' : '✍️ 整句', toggleMode, 'modebtn');
  if (T.mode === 'char') {
    T.cmdbar.style.display = 'none';
    mk('Esc', ctl('\x1b')); mk('Tab', ctl('\t')); mk('⇧Tab', ctl('\x1b[Z'));
    mk('↑', ctl('\x1b[A')); mk('↓', ctl('\x1b[B')); mk('←', ctl('\x1b[D')); mk('→', ctl('\x1b[C'));
    mk('Home', ctl('\x1b[H')); mk('End', ctl('\x1b[F')); mk('PgUp', ctl('\x1b[5~')); mk('PgDn', ctl('\x1b[6~'));
    mk('⌫', ctl('\x7f')); mk('Del', ctl('\x1b[3~'));
    mk('^A', ctl('\x01')); mk('^E', ctl('\x05')); mk('^C', ctl('\x03')); mk('^D', ctl('\x04'));
    mk('^B', ctl('\x02')); mk('^K', ctl('\x0b')); mk('^U', ctl('\x15')); mk('^W', ctl('\x17')); mk('^R', ctl('\x12'));
    mk('^Z', ctl('\x1a')); mk('^L', ctl('\x0c')); mk('^T', ctl('\x14'));
    mk('清屏', () => T.term.clear());
  } else {
    T.cmdbar.style.display = '';
    mk('Esc', ctl('\x1b')); mk('⇧Tab', ctl('\x1b[Z')); mk('^C', ctl('\x03')); mk('^B', ctl('\x02')); mk('清屏', () => T.term.clear());
  }
  mk('📋', openCopyPanel); mk('复制屏', () => copyText(termText())); mk('🔄', () => { T.retries = 0; connect(); });
}

// ---- websocket -------------------------------------------------------------
function wsURL() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}${BASE}ws`;
}

function connect() {
  if (!T || T.closing) return;
  clearTimeout(T.reconnectTimer);
  if (T.ws) { const old = T.ws; old.onopen = old.onmessage = old.onerror = old.onclose = null; try { if (old.readyState <= 1) old.close(); } catch {} }
  T.term.write('\r\n\x1b[90m[连接中…]\x1b[0m\r\n');
  const ws = new WebSocket(wsURL());
  ws.binaryType = 'arraybuffer';
  T.ws = ws;
  ws.onopen = () => { if (T.ws !== ws) return; T.retries = 0; setTimeout(() => { sendResize(); sendPresence(); }, 60); };
  ws.onmessage = (ev) => {
    if (T.ws !== ws) return;
    if (typeof ev.data === 'string') { handleCtl(ev.data); return; }
    T.term.write(new Uint8Array(ev.data));
  };
  ws.onerror = () => {};
  ws.onclose = (e) => {
    if (T.closing || T.ws !== ws) return;
    if (e.code === 4401) { T.term.write('\r\n\x1b[91m会话失效，请重新登录\x1b[0m\r\n'); return; }
    scheduleReconnect();
  };
}

function scheduleReconnect() {
  if (!T || T.closing) return;
  T.retries = (T.retries || 0) + 1;
  const delay = Math.min(15000, 500 * Math.pow(2, T.retries - 1)) + Math.random() * 400;
  clearTimeout(T.reconnectTimer);
  T.reconnectTimer = setTimeout(connect, delay);
}

function handleCtl(txt) {
  let m; try { m = JSON.parse(txt); } catch { return; }
  if (!m) return;
  if (m.type === 'ping') { wsSend({ type: 'pong' }); return; }
  if (m.type === 'session') { state.current = m.name || ''; setTitle(); return; }
  if (m.type === 'attention') {
    if (!document.hidden) toast(m.message || '终端有新消息');
  }
}

// ---- 前台/后台 -------------------------------------------------------------
function presenceState() { return document.hidden ? 'background' : 'foreground'; }
function sendPresence() { wsSend({ type: 'presence', state: presenceState() }); }
function reconnectStale() { if (T && !T.closing && (!T.ws || T.ws.readyState > 1)) { T.retries = 0; connect(); } }
function sendResize() { if (T) wsSend({ type: 'resize', cols: T.term.cols, rows: T.term.rows }); }

// ============================================================================
// 会话抽屉
// ============================================================================
function openDrawer() { $('#drawer').classList.remove('hidden'); updateNotifyBar(); loadSessions(); }
function closeDrawer() { $('#drawer').classList.add('hidden'); }

async function loadSessions() {
  const wrap = $('#sessions'); wrap.innerHTML = ''; wrap.appendChild(el('div', 'mut', '加载中…'));
  try {
    const r = await api('/api/sessions');
    state.live = r.live || []; state.remotes = r.remotes || [];
  } catch (e) { wrap.innerHTML = ''; wrap.appendChild(el('div', 'err', '加载会话失败：' + e.message)); return; }
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
  wrap.appendChild(srvCard('➕', '新建 Bash', '本机新终端', () => newBash()));
  for (const q of state.remotes) {
    wrap.appendChild(srvCard('🌐', '＋ ' + (q.name || q.id), 'ssh · ' + (q.host || ''), () => newRemote(q.id)));
  }
  if (state.live.length) wrap.appendChild(el('div', 'mut', '运行中的会话（点击切换）'));
  for (const s of state.live) {
    const cur = s.name === state.current;
    const card = srvCard(s.type === 'ssh' ? '🌐' : '🖥️', s.name + (cur ? '  ·  当前' : ''),
      (s.type === 'ssh' ? 'ssh' : 'bash') + (s.window ? ' · ' + s.window : '') + (s.attached ? ' · 已连接' : ''),
      () => switchTo(s.name));
    if (cur) card.classList.add('cur');
    const ren = el('button', 'btn-sm', '✎'); ren.onclick = (e) => { e.stopPropagation(); renameSession(s.name); };
    const del = el('button', 'btn-sm', '🗑'); del.onclick = (e) => { e.stopPropagation(); killSession(s.name); };
    card.appendChild(ren); card.appendChild(del);
    wrap.appendChild(card);
  }
}

function switchTo(name) { wsSend({ type: 'switch', target: name }); state.current = name; setTitle(); closeDrawer(); T.term.focus(); }
function newBash() { wsSend({ type: 'new', kind: 'bash' }); closeDrawer(); T.term.focus(); }
function newRemote(id) { wsSend({ type: 'new', kind: 'remote', id }); closeDrawer(); T.term.focus(); }

function remoteSuffix(name) {
  const at = name.lastIndexOf('@');
  if (at >= 0 && state.remotes.some(r => r.id === name.slice(at + 1))) return name.slice(at); // "@server"
  return '';
}

async function renameSession(target) {
  let newName;
  const suffix = remoteSuffix(target);
  if (suffix) {
    // Remote session: only the part before "@server" is editable.
    const p = prompt('远端会话名（' + suffix + ' 固定不可改，不含 @）', target.slice(0, target.length - suffix.length));
    if (p === null) return;
    newName = p.trim() + suffix;
  } else {
    const p = prompt('会话名称（不含 . : | @）', target);
    if (p === null) return;
    newName = p.trim();
  }
  try {
    await api('/api/sessions/rename', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ target, name: newName }) });
    if (target === state.current) { state.current = newName; setTitle(); }
    loadSessions();
  } catch (e) { toast('改名失败：' + e.message); }
}

async function killSession(target) {
  if (!confirm(`删除会话「${target}」？会终止其中运行的程序。`)) return;
  try { await api('/api/sessions/kill', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ target }) }); }
  catch (e) { toast('删除失败：' + e.message); return; }
  loadSessions();
}

// ---- 通知 ------------------------------------------------------------------
function updateNotifyBar() {
  const bar = $('#notifyBar');
  const ok = ('serviceWorker' in navigator) && ('PushManager' in window) && ('Notification' in window);
  if (ok && Notification.permission !== 'granted') bar.classList.remove('hidden'); else bar.classList.add('hidden');
}
function urlB64ToUint8Array(b64) {
  const pad = '='.repeat((4 - b64.length % 4) % 4);
  const base64 = (b64 + pad).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64); const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}
async function enablePush() {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) { toast('此浏览器不支持推送'); return; }
  try {
    if (await Notification.requestPermission() !== 'granted') { toast('通知权限被拒绝'); return; }
    const reg = await navigator.serviceWorker.ready;
    const { key } = await api('/api/push/vapid-public-key');
    let sub = await reg.pushManager.getSubscription();
    if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: urlB64ToUint8Array(key) });
    await api('/api/push/subscribe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sub) });
    toast('通知已开启'); updateNotifyBar();
  } catch (e) { toast('开启通知失败：' + e.message); }
}

// ---- 复制 / 缩放 -----------------------------------------------------------
function termText() {
  const buf = T.term.buffer.active, lines = [];
  for (let i = 0; i < buf.length; i++) { const line = buf.getLine(i); lines.push(line ? line.translateToString(true) : ''); }
  return lines.join('\n').replace(/\s+$/, '') + '\n';
}
function openCopyPanel() {
  const ov = el('div', 'copyov');
  const bar = el('div', 'copybar');
  const close = el('button', 'ghost', '✕ 关闭');
  const all = el('button', 'ghost', '复制全部');
  bar.appendChild(close); bar.appendChild(all); bar.appendChild(el('span', 'grow mut', '长按选择 → 系统「复制」'));
  const ta = el('textarea', 'copyta'); ta.value = termText(); ta.readOnly = true; ta.spellcheck = false;
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
function attachPinch(elm) {
  let startDist = 0, startFont = fontSize;
  const dist = (e) => Math.hypot(e.touches[0].clientX - e.touches[1].clientX, e.touches[0].clientY - e.touches[1].clientY);
  elm.addEventListener('touchstart', e => { if (e.touches.length === 2) { startDist = dist(e); startFont = T.term.options.fontSize; } }, { passive: true });
  elm.addEventListener('touchmove', e => {
    if (e.touches.length === 2 && startDist) {
      const f = Math.max(8, Math.min(28, Math.round(startFont * dist(e) / startDist)));
      if (f !== T.term.options.fontSize) { T.term.options.fontSize = f; try { T.fit.fit(); sendResize(); } catch {} }
    }
  }, { passive: true });
}
function changeFont(delta) {
  fontSize = Math.max(8, Math.min(28, fontSize + delta));
  localStorage.setItem(FONT_KEY, String(fontSize));
  if (T) { T.term.options.fontSize = fontSize; try { T.fit.fit(); sendResize(); } catch {} }
}

// ---- 启动 ------------------------------------------------------------------
function bind() {
  $('#loginBtn').onclick = doLogin;
  $('#otp').addEventListener('keydown', e => { if (e.key === 'Enter') doLogin(); });
  $('#logoutBtn').onclick = logout;
  $('#menuBtn').onclick = openDrawer;
  $('#drawerClose').onclick = closeDrawer;
  $('#notifyBtn').onclick = enablePush;
  $('#workTitle').onclick = () => { if (state.current) renameSession(state.current); };
  $('#fontInc').onclick = () => changeFont(1);
  $('#fontDec').onclick = () => changeFont(-1);

  document.addEventListener('visibilitychange', () => { sendPresence(); if (!document.hidden) reconnectStale(); });
  window.addEventListener('focus', sendPresence);
  window.addEventListener('blur', sendPresence);
  window.addEventListener('online', reconnectStale);

  if ('serviceWorker' in navigator) {
    // The app shell is served network-first, so a reload always picks up a new
    // app.js when online. When the SW itself changes it activates immediately
    // (skipWaiting) and we reload once to apply it.
    navigator.serviceWorker.register(BASE + 'service-worker.js', { scope: BASE }).then(reg => {
      document.addEventListener('visibilitychange', () => { if (!document.hidden) reg.update().catch(() => {}); });
    }).catch(() => {});
    let swReloaded = false;
    navigator.serviceWorker.addEventListener('controllerchange', () => { if (swReloaded) return; swReloaded = true; location.reload(); });
  }
}

async function boot() {
  bind();
  try { const r = await api('/api/me'); state.user = r.user; enterApp(); }
  catch { show('login'); }
}
boot();
