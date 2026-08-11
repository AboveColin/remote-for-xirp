/* ANSI SGR to DOM, for showing a tmux pane as it actually looks.
 *
 * Agent TUIs use 256-colour foreground/background, bold, dim, italic, underline and
 * reverse. That is the whole set this handles; anything else in the stream is
 * dropped rather than shown as literal escape text.
 *
 * Like the markdown renderer, this only ever creates text nodes and spans with a
 * style object, so pane content cannot inject markup.
 */

// xterm's 256-colour palette, computed rather than tabulated: 16 system colours,
// a 6x6x6 cube, then 24 greys.
// The first 16 entries are xterm's own documented defaults, not any particular
// editor's or terminal's theme, so nothing here is derived from someone else's design.
const SYSTEM = [
  '#000000', '#800000', '#008000', '#808000', '#000080', '#800080', '#008080', '#c0c0c0',
  '#808080', '#ff0000', '#00ff00', '#ffff00', '#0000ff', '#ff00ff', '#00ffff', '#ffffff',
];

function xterm256(n) {
  if (n < 16) return SYSTEM[n];
  if (n < 232) {
    const i = n - 16;
    const steps = [0, 95, 135, 175, 215, 255];
    return rgb(steps[Math.floor(i / 36) % 6], steps[Math.floor(i / 6) % 6], steps[i % 6]);
  }
  const v = 8 + (n - 232) * 10;
  return rgb(v, v, v);
}

function rgb(r, g, b) {
  return `rgb(${r},${g},${b})`;
}

const DEFAULT = { fg: null, bg: null, bold: false, dim: false, italic: false, underline: false, reverse: false };

function applySGR(state, params) {
  for (let i = 0; i < params.length; i++) {
    const p = params[i];
    if (p === 0) Object.assign(state, DEFAULT);
    else if (p === 1) state.bold = true;
    else if (p === 2) state.dim = true;
    else if (p === 3) state.italic = true;
    else if (p === 4) state.underline = true;
    else if (p === 7) state.reverse = true;
    else if (p === 22) { state.bold = false; state.dim = false; }
    else if (p === 23) state.italic = false;
    else if (p === 24) state.underline = false;
    else if (p === 27) state.reverse = false;
    else if (p >= 30 && p <= 37) state.fg = SYSTEM[p - 30];
    else if (p === 39) state.fg = null;
    else if (p >= 40 && p <= 47) state.bg = SYSTEM[p - 40];
    else if (p === 49) state.bg = null;
    else if (p >= 90 && p <= 97) state.fg = SYSTEM[p - 90 + 8];
    else if (p >= 100 && p <= 107) state.bg = SYSTEM[p - 100 + 8];
    else if (p === 38 || p === 48) {
      // 38;5;N indexed, or 38;2;R;G;B truecolour.
      const target = p === 38 ? 'fg' : 'bg';
      if (params[i + 1] === 5) {
        state[target] = xterm256(params[i + 2] || 0);
        i += 2;
      } else if (params[i + 1] === 2) {
        state[target] = rgb(params[i + 2] || 0, params[i + 3] || 0, params[i + 4] || 0);
        i += 4;
      }
    }
  }
}

function spanFor(state, text) {
  if (!text) return null;
  const plain =
    !state.fg && !state.bg && !state.bold && !state.dim && !state.italic && !state.underline && !state.reverse;
  if (plain) return document.createTextNode(text);

  const span = document.createElement('span');
  let fg = state.fg;
  let bg = state.bg;
  if (state.reverse) {
    // Reverse video is how TUIs draw selections and status bars; without honouring
    // it the pane loses its structure entirely.
    [fg, bg] = [bg || 'var(--bg)', fg || 'var(--text)'];
  }
  if (fg) span.style.color = fg;
  if (bg) span.style.background = bg;
  if (state.bold) span.style.fontWeight = '700';
  if (state.dim) span.style.opacity = '0.6';
  if (state.italic) span.style.fontStyle = 'italic';
  if (state.underline) span.style.textDecoration = 'underline';
  span.textContent = text;
  return span;
}

// CSI sequences: ESC [ params letter. Only 'm' (SGR) changes appearance; the rest
// are cursor moves and erases, which a static capture has already applied.
// OSC sequences (window titles) and charset selects carry nothing to draw, so they
// are consumed too rather than left to appear as stray text.
const ESCAPES = /\[([0-9;?]*)([A-Za-z])|\][^\x07]*(?:\x07|\\)|[()][A-Za-z0-9]/g;

function renderAnsi(text) {
  const frag = document.createDocumentFragment();
  const state = { ...DEFAULT };
  let last = 0;

  for (const m of String(text).matchAll(ESCAPES)) {
    if (m.index > last) {
      const node = spanFor(state, String(text).slice(last, m.index));
      if (node) frag.append(node);
    }
    if (m[2] === 'm') {
      const params = m[1] === '' ? [0] : m[1].split(';').map((n) => parseInt(n, 10) || 0);
      applySGR(state, params);
    }
    last = m.index + m[0].length;
  }
  if (last < String(text).length) {
    const node = spanFor(state, String(text).slice(last));
    if (node) frag.append(node);
  }
  return frag;
}

window.renderAnsi = renderAnsi;
