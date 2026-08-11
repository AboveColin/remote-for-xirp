/* A deliberately small markdown renderer for agent replies.
 *
 * Agent output is markdown — fenced code, inline code, lists, headings, links —
 * and rendering it as flat preformatted text is what made the chat hard to read on
 * a phone. This covers what agents actually emit and stops there.
 *
 * It builds DOM nodes and never touches innerHTML, so there is no HTML-injection
 * path: a message containing `<script>` becomes literal text, because it can only
 * ever become a text node. That is also why there is no sanitiser here — nothing
 * is ever parsed as HTML in the first place.
 */

// Inline: `code`, **bold**, *italic*, [text](url), and bare URLs.
const INLINE = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(\*[^*\n]+\*)|(\[[^\]\n]+\]\([^)\s]+\))|(https?:\/\/[^\s<>()]+)/g;

function appendInline(parent, text) {
  let last = 0;
  for (const m of text.matchAll(INLINE)) {
    if (m.index > last) parent.append(document.createTextNode(text.slice(last, m.index)));
    const [tok] = m;
    if (tok.startsWith('`')) {
      const c = document.createElement('code');
      c.textContent = tok.slice(1, -1);
      parent.append(c);
    } else if (tok.startsWith('**')) {
      const b = document.createElement('strong');
      b.textContent = tok.slice(2, -2);
      parent.append(b);
    } else if (tok.startsWith('[')) {
      const close = tok.indexOf('](');
      const a = document.createElement('a');
      a.textContent = tok.slice(1, close);
      a.href = tok.slice(close + 2, -1);
      a.target = '_blank';
      a.rel = 'noreferrer';
      parent.append(a);
    } else if (tok.startsWith('http')) {
      const a = document.createElement('a');
      a.textContent = tok.replace(/^https?:\/\//, '');
      a.href = tok;
      a.target = '_blank';
      a.rel = 'noreferrer';
      parent.append(a);
    } else {
      const i = document.createElement('em');
      i.textContent = tok.slice(1, -1);
      parent.append(i);
    }
    last = m.index + tok.length;
  }
  if (last < text.length) parent.append(document.createTextNode(text.slice(last)));
}

function flushList(frag, items, ordered) {
  if (!items.length) return;
  const list = document.createElement(ordered ? 'ol' : 'ul');
  for (const item of items) {
    const li = document.createElement('li');
    appendInline(li, item);
    list.append(li);
  }
  frag.append(list);
  items.length = 0;
}

function flushPara(frag, lines) {
  if (!lines.length) return;
  const p = document.createElement('p');
  // A single newline inside a paragraph is a soft wrap in markdown, but agents use
  // it to mean a line break, so honour it.
  appendInline(p, lines.join('\n'));
  frag.append(p);
  lines.length = 0;
}

function renderMarkdown(text) {
  const frag = document.createDocumentFragment();
  const lines = String(text).split('\n');
  let para = [];
  let items = [];
  let ordered = false;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];

    // Fenced code. Everything until the closing fence is literal, including any
    // markdown-looking characters inside it.
    const fence = /^\s*```(\w+)?\s*$/.exec(line);
    if (fence) {
      flushPara(frag, para);
      flushList(frag, items, ordered);
      const body = [];
      i++;
      while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) body.push(lines[i++]);
      const pre = document.createElement('pre');
      const code = document.createElement('code');
      code.textContent = body.join('\n');
      if (fence[1]) pre.dataset.lang = fence[1];
      pre.append(code);
      frag.append(pre);
      continue;
    }

    const heading = /^(#{1,4})\s+(.*)$/.exec(line);
    if (heading) {
      flushPara(frag, para);
      flushList(frag, items, ordered);
      const h = document.createElement('div');
      h.className = `md-h md-h${heading[1].length}`;
      appendInline(h, heading[2]);
      frag.append(h);
      continue;
    }

    const bullet = /^\s*[-*+]\s+(.*)$/.exec(line);
    const numbered = /^\s*\d+[.)]\s+(.*)$/.exec(line);
    if (bullet || numbered) {
      flushPara(frag, para);
      const isOrdered = Boolean(numbered);
      if (items.length && isOrdered !== ordered) flushList(frag, items, ordered);
      ordered = isOrdered;
      items.push((bullet || numbered)[1]);
      continue;
    }

    if (!line.trim()) {
      flushPara(frag, para);
      flushList(frag, items, ordered);
      continue;
    }

    flushList(frag, items, ordered);
    para.push(line);
  }

  flushPara(frag, para);
  flushList(frag, items, ordered);
  return frag;
}

// Loaded as a classic script before app.js, so it publishes one global rather than
// forcing the whole UI to become an ES module.
window.renderMarkdown = renderMarkdown;
