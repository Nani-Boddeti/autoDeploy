/* Live logs: upgrades /deployments/logs to a Server-Sent Events tail.
   Same-origin, no framework. DOM is built with createElement/textContent only,
   so log text can never be parsed as markup. Without JavaScript the page keeps
   its <noscript> meta refresh. */
'use strict';

(function () {
  var root = document.querySelector('[data-stream-url]');
  if (!root) { return; }

  // Without EventSource, keep the old behaviour: reload while the job runs.
  if (typeof window.EventSource !== 'function') {
    window.setTimeout(function () { window.location.reload(); }, 3000);
    return;
  }

  var KNOWN = ['build', 'phase', 'health', 'activation', 'drain', 'system'];
  var NEAR_BOTTOM_PX = 80;  var reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  var base = new URL(root.getAttribute('data-stream-url'), window.location.href);
  var lastId = Number(base.searchParams.get('after')) || 0;
  var following = true;
  var ended = false;
  var jumpUntil = 0;
  var source = null;

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) { node.className = className; }
    if (text !== undefined) { node.textContent = text; }
    return node;
  }

  // Status bar: badge plus the Jump button, sticky at the bottom of the log.
  var bar = el('div', 'live-bar');
  var badge = el('span', 'pill neutral', 'Reconnecting');
  badge.setAttribute('role', 'status');
  var jump = el('button', 'live-jump', 'Jump to live');
  jump.setAttribute('type', 'button');
  jump.hidden = true;
  var signin = el('a', 'live-signin', 'Sign in again');
  signin.setAttribute('href', '/login');
  signin.hidden = true;
  var retry = el('button', 'live-retry', 'Reload');
  retry.setAttribute('type', 'button');
  retry.hidden = true;
  retry.addEventListener('click', function () { window.location.reload(); });
  [badge, jump, signin, retry].forEach(function (node) { bar.appendChild(node); });
  root.appendChild(bar);

  function setBadge(text, tone) {
    badge.textContent = text;
    badge.className = 'pill ' + tone;
  }

  function showLive() {
    if (ended) { return; }
    setBadge(following ? 'Live' : 'Paused', following ? 'ok' : 'warn');
    jump.hidden = following;
  }

  function atBottom() {
    var doc = document.documentElement;
    return window.innerHeight + window.scrollY >= doc.scrollHeight - NEAR_BOTTOM_PX;
  }

  function scrollToEnd(smooth) {
    window.scrollTo({ top: document.documentElement.scrollHeight, behavior: smooth && !reduceMotion ? 'smooth' : 'auto' });
  }

  window.addEventListener('scroll', function () {
    if (Date.now() < jumpUntil) { return; } // ignore events from our own smooth scroll
    following = atBottom();
    showLive();
  }, { passive: true });

  jump.addEventListener('click', function () {
    following = true;
    jumpUntil = Date.now() + 700;
    showLive();
    scrollToEnd(true);
  });

  // Current attempt group: continue the last server-rendered one if present.
  var sections = root.querySelectorAll('section.attempt');
  var currentAttempt = '';
  var currentList = null;
  if (sections.length > 0) {
    var last = sections[sections.length - 1];
    var match = /(\d+)\s*$/.exec(last.querySelector('h2').textContent);
    currentAttempt = match ? match[1] : '';
    currentList = last.querySelector('ol.logs');
  }

  function startAttempt(number) {
    var section = el('section', 'attempt');
    section.appendChild(el('h2', '', 'Attempt ' + number));
    currentList = el('ol', 'logs');
    section.appendChild(currentList);
    root.insertBefore(section, bar);
    currentAttempt = number;
  }

  function addLine(d) {
    var number = String(d.attempt);
    if (!currentList || number !== currentAttempt) { startAttempt(number); }
    var empty = root.querySelector('p.empty');
    if (empty) { root.removeChild(empty); }
    var row = el('li', d.marker ? 'marker' : '');
    var time = el('time', '', d.clock);
    time.setAttribute('datetime', d.stamp);
    var stream = KNOWN.indexOf(d.stream) >= 0 ? d.stream : 'other';
    var chip = el('span', 'stream stream-' + stream, stream);
    [time, chip, el('pre', '', d.body)].forEach(function (node) { row.appendChild(node); });
    currentList.appendChild(row);
    if (following) { scrollToEnd(false); }
  }

  function finish(label, needsSignIn, needsReload) {
    ended = true;
    if (source) { source.close(); }
    setBadge(label, 'neutral');
    jump.hidden = true;
    signin.hidden = !needsSignIn;
    retry.hidden = !needsReload;
  }

  function connect() {
    var url = new URL(base.href);
    url.searchParams.set('after', String(lastId));
    source = new window.EventSource(url.href);
    source.addEventListener('open', showLive);
    source.addEventListener('line', function (event) {
      var data;
      try { data = JSON.parse(event.data); } catch (e) { return; }
      lastId = Number(event.lastEventId) || lastId;
      addLine(data);
    });
    source.addEventListener('end', function (event) {
      var reason = 'error';
      try { reason = JSON.parse(event.data).reason; } catch (e) { /* keep default */ }
      source.close();
      if (reason === 'rotate') { connect(); return; }
      finish('Ended', reason === 'session', false);
      // A late commit to an earlier attempt can have a lower chunk id than
      // lines already streamed. The terminal page render re-reads from its own
      // start and carries no script, so one reload shows any such row (no loop).
      if (reason === 'done') { window.location.reload(); }
    });
    source.addEventListener('error', function () {
      if (ended) { return; }
      // CONNECTING: the browser retries on its own. CLOSED: the response was
      // refused (429, non-200, redirect), so offer a manual reload.
      if (source.readyState === window.EventSource.CLOSED) {
        finish('Live updates unavailable \u2014 reload to retry', false, true);
        return;
      }
      setBadge('Reconnecting', 'busy');
    });
  }

  connect();
}());
