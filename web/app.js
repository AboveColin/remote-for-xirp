/* Remote For Xirp — mobile control surface for the Xirp daemon.
 *
 * Deliberately dependency-free and build-free: the whole UI is three files the
 * Go binary embeds, so deploying is copying one binary.
 *
 * Updates are polled, not streamed. The daemon does broadcast session events
 * over its WebSocket, but a phone that sleeps and wakes has to reconcile state
 * anyway, and a 5s poll of one small JSON payload is less machinery for the
 * same result.
 */

const LIST_POLL_MS = 5000;
const DETAIL_POLL_MS = 4000;
const ACTIVE_STATUSES = ['running', 'idle', 'waiting', 'starting'];

const el = (id) => document.getElementById(id);
const views = {
  login: el('login'),
  welcome: el('welcome'),
  machines: el('machines-view'),
  projects: el('projects-view'),
  scan: el('scan-view'),
  manual: el('manual-view'),
  list: el('list-view'),
  detail: el('detail-view'),
  settings: el('settings-view'),
  diag: el('diag-view'),
  changes: el('changes-view'),
  diff: el('diff-view'),
};

// ---- settings ----
//
// Kept in localStorage rather than on the server: these are per-device reading
// preferences, and the phone and the laptop want different answers. Nothing here
// changes what the daemon does.
// ---- hosts ----
//
// One host is one machine running Xirp. The app is served by one of them, so that
// one is always present as "this machine"; extra hosts are reached cross-origin,
// which their bridge allows only when it requires a key.
const HOSTS_KEY = 'xr.machines';
const LEGACY_HOSTS_KEY = 'xr.hosts';

function loadHosts() {
  let extra = [];
  try {
    extra = JSON.parse(localStorage.getItem(HOSTS_KEY) || localStorage.getItem(LEGACY_HOSTS_KEY) || '[]');
  } catch {
    extra = [];
  }
  return [{ id: 'local', name: 'This machine', url: '', key: '' }, ...extra];
}

let hosts = loadHosts();
let activeHostId = localStorage.getItem('xr.activeHost') || 'local';

// "machine" is the word the UI uses; the storage and helpers predate it.
Object.defineProperty(window, 'machines', { get: () => hosts, set: (v) => (hosts = v) });
Object.defineProperty(window, 'activeMachineId', {
  get: () => activeHostId,
  set: (v) => (activeHostId = v),
});
const saveMachines = () => saveHosts();

function activeHost() {
  return hosts.find((h) => h.id === activeHostId) || hosts[0];
}

function saveHosts() {
  try {
    localStorage.setItem(HOSTS_KEY, JSON.stringify(hosts.filter((h) => h.id !== 'local')));
    localStorage.setItem('xr.activeHost', activeHostId);
  } catch {
    // Nothing actionable; the in-memory list still works for this session.
  }
}

const SETTINGS_KEY = 'xr.settings';
const DEFAULTS = {
  mode: 'chat',
  limit: 40,
  filter: 'active',
  timestamps: 'off',
  sendMode: 'submit',
  theme: 'auto',
};

function loadSettings() {
  try {
    return { ...DEFAULTS, ...JSON.parse(localStorage.getItem(SETTINGS_KEY) || '{}') };
  } catch {
    return { ...DEFAULTS };
  }
}

let settings = loadSettings();

function saveSettings() {
  try {
    localStorage.setItem(SETTINGS_KEY, JSON.stringify(settings));
  } catch {
    // Private browsing or a full quota. The session keeps working with whatever is
    // in memory; silently losing a preference beats an error the user cannot act on.
  }
}

// The stylesheet follows the system by default and honours a `data-theme` override, so
// applying a preference is one attribute. The theme-color meta has to move with it:
// left at a dark value, iOS paints the status bar area dark above a light page.
function applyTheme() {
  const root = document.documentElement;
  if (settings.theme === 'auto') delete root.dataset.theme;
  else root.dataset.theme = settings.theme;
  const dark =
    settings.theme === 'dark' ||
    (settings.theme === 'auto' && matchMedia('(prefers-color-scheme: dark)').matches);
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.content = dark ? '#0f1216' : '#f6f7f9';
}

applyTheme();
// Following the system means following it while the app is open, not only at load.
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (settings.theme === 'auto') applyTheme();
});

let state = {
  view: 'login',
  filter: settings.filter,
  sessions: [],
  sessionId: null,
  timer: null,
  lastError: null,
  query: '',
  project: null,
  modules: null,
  diffPath: null,
};

// ---- plumbing ----

async function api(path, options = {}) {
  const host = activeHost();
  const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
  // The local host authenticates with its cookie; remote hosts get the key as a
  // header, because a cookie for another origin is not ours to send.
  if (host.url && host.key) headers['X-Xirp-Key'] = host.key;
  const res = await fetch((host.url || '') + path, {
    credentials: host.url ? 'omit' : 'same-origin',
    headers,
    ...options,
  });
  if (res.status === 401) {
    show('login');
    throw new Error('unauthorized');
  }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
  return body;
}

// Polling is per-view, and it stops when the page is not being looked at.
//
// Every poll is work on the other end: the daemon's own database panel shows the
// sessions table taking the overwhelming majority of its queries, and an app left open
// in a background tab was contributing to that around the clock while showing nobody
// anything. It also costs phone battery for the same nothing.
function startPolling() {
  if (state.timer) clearInterval(state.timer);
  state.timer = null;
  if (document.hidden) return;
  const view = state.view;
  if (view === 'projects' || view === 'list') state.timer = setInterval(refreshList, LIST_POLL_MS);
  else if (view === 'detail') state.timer = setInterval(refreshDetail, DETAIL_POLL_MS);
  else if (view === 'machines') state.timer = setInterval(renderMachines, 15000);
}

function show(view) {
  state.view = view;
  for (const [name, node] of Object.entries(views)) node.hidden = name !== view;
  startPolling();
  if (view !== 'scan') stopScan();
}

// The dot beside the title reports whether the last poll actually reached the
// daemon. Green is not "the page loaded" — an unreachable Mac still serves this
// page from cache, and a reachable bridge whose daemon died still answers HTTP.
// It goes green only when a request came back with data.
function setLink(ok, why) {
  for (const dot of document.querySelectorAll('.brand-dot')) {
    dot.classList.toggle('ok', ok === true);
    dot.classList.toggle('bad', ok === false);
    dot.title = ok === true ? 'Connected to Xirp' : ok === false ? why || 'Not connected' : 'Connecting…';
  }
}

let toastTimer = null;
function toast(msg) {
  const t = el('toast');
  const text = String(msg);
  // A rejected daemon query comes back as the whole SQL statement. The first sentence is
  // the part that means anything on a phone; the rest stays in the title, and the
  // stylesheet clamps whatever is left to three lines.
  t.textContent = text.length > 180 ? `${text.slice(0, 180)}…` : text;
  t.title = text;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 2600);
}

function ago(iso) {
  if (!iso) return '';
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 45) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

// Working trees can be enormous — a scratch repo here reports 130,610 untracked
// files — and "130610 untracked" in a 12px tag is noise, not information.
function compact(n) {
  if (n < 1000) return String(n);
  if (n < 1e6) return `${(n / 1000).toFixed(n < 10000 ? 1 : 0)}k`;
  return `${(n / 1e6).toFixed(1)}M`;
}

function statusClass(s) {
  const known = ['running', 'idle', 'completed', 'waiting', 'failed', 'stopped'];
  return known.includes(s) ? `pill-${s}` : '';
}

function pct(session) {
  const used = session.contextTokens;
  const size = session.contextWindowSize;
  if (!used || !size) return null;
  return Math.round((used / size) * 100);
}

// ---- login ----

el('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const key = el('login-key').value.trim();
  const err = el('login-error');
  err.hidden = true;
  try {
    await api('/api/auth', { method: 'POST', body: JSON.stringify({ key }) });
    el('login-key').value = '';
    await boot();
  } catch (e2) {
    err.textContent = e2.message === 'unauthorized' ? 'Wrong key.' : e2.message;
    err.hidden = false;
  }
});

// ---- session list ----

// ---- welcome, scanning, adding a machine ----

const SEEN_KEY = 'xr.seen';

function markSeen() {
  try {
    localStorage.setItem(SEEN_KEY, '1');
  } catch {
    /* preference only */
  }
}

el('welcome-here').addEventListener('click', () => {
  markSeen();
  activeMachineId = 'local';
  saveMachines();
  show('machines');
  renderMachines();
});
el('welcome-scan').addEventListener('click', () => openScanner());
el('welcome-manual').addEventListener('click', () => show('manual'));
el('add-machine').addEventListener('click', () => openScanner());
el('machines-settings').addEventListener('click', () => {
  paintSettings();
  renderHosts();
  refreshPairing();
  paintPush();
  show('settings');
});
el('scan-close').addEventListener('click', () => {
  stopScan();
  show(machines.length > 1 || localStorage.getItem(SEEN_KEY) ? 'machines' : 'welcome');
  renderMachines();
});
el('scan-manual').addEventListener('click', () => {
  stopScan();
  show('manual');
});
el('manual-back').addEventListener('click', () => {
  show(localStorage.getItem(SEEN_KEY) ? 'machines' : 'welcome');
  renderMachines();
});

async function openScanner() {
  show('scan');
  const status = el('scan-status');
  status.textContent = 'Starting camera…';
  const ok = await startScan(
    (value) => {
      const parsed = parsePairing(value);
      if (!parsed) {
        status.textContent = 'That code is not a pairing link.';
        setTimeout(() => openScanner(), 1200);
        return;
      }
      addMachine(parsed.host, parsed.url, parsed.key);
    },
    (msg) => {
      status.textContent = msg;
    }
  );
  if (!ok) {
    // No scanner: offer the fallback rather than leaving a dead camera screen.
    el('scan-manual').classList.add('is-primary');
  }
}

// A machine reached at this very origin is "this machine", not a second entry — and
// pairing links point at the origin the app is already served from most of the time.
function addMachine(name, url, key) {
  markSeen();
  const sameOrigin = !url || url === location.origin;
  if (sameOrigin) {
    if (key) {
      fetch('/api/auth', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key }),
      }).catch(() => {});
    }
    activeMachineId = 'local';
    saveMachines();
    toast('Paired with this machine');
    show('machines');
    renderMachines();
    return;
  }
  const existing = machines.find((m) => m.url === url);
  if (existing) {
    existing.key = key || existing.key;
    activeMachineId = existing.id;
    toast(`Updated ${existing.name}`);
  } else {
    const m = { id: `m${Date.now()}`, name, url, key };
    machines.push(m);
    activeMachineId = m.id;
    toast(`Added ${name}`);
  }
  saveMachines();
  show('machines');
  renderMachines();
}

el('manual-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const err = el('manual-error');
  err.hidden = true;
  const raw = el('manual-url').value.trim();
  if (!raw) {
    err.textContent = 'An address is required.';
    err.hidden = false;
    return;
  }
  let url;
  try {
    url = new URL(raw.includes('://') ? raw : 'https://' + raw);
  } catch {
    err.textContent = 'That address is not a URL.';
    err.hidden = false;
    return;
  }
  addMachine(el('manual-name').value.trim() || url.host, url.origin, el('manual-key').value.trim());
  el('manual-name').value = '';
  el('manual-url').value = '';
  el('manual-key').value = '';
});

el('projects-back').addEventListener('click', () => {
  show('machines');
  renderMachines();
});
el('sessions-back').addEventListener('click', () => {
  show('projects');
  renderFolders();
});
el('projects-new').addEventListener('click', openCreateSheet);

// ---- sessions waiting to be restored ----
//
// After Xirp restarts, the sessions it was running need a decision: bring them back or
// let them go. That decision used to require the desk.

async function refreshRestorable() {
  const box = el('restore-banner');
  try {
    const { sessions } = await api('/api/restorable');
    if (!sessions || !sessions.length) {
      box.hidden = true;
      return;
    }
    box.innerHTML = '';
    const h = document.createElement('h2');
    h.textContent = `${sessions.length} session${sessions.length === 1 ? '' : 's'} can be restored`;
    box.append(h);
    for (const s of sessions.slice(0, 6)) {
      const line = document.createElement('div');
      line.className = 'machine-sub subdued';
      line.style.paddingLeft = '0';
      line.textContent = `${s.name || s.id.slice(0, 8)} · ${s.projectName || ''}`;
      box.append(line);
    }
    const actions = document.createElement('div');
    actions.className = 'approval-actions';
    const ids = sessions.map((s) => s.id);
    for (const [label, body, cls] of [
      ['Restore all', { restore: ids }, 'btn btn-accent'],
      ['Dismiss all', { dismiss: ids }, 'btn btn-deny'],
    ]) {
      const b = document.createElement('button');
      b.className = cls;
      b.textContent = label;
      b.onclick = async () => {
        b.disabled = true;
        b.textContent = `${label}…`;
        try {
          const r = await api('/api/restore', { method: 'POST', body: JSON.stringify(body) });
          toast(`Restored ${r.restored}, dismissed ${r.dismissed}`);
        } catch (e) {
          toast(e.message);
        }
        refreshRestorable();
        renderMachines();
      };
      actions.append(b);
    }
    box.append(actions);
    box.hidden = false;
  } catch {
    box.hidden = true;
  }
}

// ---- notifications ----
//
// Three parties have to agree: the browser grants permission, the push service issues a
// subscription, and this server keeps it and signs the messages. Each step can fail on
// its own, so each one says which failed rather than reporting a generic "off".

function urlBase64ToUint8Array(base64) {
  // The VAPID key travels as base64url; the browser wants raw bytes.
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const raw = atob((base64 + padding).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from([...raw].map((c) => c.charCodeAt(0)));
}

function pushSay(msg) {
  el('push-status').textContent = msg;
}

async function pushState() {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) return 'unsupported';
  const reg = await navigator.serviceWorker.getRegistration();
  if (!reg) return 'off';
  const sub = await reg.pushManager.getSubscription();
  return sub ? 'on' : 'off';
}

async function paintPush() {
  const st = await pushState();
  paintSegmented('set-push', st === 'on' ? 'on' : 'off');
  if (st === 'unsupported') {
    pushSay('This browser cannot receive push notifications.');
    return;
  }
  if (Notification.permission === 'denied') {
    pushSay('Notifications are blocked for this site in the browser settings.');
    return;
  }
  try {
    const { subscriptions } = await api('/api/push/key');
    pushSay(
      st === 'on'
        ? `On for this device. ${subscriptions} device${subscriptions === 1 ? '' : 's'} subscribed.`
        : `Off for this device. ${subscriptions} other device${subscriptions === 1 ? '' : 's'} subscribed.`
    );
  } catch {
    pushSay(st === 'on' ? 'On for this device.' : 'Off for this device.');
  }
}

async function pushEnable() {
  try {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
      pushSay('This browser cannot receive push notifications.');
      return;
    }
    const permission = await Notification.requestPermission();
    if (permission !== 'granted') {
      pushSay('Permission was not granted, so nothing can be delivered.');
      paintSegmented('set-push', 'off');
      return;
    }
    const reg = await navigator.serviceWorker.ready;
    const { publicKey } = await api('/api/push/key');
    if (!publicKey) {
      pushSay('The server has no signing key.');
      return;
    }
    const sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(publicKey),
    });
    const raw = sub.toJSON();
    await api('/api/push/subscribe', {
      method: 'POST',
      body: JSON.stringify({
        endpoint: raw.endpoint,
        keys: raw.keys,
        label: navigator.userAgent.includes('Android')
          ? 'Android'
          : /iPhone|iPad/.test(navigator.userAgent)
            ? 'iOS'
            : 'desktop',
      }),
    });
    toast('Notifications on');
    paintPush();
  } catch (e) {
    pushSay(`Could not subscribe: ${e.message}`);
    paintSegmented('set-push', 'off');
  }
}

async function pushDisable() {
  try {
    const reg = await navigator.serviceWorker.getRegistration();
    const sub = reg && (await reg.pushManager.getSubscription());
    if (sub) {
      // Tell the server first: unsubscribing locally would otherwise leave it pushing
      // to an endpoint that no longer accepts anything.
      await api('/api/push/unsubscribe', {
        method: 'POST',
        body: JSON.stringify({ endpoint: sub.endpoint }),
      }).catch(() => {});
      await sub.unsubscribe();
    }
    toast('Notifications off');
    paintPush();
  } catch (e) {
    pushSay(e.message);
  }
}

el('set-push').addEventListener('click', (e) => {
  const btn = e.target.closest('button');
  if (!btn) return;
  if (btn.dataset.value === 'on') pushEnable();
  else pushDisable();
});

el('push-test').addEventListener('click', async () => {
  pushSay('Sending…');
  try {
    const r = await api('/api/push/test', { method: 'POST' });
    pushSay(
      r.sent
        ? `Sent to ${r.sent} device${r.sent === 1 ? '' : 's'}.`
        : `Nothing was sent. ${(r.errors || []).join('; ') || 'No device is subscribed.'}`
    );
  } catch (e) {
    pushSay(e.message);
  }
});

// A notification tap on an already-open app arrives as a message from the worker.
if ('serviceWorker' in navigator) {
  navigator.serviceWorker.addEventListener('message', (event) => {
    const id = event.data && event.data.openSession;
    if (id) openSession(id);
  });
}

// ---- fork, and handing a session to another agent ----

async function openHandover() {
  const sheet = el('handover-sheet');
  el('handover-error').hidden = true;
  sheet.hidden = false;
  // The agent list comes from the same place the create sheet uses, so only agents
  // that are actually installed are offered.
  const sel = el('swap-agent');
  if (!sel.options.length) {
    try {
      const meta = await api('/api/meta');
      for (const a of meta.agents || []) {
        const o = document.createElement('option');
        o.value = a.agentName;
        o.textContent = a.version ? `${a.agentName} ${a.version}` : a.agentName;
        sel.append(o);
      }
    } catch (e) {
      el('handover-error').textContent = e.message;
      el('handover-error').hidden = false;
    }
  }
}

function closeHandover() {
  el('handover-sheet').hidden = true;
}

async function handover(path, body, label, btn) {
  if (!state.sessionId) return;
  const err = el('handover-error');
  err.hidden = true;
  const original = btn.textContent;
  btn.disabled = true;
  btn.textContent = `${label}…`;
  try {
    const res = await api(`/api/sessions/${encodeURIComponent(state.sessionId)}/${path}`, {
      method: 'POST',
      body: JSON.stringify(body),
    });
    closeHandover();
    await refreshList();
    if (res.session && res.session.id && res.session.id !== state.sessionId) {
      toast('Forked');
      openSession(res.session.id);
    } else {
      if (res.session && res.session.name) el('detail-name').textContent = res.session.name;
      toast(res.agent ? `Handed to ${res.agent}` : 'Updated');
      refreshDetail();
    }
  } catch (e) {
    err.textContent = e.message;
    err.hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = original;
  }
}

el('open-handover').addEventListener('click', openHandover);
el('handover-close').addEventListener('click', closeHandover);
el('handover-sheet').addEventListener('click', (e) => {
  if (e.target === el('handover-sheet')) closeHandover();
});
el('rename-go').addEventListener('click', (e) =>
  handover('rename', { name: el('rename-name').value.trim() }, 'Renaming', e.target)
);
el('retitle-go').addEventListener('click', (e) =>
  // The agent writes the title from the conversation, which is usually better than
  // anything typed with a thumb.
  handover('rename', { regenerate: true }, 'Retitling', e.target)
);
el('fork-go').addEventListener('click', (e) =>
  handover('fork', { newBranch: el('fork-branch').value.trim() }, 'Forking', e.target)
);
el('swap-go').addEventListener('click', (e) =>
  handover('swap', { agent: el('swap-agent').value, reason: 'handed over from the phone' }, 'Handing over', e.target)
);

// ---- changes and diffs ----
//
// Two questions, kept separate because the answers differ: what is changed but not
// committed, and what this branch has done against its base. The daemon returns a ready
// unified diff per file, so this only has to render one.

function fileRow(f, mode, sessionId) {
  const row = document.createElement('button');
  row.className = 'filerow';
  row.addEventListener('click', () => openDiff(sessionId, f.path, mode));

  const stat = document.createElement('span');
  const st = (f.status || '?').slice(0, 1);
  stat.className = `filestat st-${st}`;
  stat.textContent = st;

  const path = document.createElement('span');
  path.className = 'filepath';
  // With direction:rtl the browser keeps the end of the path visible; the wrapping
  // marks stop the leading slash being rendered on the wrong side.
  path.textContent = `\u202a${f.path}\u202c`;

  const nums = document.createElement('span');
  nums.className = 'filenums';
  if (f.additions) {
    const a = document.createElement('span');
    a.className = 'add';
    a.textContent = `+${f.additions}`;
    nums.append(a, document.createTextNode(' '));
  }
  if (f.deletions) {
    const d = document.createElement('span');
    d.className = 'del';
    d.textContent = `-${f.deletions}`;
    nums.append(d);
  }

  row.append(stat, path, nums);
  return row;
}

async function refreshChanges() {
  const id = state.sessionId;
  if (!id) return;
  const body = el('changes-body');
  try {
    const d = await api(`/api/sessions/${encodeURIComponent(id)}/changes`);
    setLink(true);
    body.innerHTML = '';
    el('changes-sub').textContent = d.branch ? `${d.branch}${d.base ? ` → ${d.base}` : ''}` : '';

    const pr = el('changes-pr');
    if (d.pr && d.pr.url) {
      pr.innerHTML = '';
      const a = document.createElement('a');
      a.href = d.pr.url;
      a.target = '_blank';
      a.rel = 'noreferrer';
      a.textContent = `#${d.pr.number} ${d.pr.title || ''}`.trim();
      pr.append(document.createTextNode(`${d.pr.state || 'PR'}${d.pr.isDraft ? ' (draft)' : ''} · `), a);
      pr.hidden = false;
    } else {
      pr.hidden = true;
    }

    let total = 0;
    const bd = d.branchDiff;
    if (bd && bd.files && bd.files.length) {
      const h = document.createElement('h3');
      h.textContent = `This branch vs ${d.base}`;
      body.append(h);
      for (const f of bd.files) body.append(fileRow(f, 'branch', id));
      total += bd.files.length;
    }
    const wt = d.worktree;
    if (wt && wt.files && wt.files.length) {
      const h = document.createElement('h3');
      h.textContent = 'Not committed';
      body.append(h);
      for (const f of wt.files) body.append(fileRow(f, 'worktree', id));
      total += wt.files.length;
    }
    if (!total) {
      const p = document.createElement('p');
      p.className = 'subdued';
      p.textContent = d.unavailable || 'Nothing changed.';
      body.append(p);
    }
    const untracked = (wt && wt.untracked) || 0;
    el('changes-foot').textContent =
      (d.branchDiffError ? `${d.branchDiffError} · ` : '') +
      `${total} ${total === 1 ? 'file' : 'files'}` +
      (untracked ? ` · ${compact(untracked)} untracked, not listed` : '');
  } catch (e) {
    if (e.message !== 'unauthorized') {
      setLink(false, e.message);
      body.textContent = e.message;
    }
  }
}

// A unified diff renders line by line. Nothing here reflows or wraps: a diff whose
// columns do not line up is harder to read than one you have to scroll sideways.
function renderDiff(text) {
  const frag = document.createDocumentFragment();
  for (const line of String(text).split('\n')) {
    const div = document.createElement('div');
    let cls = '';
    if (line.startsWith('@@')) cls = 'hunk';
    else if (line.startsWith('+++') || line.startsWith('---') || line.startsWith('diff ') || line.startsWith('index ')) cls = 'meta';
    else if (line.startsWith('+')) cls = 'add';
    else if (line.startsWith('-')) cls = 'del';
    div.className = `diffline ${cls}`.trim();
    div.textContent = line || ' ';
    frag.append(div);
  }
  return frag;
}

async function openDiff(sessionId, path, mode) {
  show('diff');
  state.diffPath = path;
  el('diff-title').textContent = path.split('/').pop();
  el('diff-sub').textContent = mode === 'branch' ? 'vs base branch' : 'not committed';
  const body = el('diff-body');
  body.innerHTML = '';
  el('diff-foot').textContent = 'Loading…';
  try {
    const d = await api(
      `/api/sessions/${encodeURIComponent(sessionId)}/diff?mode=${mode}&path=${encodeURIComponent(path)}`
    );
    if (d.unavailable) {
      el('diff-foot').textContent = d.unavailable;
      return;
    }
    body.append(renderDiff(d.diff || ''));
    const lines = (d.diff || '').split('\n');
    el('diff-foot').textContent =
      `${lines.filter((l) => l.startsWith('+') && !l.startsWith('+++')).length} added, ` +
      `${lines.filter((l) => l.startsWith('-') && !l.startsWith('---')).length} removed` +
      (d.truncated ? ' · truncated' : '');
  } catch (e) {
    if (e.message !== 'unauthorized') el('diff-foot').textContent = e.message;
  }
}

el('open-changes').addEventListener('click', () => {
  show('changes');
  el('changes-title').textContent = el('detail-name').textContent;
  refreshChanges();
});
el('changes-refresh').addEventListener('click', refreshChanges);
el('changes-back').addEventListener('click', () => show('detail'));
el('diff-back').addEventListener('click', () => show('changes'));

// The diff shows what changed; sometimes the question is what the file now says.
el('diff-whole').addEventListener('click', async () => {
  const path = state.diffPath;
  if (!path || !state.sessionId) return;
  el('diff-sub').textContent = 'whole file';
  const body = el('diff-body');
  body.innerHTML = '';
  el('diff-foot').textContent = 'Loading…';
  try {
    const d = await api(
      `/api/sessions/${encodeURIComponent(state.sessionId)}/file?path=${encodeURIComponent(path)}`
    );
    if (d.unavailable) {
      el('diff-foot').textContent = d.unavailable;
      return;
    }
    // Rendered as plain lines, not as a diff: nothing here is an addition or a removal.
    for (const line of (d.content || '').split('\n')) {
      const div = document.createElement('div');
      div.className = 'diffline';
      div.textContent = line || ' ';
      body.append(div);
    }
    el('diff-foot').textContent =
      `${(d.content || '').split('\n').length} lines${d.truncated ? ' · truncated' : ''}`;
  } catch (e) {
    el('diff-foot').textContent = e.message;
  }
});

// ---- diagnostics ----
//
// Three of these change what the app does rather than just being shown: `modules`
// decides whether search is offered at all, `tmux.available` decides whether the
// terminal view can work, and a session's `hasPane` decides whether its pane and
// composer are worth anything.

let diagLevel = 30;

function diagCard(title, rows) {
  const card = document.createElement('div');
  card.className = 'diag-card';
  const h = document.createElement('h3');
  h.textContent = title;
  card.append(h);
  for (const [label, value, tone] of rows) {
    const row = document.createElement('div');
    row.className = 'diag-row' + (tone ? ` is-${tone}` : '');
    const l = document.createElement('span');
    l.textContent = label;
    const v = document.createElement('span');
    v.textContent = value;
    row.append(l, v);
    card.append(row);
  }
  return card;
}

async function refreshDiagnostics() {
  const body = el('diag-body');
  try {
    const d = await api('/api/diagnostics');
    setLink(true);
    body.innerHTML = '';

    body.append(
      diagCard('Daemon', [
        ['Reachable', d.daemon?.reachable ? 'yes' : 'no', d.daemon?.reachable ? 'good' : 'bad'],
        ['Port', d.daemon?.port || d.daemon?.error || '—'],
        ['Update waiting', d.update?.disabled ? 'updates disabled' : d.update?.available ? `yes (${d.update.updateCount})` : 'no'],
      ])
    );

    body.append(
      diagCard('tmux', [
        ['Available', d.tmux?.available ? 'yes' : 'no', d.tmux?.available ? 'good' : 'bad'],
        ['Live panes', String(d.tmux?.panes ?? '—')],
        ['Orphaned', String(d.tmux?.orphaned ?? 0), d.tmux?.orphaned ? 'bad' : ''],
      ])
    );

    if (d.db) {
      const cpm = d.db.callsPerMinute || {};
      const rows = [
        ['Queries / min (1m)', String(Math.round(cpm['1m'] ?? 0))],
        ['Queries / min (15m avg)', String(Math.round(cpm['15m'] ?? 0))],
        ['p95 SELECT', `${d.db.p95Duration?.SELECT ?? '—'} ms`],
      ];
      for (const t of d.db.topTables || []) rows.push([`  ${t.table}`, String(Math.round(t.queries))]);
      body.append(diagCard('Database', rows));
    }

    body.append(diagCard('Modules', (d.modules || []).map((m) => [m, 'active'])));
    el('diag-foot').textContent = `checked ${new Date().toLocaleTimeString()}`;
  } catch (e) {
    if (e.message !== 'unauthorized') {
      setLink(false, e.message);
      body.textContent = e.message;
    }
  }

  try {
    const { records } = await api(`/api/logs?level=${diagLevel}&limit=60`);
    const box = el('diag-log');
    box.innerHTML = '';
    if (!records || !records.length) {
      const p = document.createElement('p');
      p.className = 'subdued';
      p.textContent = 'Nothing at this level.';
      box.append(p);
    }
    for (const r of records || []) {
      const line = document.createElement('div');
      line.className = `logline lvl-${r.level}`;
      const t = document.createElement('time');
      t.textContent = new Date(r.time).toLocaleTimeString();
      const msg = document.createElement('p');
      msg.textContent = r.msg;
      line.append(t, msg);
      box.append(line);
    }
  } catch {
    el('diag-log').textContent = 'Log unavailable.';
  }
}

el('open-diag').addEventListener('click', () => {
  show('diag');
  refreshDiagnostics();
});
el('diag-back').addEventListener('click', () => {
  paintSettings();
  show('settings');
});
el('diag-refresh').addEventListener('click', refreshDiagnostics);
el('diag-levels').addEventListener('click', (e) => {
  const btn = e.target.closest('.chip');
  if (!btn) return;
  diagLevel = Number(btn.dataset.level);
  for (const c of el('diag-levels').children) c.classList.toggle('chip-on', c === btn);
  refreshDiagnostics();
});

// ---- machines screen ----
//
// Cards, one per machine, each probed independently so one unreachable machine does
// not hold up the rest. Probes run on every render (15s while this screen is open),
// which is cheap: /healthz is a fixed 15-byte answer.

// One listener for the whole list, attached once.
el('machines').addEventListener('click', (e) => {
  const card = e.target.closest('.machine');
  if (card && card.dataset.mid) openMachine(card.dataset.mid);
});

let machineRenderBusy = false;
// Cards are built once per machine and then updated in place. Rebuilding the list on
// every 15s poll detached the node under the user's finger, so a tap that landed
// during a refresh did nothing — and it threw away scroll position and focus with it.
const machineCards = new Map();

function machineCard(m) {
  const card = document.createElement('button');
  card.className = 'machine';
  card.dataset.mid = m.id;

  const head = document.createElement('div');
  head.className = 'machine-head';
  const dot = document.createElement('span');
  dot.className = 'machine-dot is-unknown';
  const name = document.createElement('span');
  name.className = 'machine-name';
  const chev = document.createElement('span');
  chev.className = 'machine-chev';
  chev.textContent = '›';
  head.append(dot, name, chev);

  const sub = document.createElement('div');
  sub.className = 'machine-sub subdued';
  const meta = document.createElement('div');
  meta.className = 'machine-meta subdued';
  meta.textContent = 'checking…';

  card.append(head, sub, meta);
  return { card, dot, name, sub, meta };
}

async function renderMachines() {
  if (machineRenderBusy) return;
  machineRenderBusy = true;
  try {
    const box = el('machines');
    const wanted = machines.map((m) => m.id).join(',');
    if (box.dataset.ids !== wanted) {
      // The set of machines changed, so the DOM has to change. This is rare — adding
      // or removing a machine — not something a poll does.
      box.replaceChildren();
      machineCards.clear();
      for (const m of machines) {
        const parts = machineCard(m);
        machineCards.set(m.id, parts);
        box.append(parts.card);
      }
      box.dataset.ids = wanted;
    }

    for (const m of machines) {
      const parts = machineCards.get(m.id);
      if (!parts) continue;
      parts.name.textContent = m.name;
      parts.sub.textContent = m.url ? m.url.replace(/^https?:\/\//, '') : location.host;
      parts.card.classList.toggle('is-active', m.id === activeMachineId);
    }

    let running = 0;
    let online = 0;
    await Promise.all(
      machines.map(async (m) => {
        const parts = machineCards.get(m.id);
        if (!parts) return;
        const health = await probeMachine(m);
        parts.dot.className = 'machine-dot ' + (health.online ? 'is-online' : 'is-offline');
        if (!health.online) {
          parts.meta.textContent = 'unreachable';
          return;
        }
        online++;
        const sum = await machineSummary(m);
        if (sum.needsKey) {
          parts.meta.textContent = 'needs an access key — open settings to add it';
          return;
        }
        if (sum.total === undefined) {
          parts.meta.textContent = `online · ${health.ms} ms`;
          return;
        }
        running += sum.running || 0;
        parts.meta.textContent = `${sum.projects} projects · ${sum.running} running of ${sum.total} · ${health.ms} ms`;
      })
    );

    const stats = el('machine-stats');
    if (!stats.dataset.built) {
      for (let i = 0; i < 2; i++) {
        const stat = document.createElement('div');
        stat.className = 'stat';
        stat.append(document.createElement('strong'), document.createElement('span'));
        stat.lastChild.className = 'subdued';
        stats.append(stat);
      }
      stats.dataset.built = '1';
    }
    const values = [
      [String(running), running === 1 ? 'agent running' : 'agents running'],
      [`${online}/${machines.length}`, 'machines online'],
    ];
    [...stats.children].forEach((stat, i) => {
      stat.firstChild.textContent = values[i][0];
      stat.lastChild.textContent = values[i][1];
    });
    el('machines-foot').textContent = 'Remote For Xirp';
    refreshRestorable();
  } finally {
    machineRenderBusy = false;
  }
}

function openMachine(id) {
  activeMachineId = id;
  saveMachines();
  state.sessions = [];
  state.project = null;
  el('projects-title').textContent = activeHost().name;
  el('projects-sub').textContent = activeHost().url || location.host;
  show('projects');
  refreshList();
}

// ---- folders: the projects on the active machine ----

el('folders').addEventListener('click', (e) => {
  const row = e.target.closest('.folder');
  if (row) openProject(row.dataset.project);
});

function renderFolders() {
  const box = el('folders');
  const shown =
    state.filter === 'active'
      ? state.sessions.filter((s) => ACTIVE_STATUSES.includes(s.status))
      : state.sessions;

  const groups = new Map();
  for (const s of shown) {
    const key = s.projectName || 'No project';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(s);
  }
  const ordered = [...groups.entries()].sort((a, b) => {
    const at = Math.max(...a[1].map((s) => new Date(s.lastActivityAt || 0).getTime()), 0);
    const bt = Math.max(...b[1].map((s) => new Date(s.lastActivityAt || 0).getTime()), 0);
    return bt - at;
  });

  box.innerHTML = '';
  for (const [project, list] of ordered) {
    const row = document.createElement('button');
    row.className = 'folder';
    row.dataset.project = project;

    const swatch = document.createElement('span');
    swatch.className = 'folder-dot';
    swatch.style.background = `hsl(${projectHue(project)} 70% 62%)`;

    const text = document.createElement('span');
    text.className = 'folder-text';
    const nm = document.createElement('strong');
    nm.textContent = project;
    const meta = document.createElement('em');
    meta.className = 'subdued';
    const running = list.filter((s) => s.status === 'running').length;
    const branches = new Set(list.map((s) => s.branch).filter(Boolean));
    const parts = [`${list.length} ${list.length === 1 ? 'session' : 'sessions'}`];
    if (running) parts.push(`${running} running`);
    if (branches.size > 1) parts.push(`${branches.size} branches`);
    meta.textContent = parts.join(' · ');
    text.append(nm, meta);

    const chev = document.createElement('span');
    chev.className = 'folder-chev';
    chev.textContent = '›';

    row.append(swatch, text, chev);
    box.append(row);
  }

  el('list-empty').hidden = ordered.length > 0;
  el('list-foot').textContent = state.lastError
    ? state.lastError
    : `${shown.length} of ${state.sessions.length} sessions · ${ordered.length} projects`;
}

function openProject(project) {
  state.project = project;
  el('sessions-title').textContent = project;
  el('sessions-sub').textContent = activeHost().name;
  show('list');
  renderList();
}

// Sessions are grouped by project, the way the desktop organises them. A flat list
// of 18 sessions across 9 projects is a list you scan; grouped, it is a list you
// navigate. Groups collapse, and the collapsed set is remembered.
const COLLAPSED_KEY = 'xr.collapsed';
let collapsed = new Set(JSON.parse(localStorage.getItem(COLLAPSED_KEY) || '[]'));

function toggleGroup(name) {
  if (collapsed.has(name)) collapsed.delete(name);
  else collapsed.add(name);
  try {
    localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...collapsed]));
  } catch {
    /* preference only */
  }
  renderList();
}

// A stable colour per project, so the same project is the same hue every time.
function projectHue(name) {
  let h = 0;
  for (const ch of String(name)) h = (h * 31 + ch.charCodeAt(0)) % 360;
  return h;
}

function sessionCard(s) {
  const card = document.createElement('button');
  card.className = 'card';
  card.onclick = () => openSession(s.id);

  const top = document.createElement('div');
  top.className = 'card-top';
  const name = document.createElement('div');
  name.className = 'card-name';
  name.textContent = s.name || s.goal || s.id.slice(0, 8);
  const pill = document.createElement('span');
  const noPane = s.hasPane === false && ACTIVE_STATUSES.includes(s.status);
  pill.className = `pill ${noPane ? 'pill-nopane' : statusClass(s.status)}`;
  pill.textContent = noPane ? 'no pane' : s.waitingReason ? 'waiting' : s.status;
  top.append(name, pill);

  const sub = document.createElement('div');
  sub.className = 'card-sub';
  const bits = [];
  if (s.branch) bits.push(s.branch);
  if (s.currentAgent) bits.push(s.currentAgent);
  const p = pct(s);
  if (p !== null) bits.push(`${p}% ctx`);
  if (typeof s.totalCostUsd === 'number' && s.totalCostUsd > 0) bits.push(`$${s.totalCostUsd.toFixed(2)}`);
  if (s.lastActivityAt) bits.push(ago(s.lastActivityAt));
  bits.forEach((b, i) => {
    if (i) {
      const d = document.createElement('span');
      d.className = 'dot';
      d.textContent = '·';
      sub.append(d);
    }
    const span = document.createElement('span');
    span.textContent = b;
    sub.append(span);
  });

  card.append(top, sub);
  if (s.lastUserMessage) {
    const last = document.createElement('div');
    last.className = 'card-last';
    last.textContent = s.lastUserMessage;
    card.append(last);
  }
  return card;
}

function renderList() {
  const wrap = el('sessions');
  const all =
    state.filter === 'active'
      ? state.sessions.filter((s) => ACTIVE_STATUSES.includes(s.status))
      : state.sessions;
  const shown = all.filter((s) => (s.projectName || 'No project') === state.project);

  shown.sort((a, b) => {
    const ar = a.status === 'running' ? 0 : 1;
    const br = b.status === 'running' ? 0 : 1;
    if (ar !== br) return ar - br;
    return new Date(b.lastActivityAt || 0) - new Date(a.lastActivityAt || 0);
  });

  wrap.innerHTML = '';
  for (const s of shown) wrap.append(sessionCard(s));
  el('sessions-empty').hidden = shown.length > 0;
  el('sessions-foot').textContent = `${shown.length} ${shown.length === 1 ? 'session' : 'sessions'} · ${state.project}`;
}

async function refreshList() {
  // With a query on screen the 5s poll would overwrite the results with the
  // session list a moment after they appeared.
  if (state.query) return;
  try {
    const body = await api('/api/sessions');
    state.sessions = body.sessions || [];
    if (body.modules) {
      state.modules = body.modules;
      // Search is a module. Where the edition does not have it, the box can only ever
      // return nothing, so it is removed rather than left to disappoint.
      const searchable = state.modules.includes('session-search');
      el('search').closest('.searchbar').hidden = !searchable;
    }
    state.lastError = null;
    setLink(true);
  } catch (e) {
    if (e.message === 'unauthorized') return;
    state.lastError = e.message;
    setLink(false, e.message);
  }
  if (state.view === 'projects') renderFolders();
  else if (state.view === 'list') renderList();
  refreshApprovals();
}

// ---- search ----
//
// Search hits the daemon's full-text index over metadata, messages and JSONL
// transcripts, so it finds sessions the list cannot show — including completed
// ones from months ago. It replaces the list while a query is present rather than
// opening a second screen; on a phone, fewer places to be is worth more than
// keeping both views alive.

// Search hits sessions the daemon never named, which come back as the literal
// name "Session". Their ids carry the only useful label: transcript-only sessions
// are `rollout-<ISO date>-<uuid>`, so show the date rather than the word "rollout"
// truncated to nothing.
function searchLabel(r) {
  if (r.name && r.name !== 'Session') return r.name;
  const m = /^rollout-(\d{4}-\d{2}-\d{2})/.exec(r.sessionId);
  if (m) return `session of ${m[1]}`;
  return r.sessionId.slice(0, 8);
}

function renderSearch(results, query) {
  const wrap = el('sessions');
  wrap.innerHTML = '';
  for (const r of results) {
    const card = document.createElement('button');
    card.className = 'card';
    card.onclick = () => openSession(r.sessionId);

    const top = document.createElement('div');
    top.className = 'card-top';
    const name = document.createElement('div');
    name.className = 'card-name';
    name.textContent = searchLabel(r);
    const pill = document.createElement('span');
    pill.className = `pill ${statusClass(r.status)}`;
    pill.textContent = r.status || '?';
    top.append(name, pill);

    const sub = document.createElement('div');
    sub.className = 'card-sub';
    const bits = [];
    if (r.matchField) bits.push(`match: ${r.matchField}`);
    if (r.lastActivityAt) bits.push(ago(r.lastActivityAt));
    sub.textContent = bits.join(' · ');

    card.append(top, sub);
    if (r.snippet) {
      const snip = document.createElement('div');
      snip.className = 'card-last';
      snip.textContent = r.snippet;
      card.append(snip);
    }
    wrap.append(card);
  }
  el('list-empty').hidden = results.length > 0;
  el('list-foot').textContent = `${results.length} result${results.length === 1 ? '' : 's'} for "${query}"`;
}

let searchTimer = null;
el('search').addEventListener('input', () => {
  clearTimeout(searchTimer);
  const q = el('search').value.trim();
  if (!q) {
    state.query = '';
    refreshList();
    return;
  }
  // The daemon greps every transcript on disk for this, so it is not free.
  // Wait for a pause in typing rather than firing per keystroke.
  searchTimer = setTimeout(async () => {
    state.query = q;
    el('list-foot').textContent = 'Searching…';
    try {
      const { results } = await api(`/api/search?q=${encodeURIComponent(q)}`);
      if (state.query !== q) return; // a newer query already won
      setLink(true);
      renderSearch(results || [], q);
    } catch (e) {
      if (e.message !== 'unauthorized') {
        setLink(false, e.message);
        el('list-foot').textContent = e.message;
      }
    }
  }, 400);
});

// ---- new session ----

async function openCreateSheet() {
  const sheet = el('create-sheet');
  const err = el('create-error');
  err.hidden = true;
  sheet.hidden = false;
  try {
    const meta = await api('/api/meta');
    const projects = meta.projects || [];
    const agents = meta.agents || [];

    // Most recently active project first: that is nearly always the one you mean.
    projects.sort((a, b) => new Date(b.lastActivityAt || 0) - new Date(a.lastActivityAt || 0));
    const psel = el('create-project');
    psel.innerHTML = '';
    for (const p of projects) {
      const o = document.createElement('option');
      o.value = p.id;
      o.textContent = p.name;
      psel.append(o);
    }

    const asel = el('create-agent');
    asel.innerHTML = '';
    for (const a of agents) {
      const o = document.createElement('option');
      o.value = a.agentName;
      o.textContent = a.version ? `${a.agentName} ${a.version}` : a.agentName;
      asel.append(o);
    }
    if (!agents.length) {
      const o = document.createElement('option');
      o.value = '';
      o.textContent = 'default';
      asel.append(o);
    }
    await loadModels();
  } catch (e) {
    err.textContent = e.message;
    err.hidden = false;
  }
}

async function loadModels() {
  const agent = el('create-agent').value;
  const sel = el('create-model');
  sel.innerHTML = '';
  const dflt = document.createElement('option');
  dflt.value = '';
  dflt.textContent = "agent's default";
  sel.append(dflt);
  if (!agent) return;
  try {
    const { models } = await api(`/api/models?agent=${encodeURIComponent(agent)}`);
    for (const m of models || []) {
      const o = document.createElement('option');
      o.value = m.model;
      // Context window and input price are what the choice actually turns on.
      const ctx = m.contextWindowSize ? `${Math.round(m.contextWindowSize / 1000)}k` : '?';
      const price = m.inputPerMillion != null ? ` · $${m.inputPerMillion}/M in` : '';
      o.textContent = `${m.model} — ${ctx}${price}`;
      sel.append(o);
    }
  } catch {
    // Leaving just "agent's default" is a fine outcome; the agent picks.
  }
}

el('create-agent').addEventListener('change', loadModels);
el('new-session').addEventListener('click', openCreateSheet);
el('create-close').addEventListener('click', () => (el('create-sheet').hidden = true));
el('create-sheet').addEventListener('click', (e) => {
  if (e.target === el('create-sheet')) el('create-sheet').hidden = true;
});

el('create-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const btn = el('create-submit');
  const err = el('create-error');
  err.hidden = true;
  const useTerminal = el('create-terminal').checked;
  const body = {
    projectId: el('create-project').value,
    goal: el('create-goal').value.trim(),
    agent: el('create-agent').value,
    model: el('create-model').value,
    newBranch: el('create-branch').value.trim(),
    useTerminal,
  };
  if (!body.goal && !useTerminal) {
    err.textContent = 'A goal is required for an agent session.';
    err.hidden = false;
    return;
  }
  btn.disabled = true;
  btn.textContent = 'Creating…';
  try {
    const res = await api('/api/sessions', { method: 'POST', body: JSON.stringify(body) });
    el('create-sheet').hidden = true;
    el('create-goal').value = '';
    el('create-branch').value = '';
    toast('Session created');
    await refreshList();
    if (res.session && res.session.id) openSession(res.session.id);
  } catch (e2) {
    err.textContent = e2.message;
    err.hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Create';
  }
});

// ---- git state for a session ----

// ---- URLs the agent printed, and what it committed ----

async function refreshExtras(id) {
  // Tapping a dev-server address the agent printed is the single most useful thing
  // this screen can offer, so URLs load for every session.
  try {
    const { urls } = await api(`/api/sessions/${encodeURIComponent(id)}/urls`);
    const box = el('detail-urls');
    box.innerHTML = '';
    // Loopback addresses are useless from a phone — they would resolve to the
    // phone itself. Show only what a remote device can actually reach.
    const usable = (urls || []).filter((u) => !/^https?:\/\/(127\.0\.0\.1|localhost|0\.0\.0\.0)/.test(u));
    for (const u of usable.slice(0, 8)) {
      const a = document.createElement('a');
      a.href = u;
      a.target = '_blank';
      a.rel = 'noreferrer';
      a.textContent = u.replace(/^https?:\/\//, '');
      box.append(a);
    }
    box.hidden = usable.length === 0;
  } catch {
    el('detail-urls').hidden = true;
  }

  try {
    const { commits } = await api(`/api/sessions/${encodeURIComponent(id)}/log`);
    const body = el('commits-body');
    body.innerHTML = '';
    for (const c of commits || []) {
      const row = document.createElement('div');
      row.className = 'commit';
      const code = document.createElement('code');
      code.textContent = c.shortHash || (c.hash || '').slice(0, 7);
      const txt = document.createElement('span');
      txt.textContent = c.subject || c.message || '';
      row.append(code, txt);
      body.append(row);
    }
    el('detail-commits').hidden = !(commits && commits.length);
  } catch {
    el('detail-commits').hidden = true;
  }
}

el('ack').addEventListener('click', async () => {
  if (!state.sessionId) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(state.sessionId)}/ack`, { method: 'POST' });
    toast('Acknowledged');
    el('ack').hidden = true;
  } catch (e) {
    toast(e.message);
  }
});

async function refreshGit(id) {
  const box = el('detail-git');
  try {
    const g = await api(`/api/sessions/${encodeURIComponent(id)}/git`);
    if (g.unavailable) {
      box.hidden = true;
      return;
    }
    box.innerHTML = '';
    const tags = [];
    if (g.branch) tags.push(['tag', g.branch]);
    if (g.clean) {
      tags.push(['tag git-clean', 'working tree clean']);
    } else {
      if (g.staged) tags.push(['tag git-dirty', `${compact(g.staged)} staged`]);
      if (g.modified) tags.push(['tag git-dirty', `${compact(g.modified)} modified`]);
      if (g.untracked) tags.push(['tag', `${compact(g.untracked)} untracked`]);
    }
    for (const [cls, text] of tags) {
      const s = document.createElement('span');
      s.className = cls;
      s.textContent = text;
      box.append(s);
    }
    box.hidden = tags.length === 0;
  } catch {
    box.hidden = true;
  }
}

// ---- settings screen ----

function paintSegmented(id, value) {
  for (const b of el(id).children) b.classList.toggle('on', b.dataset.value === String(value));
}

function paintSettings() {
  paintSegmented('set-mode', settings.mode);
  paintSegmented('set-limit', settings.limit);
  paintSegmented('set-filter', settings.filter);
  paintSegmented('set-timestamps', settings.timestamps);
  paintSegmented('set-theme', settings.theme);
  // The same switch also sits on the session screen, which is where the choice is
  // actually made; both write one setting, so both have to be repainted.
  paintSegmented('detail-mode', settings.mode);
}

function wireSegmented(id, key, cast = (v) => v) {
  el(id).addEventListener('click', (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    settings[key] = cast(btn.dataset.value);
    saveSettings();
    paintSettings();
    // Apply immediately: a setting you have to leave and come back to feel applied
    // reads as broken.
    if (key === 'filter') {
      state.filter = settings.filter;
      for (const c of el('filters').children) {
        c.classList.toggle('chip-on', c.dataset.filter === settings.filter);
      }
    }
    if (key === 'mode') setTerminalMode(settings.mode === 'terminal');
    if (key === 'theme') applyTheme();
    if (state.sessionId && settings.mode !== 'terminal') refreshDetail();
  });
}

wireSegmented('set-mode', 'mode');
wireSegmented('set-limit', 'limit', Number);
wireSegmented('set-filter', 'filter');
wireSegmented('set-timestamps', 'timestamps');
wireSegmented('set-theme', 'theme');
wireSegmented('detail-mode', 'mode');

// Paint the controls once at startup. The session screen's view switch is reachable
// without ever opening Settings, and an unpainted segmented control shows no selection
// at all — which reads as "none of these is active".
paintSettings();

el('settings-back').addEventListener('click', () => {
  show('machines');
  renderMachines();
});

el('filters').addEventListener('click', (e) => {
  const btn = e.target.closest('.chip');
  if (!btn) return;
  state.filter = btn.dataset.filter;
  for (const c of el('filters').children) c.classList.toggle('chip-on', c === btn);
  renderList();
});

el('refresh').addEventListener('click', () => {
  if (state.view === 'detail') refreshDetail();
  else refreshList();
});

// ---- approvals ----
//
// The daemon only holds a permission request open for 500ms before falling
// through to the agent's own dialog (permissionService.waitForDecision caps its
// wait at Math.min(timeout, 500)). So this block is almost always empty and is
// not a remote-approval workflow; it renders only when a request happens to be
// live, and stays out of the way otherwise.

async function refreshApprovals() {
  const box = el('approvals');
  let requests = [];
  try {
    const body = await api('/api/permissions');
    requests = body.requests || [];
  } catch {
    box.hidden = true;
    return;
  }
  if (!requests.length) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  box.innerHTML = '<h2>Waiting on you</h2>';
  for (const r of requests) {
    const wrap = document.createElement('div');
    wrap.className = 'approval';

    const tool = document.createElement('div');
    tool.className = 'approval-tool';
    tool.textContent = r.toolName || 'permission request';
    wrap.append(tool);

    if (r.toolInput) {
      const inp = document.createElement('div');
      inp.className = 'approval-input';
      inp.textContent =
        typeof r.toolInput === 'string' ? r.toolInput : JSON.stringify(r.toolInput, null, 1);
      wrap.append(inp);
    }

    const actions = document.createElement('div');
    actions.className = 'approval-actions';
    for (const behavior of ['allow', 'deny']) {
      const btn = document.createElement('button');
      btn.className = `btn ${behavior === 'allow' ? 'btn-accent' : 'btn-deny'}`;
      btn.textContent = behavior === 'allow' ? 'Allow' : 'Deny';
      btn.onclick = async () => {
        try {
          await api(`/api/permissions/${encodeURIComponent(r.id)}`, {
            method: 'POST',
            body: JSON.stringify({ behavior }),
          });
          toast(`${behavior === 'allow' ? 'Allowed' : 'Denied'}`);
        } catch (e) {
          toast(e.message);
        }
        refreshApprovals();
      };
      actions.append(btn);
    }
    wrap.append(actions);
    box.append(wrap);
  }
}

// ---- session detail ----

function openSession(id) {
  state.sessionId = id;
  el('transcript').innerHTML = '';
  el('detail-meta').innerHTML = '';
  el('detail-git').hidden = true;
  el('detail-urls').hidden = true;
  el('detail-commits').hidden = true;
  el('ack').hidden = true;
  el('detail-name').textContent = 'Loading…';
  el('detail-sub').textContent = '';
  show('detail');
  setTerminalMode(settings.mode === 'terminal');
  if (settings.mode === 'terminal') return;
  // Git state loads after the transcript rather than alongside it. Both travel
  // over the same serialised daemon connection, so issuing them together only
  // decides which one waits, and the transcript is what the screen is for.
  // Measured on this machine: git 1.07s even on a tree with 130k untracked files,
  // transcript 0.43s. Git is fetched once per open, not on every poll.
  refreshDetail(true)
    .then(() => refreshGit(id))
    .then(() => refreshExtras(id));
}

el('back').addEventListener('click', () => {
  setTerminalMode(false);
  state.sessionId = null;
  show('list');
  renderList();
  refreshList();
});

async function refreshDetail(scroll = false) {
  const id = state.sessionId;
  if (!id) return;
  let body;
  try {
    body = await api(`/api/sessions/${encodeURIComponent(id)}?limit=${settings.limit}`);
    setLink(true);
  } catch (e) {
    if (e.message !== 'unauthorized') {
      toast(e.message);
      setLink(false, e.message);
    }
    return;
  }
  const s = body.session || {};
  el('detail-name').textContent = s.name || s.goal || id.slice(0, 8);
  el('detail-sub').textContent = [s.projectName, s.branch].filter(Boolean).join(' · ');

  el('ack').hidden = !(s.status === 'completed' || s.status === 'failed');

  // No pane means the terminal has nothing to render and a message would be accepted
  // and dropped, since session:message is fire-and-forget. Say that plainly.
  const noPane = s.hasPane === false && ACTIVE_STATUSES.includes(s.status);
  el('nopane-note').hidden = !noPane;
  el('composer').hidden = noPane;

  // Status leads and is the only coloured thing here; the rest is one grey line of
  // reference facts, separated by the stylesheet rather than by more boxes.
  const meta = el('detail-meta');
  meta.innerHTML = '';
  const status = document.createElement('span');
  status.className = `pill ${statusClass(s.status)}`;
  status.textContent = s.waitingReason ? `waiting: ${s.waitingReason}` : s.status;
  meta.append(status);

  const tags = [];
  if (s.currentAgent) tags.push(s.currentAgent);
  if (s.model) tags.push(s.model);
  const p = pct(s);
  if (p !== null) tags.push(`${p}% of ${Math.round(s.contextWindowSize / 1000)}k ctx`);
  if (typeof s.totalCostUsd === 'number' && s.totalCostUsd > 0) {
    tags.push(`$${s.totalCostUsd.toFixed(2)}`);
  }
  if (s.lastActivityAt) tags.push(ago(s.lastActivityAt));
  for (const t of tags) {
    const tag = document.createElement('span');
    tag.className = 'tag';
    tag.textContent = t;
    meta.append(tag);
  }

  renderTranscript(body, s, scroll);
}

// ---- transcript ----
//
// Rendered as a chat: runs of the same speaker are grouped under one label, and in
// chat mode the agent's tool activity collapses into a single tappable line rather
// than vanishing, so you can see that it did twelve things without reading twelve
// things.
//
// The whole transcript is rebuilt on each poll, which would fight the user's scroll
// position every few seconds. A signature of what is on screen is compared first and
// an unchanged transcript is left alone.
function transcriptSignature(msgs, all, s) {
  // The signature has to change whenever the newest entry changes, including tool
  // entries that chat mode hides: they are what the working indicator and the tool
  // chips are built from.
  return [
    settings.mode,
    settings.timestamps,
    s.status,
    msgs.length,
    all.length,
    all.length ? all[all.length - 1].id : '',
  ].join('|');
}

function bubbleFor(m) {
  const div = document.createElement('div');
  const role = m.role || 'system';
  div.className = `msg msg-${role}`;
  if (role === 'assistant' && window.renderMarkdown) {
    // Agent replies are markdown. User messages are left as literal text: what you
    // typed should appear exactly as you typed it.
    div.append(window.renderMarkdown(m.text || ''));
  } else {
    div.append(document.createTextNode(m.text || `(${m.type || 'no text'})`));
  }
  if (settings.timestamps === 'on' && m.ts) {
    const t = document.createElement('span');
    t.className = 'msg-time';
    t.textContent = new Date(m.ts).toLocaleTimeString();
    div.append(t);
  }
  return div;
}

function toolChip(entries) {
  const wrap = document.createElement('div');
  wrap.className = 'tool-run';

  const chip = document.createElement('button');
  chip.className = 'tool-chip';
  const names = [...new Set(entries.map((e) => (/^([A-Za-z_]+)\(/.exec(e.text || '') || [])[1]).filter(Boolean))];
  const label = names.length ? names.slice(0, 3).join(', ') : 'tool activity';
  chip.textContent = `${entries.length} step${entries.length === 1 ? '' : 's'} · ${label}`;

  const body = document.createElement('div');
  body.className = 'tool-body';
  body.hidden = true;
  for (const e of entries) {
    const line = document.createElement('div');
    line.className = 'msg msg-tool';
    const label2 = document.createElement('span');
    label2.className = 'msg-role';
    label2.textContent = `${e.role || 'tool'} · ${e.type || ''}`;
    line.append(label2, document.createTextNode(e.text || ''));
    body.append(line);
  }
  chip.addEventListener('click', () => {
    body.hidden = !body.hidden;
    chip.classList.toggle('open', !body.hidden);
  });

  wrap.append(chip, body);
  return wrap;
}

function renderTranscript(body, s, forceScroll) {
  const tr = el('transcript');
  const all = body.messages || [];
  const isTool = (m) => m.type === 'tool_use' || m.type === 'tool_result';
  const chat = settings.mode === 'chat';
  const shown = chat ? all.filter((m) => !isTool(m)) : all;

  const sig = transcriptSignature(shown, all, s);
  const atBottom = window.innerHeight + window.scrollY >= document.body.offsetHeight - 140;
  if (tr.dataset.sig === sig && !forceScroll) {
    updateJump();
    return;
  }
  tr.dataset.sig = sig;
  tr.innerHTML = '';

  if (!shown.length) {
    const note = document.createElement('p');
    note.className = 'subdued';
    note.textContent = body.transcriptError
      ? `No transcript: ${body.transcriptError}`
      : all.length
        ? 'Only tool activity so far — switch to Full in settings to see it.'
        : 'No messages yet.';
    tr.append(note);
  }

  if (body.truncatedFromStart && body.totalMessages) {
    const note = document.createElement('div');
    note.className = 'older-note subdued';
    // Both numbers must count the same thing. `shown` is post-filter, so quoting it
    // against the total entry count read as "11 of 633" when 11 was chat messages and
    // 633 was every entry including tool calls.
    note.textContent = `Showing the last ${all.length} of ${body.totalMessages} entries`;
    tr.append(note);
  }

  let lastRole = null;
  let pendingTools = [];

  const flushTools = () => {
    if (!pendingTools.length) return;
    tr.append(toolChip(pendingTools));
    pendingTools = [];
    lastRole = null;
  };

  for (const m of all) {
    if (isTool(m)) {
      // In chat mode tool entries are collected into one chip; in full mode they are
      // shown as they come.
      if (chat) pendingTools.push(m);
      else {
        flushTools();
        tr.append(toolChip([m]));
      }
      continue;
    }
    flushTools();
    const role = m.role || 'system';
    if (role !== lastRole) {
      const label = document.createElement('div');
      label.className = `speaker speaker-${role}`;
      label.textContent = role === 'user' ? 'You' : role === 'assistant' ? 'Agent' : role;
      tr.append(label);
      lastRole = role;
    }
    tr.append(bubbleFor(m));
  }
  flushTools();

  // Whether the agent is working cannot be read from `status`. The daemon's statuses
  // are running / waiting / waiting_on_parent / finished / idle, and `running` means
  // the session is live, not that anything is happening: two sessions here were
  // `running` with 713 and 756 minutes since their last activity, which made the
  // working indicator stick on forever.
  //
  // lastActivityAt is no good either — a genuinely busy session showed 2 minutes
  // stale while mid-turn, so any recency threshold flickers.
  //
  // What does hold: the agent owes a reply unless the last thing in the transcript is
  // its own message. A trailing user message, tool call or tool result all mean a turn
  // is in flight.
  // One more guard: a session killed part-way through a tool call leaves a trailing
  // tool entry forever, which would look like eternal work. Observed staleness split
  // cleanly — a working session was 2 minutes behind, the two dead-but-running ones
  // 713 and 756 minutes — so an hour sits well above any real turn and well below
  // those. Only something broken reaches it.
  const STALE_MS = 60 * 60 * 1000;
  const fresh = !s.lastActivityAt || Date.now() - new Date(s.lastActivityAt).getTime() < STALE_MS;
  const tail = all.length ? all[all.length - 1] : null;
  const agentOwesReply =
    tail && !(tail.role === 'assistant' && (tail.type === 'message' || !tail.type));
  if (ACTIVE_STATUSES.includes(s.status) && agentOwesReply && fresh) {
    const typing = document.createElement('div');
    typing.className = 'typing';
    for (let i = 0; i < 3; i++) typing.append(document.createElement('span'));
    tr.append(typing);
  }

  if (forceScroll || atBottom) window.scrollTo(0, document.body.scrollHeight);
  updateJump();

}

// ---- terminal: the session's actual tmux pane ----
//
// Polled rather than streamed. A capture is the whole pane every time, so a dropped
// frame self-heals on the next one; an incremental stream would need to replay
// cursor motion to stay correct. 1.2s is fast enough to follow a build and slow
// enough that a phone radio is not held awake continuously.
const PANE_POLL_MS = 1200;
let paneTimer = null;

async function refreshPane() {
  const id = state.sessionId;
  if (!id || settings.mode !== 'terminal') return;
  const box = el('terminal');
  try {
    const body = await api(`/api/sessions/${encodeURIComponent(id)}/pane?lines=400`);
    setLink(true);
    if (body.unavailable) {
      box.textContent = body.unavailable;
      return;
    }
    const nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
    box.innerHTML = '';
    box.append(window.renderAnsi ? window.renderAnsi(body.text || '') : document.createTextNode(body.text || ''));
    // Panes are read bottom-up: new output arrives at the end. Stay pinned there
    // unless the user has scrolled up to read something.
    if (nearBottom) box.scrollTop = box.scrollHeight;
    el('detail-sub').textContent = body.size ? `${body.pane.slice(0, 12)}… · ${body.size}` : body.pane;
  } catch (e) {
    if (e.message !== 'unauthorized') {
      setLink(false, e.message);
      box.textContent = e.message;
    }
  }
}

function setTerminalMode(on) {
  el('terminal-wrap').hidden = !on;
  el('transcript').hidden = on;
  el('detail-commits').hidden = on || el('detail-commits').hidden;
  el('detail-urls').hidden = on || el('detail-urls').hidden;
  if (paneTimer) clearInterval(paneTimer);
  paneTimer = null;
  if (on && !document.hidden) {
    refreshPane();
    paneTimer = setInterval(refreshPane, PANE_POLL_MS);
  }
}

// Keys a TUI needs that a text field cannot send. Without arrows and Escape you
// cannot use a slash-command menu at all.
el('keyrow').addEventListener('click', async (e) => {
  const btn = e.target.closest('button');
  if (!btn || !state.sessionId) return;
  if (btn.dataset.slash) {
    // Typing "/" opens the agent's own command menu, which then needs arrows.
    try {
      await api(`/api/sessions/${encodeURIComponent(state.sessionId)}/message`, {
        method: 'POST',
        body: JSON.stringify({ text: '/', enter: false }),
      });
      setTimeout(refreshPane, 250);
    } catch (err) {
      toast(err.message);
    }
    return;
  }
  try {
    await api(`/api/sessions/${encodeURIComponent(state.sessionId)}/keys?key=${btn.dataset.key}`, {
      method: 'POST',
    });
    setTimeout(refreshPane, 250);
  } catch (err) {
    toast(err.message);
  }
});

// ---- hosts and pairing UI ----

function renderHosts() {
  const box = el('host-list');
  box.innerHTML = '';
  for (const h of hosts) {
    const row = document.createElement('div');
    row.className = 'host-row' + (h.id === activeHostId ? ' is-active' : '');

    const pick = document.createElement('button');
    pick.className = 'host-pick';
    pick.onclick = () => {
      activeHostId = h.id;
      saveHosts();
      renderHosts();
      state.sessions = [];
      refreshList();
      toast(`Switched to ${h.name}`);
    };
    const nm = document.createElement('span');
    nm.className = 'host-name';
    nm.textContent = h.name;
    const where = document.createElement('span');
    where.className = 'host-url subdued';
    where.textContent = h.url || location.host;
    pick.append(nm, where);
    row.append(pick);

    if (h.id !== 'local') {
      const del = document.createElement('button');
      del.className = 'host-del';
      del.textContent = '×';
      del.setAttribute('aria-label', `Remove ${h.name}`);
      del.onclick = () => {
        hosts = hosts.filter((x) => x.id !== h.id);
        if (activeHostId === h.id) activeHostId = 'local';
        saveHosts();
        renderHosts();
        refreshList();
      };
      row.append(del);
    }
    box.append(row);
  }
}

el('host-add').addEventListener('submit', (e) => {
  e.preventDefault();
  const url = el('host-url').value.trim().replace(/\/$/, '');
  if (!url) return;
  hosts.push({
    id: `h${Date.now()}`,
    name: el('host-name').value.trim() || new URL(url).host,
    url,
    key: el('host-key').value.trim(),
  });
  saveHosts();
  el('host-name').value = '';
  el('host-url').value = '';
  el('host-key').value = '';
  renderHosts();
  toast('Host added');
});

async function refreshPairing() {
  try {
    const body = await api('/api/pair');
    el('pair-url').textContent = body.hasKey
      ? body.url.replace(/#k=.*/, '#k=…')
      : `${body.url} (no key set — run: xirp-remote install --generate-key)`;
    el('pair-qr').src = (activeHost().url || '') + '/api/pair?format=png';
  } catch {
    el('pair-url').textContent = 'Pairing code unavailable.';
  }
}

// ---- jump to latest ----
//
// A long transcript plus a 4s poll means you can be reading history when new output
// lands. Nothing yanks the page; this button appears instead.
function updateJump() {
  const btn = el('jump-latest');
  if (!btn) return;
  const near = window.innerHeight + window.scrollY >= document.body.offsetHeight - 140;
  btn.hidden = state.view !== 'detail' || near;
}

el('jump-latest').addEventListener('click', () => {
  window.scrollTo({ top: document.body.scrollHeight, behavior: 'smooth' });
  el('jump-latest').hidden = true;
});

window.addEventListener('scroll', updateJump, { passive: true });

// ---- composer ----

// Two ways to deliver text. "Submit" types it and presses Enter, so the agent acts on
// it. "Type" types it and stops, leaving the text sitting in the agent's input for you
// or for whoever is at the desk. The daemon draws this distinction itself, as the
// `enter` flag on session:message.
function paintSendMode() {
  const btn = el('send-mode');
  const submit = settings.sendMode !== 'type';
  btn.textContent = submit ? '⏎' : '⌨';
  btn.classList.toggle('on', !submit);
  btn.title = submit
    ? 'Submit: send the message and let the agent run it'
    : "Type only: leave the text in the agent's input, unsent";
  el('send').textContent = submit ? 'Send' : 'Type';
}

el('send-mode').addEventListener('click', () => {
  settings.sendMode = settings.sendMode === 'type' ? 'submit' : 'type';
  saveSettings();
  paintSendMode();
});

const textarea = el('composer-text');

// Enter sends. In a textarea it inserts a newline by default, so the only way to
// send was the button — which on a phone means reaching for it after every message.
// Shift+Enter (and Alt+Enter, for keyboards without a comfortable Shift) still
// inserts a newline.
//
// `isComposing` must be respected: while an IME candidate window is open, Enter
// commits the candidate and must not send a half-typed word.
textarea.addEventListener('keydown', (e) => {
  if (e.key !== 'Enter') return;
  if (e.shiftKey || e.altKey || e.isComposing) return;
  e.preventDefault();
  el('composer').requestSubmit ? el('composer').requestSubmit() : el('send').click();
});

textarea.addEventListener('input', () => {
  textarea.style.height = 'auto';
  textarea.style.height = `${Math.min(textarea.scrollHeight, 140)}px`;
});

el('composer').addEventListener('submit', async (e) => {
  e.preventDefault();
  const text = textarea.value;
  if (!text.trim() || !state.sessionId) return;
  el('send').disabled = true;
  try {
    await api(`/api/sessions/${encodeURIComponent(state.sessionId)}/message`, {
      method: 'POST',
      body: JSON.stringify({ text, enter: settings.sendMode !== 'type' }),
    });
    textarea.value = '';
    textarea.style.height = 'auto';
    toast(settings.sendMode === 'type' ? "Left in the agent's input" : 'Sent');
    setTimeout(() => refreshDetail(true), 1200);
  } catch (e2) {
    toast(e2.message);
  } finally {
    el('send').disabled = false;
  }
});

el('stop').addEventListener('click', async () => {
  if (!state.sessionId) return;
  if (!confirm('Stop this session?')) return;
  try {
    const res = await api(`/api/sessions/${encodeURIComponent(state.sessionId)}/stop`, {
      method: 'POST',
    });
    toast(`Stopped${res.status ? ` (${res.status})` : ''}`);
    refreshDetail();
  } catch (e) {
    toast(e.message);
  }
});

// ---- boot ----

// A pairing link carries the key in the fragment. Exchange it for a cookie and get
// it out of the address bar immediately: a URL with a key in it gets shared, gets
// screenshotted, and sits in history.
async function consumeDeepLink() {
  const m = /[#&]k=([A-Za-z0-9._-]+)/.exec(location.hash || '');
  if (!m) return;
  const key = m[1];
  history.replaceState(null, '', location.pathname + location.search);
  try {
    await fetch('/api/auth', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key }),
    });
    toast('Paired with this host');
  } catch {
    // boot() will land on the login gate, which is the right fallback.
  }
}

async function boot() {
  setLink(null);
  paintSendMode();
  await consumeDeepLink();

  // Opened from a notification while the app was closed.
  const wanted = new URLSearchParams(location.search).get('session');
  if (wanted) {
    history.replaceState(null, '', location.pathname);
    try {
      const body = await api('/api/sessions');
      state.sessions = body.sessions || [];
      setLink(true);
      markSeen();
      openSession(wanted);
      return;
    } catch {
      // Fall through to the normal start-up path.
    }
  }
  // The markup hard-codes Active as the selected chip; if the saved default is All,
  // paint the chips to match before anything renders.
  for (const c of el('filters').children) {
    c.classList.toggle('chip-on', c.dataset.filter === state.filter);
  }
  if (!localStorage.getItem(SEEN_KEY)) {
    show('welcome');
    return;
  }
  show('machines');
  renderMachines();
  // Warm the session list for the active machine so opening it is instant, and so a
  // 401 surfaces as the login gate rather than as an empty folder list.
  try {
    const { sessions } = await api('/api/sessions');
    state.sessions = sessions || [];
    setLink(true);
  } catch (e) {
    setLink(false, e.message);
    if (e.message !== 'unauthorized') {
      el('machines-foot').textContent = e.message;
    }
  }
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    // Backgrounded: stop asking. Whatever changed will be fetched on return.
    startPolling();
    if (paneTimer) clearInterval(paneTimer);
    paneTimer = null;
    return;
  }
  // Back in front: catch up once immediately, then resume the interval.
  if (state.view === 'machines') renderMachines();
  if (state.view === 'projects' || state.view === 'list') refreshList();
  if (state.view === 'detail') {
    refreshDetail();
    if (settings.mode === 'terminal') setTerminalMode(true);
  }
  startPolling();
});

if ('serviceWorker' in navigator) {
  // Registered after load so it never delays first paint.
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('./sw.js').catch(() => {
      // Installability is a bonus; the app works fine without it.
    });
  });
}

boot();
