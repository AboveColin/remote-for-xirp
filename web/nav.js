/* Machines → folders → sessions.
 *
 * The app opens on the machines it knows about, not on a session list, because a
 * session list only means something once you have said which machine you mean. That
 * order also makes "add another machine" an obvious first-class action rather than a
 * setting buried three taps deep.
 *
 * Loaded before app.js. It publishes the pieces app.js calls, and reads the machine
 * list app.js owns, so there is exactly one source of truth for the active machine.
 */

// A machine is reachable if /healthz answers. That endpoint needs no key on purpose:
// a probe that requires the key cannot distinguish "wrong key" from "machine off",
// and those need different words on screen.
async function probeMachine(m) {
  const base = m.url || '';
  const started = performance.now();
  try {
    const res = await fetch(base + '/healthz', { cache: 'no-store', credentials: 'omit' });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return { online: true, ms: Math.round(performance.now() - started) };
  } catch (e) {
    return { online: false, why: e.message };
  }
}

// Session counts per machine, for the card subtitle. Failure is not an error here:
// an unreachable machine simply has no counts to show.
async function machineSummary(m) {
  const headers = {};
  if (m.url && m.key) headers['X-Xirp-Key'] = m.key;
  try {
    const res = await fetch((m.url || '') + '/api/sessions', {
      headers,
      credentials: m.url ? 'omit' : 'same-origin',
      cache: 'no-store',
    });
    if (res.status === 401) return { needsKey: true };
    if (!res.ok) return {};
    const body = await res.json();
    const sessions = body.sessions || [];
    const projects = new Set(sessions.map((s) => s.projectName).filter(Boolean));
    return {
      total: sessions.length,
      running: sessions.filter((s) => s.status === 'running').length,
      projects: projects.size,
    };
  } catch {
    return {};
  }
}

window.probeMachine = probeMachine;
window.machineSummary = machineSummary;

/* ---- QR scanning ----
 *
 * BarcodeDetector is present in Chrome on Android, which is the target. Where it is
 * missing (Safari, Firefox) the manual form is offered instead of a broken camera —
 * shipping a scanner that silently never detects anything would be worse than not
 * having one.
 */

let scanStream = null;
let scanTimer = null;

async function startScan(onResult, onStatus) {
  const video = document.getElementById('scan-video');
  if (!('BarcodeDetector' in window)) {
    onStatus("This browser cannot scan codes. Use manual entry, or open the pairing link directly.");
    return false;
  }
  let detector;
  try {
    const formats = await window.BarcodeDetector.getSupportedFormats();
    if (!formats.includes('qr_code')) {
      onStatus('This browser cannot scan QR codes specifically. Use manual entry.');
      return false;
    }
    detector = new window.BarcodeDetector({ formats: ['qr_code'] });
  } catch (e) {
    onStatus('Scanner unavailable: ' + e.message);
    return false;
  }

  try {
    scanStream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: 'environment' },
      audio: false,
    });
  } catch (e) {
    // The usual causes are a denied permission and a non-secure origin. Both are
    // worth naming, since neither is fixable from inside the app.
    onStatus(
      e && e.name === 'NotAllowedError'
        ? 'Camera permission was refused. Allow it in the site settings, or use manual entry.'
        : 'No camera available: ' + (e && e.message ? e.message : e)
    );
    return false;
  }

  video.srcObject = scanStream;
  await video.play().catch(() => {});
  onStatus('Point the camera at the code');

  scanTimer = setInterval(async () => {
    try {
      const codes = await detector.detect(video);
      if (codes && codes.length) {
        const value = codes[0].rawValue || '';
        stopScan();
        onResult(value);
      }
    } catch {
      // detect() throws while a frame is not ready; the next tick retries.
    }
  }, 350);
  return true;
}

function stopScan() {
  if (scanTimer) clearInterval(scanTimer);
  scanTimer = null;
  if (scanStream) {
    for (const track of scanStream.getTracks()) track.stop();
    scanStream = null;
  }
  const video = document.getElementById('scan-video');
  if (video) video.srcObject = null;
}

// A pairing code is a URL whose fragment carries the key: https://host/#k=<key>
function parsePairing(value) {
  try {
    const u = new URL(String(value).trim());
    const m = /[#&]k=([A-Za-z0-9._-]+)/.exec(u.hash || '');
    return {
      url: u.origin + (u.pathname === '/' ? '' : u.pathname.replace(/\/$/, '')),
      key: m ? m[1] : '',
      host: u.host,
    };
  } catch {
    return null;
  }
}

window.startScan = startScan;
window.stopScan = stopScan;
window.parsePairing = parsePairing;
