// Demo data for the landing page's screenshots.
//
// Paste this into the console with the app open, then use shot() to walk the views. It
// replaces the app's `api()` with a lookup table, so nothing is sent to the daemon and no
// real session is read or touched — which is the point: the shots have to show the real
// layout with names that are not anyone's.
//
// It is kept in the repo so the screenshots can be retaken after a UI change instead of
// being re-invented, and so the invented names stay consistent.
//
// Two things that cost time the first time round:
//
//   - Install this *after* the app's own boot requests have landed. They finish a second
//     or so after load and overwrite state.sessions with the real list, so a stub applied
//     immediately looks like it did nothing.
//   - The published screenshots are one phone screen each, 393x852, which is what makes a
//     row of them look like a set rather than seven different crops. This injects that
//     size so what you capture is what the site shows:
//
//       shotViewport();   // 393x852 column, composer pinned to its bottom
//
// The site's images are the top-left 393x852 region of the capture.

(() => {
  const now = Date.now();
  const min = (m) => new Date(now - m * 60000).toISOString();

  const sessions = [
    {
      id: 'd1',
      name: 'Fix flaky auth test',
      projectId: 'p1',
      projectName: 'webapp',
      branch: 'fix/auth-flake',
      status: 'running',
      currentAgent: 'claude',
      lastUserMessage: 'The retry helper is swallowing the timeout, so look at the fixture first.',
      contextTokens: 210000,
      contextWindowSize: 1000000,
      totalCostUsd: 0.42,
      lastActivityAt: min(8),
      hasPane: true,
    },
    {
      id: 'd2',
      name: 'Port the billing module to the new API',
      projectId: 'p1',
      projectName: 'webapp',
      branch: 'feat/billing-v2',
      status: 'running',
      currentAgent: 'codex',
      lastUserMessage: 'Keep the old endpoint working until the cutover.',
      contextTokens: 480000,
      contextWindowSize: 1000000,
      totalCostUsd: 0.18,
      lastActivityAt: min(14),
      hasPane: true,
    },
    {
      id: 'd3',
      name: 'Add pagination to the search endpoint',
      projectId: 'p2',
      projectName: 'api',
      branch: 'feat/search-pages',
      status: 'running',
      currentAgent: 'claude',
      contextTokens: 120000,
      contextWindowSize: 1000000,
      lastActivityAt: min(3),
      hasPane: true,
    },
    {
      id: 'd4',
      name: 'Upgrade the CI runners',
      projectId: 'p3',
      projectName: 'infra',
      branch: 'chore/runners',
      status: 'waiting',
      waitingReason: 'needs your answer',
      currentAgent: 'gemini',
      contextTokens: 90000,
      contextWindowSize: 1000000,
      lastActivityAt: min(21),
      hasPane: true,
    },
    {
      id: 'd5',
      name: 'Tidy the deploy scripts',
      projectId: 'p3',
      projectName: 'infra',
      branch: 'chore/deploy-tidy',
      status: 'running',
      currentAgent: 'claude',
      contextTokens: 60000,
      contextWindowSize: 1000000,
      lastActivityAt: min(45),
      hasPane: true,
    },
  ];

  const transcript = [
    {
      role: 'user',
      type: 'message',
      ts: min(12),
      text: 'The auth test fails about one run in five. Find out why before changing anything.',
    },
    { role: 'assistant', type: 'tool_use', ts: min(11), text: 'Bash(pytest -k auth --count 20)' },
    { role: 'assistant', type: 'tool_use', ts: min(10), text: 'Read(tests/conftest.py)' },
    {
      role: 'assistant',
      type: 'message',
      ts: min(9),
      text:
        'It is the fixture, not the assertion.\n\n' +
        'The helper builds a token with a **one second** expiry and the suite reuses it ' +
        'across two requests. When the second request lands late the token is already ' +
        'dead, so the failure looks like a flaky assertion.\n\n' +
        '```python\ntoken = make_token(ttl=1)   # too short to survive a slow run\n```\n\n' +
        'Two options:\n\n' +
        '- widen the TTL for tests only\n' +
        '- mint a fresh token per request\n\n' +
        'The second is slower but honest, and it would have caught this. Which do you want?',
    },
  ];

  const changes = {
    branch: 'fix/auth-flake',
    base: 'main',
    pr: { number: 284, title: 'Mint a fresh auth token per request', url: '#', state: 'OPEN' },
    branchDiff: {
      files: [
        { path: 'app/auth/tokens.py', status: 'M', additions: 31, deletions: 9 },
        { path: 'tests/test_auth.py', status: 'M', additions: 18, deletions: 6 },
        { path: 'tests/conftest.py', status: 'M', additions: 12, deletions: 4 },
        { path: 'CHANGELOG.md', status: 'M', additions: 3, deletions: 0 },
      ],
    },
    worktree: {
      untracked: 2,
      files: [{ path: 'tests/conftest.py', status: 'M', additions: 12, deletions: 4 }],
    },
  };

  const fileDiff = `diff --git a/app/auth/tokens.py b/app/auth/tokens.py
index 3f1c2ab..9d4e77c 100644
--- a/app/auth/tokens.py
+++ b/app/auth/tokens.py
@@ -18,9 +18,12 @@ def make_token(subject: str, ttl: int = 1)
     """Build a signed token for the given subject."""
-    expires = now() + timedelta(seconds=ttl)
-    return sign({"sub": subject, "exp": expires})
+    # A one second expiry cannot survive a slow test run, and when it lapses mid-suite it
+    # produces what looks like a flaky assertion rather than an expired token.
+    expires = now() + timedelta(seconds=max(ttl, 30))
+    return sign({"sub": subject, "exp": expires, "iat": now()})
@@ -44,6 +47,7 @@ def verify(token: str) -> Claims:
     claims = unsign(token)
+    if claims.expired:
+        raise TokenExpired(claims.sub)
     return claims`;

  // What the app can offer depends on the daemon's active modules, so the stub has to
  // report them: without `saved-prompts` the composer hides its prompts button.
  const prompts = [
    {
      id: '0c9f7b26-1d4a-4c3b-9f21-8ab0c5e41f77',
      name: 'Review before merge',
      prompt:
        'Read the diff against the base branch. List anything that changes behaviour without a test, then stop and wait.',
      createdAt: '2026-08-24T09:12:00.000Z',
      updatedAt: '2026-08-24T09:12:00.000Z',
    },
    {
      id: '4b1d8e50-77aa-4a11-8c62-2f0e9d3b6a15',
      name: 'Reproduce first',
      prompt:
        'Write the failing test that shows the bug, run it, paste the output, and only then change any source file.',
      createdAt: '2026-08-19T18:40:00.000Z',
      updatedAt: '2026-08-19T18:40:00.000Z',
    },
  ];

  const routes = [
    [/^\/api\/sessions$/, () => ({ sessions, modules: ['session-search', 'saved-prompts'] })],
    [/^\/api\/prompts$/, () => ({ prompts })],
    [
      /^\/api\/sessions\/([^/?]+)\?/,
      (id) => ({
        session: sessions.find((s) => s.id === id) || sessions[0],
        messages: transcript,
        totalMessages: 128,
        truncatedFromStart: true,
      }),
    ],
    [/^\/api\/sessions\/[^/]+\/changes$/, () => changes],
    [/^\/api\/sessions\/[^/]+\/diff/, () => ({ diff: fileDiff, truncated: false })],
    [/^\/api\/sessions\/[^/]+\/urls$/, () => ({ urls: ['https://staging.example.com/preview/284'] })],
    [
      /^\/api\/sessions\/[^/]+\/log$/,
      () => ({
        commits: [
          { shortHash: 'c1f90ab', subject: 'Mint a fresh token per request' },
          { shortHash: '77de204', subject: 'Widen the fixture TTL and prove the failure' },
        ],
      }),
    ],
    [
      /^\/api\/sessions\/[^/]+\/git$/,
      () => ({ branch: 'fix/auth-flake', modified: 1, untracked: 2 }),
    ],
    [/^\/api\/permissions$/, () => ({ requests: [] })],
    [
      /^\/api\/health$/,
      () => ({ ok: true, projects: 5, sessions: 26, running: 5, latencyMs: 64 }),
    ],
  ];

  window.api = async (path) => {
    for (const [re, fn] of routes) {
      const m = re.exec(path);
      if (m) return fn(m[1]);
    }
    return {};
  };

  // The header shows the machine you are pointed at, which is a real hostname. The
  // screenshots need a made-up one. This renames the app's own first entry, the machine
  // serving the page, rather than the stored list: the stored list holds only the extra
  // machines, so writing to it added a second card instead of renaming the one there.
  if (typeof hosts !== 'undefined' && hosts[0]) hosts[0].name = 'studio mac';

  // The machines card does not go through api(): it reads /healthz and /api/sessions
  // with fetch, so without these two the card reports this machine's real counts.
  window.probeMachine = async () => ({ online: true, ms: 24 });
  window.machineSummary = async () => ({ total: 5, running: 5, projects: 3 });

  window.__demoHost = 'xirp.local:8790';
  const scrub = (root) => {
    const w = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    let n;
    while ((n = w.nextNode())) {
      if (/\.home\.|\d+\.\d+\.\d+\.\d+/.test(n.nodeValue)) {
        n.nodeValue = window.__demoHost;
      }
    }
  };

  // A real phone's CSS viewport, applied to this page rather than to a device: the app's
  // own layout is driven by --col, so pinning that plus the body box reproduces a phone
  // screen in a desktop window.
  window.shotViewport = () => {
    document.querySelectorAll('style[data-shot]').forEach((n) => n.remove());
    const style = document.createElement('style');
    style.dataset.shot = '1';
    style.textContent = `
      :root { --col: 393px; }
      html { background: #5a5f6a; }
      body { position: relative; width: 393px; height: 852px; overflow: hidden; }
      .composer { position: absolute; left: 0; right: auto; bottom: 0; width: 393px; margin: 0; }
      .jump { position: absolute; left: 196px; bottom: 96px; }
    `;
    document.head.append(style);
    // Polling would repaint a machine card mid-capture and put the real hostname back.
    window.startPolling = () => {};
    if (state.timer) clearInterval(state.timer);
  };

  // Light or dark, for the pair of transcript shots.
  window.shotTheme = (which) => {
    settings.theme = which;
    applyTheme();
    paintSettings();
  };

  // Each shot: set the view, let it settle, rename what the app cannot know is private.
  window.shot = async (view) => {
    const wait = (ms) => new Promise((r) => setTimeout(r, ms));
    if (view === 'machines') {
      show('machines');
      await renderMachines();
      await wait(700);
      scrub(el('machines-view'));
    } else if (view === 'projects') {
      await refreshList();
      show('projects');
      renderFolders();
      el('projects-title').textContent = 'studio mac';
      el('projects-sub').textContent = window.__demoHost;
    } else if (view === 'sessions') {
      openProject('webapp');
      el('sessions-sub').textContent = 'studio mac';
    } else if (view === 'chat') {
      openSession('d1');
      await wait(900);
      el('detail-sub').textContent = 'webapp · fix/auth-flake';
    } else if (view === 'changes') {
      show('changes');
      el('changes-title').textContent = 'Fix flaky auth test';
      await refreshChanges();
    } else if (view === 'diff') {
      await openDiff('d1', 'app/auth/tokens.py', 'branch');
    } else if (view === 'prompts') {
      openSession('d1');
      await wait(900);
      el('detail-sub').textContent = 'webapp · fix/auth-flake';
      await openPrompts();
    }
    await wait(400);
    return { view: state.view };
  };

  return 'demo data loaded. call shot("machines" | "projects" | "sessions" | "chat" | "changes" | "diff" | "prompts")';
})();
