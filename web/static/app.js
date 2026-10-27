// Live backup progress over SSE, and the one-tap action page.
(() => {
  // Action link page: start right away. Link-preview bots don't run JS, so a GET alone never starts a backup.
  const state = document.getElementById('state');
  if (state && state.dataset.token) {
    fetch('/go/' + encodeURIComponent(state.dataset.token), { method: 'POST' })
      .then((r) => r.ok)
      .catch(() => false)
      .then((ok) => {
        state.className = 'big-state ' + (ok ? 'ok' : 'bad');
        state.replaceChildren(document.getElementById(ok ? 't-ok' : 't-fail').content.cloneNode(true));
      });
    return;
  }

  if (!document.querySelector('[data-live]')) return;
  const refresh = (el) => window.htmx && htmx.trigger(el, 'refresh');

  let es;
  const connect = () => {
    es = new EventSource('/events');
    es.onmessage = (msg) => {
      const e = JSON.parse(msg.data);
      if (e.kind === 'presence') {
        // Don't redraw cards with a running backup; their progress updates in place.
        document.querySelectorAll('article.device').forEach((el) => { if (!el.querySelector('.live')) refresh(el); });
        return;
      }
      const card = document.getElementById('dev-' + e.udid);
      if (!card) return;
      const live = card.querySelector('.live');
      if (!live || e.phase === 'done') return refresh(card);
      live.className = 'live phase-' + e.phase;
      live.querySelector('[data-phase-text]').textContent = e.text;
      const bar = live.querySelector('[data-progress]');
      if (e.phase === 'transfer') { bar.max = 100; bar.value = e.progress; } else bar.removeAttribute('value');
      if (e.phase === 'passcode' && navigator.vibrate) navigator.vibrate(200);
    };
    es.onerror = () => { es.close(); setTimeout(connect, 5000); };
  };
  connect();
  // Coming back to the tab (e.g. after typing the passcode): resync everything.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') document.querySelectorAll('article.device').forEach(refresh);
  });
})();
