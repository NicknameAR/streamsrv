/**
 * streamer.js — StreamSrv WebRTC Engine
 *
 * Quality vs Bitrate — correctly separated:
 *   • Quality = resolution scale (scaleResolutionDownBy) — what resolution the viewer gets
 *   • Bitrate  = data rate ceiling the streamer sets per-viewer — secondary to quality
 *   • "Quality 720p" means: scale source resolution to ~720 lines, bitrate is automatic
 *   • Viewer requests a quality LEVEL, streamer applies scale + optional bitrate ceiling
 *
 * ABR (Adaptive Bitrate) — "Auto" quality mode:
 *   • Measures packet loss + jitter every 3 seconds from WebRTC inbound-rtp stats
 *   • Degrades quality if loss > 2% or jitter > 50ms (2 consecutive bad readings)
 *   • Upgrades quality if stable for 15 seconds (5 consecutive good readings)
 *   • Shows current auto-selected level in the quality button
 *
 * Reconnect UX:
 *   • Exponential backoff: 1s, 2s, 4s, 8s, 16s, 30s max
 *   • Animated countdown on stage overlay
 *   • Cancel button stops auto-reconnect
 *   • Different messages per close code
 *
 * Latency indicator:
 *   • RTT from WebRTC candidate-pair stats (measured every 2.5s)
 *   • Color: green <80ms, yellow <160ms, red ≥160ms
 *   • Shown in chat header + as overlay badge on video
 */
(function () {
'use strict';

// ─── Quality presets ──────────────────────────────────────────────────────────
const QUALITY = {
  auto:   { label: 'Auto',   scale: 1,    maxBitrate: 0           },
  source: { label: 'Source', scale: 1,    maxBitrate: 0           },
  '1080p':{ label: '1080p',  scale: 1,    maxBitrate: 8_000_000  },
  '720p': { label: '720p',   scale: 1.5,  maxBitrate: 4_000_000  },
  '480p': { label: '480p',   scale: 2.25, maxBitrate: 1_500_000  },
  '360p': { label: '360p',   scale: 3,    maxBitrate: 700_000    },
  '240p': { label: '240p',   scale: 4.5,  maxBitrate: 350_000    },
};
const ABR_LADDER = ['240p', '360p', '480p', '720p', '1080p', 'source'];

// ─── ICE ─────────────────────────────────────────────────────────────────────
const ICE = { iceServers: [
  { urls: 'stun:stun.l.google.com:19302' },
  { urls: 'stun:stun.relay.metered.ca:80' },
  { urls: 'turn:global.relay.metered.ca:80',              username: '0f2eaddddff306f1dd44e8f5', credential: 'ZlyV767tyzNKw9tb' },
  { urls: 'turn:global.relay.metered.ca:80?transport=tcp', username: '0f2eaddddff306f1dd44e8f5', credential: 'ZlyV767tyzNKw9tb' },
  { urls: 'turn:global.relay.metered.ca:443',             username: '0f2eaddddff306f1dd44e8f5', credential: 'ZlyV767tyzNKw9tb' },
  { urls: 'turns:global.relay.metered.ca:443?transport=tcp', username: '0f2eaddddff306f1dd44e8f5', credential: 'ZlyV767tyzNKw9tb' },
]};

// ─── State ────────────────────────────────────────────────────────────────────
let ws = null;
let myConnId = null, myName = null, myAvatar = '', myRole = 'viewer', myRoom = '';
let streamerConnId = null;   


let connState = 'idle';
let reconnectTimer = null, reconnectAttempts = 0, reconnectCancelFlag = false;
const RECONNECT_DELAYS = [1000, 2000, 4000, 8000, 16000, 30000];


let localScreen = null, localCam = null, localMic = null;
let screenOn = false, camOn = false, micOn = false;


const pcs     = new Map();   
const viewers = new Set();   
const vqMap   = new Map();   


let viewerPc = null, rttTimer = null;
let vidCount = 0, audCount = 0;


let actx = null, gainGame = null, gainMic = null, srcGame = null, srcMic = null;
let audioReady = false;


let viewerQuality = 'auto';  
let abrIndex = ABR_LADDER.indexOf('720p');  
let abrTimer = null, abrGoodCount = 0, abrBadCount = 0;
let abrLastLost = 0, abrLastReceived = 0;

// ─── DOM helpers ──────────────────────────────────────────────────────────────
const $ = id => document.getElementById(id);
const setText  = (id, v)     => { const e = $(id); if (e) e.textContent = v; };
const setClass = (id, cls, on) => { const e = $(id); if (e) e.classList.toggle(cls, !!on); };
function cfg(key, def) { try { const v = localStorage.getItem('ss_cfg_' + key); return v !== null ? JSON.parse(v) : def; } catch { return def; } }

// ─── Web Audio ────────────────────────────────────────────────────────────────
function initAudio() {
  if (audioReady) return;
  try {
    actx    = new (window.AudioContext || window.webkitAudioContext)();
    gainGame = actx.createGain();
    gainMic  = actx.createGain();
    gainGame.gain.value = parseFloat(($('volGame') || { value: '1' }).value);
    gainMic.gain.value  = parseFloat(($('volMic')  || { value: '1' }).value);
    gainGame.connect(actx.destination);
    gainMic.connect(actx.destination);
    audioReady = true;
  } catch (e) { dlog('Web Audio init failed: ' + e.message); }
}

function resumeAudio() {
  if (actx && actx.state === 'suspended') actx.resume().catch(() => {});
}

window.setGameVolume = v => { resumeAudio(); if (gainGame) gainGame.gain.value = parseFloat(v); };
window.setMicVolume  = v => { resumeAudio(); if (gainMic)  gainMic.gain.value  = parseFloat(v); };

function routeAudio(track, gainNode, oldSrc) {
  if (!audioReady) initAudio();
  if (!actx) return null;
  if (oldSrc) { try { oldSrc.disconnect(); } catch {} }
  const src = actx.createMediaStreamSource(new MediaStream([track]));
  src.connect(gainNode);
  return src;
}

// ─── Logging ─────────────────────────────────────────────────────────────────
function dlog(msg) {
  const el = $('devLog'); if (!el) return;
  el.textContent += `[${new Date().toLocaleTimeString('en', {hour12: false})}] ${msg}\n`;
  el.scrollTop = el.scrollHeight;
}

// ─── Latency display ──────────────────────────────────────────────────────────
function showRtt(ms) {
  setText('rttVal', ms + ' ms');
  const dot = $('rttDot');
  if (dot) dot.style.background = ms < 80 ? '#00e676' : ms < 160 ? '#ffea00' : '#e91e63';
  
  const badge = $('latencyBadge');
  if (badge) {
    badge.textContent = ms + 'ms';
    badge.className = 'latency-badge ' + (ms < 80 ? 'good' : ms < 160 ? 'ok' : 'bad');
  }
}
function resetRtt() {
  setText('rttVal', '– ms');
  const dot = $('rttDot'); if (dot) dot.style.background = 'var(--dim)';
  const badge = $('latencyBadge'); if (badge) { badge.textContent = ''; badge.className = 'latency-badge'; }
}

function startRttCollector(pc) {
  if (rttTimer) clearInterval(rttTimer);
  rttTimer = setInterval(async () => {
    if (!pc || !['connected', 'completed'].includes(pc.iceConnectionState)) return;
    try {
      const stats = await pc.getStats();
      stats.forEach(r => {
        if (r.type === 'candidate-pair' && r.state === 'succeeded' && r.currentRoundTripTime) {
          showRtt(Math.round(r.currentRoundTripTime * 1000));
        }
      });
    } catch {}
  }, 2500);
}

function stopRttCollector() {
  if (rttTimer) { clearInterval(rttTimer); rttTimer = null; }
  resetRtt();
}

// ─── WS connection state display ──────────────────────────────────────────────
function setWsDot(state) {
  const dot = $('wsDot'), txt = $('wsState');
  if (dot) dot.className = 'ws-dot' + (state === 'on' ? ' on' : state === 'err' ? ' err' : '');
  if (txt) txt.textContent = { on: 'online', err: 'error', off: 'offline', reconnecting: 'reconnecting' }[state] || state;
}

function wsSend(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// ─── Reconnect UX ────────────────────────────────────────────────────────────
function showReconnectOverlay(secondsLeft, attempt) {
  const ov = $('reconnectOverlay');
  if (!ov) return;
  ov.style.display = 'flex';
  const msg = $('reconnectMsg');
  if (msg) {
    if (secondsLeft <= 0) {
      msg.textContent = 'Connecting…';
    } else {
      msg.textContent = `Reconnecting in ${secondsLeft}s (attempt ${attempt})`;
    }
  }
  
  const bar = $('reconnectBar');
  if (bar && secondsLeft > 0) {
    const total = RECONNECT_DELAYS[Math.min(attempt - 1, RECONNECT_DELAYS.length - 1)] / 1000;
    bar.style.width = ((total - secondsLeft) / total * 100) + '%';
  }
}

function hideReconnectOverlay() {
  const ov = $('reconnectOverlay'); if (ov) ov.style.display = 'none';
  const bar = $('reconnectBar'); if (bar) bar.style.width = '0%';
}

function scheduleReconnect(closeCode) {
  if (reconnectCancelFlag || closeCode === 1000) return;
  if (reconnectTimer) return;

  reconnectAttempts++;
  const delay = RECONNECT_DELAYS[Math.min(reconnectAttempts - 1, RECONNECT_DELAYS.length - 1)];
  dlog(`Reconnect in ${delay / 1000}s (attempt ${reconnectAttempts})`);
  setWsDot('reconnecting');
  setText('connectLbl', 'reconnecting');

  let remaining = Math.round(delay / 1000);
  showReconnectOverlay(remaining, reconnectAttempts);

  const countTmr = setInterval(() => {
    remaining--;
    showReconnectOverlay(remaining, reconnectAttempts);
    if (remaining <= 0) clearInterval(countTmr);
  }, 1000);

  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    clearInterval(countTmr);
    if (!reconnectCancelFlag) wsConnect();
  }, delay);
}

function cancelReconnect() {
  reconnectCancelFlag = true;
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  hideReconnectOverlay();
  onClose(); 
  dlog('Reconnect cancelled by user');
}
window.cancelReconnect = cancelReconnect;

// ─── Error overlay ────────────────────────────────────────────────────────────
function showError(type, message) {
  
  const ov = $('stageOver');
  if (!ov) return;
  ov.style.display = 'flex';
  ov.className = 'stage-over on error-' + type;

  const icons = {
    ended:        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="10"/><polyline points="9 9 12 12 15 9"/><line x1="12" y1="12" x2="12" y2="16"/></svg>',
    error:        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>',
    banned:       '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></svg>',
    kicked:       '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>',
    disconnected: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><line x1="1" y1="1" x2="23" y2="23"/><path d="M16.72 11.06A10.94 10.94 0 0 1 19 12.55"/><path d="M5 12.55a10.94 10.94 0 0 1 5.17-2.39"/><path d="M10.71 5.05A16 16 0 0 1 22.56 9"/><path d="M1.42 9a15.91 15.91 0 0 1 4.7-2.88"/><path d="M8.53 16.11a6 6 0 0 1 6.95 0"/><line x1="12" y1="20" x2="12.01" y2="20"/></svg>',
  };

  const labels = {
    ended:        'Stream Ended',
    error:        'Connection Error',
    banned:       'You are banned',
    kicked:       'You were kicked',
    disconnected: 'Disconnected',
  };

  const icon = $('stageOverIcon'); if (icon) icon.innerHTML = icons[type] || icons.error;
  const lbl  = $('stageOverTitle'); if (lbl) lbl.textContent = labels[type] || 'Error';
  const sub  = $('stageOverSub');  if (sub) sub.textContent = message || '';
}

function hideError() {
  const ov = $('stageOver');
  if (ov) { ov.style.display = 'none'; ov.className = 'stage-over'; }
}

// ─── Chat ─────────────────────────────────────────────────────────────────────
function appendChat(connId, username, role, text, av) {
  const log = $('chatLog'); if (!log) return;
  const isSelf = (connId === myConnId || username === myName);

  const wrap = document.createElement('div');
  wrap.className = 'cmsg' + (isSelf ? ' cmsg-self' : '');
  wrap.dataset.cid = connId || '';

  const avel = document.createElement('div');
  avel.className = 'cmsg-av';
  if (av) {
    const img = document.createElement('img'); img.src = av;
    img.onerror = () => { avel.innerHTML = ''; avel.textContent = (username || '?')[0].toUpperCase(); };
    avel.appendChild(img);
  } else {
    avel.textContent = (username || '?')[0].toUpperCase();
  }

  const body = document.createElement('div'); body.className = 'cmsg-body';
  const nm = document.createElement('span');
  nm.className = 'cmsg-name ' + (role === 'streamer' ? 'streamer' : role === 'viewer' ? 'viewer' : 'guest');
  nm.textContent = username + ':';
  const tx = document.createElement('span'); tx.className = 'cmsg-text'; tx.textContent = ' ' + text;
  body.append(nm, tx);
  wrap.append(avel, body);

  if (myRole === 'streamer' && !isSelf && connId) {
    const acts = document.createElement('div'); acts.className = 'cmsg-actions';
    const kb = document.createElement('button'); kb.className = 'bmod bkick'; kb.textContent = 'kick';
    kb.onclick = () => { const r = prompt('Kick reason:') ?? ''; wsSend({ type: 'kick', data: JSON.stringify({ target_id: connId, reason: r }) }); };
    const bb = document.createElement('button'); bb.className = 'bmod bban'; bb.textContent = 'ban';
    bb.onclick = () => {
      const r = prompt('Ban reason:') ?? '';
      wsSend({ type: 'ban', data: JSON.stringify({ target_id: connId, reason: r }) });
      log.querySelectorAll('[data-cid="' + connId + '"]').forEach(e => e.remove());
    };
    acts.append(kb, bb); wrap.appendChild(acts);
  }

  log.appendChild(wrap);
  if (log.scrollHeight - log.scrollTop < log.clientHeight + 120) log.scrollTop = log.scrollHeight;
}

function sendChat() {
  const inp = $('chatInput'); if (!inp) return;
  const text = inp.value.trim();
  if (!text || !ws || ws.readyState !== WebSocket.OPEN) return;
  wsSend({ type: 'chat', data: { text } });
  inp.value = '';
}

// ─── Viewer count ─────────────────────────────────────────────────────────────
function setViewers(n) { setText('siViewers', n); setText('peersLbl', n); }

// ─── Stream info bar ──────────────────────────────────────────────────────────
function showStreamInfo(title, cat) {
  const bar = $('streamInfoBar'); if (!bar) return;
  bar.style.display = 'flex';
  setText('siTitle', title || 'Live stream');
  const c = $('siCat'); if (c) { c.textContent = cat || ''; c.style.display = cat ? '' : 'none'; }
  const av = $('siAvatar'); if (av) {
    if (myAvatar) {
      const img = document.createElement('img');
      img.src = myAvatar; img.style.cssText = 'width:100%;height:100%;object-fit:cover;border-radius:inherit';
      img.onerror = () => { av.innerHTML = ''; av.textContent = (myName || '?')[0].toUpperCase(); };
      av.innerHTML = ''; av.appendChild(img);
    } else { av.textContent = (myName || '?')[0].toUpperCase(); }
  }
}

// ─── Ctrl button helper ───────────────────────────────────────────────────────
function setCtrl(btnId, lblId, active, onLabel, offLabel) {
  const btn = $(btnId);
  if (btn) { btn.classList.toggle('on', active); btn.classList.toggle('off-state', !active); if (active) btn.classList.remove('off-state'); }
  const lbl = $(lblId);
  if (lbl) { lbl.textContent = active ? (onLabel || offLabel || '') : (offLabel || ''); lbl.className = 'ctrl-lbl' + (active ? ' on' : ' off'); }
}

function setStreamerButtons(enabled) {
  ['btnScreen', 'btnCam', 'btnMic', 'btnStop'].forEach(id => { const b = $(id); if (b) b.disabled = !enabled; });
}

function applyRoleUI(role) {
  const stb = $('streamerToolbar'), vtb = $('viewerToolbar');
  if (stb) stb.style.display = role === 'streamer' ? 'flex' : 'none';
  if (vtb) vtb.style.display = role === 'viewer'   ? 'flex' : 'none';
}
window.applyRoleUI = applyRoleUI;

// ─── Quality system ───────────────────────────────────────────────────────────

function requestQuality(key) {
  if (!QUALITY[key]) return;
  viewerQuality = key;

  
  document.querySelectorAll('.qual-btn').forEach(b => b.classList.toggle('active', b.dataset.quality === key));

  if (key === 'auto') {
    startABR();
    return;
  }
  stopABR();
  sendQualityRequest(key);
}
window.requestQuality = requestQuality;

function sendQualityRequest(key) {
  if (!streamerConnId || !QUALITY[key]) return;
  const preset = QUALITY[key];
  wsSend({
    type: 'quality-request',
    to:   streamerConnId,
    data: JSON.stringify({ scale: preset.scale, maxBitrate: preset.maxBitrate, label: preset.label }),
  });
  dlog('Quality request → ' + preset.label + ' (scale=' + preset.scale + ')');
}

// ─── ABR (Adaptive Bitrate) — Viewer side ────────────────────────────────────
function startABR() {
  stopABR();
  abrGoodCount = 0; abrBadCount = 0;
  abrLastLost = 0; abrLastReceived = 0;
  
  if (abrIndex < 0) abrIndex = ABR_LADDER.indexOf('720p');
  abrTimer = setInterval(checkABR, 3000);
  dlog('ABR started at ' + ABR_LADDER[abrIndex]);
}

function stopABR() {
  if (abrTimer) { clearInterval(abrTimer); abrTimer = null; }
}

async function checkABR() {
  if (!viewerPc || viewerQuality !== 'auto') { stopABR(); return; }
  try {
    const stats = await viewerPc.getStats();
    let lostDelta = 0, receivedDelta = 0, jitter = 0;
    stats.forEach(r => {
      if (r.type === 'inbound-rtp' && r.kind === 'video') {
        lostDelta     = (r.packetsLost    || 0) - abrLastLost;
        receivedDelta = (r.packetsReceived || 0) - abrLastReceived;
        jitter        = r.jitter || 0;
        abrLastLost     = r.packetsLost    || 0;
        abrLastReceived = r.packetsReceived || 0;
      }
    });

    const total    = lostDelta + receivedDelta;
    const lossRate = total > 0 ? lostDelta / total : 0;
    const isBad    = lossRate > 0.02 || jitter > 0.05;

    if (isBad) {
      abrBadCount++; abrGoodCount = 0;
      if (abrBadCount >= 2 && abrIndex > 0) {
        abrIndex--;
        abrBadCount = 0;
        const key = ABR_LADDER[abrIndex];
        dlog('ABR ↓ → ' + key + ' (loss=' + (lossRate * 100).toFixed(1) + '% jitter=' + (jitter * 1000).toFixed(0) + 'ms)');
        sendQualityRequest(key);
        updateABRLabel(key);
      }
    } else {
      abrGoodCount++; abrBadCount = 0;
      
      if (abrGoodCount >= 5 && abrIndex < ABR_LADDER.length - 1) {
        abrIndex++;
        abrGoodCount = 0;
        const key = ABR_LADDER[abrIndex];
        dlog('ABR ↑ → ' + key);
        sendQualityRequest(key);
        updateABRLabel(key);
      }
    }
  } catch (e) { dlog('ABR stats error: ' + e.message); }
}

function updateABRLabel(key) {
  const btn = document.querySelector('.qual-btn[data-quality="auto"]');
  if (btn) btn.textContent = 'Auto (' + (QUALITY[key] ? QUALITY[key].label : key) + ')';
}

// ─── Streamer: apply quality to a viewer's PC ─────────────────────────────────
async function applyViewerQuality(pc, scale, maxBitrate) {
  for (const sender of pc.getSenders()) {
    if (!sender.track) continue;
    try {
      const params = sender.getParameters();
      if (!params.encodings || params.encodings.length === 0) continue;
      if (sender.track.kind === 'video') {
  if (scale && scale > 1) {
    params.encodings[0].scaleResolutionDownBy = scale;
  } else {
    delete params.encodings[0].scaleResolutionDownBy;
  }

  if (maxBitrate > 0) {
    params.encodings[0].maxBitrate = maxBitrate;
  } else {
    delete params.encodings[0].maxBitrate;
  }
}
      if (sender.track.kind === 'audio') params.encodings[0].maxBitrate = 128_000;
      await sender.setParameters(params);
    } catch (e) { /* may fail if connection not yet stable */ }
  }
}

// ─── Streamer: create PeerConnection for a viewer ─────────────────────────────
function makePcForViewer(vid) {
  const pc = new RTCPeerConnection(ICE);
  pc.oniceconnectionstatechange = () => {
    dlog('ICE[' + vid.slice(0, 6) + ']: ' + pc.iceConnectionState);
    if (pc.iceConnectionState === 'failed') { dlog('ICE restart'); pc.restartIce(); }
  };
  pc.onicecandidate = e => {
    if (e.candidate) wsSend({ type: 'candidate', to: vid, data: e.candidate });
  };
  pc.onconnectionstatechange = () => {
    dlog('PC[' + vid.slice(0, 6) + ']: ' + pc.connectionState);
  };
  return pc;
}

async function renegotiate(vid) {
  const pc = pcs.get(vid); if (!pc) return;
if (pc.signalingState !== 'stable') { dlog('renegotiate: not stable (' + pc.signalingState + ')'); return; }

 
  pc.getSenders().forEach(s => { try { pc.removeTrack(s); } catch {} });

  
  if (localScreen) {
    const vt = localScreen.getVideoTracks()[0];
    const at = localScreen.getAudioTracks()[0];
    if (vt) { vt.contentHint = 'motion'; pc.addTrack(vt, localScreen); }  
    if (at) pc.addTrack(at, localScreen);                                  
  }
  if (localMic && micOn) {
    localMic.getAudioTracks().forEach(t => { t.enabled = true; pc.addTrack(t, localMic); }); 
  }
  if (localCam && camOn) {
    const vt = localCam.getVideoTracks()[0];
    if (vt) { vt.contentHint = 'motion'; pc.addTrack(vt, localCam); }     
  }

  
  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  wsSend({ type: 'offer', to: vid, data: pc.localDescription });
}

async function renegotiateAll() {
  for (const vid of viewers) {
    if (!pcs.has(vid)) pcs.set(vid, makePcForViewer(vid));
    await renegotiate(vid);
  }
}


async function applyStoredQuality(vid, pc) {
  const q = vqMap.get(vid);
  if (!q) return;
  
  setTimeout(() => applyViewerQuality(pc, q.scale, q.maxBitrate), 300);
}

// ─── CONNECT ─────────────────────────────────────────────────────────────────
async function wsConnect() {
  if (ws && ws.readyState <= WebSocket.OPEN) return;

  myRole = ($('role') || { value: 'viewer' }).value || 'viewer';
  let session = {};
  try { session = JSON.parse(localStorage.getItem('ss_session') || '{}'); } catch {}
  myName   = session.username   || '';
  myAvatar = session.avatar_url || '';
  const token = session.token   || '';

  if (myRole === 'streamer') {
    if (!myName) { if (typeof toast === 'function') toast('Log in to stream', 'err'); return; }
    myRoom = myName;
  } else {
    myRoom = (($('room') || { value: '' }).value || '').trim() || 'lobby';
  }

  const pass  = ($('roomPass')    || { value: '' }).value || '';
  const title = ($('streamTitle') || { value: '' }).value.trim() || '';
  const cat   = ($('streamCat')   || { value: '' }).value || '';

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  let url = `${proto}://${location.host}/ws?room=${encodeURIComponent(myRoom)}&role=${myRole}`;
  if (pass)  url += `&pass=${encodeURIComponent(pass)}`;
  if (token) url += `&token=${encodeURIComponent(token)}`;
  if (title) url += `&title=${encodeURIComponent(title)}`;
  if (cat)   url += `&category=${encodeURIComponent(cat)}`;

  dlog(`Connecting → room="${myRoom}" role=${myRole}`);
  connState = 'connecting';
  ws = new WebSocket(url);

  ws.onopen = () => {
    connState = 'connected';
    reconnectAttempts = 0;
    reconnectCancelFlag = false;
    hideReconnectOverlay();
    setWsDot('on');
    const bc = $('btnConnect');
    if (bc) { bc.disabled = true; bc.classList.add('on'); bc.classList.remove('primary'); }
    setText('connectLbl', 'connected');
    const cd = $('ctrlDisc'); if (cd) cd.style.display = '';
    applyRoleUI(myRole);
    if (myRole === 'streamer') { setStreamerButtons(true); if (title) showStreamInfo(title, cat); }
    dlog('Connected ✓');
  };

  ws.onclose = e => {
    connState = 'idle';
    setWsDot('off');
    dlog(`WS closed (code=${e.code} reason="${e.reason || ''}")`);
    stopRttCollector();
    
    resetConnectBtn();
    if (!reconnectCancelFlag && e.code !== 1000) {
      scheduleReconnect(e.code);
    }
  };

  ws.onerror = () => setWsDot('err');

  ws.onmessage = async e => {
    let m; try { m = JSON.parse(e.data); } catch { return; }
    await handleMsg(m);
  };
}

function wsDisconnect() {
  reconnectCancelFlag = true;
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  hideReconnectOverlay();
  if (ws) { try { ws.close(1000); } catch {} ws = null; }
  stopRttCollector(); stopABR();
  [localScreen, localCam, localMic].forEach(s => s?.getTracks().forEach(t => t.stop()));
  localScreen = localCam = localMic = null; screenOn = camOn = micOn = false;
  
  if (viewerPc) { try { viewerPc.close(); } catch {} viewerPc = null; }
  cleanupViewerVideo();

  onClose();
}

function resetConnectBtn() {
  const bc = $('btnConnect');
  if (bc) { bc.disabled = false; bc.classList.remove('on'); bc.classList.add('primary'); }
  setText('connectLbl', 'connect');
  const cd = $('ctrlDisc'); if (cd) cd.style.display = 'none';
  setStreamerButtons(false);
}

function onClose() {
  resetConnectBtn();
  viewers.clear();
  pcs.forEach(pc => { try { pc.close(); } catch {} });
  pcs.clear(); vqMap.clear();
  streamerConnId = null;
  setViewers(0);
  applyRoleUI('none');
}

// ─── Message handler ──────────────────────────────────────────────────────────
async function handleMsg(m) {
  const data = parseData(m.data);

  switch (m.type) {

  case 'server-shutdown':
    if (typeof toast === 'function') toast('Server restarting…', 'err');
    reconnectCancelFlag = false; 
    wsDisconnect();
    setTimeout(() => wsConnect(), 3000);
    return;

  case 'init':
    myConnId = data.id;
    myName   = data.name || data.id;
    myAvatar = data.avatar_url || myAvatar;
    myRole   = data.role || myRole;
    myRoom   = data.room || myRoom;
    
    if (data.streamer_conn_id) {
      streamerConnId = data.streamer_conn_id;
      dlog('Streamer connId from init: ' + streamerConnId.slice(0, 6));
    }
    setText('myId', myName);
    if (myRole === 'viewer') { const re = $('room'); if (re) re.value = myRoom; }
    dlog(`Init: id=${myConnId} name=${myName} role=${myRole} room=${myRoom}`);
    return;

  case 'streamer-info':
    
    if (data.streamer_conn_id) {
      streamerConnId = data.streamer_conn_id;
      dlog('Streamer info: connId=' + streamerConnId.slice(0, 6));
    }
    if (data.title || data.category) showStreamInfo(data.title, data.category);
    
    if (viewerQuality !== 'auto') sendQualityRequest(viewerQuality);
    return;

  case 'error': {
    const msg = typeof m.data === 'string' ? m.data.replace(/"/g, '') : (data.message || JSON.stringify(data));
    dlog('Server error: ' + msg);
    showError('error', msg);
    if (typeof toast === 'function') toast(msg, 'err');
    return;
  }

  case 'kicked':
    showError('kicked', data.reason || '');
    if (typeof toast === 'function') toast('You were kicked: ' + (data.reason || ''), 'err');
    reconnectCancelFlag = true; 
    wsDisconnect();
    return;

  case 'banned':
    showError('banned', data.reason || '');
    if (typeof toast === 'function') toast('You are banned: ' + (data.reason || ''), 'err');
    reconnectCancelFlag = true; 
    wsDisconnect();
    return;

  case 'chat':
    
    appendChat(data.conn_id || m.from, data.username, data.role, data.text, data.avatar_url);
    return;

  case 'stream-meta':
    showStreamInfo(data.title, data.category);
    return;

  case 'stream-ended':
    stopABR(); stopRttCollector();
    showError('ended', 'The streamer ended the broadcast');
    if (viewerPc) { try { viewerPc.close(); } catch {} viewerPc = null; }
    cleanupViewerVideo();
    streamerConnId = null;
    return;

  // ── Streamer receives these ──
  case 'join':
    if (myRole !== 'streamer') return;
    if (!data || data.role !== 'viewer') return;
    {
      const vid = m.from;
      viewers.add(vid);
      setViewers(viewers.size);
      dlog('Viewer joined: ' + data.name + ' (' + vid.slice(0, 6) + ')');
      if (!pcs.has(vid)) pcs.set(vid, makePcForViewer(vid));
      if (localScreen || localCam || localMic) await renegotiate(vid);
    }
    return;

  case 'leave':
    if (myRole === 'streamer') {
      const vid = m.from;
      viewers.delete(vid);
      setViewers(viewers.size);
      const pc = pcs.get(vid); if (pc) { try { pc.close(); } catch {} }
      pcs.delete(vid); vqMap.delete(vid);
      dlog('Viewer left: ' + (data.name || vid.slice(0, 6)));
    } else if (myRole === 'viewer' && data.role === 'streamer') {
      
      cleanupViewerVideo();
    }
    return;

  case 'quality-request':
    
    if (myRole === 'streamer') {
      const vid = m.from;
      const q = data; 
      vqMap.set(vid, { scale: q.scale || 1, maxBitrate: q.maxBitrate || 0 });
      const pc = pcs.get(vid);
      if (pc && pc.connectionState === 'connected') {
        await applyViewerQuality(pc, q.scale || 1, q.maxBitrate || 0);
        dlog('Quality applied for ' + vid.slice(0, 6) + ': ' + q.label);
      }
    }
    return;

  case 'answer':
    if (myRole === 'streamer') {
      const pc = pcs.get(m.from);
      if (pc) {
        try {
          await pc.setRemoteDescription(m.data);
          await applyStoredQuality(m.from, pc);
        } catch (e) { dlog('setRemoteDescription error: ' + e.message); }
      }
    }
    return;

  case 'offer':
    if (myRole === 'viewer') await handleViewerOffer(m);
    return;

  case 'candidate':
    if (myRole === 'streamer') {
      const pc = pcs.get(m.from);
      if (pc) try { await pc.addIceCandidate(m.data); } catch {}
    } else if (viewerPc) {
      try { await viewerPc.addIceCandidate(m.data); } catch {}
    }
    return;
  }
}

// ─── Viewer: handle offer from streamer ───────────────────────────────────────
async function handleViewerOffer(m) {
  if (viewerPc) { try { viewerPc.close(); } catch {} viewerPc = null; }
  stopRttCollector();
  vidCount = 0; audCount = 0;
  initAudio();
const vidEl = $('screenVideo');
const camEl = $('camVideo');

if (vidEl) {
  vidEl.pause();
  vidEl.srcObject = null;
  vidEl.load();
}
  viewerPc = new RTCPeerConnection(ICE);

  viewerPc.oniceconnectionstatechange = () => {
    dlog('ICE(viewer): ' + viewerPc.iceConnectionState);
    if (viewerPc.iceConnectionState === 'failed') viewerPc.restartIce();
  };
  viewerPc.onconnectionstatechange = () => {
    const s = viewerPc.connectionState;
    dlog('PC(viewer): ' + s);
    if (s === 'connected') {
      startRttCollector(viewerPc);
      if (viewerQuality === 'auto') startABR();
    }
    if (['failed', 'disconnected', 'closed'].includes(s)) {
      stopRttCollector(); stopABR();
    }
  };
  viewerPc.onicecandidate = ev => {
    if (ev.candidate) wsSend({ type: 'candidate', to: m.from, data: ev.candidate });
  };

viewerPc.ontrack = ev => {
  const track = ev.track;
  const vidEl = $('screenVideo');
  const camEl = $('camVideo');

  if (track.kind === 'video') {
    vidCount++; 

    if (vidCount === 1) {
      const ms = new MediaStream([track]);

      if (vidEl) {
        vidEl.srcObject = ms;
        vidEl.muted = true;
        vidEl.play().catch(() => {});
      }
    vidEl.play().catch(() => {});

    track.onunmute = () => {
      vidEl.play().catch(() => {});
      dlog('Video track unmuted → play()');
    };

    
    if (!track.muted) {
      vidEl.play().catch(() => {});
    }

      const ph = $('stagePh');
      if (ph) ph.style.display = 'none';

      hideError();
      dlog('Screen video track ▶');

    } else {
      
      const cs = new MediaStream([track]);

      if (camEl) camEl.srcObject = cs;

      setClass('camPip', 'on', true);
      dlog('Camera PIP track ▶');
    }
  }else if (track.kind === 'audio') {
      audCount++;
      if (audCount === 1) {
        
        if (audioReady && gainGame) {
          srcGame = routeAudio(track, gainGame, srcGame);
          
          if (vidEl) vidEl.muted = true;
          dlog('Game audio → Web Audio (gainGame)');
        } else {
          
          if (vidEl && vidEl.srcObject) vidEl.srcObject.addTrack(track);
          dlog('Game audio → video element (Web Audio unavailable)');
        }
      } else {
        if (audioReady && gainMic) {
          srcMic = routeAudio(track, gainMic, srcMic);
          dlog('Mic audio → Web Audio (gainMic)');
        }
      }
    }
  };

  try {
    await viewerPc.setRemoteDescription(m.data);
    const answer = await viewerPc.createAnswer();
    await viewerPc.setLocalDescription(answer);
    wsSend({ type: 'answer', to: m.from, data: viewerPc.localDescription });
    dlog('Answer sent ✓');

    if (streamerConnId && viewerQuality !== 'auto') {
      setTimeout(() => sendQualityRequest(viewerQuality), 500);
    }
  } catch (e) {
    dlog('Offer handling error: ' + e.message);
    showError('error', 'Failed to connect to stream: ' + e.message);
  }
}

function cleanupViewerVideo() {
  const sv = $('screenVideo'); if (sv) { sv.srcObject = null; }
  const cv = $('camVideo');    if (cv) { cv.srcObject = null; }

  setClass('camPip', 'on', false);
  
  const ph = $('stagePh'); if (ph) ph.style.display = 'flex';
  if (srcGame) { try { srcGame.disconnect(); } catch {} srcGame = null; }
  if (srcMic)  { try { srcMic.disconnect();  } catch {} srcMic  = null; }
}

// ─── Streamer media controls ──────────────────────────────────────────────────
async function startScreen() {
  if (screenOn) {
    if (localScreen) { localScreen.getTracks().forEach(t => t.stop()); localScreen = null; }
    const el = $('screenVideo'); if (el) el.srcObject = null;
    screenOn = false;
    setCtrl('btnScreen', 'screenLbl', false, '', 'screen');
    const ph = $('stagePh'); if (ph) ph.style.display = '';
    await renegotiateAll();
    dlog('Screen off');
    return;
  }
  try {
    const fps = parseInt(cfg('fps', 60));
    localScreen = await navigator.mediaDevices.getDisplayMedia({
      video: { frameRate: { ideal: fps, max: fps === 0 ? undefined : fps }, cursor: 'always'},
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, sampleRate: 48000 },
    });
    const el = $('screenVideo'); if (el) { el.srcObject = localScreen; el.muted = true; }
    screenOn = true;
    const ph = $('stagePh'); if (ph) ph.style.display = 'none';
    hideError();
    setCtrl('btnScreen', 'screenLbl', true, 'screen ●', 'screen');
    showStreamInfo(($('streamTitle') || { value: '' }).value, ($('streamCat') || { value: '' }).value);

    localScreen.getVideoTracks()[0].addEventListener('ended', () => {
      localScreen = null; screenOn = false;
      const el2 = $('screenVideo'); if (el2) el2.srcObject = null;
      setCtrl('btnScreen', 'screenLbl', false, '', 'screen');
      const ph2 = $('stagePh'); if (ph2) ph2.style.display = '';
      renegotiateAll(); wsSend({ type: 'stream-ended' });
      dlog('Screen capture ended by OS');
    });

    await renegotiateAll();
    dlog('Screen on (' + fps + 'fps)');
  } catch (e) {
    dlog('Screen error: ' + (e.message || e));
    if (typeof toast === 'function') toast('Screen share failed: ' + (e.message || e), 'err');
  }
}

async function toggleCam() {
  if (camOn) {
    if (localCam) { localCam.getTracks().forEach(t => t.stop()); localCam = null; }
    const el = $('camVideo'); if (el) el.srcObject = null;
    setClass('camPip', 'on', false); camOn = false;
    setCtrl('btnCam', 'camLbl', false, '', 'camera');
    dlog('Camera off');
  } else {
    try {
      localCam = await navigator.mediaDevices.getUserMedia({
        video: { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 30 } },
        audio: false,
      });
      const el = $('camVideo'); if (el) el.srcObject = localCam;
      setClass('camPip', 'on', true); camOn = true;
      setCtrl('btnCam', 'camLbl', true, 'cam ●', 'camera');
      dlog('Camera on');
    } catch (e) { dlog('Camera error: ' + (e.message || e)); return; }
  }
  await renegotiateAll();
}

async function toggleMic() {
  if (micOn) {
    if (localMic) { localMic.getTracks().forEach(t => t.stop()); localMic = null; }
    micOn = false; setClass('micBadge', 'on', true);
    setCtrl('btnMic', 'micLbl', false, '', 'mic'); dlog('Mic off');
  } else {
    try {
      localMic = await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: cfg('echoCancel', true), noiseSuppression: cfg('noiseSup', true), autoGainControl: true, sampleRate: 48000 },
        video: false,
      });
      micOn = true; setClass('micBadge', 'on', false);
      setCtrl('btnMic', 'micLbl', true, 'mic ●', 'mic'); dlog('Mic on');
    } catch (e) { dlog('Mic error: ' + (e.message || e)); return; }
  }
  await renegotiateAll();
}

async function stopAll() {
  wsSend({ type: 'stream-ended' });
  [localScreen, localCam, localMic].forEach(s => s?.getTracks().forEach(t => t.stop()));
  localScreen = localCam = localMic = null; screenOn = camOn = micOn = false;
  const sv = $('screenVideo'); if (sv) sv.srcObject = null;
  const cv = $('camVideo');    if (cv) cv.srcObject = null;
  setClass('camPip', 'on', false);
  const ph = $('stagePh'); if (ph) ph.style.display = '';
  setClass('micBadge', 'on', false);
  setCtrl('btnScreen', 'screenLbl', false, '', 'screen');
  setCtrl('btnCam',    'camLbl',    false, '', 'camera');
  setCtrl('btnMic',    'micLbl',    false, '', 'mic');
  pcs.forEach(pc => { try { pc.close(); } catch {} });
  pcs.clear(); vqMap.clear();
  const bar = $('streamInfoBar'); if (bar) bar.style.display = 'none';
  dlog('Stream stopped');
}

function updateStreamMeta() {
  const title = ($('streamTitle') || { value: '' }).value || '';
  const cat   = ($('streamCat')   || { value: '' }).value || '';
  wsSend({ type: 'stream-update', data: JSON.stringify({ title, category: cat }) });
  showStreamInfo(title, cat);
}

// ─── Viewer fullscreen / theater ──────────────────────────────────────────────
function toggleFullscreen() {
  const el = $('stage'); if (!el) return;
  if (!document.fullscreenElement) el.requestFullscreen().catch(() => {});
  else document.exitFullscreen();
}

let theaterMode = false;
function toggleTheater() {
  theaterMode = !theaterMode;
  const sl = document.querySelector('.stream-layout');
  if (sl) sl.style.gridTemplateColumns = theaterMode ? '1fr' : '';
  const cp = document.querySelector('.chat-panel');
  if (cp) cp.style.display = theaterMode ? 'none' : '';
  const btn = $('btnTheater'); if (btn) btn.classList.toggle('on', theaterMode);
  const lbl = $('theaterLbl'); if (lbl) lbl.textContent = theaterMode ? 'exit theater' : 'theater';
}

// ─── Camera PIP drag ──────────────────────────────────────────────────────────
(() => {
  let drag = false, sx = 0, sy = 0, sl = 0, st = 0;
  const clamp = (v, a, b) => Math.max(a, Math.min(b, v));
  document.addEventListener('mousedown', e => {
    const pip = $('camPip'), stage = $('stage');
    if (!pip || !stage || !pip.contains(e.target)) return;
    const r = pip.getBoundingClientRect();
    if ((r.right - e.clientX) < 20 && (r.bottom - e.clientY) < 20) return;
    drag = true;
    const sr = stage.getBoundingClientRect();
    sx = e.clientX; sy = e.clientY; sl = r.left - sr.left; st = r.top - sr.top;
    pip.style.right = 'auto'; pip.style.bottom = 'auto';
    pip.style.left = sl + 'px'; pip.style.top = st + 'px';
    e.preventDefault();
  });
  document.addEventListener('mouseup', () => { drag = false; });
  document.addEventListener('mousemove', e => {
    if (!drag) return;
    const pip = $('camPip'), stage = $('stage'); if (!pip || !stage) return;
    const sr = stage.getBoundingClientRect();
    pip.style.left = clamp(sl + (e.clientX - sx), 0, sr.width  - pip.offsetWidth)  + 'px';
    pip.style.top  = clamp(st + (e.clientY - sy), 0, sr.height - pip.offsetHeight) + 'px';
  });
})();

// ─── Utils ────────────────────────────────────────────────────────────────────
function parseData(raw) {
  if (!raw) return {};
  if (typeof raw === 'object') return raw;
  try { return JSON.parse(raw); } catch { return {}; }
}

document.addEventListener('click',    resumeAudio, { passive: true });
document.addEventListener('keydown',  e => { if (e.key === 'f' && !e.target.matches('input,textarea,select')) toggleFullscreen(); }, { passive: true });

// ─── Exports ──────────────────────────────────────────────────────────────────
window.applyViewerQualityAll = async function(key) {
  const preset = QUALITY[key];
  if (!preset) return;
  dlog('Source quality → ' + preset.label);
  for (const [vid, pc] of pcs.entries()) {
    if (pc.connectionState === 'connected') {
      vqMap.set(vid, { scale: preset.scale, maxBitrate: preset.maxBitrate });
      await applyViewerQuality(pc, preset.scale, preset.maxBitrate);
    }
  }
};

window.wsConnect        = wsConnect;
window.wsDisconnect     = wsDisconnect;
window.startScreen      = startScreen;
window.toggleCam        = toggleCam;
window.toggleMic        = toggleMic;
window.stopAll          = stopAll;
window.sendChat         = sendChat;
  window.wsSend           = wsSend; 
window.updateStreamMeta = updateStreamMeta;
window.toggleFullscreen = toggleFullscreen;
window.toggleTheater    = toggleTheater;
window.requestQuality   = requestQuality;
window.applyRoleUI      = applyRoleUI;

})();