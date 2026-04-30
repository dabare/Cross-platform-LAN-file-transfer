const state = {
  self: null,
  apiBase: '',
  serverPort: Number(window.location.port) || 32998,
  scanningForServer: false,
  peers: [],
  selectedPeer: null,
  mode: 'local',
  filePeer: null,
  currentPath: '.',
  fileEntries: [],
  fileView: 'list',
  fileSearch: '',
  fileSort: 'name',
  contextEntry: null,
  editorPath: '',
  editorPeer: null,
  editorDirty: false,
  browserClientId: '',
  browserDisplayName: '',
  isBrowserOnlyClient: false,
  eventSource: null,
  browserSendPeer: null,
  transfers: 0,
  transferRows: new Map(),
  nodes: [],
  pointer: { x: -1, y: -1 },
};

const canvas = document.getElementById('peerCanvas');
const ctx = canvas.getContext('2d');
const fileModal = document.getElementById('fileModal');
const fileList = document.getElementById('fileList');
const editorModal = document.getElementById('editorModal');
const editorText = document.getElementById('editorText');
const fileContextMenu = document.getElementById('fileContextMenu');
const uploadInput = document.getElementById('uploadInput');
const browserSendInput = document.getElementById('browserSendInput');
const activity = document.getElementById('activity');
const transferList = document.getElementById('transferList');
let animationFrame = 0;

function log(message) {
  const li = document.createElement('li');
  li.textContent = `${new Date().toLocaleTimeString()}  ${message}`;
  activity.prepend(li);
  while (activity.children.length > 9) activity.lastChild.remove();
}

async function api(url, options = {}) {
  const response = await fetch(apiURL(url), options);
  if (!response.ok) throw new Error(await response.text() || response.statusText);
  const type = response.headers.get('content-type') || '';
  return type.includes('application/json') ? response.json() : response;
}

function apiURL(path) {
  if (/^https?:\/\//i.test(path)) return path;
  return `${state.apiBase}${path}`;
}

function uploadWithProgress(url, form, label) {
  const id = crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random()}`;
  const row = upsertTransferRow({
    id: `out-${id}`,
    name: label,
    direction: 'outbound',
    bytes: 0,
    total: 0,
    status: 'active',
  });
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', apiURL(url));
    xhr.setRequestHeader('X-Transfer-ID', id);
    xhr.setRequestHeader('X-Transfer-Name', label);
    xhr.upload.addEventListener('progress', event => {
      updateTransferRow(row, {
        id: `out-${id}`,
        name: label,
        direction: 'outbound',
        bytes: event.loaded,
        total: event.lengthComputable ? event.total : 0,
        status: 'active',
      });
    });
    xhr.addEventListener('load', () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        const latest = row.latest || {};
        updateTransferRow(row, {
          id: `out-${id}`,
          name: label,
          direction: 'outbound',
          bytes: latest.total || latest.bytes || 1,
          total: latest.total || latest.bytes || 1,
          status: 'complete',
        });
        setTimeout(() => removeTransferRow(`out-${id}`), 45000);
        const type = xhr.getResponseHeader('content-type') || '';
        try {
          resolve(type.includes('application/json') ? JSON.parse(xhr.responseText || '{}') : xhr.responseText);
        } catch (error) {
          reject(error);
        }
      } else {
        updateTransferRow(row, { id: `out-${id}`, name: label, direction: 'outbound', status: 'failed' });
        reject(new Error(xhr.responseText || xhr.statusText));
      }
    });
    xhr.addEventListener('error', () => {
      updateTransferRow(row, { id: `out-${id}`, name: label, direction: 'outbound', status: 'failed' });
      reject(new Error('network error'));
    });
    xhr.addEventListener('abort', () => {
      updateTransferRow(row, { id: `out-${id}`, name: label, direction: 'outbound', status: 'failed' });
      reject(new Error('upload aborted'));
    });
    xhr.send(form);
  });
}

async function init() {
  fitCanvas();
  window.addEventListener('resize', fitCanvas);
  bindEvents();
  registerServiceWorker();
  await connectToServer();
  setInterval(maintainServerConnection, 15000);
  setInterval(() => {
    if (state.self) registerBrowserClient();
  }, 10000);
  setInterval(refreshPeers, 2500);
  refreshInboundTransfers();
  setInterval(refreshInboundTransfers, 1000);
  draw();
}

function bindEvents() {
  document.getElementById('scanBtn').addEventListener('click', scan);
  document.getElementById('localBtn').addEventListener('click', () => openFiles(null));
  document.getElementById('ipToggle').addEventListener('click', toggleLocalIPs);
  document.getElementById('closeFiles').addEventListener('click', () => fileModal.classList.add('hidden'));
  document.getElementById('closeEditor').addEventListener('click', closeEditor);
  document.getElementById('saveEditor').addEventListener('click', () => saveEditor().catch(error => setEditorStatus(`Save failed: ${cleanError(error)}`)));
  document.getElementById('downloadEditor').addEventListener('click', () => {
    if (state.editorPath) downloadFile(state.editorPath);
  });
  editorText.addEventListener('input', () => {
    state.editorDirty = true;
    setEditorStatus('Edited');
  });
  document.getElementById('rootBtn').addEventListener('click', () => {
    state.currentPath = '.';
    loadFiles().catch(error => log(cleanError(error)));
  });
  document.getElementById('upBtn').addEventListener('click', goUp);
  document.getElementById('newFolderBtn').addEventListener('click', createFolder);
  document.getElementById('uploadBtn').addEventListener('click', () => uploadInput.click());
  document.getElementById('fileSearch').addEventListener('input', event => {
    state.fileSearch = event.target.value;
    renderFiles();
  });
  document.getElementById('sortSelect').addEventListener('change', event => {
    state.fileSort = event.target.value;
    renderFiles();
  });
  document.getElementById('listViewBtn').addEventListener('click', () => setFileView('list'));
  document.getElementById('gridViewBtn').addEventListener('click', () => setFileView('grid'));
  document.getElementById('contextOpen').addEventListener('click', () => {
    hideFileContextMenu();
    if (state.contextEntry) openEntry(state.contextEntry, true);
  });
  document.getElementById('contextDownload').addEventListener('click', () => {
    hideFileContextMenu();
    if (state.contextEntry && state.contextEntry.type !== 'dir') downloadFile(state.contextEntry.path);
  });
  document.addEventListener('click', event => {
    if (!fileContextMenu.contains(event.target)) hideFileContextMenu();
  });
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape') hideFileContextMenu();
  });
  uploadInput.addEventListener('change', () => {
    if (uploadInput.files.length) {
      uploadFiles(uploadInput.files).catch(error => {
        log(`Upload failed: ${cleanError(error)}`);
      });
    }
    uploadInput.value = '';
  });
  browserSendInput.addEventListener('change', () => {
    if (browserSendInput.files.length && state.browserSendPeer) {
      sendFilesToBrowserPeer(state.browserSendPeer, browserSendInput.files).catch(error => {
        log(`Browser send failed: ${cleanError(error)}`);
      });
    }
    browserSendInput.value = '';
  });
  const dropZone = document.getElementById('dropZone');
  ['dragenter', 'dragover'].forEach(name => dropZone.addEventListener(name, event => {
    event.preventDefault();
    dropZone.classList.add('drag');
  }));
  ['dragleave', 'drop'].forEach(name => dropZone.addEventListener(name, event => {
    event.preventDefault();
    dropZone.classList.remove('drag');
  }));
  dropZone.addEventListener('drop', event => {
    if (event.dataTransfer.files.length) {
      uploadFiles(event.dataTransfer.files).catch(error => {
        log(`Upload failed: ${cleanError(error)}`);
      });
    }
  });
  canvas.addEventListener('mousemove', event => {
    const rect = canvas.getBoundingClientRect();
    state.pointer.x = (event.clientX - rect.left) * (canvas.width / rect.width);
    state.pointer.y = (event.clientY - rect.top) * (canvas.height / rect.height);
  });
  canvas.addEventListener('mouseleave', () => state.pointer = { x: -1, y: -1 });
  canvas.addEventListener('click', event => {
    const rect = canvas.getBoundingClientRect();
    const x = (event.clientX - rect.left) * (canvas.width / rect.width);
    const y = (event.clientY - rect.top) * (canvas.height / rect.height);
    const hit = state.nodes.find(node => node.peer && distance(x, y, node.x, node.y) < node.r + 8);
    if (hit && hit.peer.canReceive === false) {
      state.browserSendPeer = hit.peer;
      browserSendInput.click();
      return;
    }
    if (hit) openFiles(hit.peer).catch(error => log(`Open failed: ${cleanError(error)}`));
  });
}

function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return;
  navigator.serviceWorker.register('/sw.js').catch(error => {
    log(`PWA cache unavailable: ${cleanError(error)}`);
  });
}

async function connectToServer() {
  const server = await discoverServer();
  if (!server) {
    document.getElementById('deviceName').textContent = 'Scanning for server';
    document.getElementById('deviceRoot').textContent = `Same port ${state.serverPort}`;
    log(`No server found yet. Scanning port ${state.serverPort}.`);
    return false;
  }
  setServer(server.base, server.self);
  await scan();
  refreshPeers();
  registerBrowserClient();
  log(`Connected to ${server.base}`);
  return true;
}

async function maintainServerConnection() {
  if (state.scanningForServer) return;
  try {
    if (state.apiBase) {
      await fetchWithTimeout(apiURL('/api/self'), 1600);
      return;
    }
  } catch {
    log('Server lost. Scanning for another server.');
  }
  await connectToServer();
}

function setServer(base, self) {
  state.apiBase = base.replace(/\/$/, '');
  state.self = self;
  state.serverPort = Number(self.port) || state.serverPort;
  localStorage.setItem('lanFileTransferServer', state.apiBase);
  document.getElementById('deviceName').textContent = self.name;
  document.getElementById('deviceRoot').textContent = self.root;
  renderLocalIPs(self.ips || [], self.port);
  renderConnectQR(self.url);
  log(`Sharing ${self.root}`);
}

async function discoverServer() {
  state.scanningForServer = true;
  try {
    const candidates = serverCandidates();
    for (let i = 0; i < candidates.length; i += 48) {
      const batch = candidates.slice(i, i + 48);
      const found = await Promise.any(batch.map(checkServerCandidate)).catch(() => null);
      if (found) return found;
    }
    return null;
  } finally {
    state.scanningForServer = false;
  }
}

function serverCandidates() {
  const port = state.serverPort;
  const bases = new Set();
  const add = host => {
    if (!host) return;
    bases.add(`http://${host}:${port}`);
  };
  const stored = localStorage.getItem('lanFileTransferServer');
  if (stored) bases.add(stored);
  if (window.location.hostname) {
    add(window.location.hostname);
  }
  for (const prefix of scanPrefixes()) {
    for (let i = 1; i <= 254; i++) add(`${prefix}.${i}`);
  }
  return [...bases];
}

function scanPrefixes() {
  const prefixes = new Set();
  const host = window.location.hostname;
  const match = host.match(/^(\d+)\.(\d+)\.(\d+)\.\d+$/);
  if (match && match[1] !== '127') prefixes.add(`${match[1]}.${match[2]}.${match[3]}`);
  ['192.168.0', '192.168.1', '10.0.0', '10.0.1', '172.16.0', '172.20.10'].forEach(prefix => prefixes.add(prefix));
  return [...prefixes];
}

async function checkServerCandidate(base) {
  const response = await fetchWithTimeout(`${base}/api/self`, 1200);
  if (!response.ok) throw new Error('not a server');
  const self = await response.json();
  if (!self?.id || !self?.port) throw new Error('invalid server');
  return { base, self };
}

function fetchWithTimeout(url, timeoutMs) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  return fetch(url, { signal: controller.signal, cache: 'no-store' })
    .finally(() => clearTimeout(timer));
}

async function registerBrowserClient() {
  const storageKey = 'lanFileTransferClientId';
  const nameKey = 'lanFileTransferClientName';
  let id = localStorage.getItem(storageKey);
  if (!id) {
    id = crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random()}`;
    localStorage.setItem(storageKey, id);
  }
  let name = localStorage.getItem(nameKey) || '';
  if (!name && !isLocalPageHost()) {
    name = prompt('Enter a 4-letter name for this browser') || '';
  }
  name = normalizeClientName(name || 'WEB');
  localStorage.setItem(nameKey, name);
  const result = await api('/api/client', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id, name }),
  }).catch(() => null);
  if (result?.status === 'ok' && result.id) {
    state.browserDisplayName = name;
    state.isBrowserOnlyClient = true;
    startBrowserEvents(result.id);
  }
}

function normalizeClientName(name) {
  return String(name || 'WEB').replace(/[^a-z0-9]/gi, '').toUpperCase().slice(0, 4) || 'WEB';
}

function isLocalPageHost() {
  const host = window.location.hostname;
  return host === 'localhost' || host === '127.0.0.1' || host === '::1';
}

function startBrowserEvents(id) {
  if (state.browserClientId === id && state.eventSource) return;
  if (state.eventSource) state.eventSource.close();
  state.browserClientId = id;
  const source = new EventSource(apiURL(`/api/client/events?id=${encodeURIComponent(id)}`));
  source.onmessage = event => {
    try {
      const data = JSON.parse(event.data);
      if (data.type === 'download') triggerBrowserDownload(data);
    } catch (error) {
      log(`Browser download event failed: ${cleanError(error)}`);
    }
  };
  source.onerror = () => {
    source.close();
    if (state.eventSource === source) state.eventSource = null;
  };
  state.eventSource = source;
}

function triggerBrowserDownload(data) {
  const link = document.createElement('a');
  link.href = apiURL(data.url);
  link.download = data.name || '';
  link.style.display = 'none';
  document.body.append(link);
  link.click();
  link.remove();
  log(`Downloading ${data.name || 'file'} from host`);
}

function renderLocalIPs(ips, port) {
  const list = document.getElementById('ipList');
  const summary = document.getElementById('ipSummary');
  list.innerHTML = '';
  const filtered = ips.filter(ip => ip && ip !== 'localhost' && ip !== '127.0.0.1' && ip !== '::1' && !ip.startsWith('127.'));
  summary.textContent = `${filtered.length} address${filtered.length === 1 ? '' : 'es'}`;
  if (!filtered.length) {
    const empty = document.createElement('span');
    empty.className = 'muted';
    empty.textContent = 'No LAN IP found';
    list.append(empty);
    return;
  }
  for (const ip of filtered) {
    const item = document.createElement('code');
    const host = ip.includes(':') ? `[${ip}]` : ip;
    item.textContent = `${host}:${port}`;
    list.append(item);
  }
}

function renderConnectQR(url) {
  const image = document.getElementById('qrImage');
  const link = document.getElementById('qrLink');
  const label = document.getElementById('qrUrl');
  if (!url) {
    label.textContent = 'No LAN URL available';
    return;
  }
  const qrURL = apiURL(`/api/qr?text=${encodeURIComponent(url)}`);
  image.src = qrURL;
  link.href = url;
  label.textContent = url;
}

function toggleLocalIPs() {
  const section = document.getElementById('ipSection');
  const toggle = document.getElementById('ipToggle');
  const collapsed = section.classList.toggle('collapsed');
  toggle.setAttribute('aria-expanded', String(!collapsed));
}

function fitCanvas() {
  const rect = canvas.getBoundingClientRect();
  const scale = window.devicePixelRatio || 1;
  canvas.width = Math.max(700, Math.floor(rect.width * scale));
  canvas.height = Math.max(420, Math.floor(rect.height * scale));
}

async function scan() {
  document.getElementById('scanBtn').textContent = 'Scanning...';
  try {
    await api('/api/scan', { method: 'POST' });
    log('Network scan started');
    setTimeout(refreshPeers, 1200);
  } finally {
    setTimeout(() => document.getElementById('scanBtn').textContent = 'Scan network', 900);
  }
}

async function refreshPeers() {
  try {
    state.peers = await api('/api/peers');
    document.getElementById('peerCount').textContent = state.peers.length;
  } catch (error) {
    log(`Peer refresh failed: ${error.message.trim()}`);
  }
}

async function refreshInboundTransfers() {
  try {
    const transfers = await api('/api/transfers');
    for (const item of transfers) {
      const id = `in-${item.id}`;
      const row = upsertTransferRow({ ...item, id });
      updateTransferRow(row, { ...item, id });
      if (item.status !== 'active') {
        setTimeout(() => removeTransferRow(id), 45000);
      }
    }
    renderEmptyTransferState();
  } catch (error) {
    log(`Transfer refresh failed: ${cleanError(error)}`);
  }
}

function upsertTransferRow(item) {
  if (state.transferRows.has(item.id)) return state.transferRows.get(item.id);
  const root = document.createElement('div');
  root.className = 'transferItem';
  root.innerHTML = `
    <div class="transferTop"><strong></strong><span></span></div>
    <div class="progressTrack"><div class="progressFill"></div></div>
    <div class="transferMeta"></div>
  `;
  transferList.prepend(root);
  const row = {
    root,
    title: root.querySelector('strong'),
    status: root.querySelector('span'),
    fill: root.querySelector('.progressFill'),
    meta: root.querySelector('.transferMeta'),
    latest: item,
  };
  state.transferRows.set(item.id, row);
  renderEmptyTransferState();
  return row;
}

function updateTransferRow(row, item) {
  row.latest = { ...row.latest, ...item };
  const data = row.latest;
  const total = Number(data.total || 0);
  const bytes = Number(data.bytes || 0);
  const pct = total > 0 ? Math.min(100, Math.round((bytes / total) * 100)) : 0;
  const direction = data.direction === 'inbound' ? 'Receiving' : 'Sending';
  row.title.textContent = data.name || 'Transfer';
  row.status.textContent = data.status === 'active' ? `${pct}%` : data.status;
  row.fill.style.width = data.status === 'complete' ? '100%' : `${pct}%`;
  row.meta.textContent = total > 0
    ? `${direction} ${formatBytes(bytes)} of ${formatBytes(total)}`
    : `${direction} ${formatBytes(bytes)}`;
  row.root.dataset.status = data.status;
}

function removeTransferRow(id) {
  const row = state.transferRows.get(id);
  if (!row) return;
  row.root.remove();
  state.transferRows.delete(id);
  renderEmptyTransferState();
}

function renderEmptyTransferState() {
  let empty = transferList.querySelector('[data-empty="true"]');
  if (state.transferRows.size === 0) {
    if (!empty) {
      empty = document.createElement('div');
      empty.dataset.empty = 'true';
      empty.className = 'transferItem muted';
      empty.textContent = 'No active transfers';
      transferList.append(empty);
    }
  } else if (empty) {
    empty.remove();
  }
}

function draw() {
  animationFrame += 0.018;
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  drawGrid();
  const cx = canvas.width / 2;
  const cy = canvas.height / 2;
  const peers = drawablePeers();
  const count = Math.max(peers.length, 1);
  const radius = Math.min(canvas.width, canvas.height) * 0.31;
  state.nodes = [{ x: cx, y: cy, r: 52, peer: null }];
  peers.forEach((peer, index) => {
    const angle = (Math.PI * 2 * index / count) - Math.PI / 2 + Math.sin(animationFrame * .7) * .04;
    const pulse = Math.sin(animationFrame * 2 + index) * 7;
    state.nodes.push({
      x: cx + Math.cos(angle) * (radius + pulse),
      y: cy + Math.sin(angle) * (radius + pulse),
      r: 38,
      peer,
    });
  });
  state.nodes.slice(1).forEach(node => drawLink(cx, cy, node.x, node.y, node.peer?.canReceive === false));
  const centerTitle = state.isBrowserOnlyClient ? state.browserDisplayName : state.self?.name || 'This device';
  const centerSubtitle = state.isBrowserOnlyClient ? 'Browser client' : 'Ready';
  drawNode(cx, cy, 52, centerTitle, centerSubtitle, true);
  if (!peers.length) drawEmpty(cx, cy, radius);
  state.nodes.slice(1).forEach(node => {
    const age = (Date.now() - new Date(node.peer.lastSeen).getTime()) / 1000;
    const subtitle = node.peer.canReceive === false ? `${node.peer.host}  click to send` : `${node.peer.host}:${node.peer.port}`;
    drawNode(node.x, node.y, node.r, node.peer.name, subtitle, false, age, node.peer.canReceive === false);
  });
  requestAnimationFrame(draw);
}

function drawablePeers() {
  const peers = state.peers.filter(peer => peer.id !== state.browserClientId);
  if (!state.isBrowserOnlyClient || !state.self) return peers;
  const hostPeer = {
    id: state.self.id,
    name: state.self.name,
    host: endpointHost(state.self.url),
    port: state.self.port,
    url: state.self.url,
    os: state.self.os,
    kind: 'server',
    canReceive: true,
    lastSeen: new Date().toISOString(),
  };
  return [hostPeer, ...peers.filter(peer => peer.id !== hostPeer.id)];
}

function endpointHost(url) {
  try {
    return new URL(url).hostname;
  } catch {
    return window.location.hostname;
  }
}

function drawGrid() {
  ctx.save();
  ctx.strokeStyle = 'rgba(255,255,255,.045)';
  ctx.lineWidth = 1;
  const step = 44 * (window.devicePixelRatio || 1);
  for (let x = 0; x < canvas.width; x += step) {
    ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, canvas.height); ctx.stroke();
  }
  for (let y = 0; y < canvas.height; y += step) {
    ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(canvas.width, y); ctx.stroke();
  }
  ctx.restore();
}

function drawLink(x1, y1, x2, y2, disabled = false) {
  const gradient = ctx.createLinearGradient(x1, y1, x2, y2);
  gradient.addColorStop(0, disabled ? 'rgba(115,128,138,.22)' : 'rgba(57,210,160,.45)');
  gradient.addColorStop(1, disabled ? 'rgba(115,128,138,.34)' : 'rgba(74,163,255,.55)');
  ctx.save();
  ctx.strokeStyle = gradient;
  ctx.lineWidth = 2;
  ctx.setLineDash([10, 12]);
  ctx.lineDashOffset = -animationFrame * 42;
  ctx.beginPath();
  ctx.moveTo(x1, y1);
  ctx.lineTo(x2, y2);
  ctx.stroke();
  ctx.restore();
}

function drawNode(x, y, r, title, subtitle, self = false, age = 0, disabled = false) {
  const hover = distance(state.pointer.x, state.pointer.y, x, y) < r + 8;
  const color = disabled ? '#73808a' : self ? '#39d2a0' : age > 25 ? '#f2ba4b' : '#4aa3ff';
  ctx.save();
  ctx.shadowColor = color;
  ctx.shadowBlur = hover ? 34 : 20;
  ctx.fillStyle = color;
  ctx.globalAlpha = .18;
  ctx.beginPath(); ctx.arc(x, y, r + 12 + Math.sin(animationFrame * 4) * 2, 0, Math.PI * 2); ctx.fill();
  ctx.globalAlpha = 1;
  ctx.fillStyle = '#151b20';
  ctx.strokeStyle = color;
  ctx.lineWidth = hover ? 4 : 2;
  ctx.beginPath(); ctx.arc(x, y, hover ? r + 4 : r, 0, Math.PI * 2); ctx.fill(); ctx.stroke();
  ctx.shadowBlur = 0;
  ctx.fillStyle = color;
  ctx.font = `${self ? 28 : 22}px Inter, sans-serif`;
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.fillText(self ? 'You' : disabled ? clip(normalizeClientName(title), 4) : initials(title), x, y - 2);
  ctx.fillStyle = '#f4f7f8';
  ctx.font = '700 15px Inter, sans-serif';
  ctx.fillText(clip(title, 18), x, y + r + 26);
  ctx.fillStyle = '#9aa8b2';
  ctx.font = '12px Inter, sans-serif';
  ctx.fillText(clip(subtitle, 24), x, y + r + 44);
  ctx.restore();
}

function drawEmpty(cx, cy, radius) {
  ctx.save();
  ctx.strokeStyle = 'rgba(242,186,75,.26)';
  ctx.lineWidth = 2;
  ctx.beginPath();
  ctx.arc(cx, cy, radius, 0, Math.PI * 2);
  ctx.stroke();
  ctx.fillStyle = '#9aa8b2';
  ctx.font = '15px Inter, sans-serif';
  ctx.textAlign = 'center';
  ctx.fillText('No peers yet. Start this app on another device in the same network.', cx, cy + radius + 54);
  ctx.restore();
}

async function openFiles(peer) {
  state.filePeer = peer;
  state.selectedPeer = peer;
  state.currentPath = '.';
  state.fileSearch = '';
  document.getElementById('fileSearch').value = '';
  document.getElementById('fileScope').textContent = peer
    ? `${peer.host}:${peer.port}  ${peer.os || ''}`
    : 'This device';
  document.getElementById('fileTitle').textContent = peer ? peer.name : 'This device';
  fileModal.classList.remove('hidden');
  await loadFiles();
}

async function loadFiles() {
  const base = state.filePeer ? `/api/peer/${state.filePeer.id}/files` : '/api/files';
  const data = await api(`${base}?path=${encodeURIComponent(state.currentPath)}`);
  state.currentPath = data.path || '.';
  document.getElementById('filePath').textContent = state.currentPath;
  state.fileEntries = data.entries || [];
  renderBreadcrumb();
  renderFiles();
}

function renderBreadcrumb() {
  const breadcrumb = document.getElementById('breadcrumb');
  breadcrumb.innerHTML = '';
  const root = document.createElement('button');
  root.textContent = 'Shared';
  root.addEventListener('click', () => {
    state.currentPath = '.';
    loadFiles().catch(error => log(cleanError(error)));
  });
  breadcrumb.append(root);
  if (state.currentPath === '.') return;
  const parts = state.currentPath.split('/').filter(Boolean);
  parts.forEach((part, index) => {
    const sep = document.createElement('span');
    sep.textContent = '/';
    breadcrumb.append(sep);
    const crumb = document.createElement('button');
    crumb.textContent = part;
    crumb.addEventListener('click', () => {
      state.currentPath = parts.slice(0, index + 1).join('/');
      loadFiles().catch(error => log(cleanError(error)));
    });
    breadcrumb.append(crumb);
  });
}

function renderFiles() {
  const entries = sortedEntries();
  fileList.innerHTML = '';
  fileList.className = `fileList ${state.fileView === 'grid' ? 'gridView' : 'listView'}`;
  document.querySelector('.fileHeader').classList.toggle('hidden', state.fileView === 'grid');
  document.getElementById('fileStatus').textContent = `${entries.length} item${entries.length === 1 ? '' : 's'}`;
  if (!entries.length) {
    const empty = document.createElement('div');
    empty.className = 'emptyFiles';
    empty.textContent = state.fileSearch ? 'No matching files' : 'This folder is empty';
    fileList.append(empty);
    return;
  }
  for (const entry of entries) {
    const row = document.createElement('div');
    row.className = `row ${entry.type === 'dir' ? 'folderRow' : 'fileRow'}`;
    row.addEventListener('dblclick', () => openEntry(entry, true));
    row.addEventListener('contextmenu', event => showFileContextMenu(event, entry));
    const name = document.createElement('div');
    name.className = 'fileName';
    name.innerHTML = `${entry.type === 'dir' ? folderIcon() : fileIcon(entry.name)}<strong title="${escapeAttr(entry.name)}">${escapeHTML(entry.name)}</strong>`;
    const kind = document.createElement('span');
    kind.className = 'muted';
    kind.textContent = entry.type === 'dir' ? 'Folder' : fileKind(entry.name);
    const modified = document.createElement('span');
    modified.className = 'muted modified';
    modified.textContent = new Date(entry.modified).toLocaleString();
    const size = document.createElement('span');
    size.className = 'muted';
    size.textContent = entry.type === 'dir' ? '--' : formatBytes(entry.size);
    row.append(name, kind, modified, size);
    fileList.append(row);
  }
}

function openEntry(entry, ask) {
  if (entry.type === 'dir') {
    enterFolder(entry.path);
    return;
  }
  if (ask && !confirm(`Open ${entry.name}?`)) return;
  openFile(entry.path).catch(error => log(`Open failed: ${cleanError(error)}`));
}

function showFileContextMenu(event, entry) {
  event.preventDefault();
  state.contextEntry = entry;
  document.getElementById('contextDownload').classList.toggle('hidden', entry.type === 'dir');
  fileContextMenu.style.left = `${Math.min(event.clientX, window.innerWidth - 190)}px`;
  fileContextMenu.style.top = `${Math.min(event.clientY, window.innerHeight - 100)}px`;
  fileContextMenu.classList.remove('hidden');
}

function hideFileContextMenu() {
  fileContextMenu.classList.add('hidden');
}

function sortedEntries() {
  const query = state.fileSearch.trim().toLowerCase();
  const entries = state.fileEntries.filter(entry => !query || entry.name.toLowerCase().includes(query));
  entries.sort((a, b) => {
    if (a.type !== b.type) return a.type === 'dir' ? -1 : 1;
    if (state.fileSort === 'modified') return new Date(b.modified) - new Date(a.modified);
    if (state.fileSort === 'size') return b.size - a.size;
    if (state.fileSort === 'type') return fileKind(a.name).localeCompare(fileKind(b.name));
    return a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });
  });
  return entries;
}

function setFileView(view) {
  state.fileView = view;
  document.getElementById('listViewBtn').classList.toggle('active', view === 'list');
  document.getElementById('gridViewBtn').classList.toggle('active', view === 'grid');
  renderFiles();
}

function enterFolder(path) {
  state.currentPath = path;
  loadFiles().catch(error => log(error.message.trim()));
}

async function openFile(path) {
  const base = state.filePeer ? `/api/peer/${state.filePeer.id}/open` : '/api/open';
  const data = await api(`${base}?path=${encodeURIComponent(path)}`);
  state.editorPath = data.path;
  state.editorPeer = state.filePeer;
  state.editorDirty = false;
  document.getElementById('editorTitle').textContent = data.name || data.path;
  document.getElementById('editorMeta').textContent = `${state.editorPeer ? state.editorPeer.name : 'This device'}  ${formatBytes(data.size || 0)}`;
  editorText.value = data.content || '';
  setEditorStatus('Ready');
  editorModal.classList.remove('hidden');
  editorText.focus();
}

async function saveEditor() {
  if (!state.editorPath) return;
  setEditorStatus('Saving...');
  const base = state.editorPeer ? `/api/peer/${state.editorPeer.id}/save` : '/api/save';
  const result = await api(base, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path: state.editorPath, content: editorText.value }),
  });
  state.editorDirty = false;
  setEditorStatus(`Saved ${formatBytes(result.size || editorText.value.length)}`);
  await loadFiles();
}

function closeEditor() {
  if (state.editorDirty && !confirm('Close without saving changes?')) return;
  editorModal.classList.add('hidden');
}

function setEditorStatus(message) {
  document.getElementById('editorStatus').textContent = message;
}

function goUp() {
  if (state.currentPath === '.') return;
  const parts = state.currentPath.split('/').filter(Boolean);
  parts.pop();
  state.currentPath = parts.length ? parts.join('/') : '.';
  loadFiles().catch(error => log(error.message.trim()));
}

async function createFolder() {
  const name = prompt('Folder name');
  if (!name) return;
  const url = state.filePeer ? `/api/peer/${state.filePeer.id}/mkdir` : '/api/mkdir';
  await api(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path: state.currentPath, name }),
  });
  log(`Created folder ${name}`);
  await loadFiles();
}

async function uploadFiles(files) {
  const form = new FormData();
  const selected = [...files];
  selected.forEach(file => form.append('files', file));
  const url = state.filePeer
    ? `/api/peer/${state.filePeer.id}/upload?path=${encodeURIComponent(state.currentPath)}`
    : `/api/upload?path=${encodeURIComponent(state.currentPath)}`;
  const label = selected.length === 1 ? selected[0].name : `${selected.length} files`;
  await uploadWithProgress(url, form, label);
  state.transfers += selected.length;
  updateTransferCount();
  log(`Uploaded ${selected.length} file${selected.length === 1 ? '' : 's'}`);
  await loadFiles();
}

async function sendFilesToBrowserPeer(peer, files) {
  const selected = [...files];
  if (!selected.length) return;
  if (!confirm(`Send ${selected.length} file${selected.length === 1 ? '' : 's'} to ${peer.name}?`)) return;
  const form = new FormData();
  selected.forEach(file => form.append('files', file));
  const label = selected.length === 1 ? selected[0].name : `${selected.length} files to ${peer.name}`;
  await uploadWithProgress(`/api/client/send?id=${encodeURIComponent(peer.id)}`, form, label);
  state.transfers += selected.length;
  updateTransferCount();
  log(`Sent ${selected.length} file${selected.length === 1 ? '' : 's'} to browser ${peer.name}`);
}

function cleanError(error) {
  return String(error?.message || error).replace(/\s+/g, ' ').trim();
}

function downloadFile(path) {
  const url = state.filePeer
    ? `/api/peer/${state.filePeer.id}/download?path=${encodeURIComponent(path)}`
    : `/api/download?path=${encodeURIComponent(path)}`;
  window.open(apiURL(url), '_blank');
  state.transfers += 1;
  updateTransferCount();
}

function updateTransferCount() {
  document.getElementById('transferCount').textContent = state.transfers;
}

function distance(x1, y1, x2, y2) {
  return Math.hypot(x1 - x2, y1 - y2);
}

function initials(text) {
  return String(text || '?').split(/[\s._-]+/).filter(Boolean).slice(0, 2).map(part => part[0]).join('').toUpperCase();
}

function clip(text, max) {
  text = String(text || '');
  return text.length > max ? `${text.slice(0, max - 1)}...` : text;
}

function formatBytes(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[index]}`;
}

function fileKind(name) {
  const info = fileTypeInfo(name);
  return info.kind;
}

function folderIcon() {
  return '<span class="fileIcon folderIcon" aria-hidden="true"><span></span></span>';
}

function fileIcon(name) {
  const info = fileTypeInfo(name);
  return `<span class="fileIcon docIcon ${info.className}" aria-hidden="true"><span>${escapeHTML(info.label)}</span></span>`;
}

function fileTypeInfo(name) {
  const lower = String(name || '').toLowerCase();
  const ext = lower.includes('.') ? lower.split('.').pop() : '';
  const groups = [
    { exts: ['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'heic', 'bmp', 'tiff'], kind: 'Image', label: 'IMG', className: 'imageIcon' },
    { exts: ['mp4', 'mov', 'avi', 'mkv', 'webm', 'm4v'], kind: 'Video', label: 'VID', className: 'videoIcon' },
    { exts: ['mp3', 'wav', 'aac', 'flac', 'm4a', 'ogg'], kind: 'Audio', label: 'AUD', className: 'audioIcon' },
    { exts: ['pdf'], kind: 'PDF document', label: 'PDF', className: 'pdfIcon' },
    { exts: ['zip', 'rar', '7z', 'tar', 'gz', 'bz2', 'xz'], kind: 'Archive', label: 'ZIP', className: 'archiveIcon' },
    { exts: ['js', 'ts', 'jsx', 'tsx', 'go', 'py', 'java', 'c', 'cpp', 'h', 'hpp', 'rs', 'rb', 'php', 'cs', 'swift', 'kt', 'html', 'css', 'json', 'xml', 'yaml', 'yml', 'sh', 'bat', 'ps1'], kind: 'Code', label: 'DEV', className: 'codeIcon' },
    { exts: ['txt', 'md', 'rtf', 'log'], kind: 'Text document', label: 'TXT', className: 'textIcon' },
    { exts: ['doc', 'docx', 'odt'], kind: 'Document', label: 'DOC', className: 'wordIcon' },
    { exts: ['xls', 'xlsx', 'csv', 'ods'], kind: 'Spreadsheet', label: 'XLS', className: 'sheetIcon' },
    { exts: ['ppt', 'pptx', 'key', 'odp'], kind: 'Presentation', label: 'PPT', className: 'slideIcon' },
    { exts: ['dmg', 'exe', 'msi', 'pkg', 'app', 'deb', 'rpm'], kind: 'Application', label: 'APP', className: 'appIcon' },
  ];
  const match = groups.find(group => group.exts.includes(ext));
  if (match) return match;
  if (!ext) return { kind: 'Document', label: 'DOC', className: 'genericIcon' };
  return { kind: `${ext.toUpperCase()} file`, label: ext.slice(0, 3).toUpperCase(), className: 'genericIcon' };
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[char]));
}

function escapeAttr(value) {
  return escapeHTML(value).replace(/"/g, '&quot;');
}

init().catch(error => {
  log(`Startup failed: ${error.message.trim()}`);
  console.error(error);
});
