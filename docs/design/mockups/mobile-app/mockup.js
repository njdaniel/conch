/* Conch phone app: illustrative flow prototype (issue #1107).
   Plain JavaScript, no dependency, no network, no storage. Everything here is
   sample content and local state. It draws the screens of docs/design/
   mobile-app-ux.md; it plays no audio, asks for no microphone, sends no
   notification and talks to no server. */
(function () {
  'use strict';

  var $ = function (id) { return document.getElementById(id); };

  // h builds an element. Children are strings (text nodes), elements, arrays
  // or null. Props: class, text, on (event map), fk (focus key), and any other
  // key becomes an attribute (true is an empty attribute, false or null none).
  function h(tag, props) {
    var el = document.createElement(tag);
    var p = props || {};
    Object.keys(p).forEach(function (k) {
      var v = p[k];
      if (v === null || v === undefined || v === false) return;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'fk') el.setAttribute('data-fk', v);
      else if (k === 'on') Object.keys(v).forEach(function (e) { el.addEventListener(e, v[e]); });
      else el.setAttribute(k, v === true ? '' : String(v));
    });
    for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
    return el;
  }
  function add(el, c) {
    if (c === null || c === undefined || c === false) return;
    if (Array.isArray(c)) c.forEach(function (x) { add(el, x); });
    else if (typeof c === 'string') el.appendChild(document.createTextNode(c));
    else el.appendChild(c);
  }

  /* ---------- sample content (all invented) ---------- */

  var longName = 'Bartholomew-Alexander Montgomery-Featherstonehaugh';
  var longChannel = 'design-review-with-a-very-long-channel-name-for-wrapping';
  var SERVERS = [
    { id: 'a', name: 'Camp Conch', addr: 'conch.example.org', me: 'Sam',
      channels: [
        { n: 'general', vis: 'public', unread: 2, voice: 0 },
        { n: 'lounge', vis: 'public', unread: 0, voice: 2 },
        { n: 'nets-demo', vis: 'public', unread: 0, voice: 0 },
        { n: longChannel, vis: 'private', unread: 0, voice: 0 },
        { n: 'ops', vis: 'private', unread: 0, voice: 0, manages: true }
      ] },
    { id: 'b', name: 'Book club', addr: 'chat.example.net', me: 'sam-reads',
      channels: [
        { n: 'welcome', vis: 'public', unread: 0, voice: 0 },
        { n: 'books', vis: 'public', unread: 1, voice: 0 }
      ] }
  ];
  var INVITE_LINK = 'https://conch.example.org/app/#/invite/EXAMPLE-NOT-A-REAL-CODE';
  var ROSTER_LOUNGE = [{ n: 'Ada', tx: true }, { n: 'Ben' }];

  function sampleMessages(server, channel) {
    if (server.id === 'a' && channel === 'general') {
      return [
        { id: 1, who: 'Ada', body: 'Morning! The draft is in the shared doc.' },
        { id: 2, who: longName, body: 'Found it here: https://example.org/a/path/that/goes/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/end ' + 'x'.repeat(60) },
        { id: 3, who: 'build-bot', agent: true, body: 'Build 4812 passed.' },
        { id: 4, who: 'Ben', body: '@Sam can you look at the thread?', mention: true, replyTo: 'Ada: Morning! The draft is in the shared doc.' },
        { id: 5, who: 'Ada', body: '', file: 'release-notes-final-v2-with-a-really-long-filename.pdf (2.1 MB)' },
        { id: 6, who: 'Sam', mine: true, status: 'sent', body: 'On it.' }
      ];
    }
    return [{ id: 1, who: 'Ada', body: 'Nothing here but sample text.' }];
  }

  /* ---------- screens and presets (hash routes) ---------- */

  var SCREENS = [
    { id: 'link', base: 'link', label: '1. Invite link opened (web page)', note: 'Uninstalled or installed: toggle "App is installed" and tap Open in Conch.' },
    { id: 'paste', base: 'paste', label: '2. Add a server: paste the invite (fallback)', note: 'Paste from clipboard fills a sample link. Try an empty field or http://.' },
    { id: 'confirm', base: 'confirm', label: '3. Confirm the server', note: 'Address, inviter, channels. Join is refused while offline.' },
    { id: 'account', base: 'account', label: '4. Create your account', note: 'Name and password twice. Try a short password or a mismatch.' },
    { id: 'signin', base: 'signin', label: '4b. Sign in (returning member)', note: 'Type the password "wrong" to see the one refusal.' },
    { id: 'channels', base: 'channels', label: '5. Channels', note: 'Choosing a channel opens it and goes to Chat. It never joins voice.' },
    { id: 'chat', base: 'chat', label: '6. Chat', note: 'Open the keyboard, send, make a send fail, go offline.' },
    { id: 'chat-trouble', base: 'chat', label: '6b. Chat: failed and not confirmed sends', note: 'Retry resends. Not confirmed must be checked before retrying.',
      apply: function (s) { s.ch = 'general'; s.extra = [{ id: 90, who: 'Sam', mine: true, status: 'failed', body: 'This one did not go.' }, { id: 91, who: 'Sam', mine: true, status: 'unsure', body: 'This one may or may not have arrived.' }]; } },
    { id: 'chat-voice-elsewhere', base: 'chat', label: '6c. Chat in #general while voice is in #lounge', note: 'The strip names #lounge. Browsing #general did not retarget voice.',
      apply: function (s) { s.ch = 'general'; s.joined = true; s.voiceCh = 'lounge'; s.mic = 'allowed'; } },
    { id: 'voice', base: 'voice', label: '7. Voice: not in voice', note: 'Join is explicit. The first join asks the system for the microphone.' },
    { id: 'voice-joined', base: 'voice', label: '7b. Voice: in voice', note: 'Hold to talk, Mute, Deafen. Capture and transmission are separate lines.',
      apply: function (s) { s.ch = 'lounge'; s.joined = true; s.voiceCh = 'lounge'; s.mic = 'allowed'; } },
    { id: 'voice-elsewhere', base: 'voice', label: '7c. Voice view in #general while voice is in #lounge', note: 'No retargeting. Leave, then join, is the only way to change channel.',
      apply: function (s) { s.ch = 'general'; s.joined = true; s.voiceCh = 'lounge'; s.mic = 'allowed'; } },
    { id: 'voice-listen', base: 'voice', label: '7d. Voice: listen only (server grant)', note: 'The server gave no publish grant, so there is nothing to hold.',
      apply: function (s) { s.ch = 'lounge'; s.joined = true; s.voiceCh = 'lounge'; s.mic = 'allowed'; s.grant = 'listen'; } },
    { id: 'mic-denied', base: 'voice', label: '7e. Microphone denied', note: 'Join is not silent: choose settings, join without a microphone, or cancel.',
      apply: function (s) { s.ch = 'lounge'; s.mic = 'denied'; s.sheet = 'micdenied'; } },
    { id: 'servers', base: 'channels', label: '8. Switch server or account', note: 'Switching while in voice asks first.',
      apply: function (s) { s.sheet = 'servers'; } },
    { id: 'settings', base: 'settings', label: '9. Settings and notifications', note: 'Notification permission is asked only from here, never at launch.' },
    { id: 'gone', base: 'gone', label: '10. Channel not available', note: 'A stale link or notification. The same words for deleted and for no access.' },
    { id: 'revoked', base: 'revoked', label: '11. Signed out by the server', note: 'Protected requests and voice stop. Drafts stay on this phone.' }
  ];
  var byId = {};
  SCREENS.forEach(function (x) { byId[x.id] = x; });

  var SIZES = {
    '360x740': { w: 360, h: 740, safe: [24, 0, 24, 0] },
    '390x844': { w: 390, h: 844, safe: [47, 0, 34, 0] },
    '740x360': { w: 740, h: 360, safe: [24, 48, 16, 0] },
    '844x390': { w: 844, h: 390, safe: [0, 47, 21, 47] }
  };

  /* ---------- state ---------- */

  var s;
  function fresh() {
    return {
      route: 'link', base: 'link', sheet: null, opener: null, toast: '',
      server: 0, ch: 'general', drafts: {}, extra: [], sent: {}, replyTo: null, msgSheet: null,
      joined: false, joining: false, voiceCh: null, noMic: false, muted: false, deafened: false,
      talking: false, mode: 'ptt', style: 'hold', pendingJoin: null, pendingSwitch: null,
      pasted: '', formErr: {}, showPw: false, inviteCode: null, target: 'tr-1',
      stick: true, focus: null, bg: false
    };
  }
  var env = { size: '390x844', scale: 1, motion: 'system', safe: true, safeShow: false, kb: false, installed: true, conn: 'connected', mic: 'ask', notif: 'ask', grant: 'speak', failnext: false };
  // mic, notif and grant live in env (the scenario controls); s mirrors none of them.
  s = fresh();
  var nextId = 100;
  var toastTimer = null;
  var joinTimer = null;

  function server() { return SERVERS[s.server]; }
  function chan(n) { return server().channels.filter(function (c) { return c.n === n; })[0]; }
  function say(t) { var l = $('live'); l.textContent = ''; window.setTimeout(function () { l.textContent = t; }, 30); }
  function alertSay(t) { var l = $('alert'); l.textContent = ''; window.setTimeout(function () { l.textContent = t; }, 30); }
  function toast(t) {
    s.toast = t; say(t); paintToast();
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(function () { s.toast = ''; paintToast(); }, 4000);
  }
  function paintToast() {
    var ph = $('phone'); var old = ph.querySelector('.toast'); if (old) old.remove();
    if (s.toast) ph.appendChild(h('div', { class: 'toast', 'aria-hidden': 'true', text: s.toast }));
  }

  /* ---------- derived voice state ---------- */

  function canSpeak() { return s.joined && !s.noMic && env.grant === 'speak' && env.mic === 'allowed'; }
  function captured() { return canSpeak(); }
  function silenced() { return s.muted || s.deafened; }
  function talkBlock() {
    if (env.conn !== 'connected') return 'Hold to talk is paused while Conch reconnects.';
    if (s.deafened) return 'Hold to talk sends nothing while you are deafened. Undeafen to talk.';
    if (s.muted) return 'Hold to talk sends nothing while you are muted. Unmute to talk.';
    return null;
  }
  function transmitting() { return s.talking && canSpeak() && talkBlock() === null; }

  /* ---------- navigation ---------- */

  function go(route, opts) {
    var def = byId[route] || byId.channels;
    var keep = opts && opts.keep;
    if (!keep) {
      s.sheet = null; s.opener = null; s.msgSheet = null; s.formErr = {}; s.replyTo = null; s.extra = [];
      if (def.apply) {
        s.grant = null;
        def.apply(s);
        if (!s.grant) s.grant = 'speak';
        // Presets name the scenario in s.mic and s.grant; the controls own them.
        if (s.mic) { env.mic = s.mic; $('mic').value = s.mic; s.mic = null; }
        if (s.grant) { env.grant = s.grant; $('grant').value = s.grant; s.grant = null; }
      }
    }
    s.route = def.id; s.base = def.base; s.stick = true; s.focus = 'heading';
    if (window.location.hash !== '#/' + def.id) window.history.replaceState(null, '', '#/' + def.id);
    $('screen').value = def.id;
    $('screen-note').textContent = def.note;
    render();
    say(def.label.replace(/^\S+\s/, '') + '.');
  }
  function view(base) {
    // Chat, Channels and Voice are the three views; the others are pages.
    s.base = base; s.stick = true; s.focus = 'heading';
    var match = SCREENS.filter(function (x) { return x.id === base; })[0];
    s.route = match ? match.id : base;
    window.history.replaceState(null, '', '#/' + s.route);
    $('screen').value = s.route;
    $('screen-note').textContent = match ? match.note : '';
    render();
  }
  function openChannel(name) {
    s.ch = name; s.extra = [];
    var c = chan(name); if (c) c.unread = 0;
    view('chat'); // browsing: nothing here touches s.joined or s.voiceCh
  }

  /* ---------- sheets ---------- */

  function openSheet(name, opener) { s.sheet = name; s.opener = opener || null; s.focus = 'sheet'; render(); }
  function closeSheet() { var o = s.opener; s.sheet = null; s.msgSheet = null; s.focus = o ? 'fk:' + o : null; s.opener = null; render(); }

  /* ---------- voice actions (local state only) ---------- */

  function tryJoin(noMic) {
    var here = s.ch;
    if (s.joined) return;
    if (env.conn === 'offline') { alertSay('Cannot join voice while offline.'); return; }
    if (!noMic && env.mic === 'denied') { s.pendingJoin = here; openSheet('micdenied', 'join'); return; }
    if (!noMic && env.mic === 'ask') { s.pendingJoin = here; openSheet('osmic', 'join'); return; }
    beginJoin(here, noMic);
  }
  function beginJoin(ch, noMic) {
    s.sheet = null; s.joining = true; s.noMic = !!noMic; s.focus = 'fk:status'; render();
    say('Connecting to #' + ch + '.');
    window.clearTimeout(joinTimer);
    joinTimer = window.setTimeout(function () {
      s.joining = false; s.joined = true; s.voiceCh = ch; s.muted = false; s.deafened = false; s.talking = false;
      s.focus = 'fk:leave'; render();
      say('In voice in #' + ch + '. ' + (canSpeak() ? 'Your microphone is on but nothing is sent until you hold to talk.' : 'Your microphone is off.'));
    }, 500);
  }
  function leave() {
    var ch = s.voiceCh;
    s.joined = false; s.joining = false; s.voiceCh = null; s.talking = false; s.noMic = false; s.muted = false; s.deafened = false;
    s.focus = 'fk:join'; render();
    say('Left voice in #' + ch + '. Text chat carries on.');
  }
  function setTalking(on) {
    if (on && (!canSpeak() || talkBlock() !== null)) return;
    if (s.talking === on) return;
    s.talking = on; paintTalk();
    say(on ? 'Transmitting.' : 'Not transmitting.');
  }
  function paintTalk() {
    var tx = transmitting();
    document.querySelectorAll('[data-talk]').forEach(function (b) { b.setAttribute('data-on', tx ? 'true' : 'false'); });
    document.querySelectorAll('[data-role="tx-text"]').forEach(function (e) { e.textContent = tx ? 'Transmitting' : 'Not transmitting'; });
    document.querySelectorAll('[data-role="tx-ring"]').forEach(function (e) { e.setAttribute('data-on', tx ? 'true' : 'false'); });
  }
  function interrupt() {
    var was = s.talking;
    s.talking = false; s.bg = false;
    render();
    toast(was
      ? 'Hold to talk stopped because the app left the screen. Press again to talk. Nothing was unmuted.'
      : (s.joined ? 'The app left the screen and came back. Mute and deafen are as you left them; nothing sends until you hold to talk.' : 'The app left the screen and came back.'));
  }

  /* ---------- building blocks ---------- */

  function btn(label, props, cls) {
    var p = props || {};
    p['class'] = 'btn ' + (cls || '');
    p.type = 'button';
    return h('button', p, label);
  }
  function topBar(title, sub, buttons, back) {
    return h('header', { class: 'top' },
      back,
      h('h1', { id: 'heading', tabindex: '-1' }, title, sub ? h('span', { class: 'sub' }, sub) : null),
      buttons);
  }
  function serverBtn() {
    var sv = server();
    return h('button', { type: 'button', class: 'ibtn', fk: 'servers', 'aria-label': 'Servers. Now on ' + sv.name + ', ' + sv.addr + ', signed in as ' + sv.me,
      on: { click: function () { openSheet('servers', 'servers'); } } }, h('span', { class: 'glyph', 'aria-hidden': 'true' }, '☰'));
  }
  function settingsBtn() {
    return h('button', { type: 'button', class: 'ibtn', fk: 'settings', 'aria-label': 'Settings', on: { click: function () { view('settings'); } } }, h('span', { class: 'glyph', 'aria-hidden': 'true' }, '⚙'));
  }
  function connBanner() {
    if (env.conn === 'offline') return h('div', { class: 'banner bad', role: 'status' }, 'Offline. What you see may be out of date. Sending is paused; your draft is kept.', btn('Try again', { on: { click: function () { toast('Trying again (illustrative).'); } } }, 'quiet'));
    if (env.conn === 'reconnecting') return h('div', { class: 'banner warn', role: 'status' }, 'Reconnecting to ' + server().addr + '. Messages shown may be out of date.');
    return null;
  }
  function navBar() {
    var items = [['chat', 'Chat', '✉'], ['channels', 'Channels', '#'], ['voice', 'Voice', '♫']];
    var cur = s.base;
    return h('nav', { class: 'nav', 'aria-label': 'Main navigation' }, items.map(function (it) {
      var live = s.joined || s.joining;
      return h('button', { type: 'button', fk: 'nav-' + it[0], 'aria-current': cur === it[0] ? 'page' : null,
        'aria-describedby': it[0] === 'voice' && live ? 'in-voice-d' : null,
        on: { click: function () { view(it[0]); } } },
        h('span', { class: 'glyph', 'aria-hidden': 'true' }, it[2]), it[1],
        it[0] === 'voice' && live ? h('span', { class: 'dot', 'aria-hidden': 'true' }) : null);
    }).concat([(s.joined || s.joining) ? h('span', { id: 'in-voice-d', class: 'sr' }, 'You are in voice in #' + (s.voiceCh || s.ch)) : null]));
  }
  function stripView() {
    if (!(s.joined || s.joining)) return null;
    var ch = s.voiceCh || s.ch;
    var re = env.conn !== 'connected';
    var where = (s.joining ? 'Connecting to #' : re ? 'Reconnecting to #' : 'In voice in #') + ch;
    var collapsed = env.scale >= 1.6 || (isLand() && env.scale >= 1.3);
    var children = [h('button', { type: 'button', class: 'btn where', fk: 'where', 'aria-label': where + '. Open Voice', on: { click: function () { view('voice'); } } }, h('span', { class: 't' }, where))];
    if (!collapsed && s.joined) {
      var block = talkBlock();
      if (canSpeak() && s.mode === 'ptt') children.push(talkButton('talk', 'Hold to talk', 'Hold to talk, sends to #' + ch, block));
      if (canSpeak()) children.push(btn(s.muted ? 'Unmute' : 'Mute', { fk: 'mute', on: { click: toggleMute } }));
      children.push(btn('Leave', { fk: 'leave', 'aria-label': 'Leave voice', on: { click: leave } }, 'quiet'));
    }
    return h('div', { class: 'strip' + (collapsed ? ' collapsed' : ''), role: 'group', 'aria-label': 'Voice controls' }, children);
  }
  function toggleMute() { s.muted = !s.muted; if (s.muted) s.talking = false; s.focus = null; render(); say(s.muted ? 'Muted. Nothing is sent.' : 'Unmuted. Nothing is sent until you hold to talk.'); }
  function toggleDeafen() { s.deafened = !s.deafened; if (s.deafened) s.talking = false; s.focus = null; render(); say(s.deafened ? 'Deafened. You hear nothing and send nothing.' : 'Undeafened.' + (s.muted ? ' Still muted.' : '')); }

  // The one Hold to talk element per place. Held with the pointer, or Space or
  // Enter held down; released on pointer up, cancel, blur or key up. With the
  // "tap to start and stop" style (an accessibility alternative) a tap toggles.
  function talkButton(kind, visible, name, block) {
    var big = kind === 'big';
    var id = 'talk-' + kind;
    var b = h('button', {
      type: 'button', 'data-talk': '', 'data-on': transmitting() ? 'true' : 'false', fk: id,
      class: big ? 'btn talkbig' : 'btn talk',
      'aria-label': name,
      'aria-disabled': block ? 'true' : null,
      'aria-describedby': block ? id + '-why' : null
    }, visible);
    b.addEventListener('pointerdown', function (e) {
      if (e.button !== undefined && e.button > 0) return;
      if (s.style === 'toggle') return;
      if (block) { say(block); return; }
      setTalking(true);
    });
    b.addEventListener('click', function (e) {
      if (s.style !== 'toggle') { if (e.detail === 0 && !block) { /* keyboard activation handled by keydown */ } return; }
      if (block) { say(block); return; }
      setTalking(!s.talking);
    });
    b.addEventListener('keydown', function (e) {
      if ((e.key === ' ' || e.key === 'Enter') && s.style !== 'toggle') { e.preventDefault(); if (!e.repeat) { if (block) say(block); else setTalking(true); } }
    });
    b.addEventListener('keyup', function (e) { if ((e.key === ' ' || e.key === 'Enter') && s.style !== 'toggle') { e.preventDefault(); setTalking(false); } });
    b.addEventListener('blur', function () { if (s.style !== 'toggle') setTalking(false); });
    b.addEventListener('contextmenu', function (e) { e.preventDefault(); });
    return block ? h('span', { class: 'contents' }, b, h('span', { id: id + '-why', class: 'sr' }, block)) : b;
  }

  /* ---------- the screens ---------- */

  function screenLink() {
    var inst = env.installed;
    var note = s.linkTried ? h('p', { class: 'err', role: 'status' }, inst ? '' : 'Nothing opened. The Conch app may not be installed on this phone. Get the app, then copy the invite link and paste it into the app, or continue in this browser.') : null;
    return h('div', { class: 'app' },
      h('div', { class: 'web-bar' }, 'A web browser: ' + INVITE_LINK),
      h('div', { class: 'content' }, h('div', { class: 'pad' },
        h('h1', { id: 'heading', tabindex: '-1' }, 'You are invited to Conch'),
        h('p', {}, 'Ada invited you to Camp Conch, at conch.example.org.'),
        h('p', { class: 'muted' }, 'This page cannot tell whether the app is installed, so it offers every way in.'),
        btn('Open in Conch', { fk: 'open', class: 'x', on: { click: function () { s.linkTried = true; if (env.installed) { s.inviteCode = 'EXAMPLE'; go('confirm'); } else { s.focus = null; render(); alertSay('Nothing opened. The Conch app may not be installed.'); } } } }, 'primary block'),
        note,
        btn('Get the Conch app', { on: { click: function () { toast('A store page. Which stores and how is decided after M1 (#1106); nothing is published.'); } } }, 'block'),
        btn('Copy invite link', { on: { click: function () { toast('Invite link copied (illustrative). Paste it in the app under Add a server.'); } } }, 'block'),
        btn('Continue in this browser', { on: { click: function () { toast('The browser client is a separate product; not drawn here.'); } } }, 'block quiet'),
        h('p', { class: 'muted' }, 'Already in the app? Open it and choose Add a server, then paste the link.'))));
  }

  function parseServerInput(text) {
    var t = text.trim();
    if (!t) return { err: 'Paste an invite link or type a server address.' };
    if (!/^[a-z]+:\/\//i.test(t)) t = 'https://' + t;
    var u;
    try { u = new URL(t); } catch (e) { return { err: 'That does not look like a server address or an invite link.' }; }
    var local = /^(localhost|127\.\d+\.\d+\.\d+|\[::1\])$/.test(u.hostname);
    if (u.protocol === 'http:' && !local) return { err: 'This server is not on HTTPS, so Conch will not send a password to it. Ask the operator for the HTTPS address.' };
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return { err: 'That does not look like a server address or an invite link.' };
    var m = /#\/invite\/([^/]+)$/.exec(u.hash);
    return { origin: u.origin, host: u.host, code: m ? m[1] : null };
  }

  function screenPaste() {
    var err = s.formErr.paste;
    var ta = h('input', { id: 'f-paste', type: 'text', inputmode: 'url', autocapitalize: 'none', autocorrect: 'off', spellcheck: 'false', autocomplete: 'off',
      'aria-invalid': err ? 'true' : null, 'aria-describedby': 'paste-help' + (err ? ' paste-err' : ''), fk: 'paste',
      on: { input: function (e) { s.pasted = e.target.value; } } });
    ta.value = s.pasted;
    return h('div', { class: 'app' },
      topBar('Add a server', null, null),
      h('div', { class: 'content' }, h('form', { class: 'pad', 'aria-labelledby': 'heading', on: { submit: function (e) {
        e.preventDefault();
        var r = parseServerInput(s.pasted);
        if (r.err) { s.formErr = { paste: r.err }; s.focus = 'fk:paste'; render(); alertSay(r.err); return; }
        s.inviteCode = r.code; s.formErr = {}; go('confirm');
      } } },
        h('div', { class: 'field' }, h('label', { 'for': 'f-paste' }, 'Invite link or server address'), ta,
          h('p', { id: 'paste-help', class: 'muted' }, 'Ask the person who invited you for a link. A link looks like https://your-server/app/#/invite/…'),
          err ? h('p', { id: 'paste-err', class: 'err' }, err) : null),
        btn('Paste from clipboard', { fk: 'clip', on: { click: function () { s.pasted = INVITE_LINK; s.formErr = {}; s.focus = 'fk:paste'; render(); say('Pasted from the clipboard.'); } } }),
        h('button', { type: 'submit', class: 'btn primary block' }, 'Continue'),
        h('p', { class: 'muted' }, 'Conch reads the clipboard only when you tap Paste from clipboard.'))));
  }

  function screenConfirm() {
    var off = env.conn === 'offline';
    var host = 'conch.example.org';
    return h('div', { class: 'app' },
      topBar('Is this the server you meant?', null, null),
      connBanner(),
      h('div', { class: 'content' }, h('div', { class: 'pad' },
        h('div', { class: 'card' },
          h('p', { class: 'mono', 'aria-label': 'Server address ' + host }, h('b', {}, host)),
          h('dl', { class: 'kv', 'aria-label': 'What the server says' },
            h('dt', {}, 'Name'), h('dd', {}, 'Camp Conch'),
            h('dt', {}, 'Invited by'), h('dd', {}, 'Ada'),
            h('dt', {}, 'You start in'), h('dd', {}, '#general, #lounge')),
          off ? h('p', { class: 'err' }, 'Cannot reach the server. Check your connection and try again.')
            : h('p', { class: 'muted', role: 'status' }, 'The server answered just now. This is what the server says about itself, not proof it is the one you were told about.')),
        h('p', {}, 'Only continue if someone you know sent you this link. Joining makes an account on this server.'),
        h('button', { type: 'button', class: 'btn primary block', fk: 'join', 'aria-disabled': off ? 'true' : null, on: { click: function () { if (off) { alertSay('Cannot reach the server.'); return; } go('account'); } } }, 'Join this server'),
        btn('Not this server', { on: { click: function () { go('paste'); } } }, 'block quiet'))));
  }

  function screenAccount() {
    var e = s.formErr;
    function field(id, label, type, ac, err) {
      var inp = h('input', { id: 'f-' + id, type: type, autocomplete: ac, autocapitalize: 'none', autocorrect: 'off', spellcheck: 'false',
        'aria-invalid': err ? 'true' : null, 'aria-describedby': err ? 'e-' + id : null, fk: id, on: { input: function (ev) { s['v_' + id] = ev.target.value; } } });
      inp.value = s['v_' + id] || '';
      return h('div', { class: 'field' }, h('label', { 'for': 'f-' + id }, label), inp, err ? h('p', { id: 'e-' + id, class: 'err' }, err) : null);
    }
    var type = s.showPw ? 'text' : 'password';
    return h('div', { class: 'app' },
      topBar('Create your account', 'Camp Conch · conch.example.org', null),
      connBanner(),
      h('div', { class: 'content' }, h('form', { class: 'pad', 'aria-labelledby': 'heading', on: { submit: function (ev) {
        ev.preventDefault();
        var er = {};
        if (!(s.v_name || '').trim()) er.name = 'Choose a name.';
        if ((s.v_pw || '').length < 8) er.pw = 'Choose a longer password. (Sample rule; the server decides what is too short or too common.)';
        else if (s.v_pw !== s.v_pw2) er.pw2 = 'The two passwords differ.';
        if (env.conn === 'offline') { er.form = 'Cannot reach the server. Your name is kept; try again when you are back online.'; }
        s.formErr = er;
        var first = er.name ? 'name' : er.pw ? 'pw' : er.pw2 ? 'pw2' : null;
        if (first || er.form) { s.focus = 'fk:' + (first || 'name'); render(); alertSay(er.form || er[first]); return; }
        s.v_pw = s.v_pw2 = ''; env.conn = 'connected'; $('conn').value = 'connected';
        go('channels'); toast('Welcome to Camp Conch. You are signed in on this phone.');
      } } },
        er_form(e.form),
        field('name', 'Your name', 'text', 'username', e.name),
        field('pw', 'Choose a password', type, 'new-password', e.pw),
        field('pw2', 'Type the password again', type, 'new-password', e.pw2),
        btn(s.showPw ? 'Hide passwords' : 'Show passwords', { 'aria-pressed': s.showPw ? 'true' : 'false', on: { click: function () { s.showPw = !s.showPw; s.focus = null; render(); } } }, 'quiet'),
        h('button', { type: 'submit', class: 'btn primary block' }, 'Create account'),
        btn('I already have an account', { on: { click: function () { go('signin'); } } }, 'block quiet'))));
  }
  function er_form(t) { return t ? h('p', { class: 'err', role: 'alert' }, t) : null; }

  function screenSignin() {
    var e = s.formErr;
    function inp(id, label, type, ac) {
      var el = h('input', { id: 'f-' + id, type: type, autocomplete: ac, autocapitalize: 'none', autocorrect: 'off', spellcheck: 'false', fk: id, 'aria-invalid': e.form ? 'true' : null, 'aria-describedby': e.form ? 'e-form' : null, on: { input: function (ev) { s['v_' + id] = ev.target.value; } } });
      el.value = s['v_' + id] || '';
      return h('div', { class: 'field' }, h('label', { 'for': 'f-' + id }, label), el);
    }
    return h('div', { class: 'app' },
      topBar('Sign in', server().name + ' · ' + server().addr, null),
      connBanner(),
      h('div', { class: 'content' }, h('form', { class: 'pad', 'aria-labelledby': 'heading', on: { submit: function (ev) {
        ev.preventDefault();
        if (env.conn === 'offline') { s.formErr = { form: 'Cannot reach the server. Check your connection and try again.' }; s.focus = 'fk:name'; render(); alertSay(s.formErr.form); return; }
        if ((s.v_pw || '') === 'wrong' || !(s.v_name || '').trim()) { s.formErr = { form: 'That name or password is not right.' }; s.v_pw = ''; s.focus = 'fk:pw'; render(); alertSay(s.formErr.form); return; }
        s.formErr = {}; s.v_pw = ''; go('channels');
      } } },
        e.form ? h('p', { id: 'e-form', class: 'err', role: 'alert' }, e.form) : null,
        inp('name', 'Name', 'text', 'username'), inp('pw', 'Password', 'password', 'current-password'),
        h('button', { type: 'submit', class: 'btn primary block' }, 'Sign in'),
        btn('Use a sign-in link from another device', { on: { click: function () { toast('A sign-in link made on another signed-in device is pasted here (existing operation; not drawn).'); } } }, 'block quiet'),
        btn('Have an invite instead?', { on: { click: function () { go('paste'); } } }, 'block quiet'))));
  }

  function screenChannels() {
    var sv = server();
    var pub = sv.channels.filter(function (c) { return c.vis === 'public'; });
    var priv = sv.channels.filter(function (c) { return c.vis === 'private'; });
    function row(c) {
      var sub = [];
      sub.push(c.vis === 'private' ? 'Private' : 'Public');
      if (c.manages) sub.push('You manage it');
      if (c.voice) sub.push(c.voice + ' in voice');
      if (s.joined && s.voiceCh === c.n) sub.push('you are in voice here');
      var label = '#' + c.n + '. ' + sub.join('. ') + (c.unread ? '. ' + c.unread + ' unread' : '');
      return h('li', {}, h('button', { type: 'button', class: 'row-btn', fk: 'ch-' + c.n, 'aria-label': label, 'aria-current': s.ch === c.n ? 'true' : null, on: { click: function () { openChannel(c.n); } } },
        h('span', { class: 'grow' }, h('span', { class: 'name' }, '#' + c.n), h('span', { class: 'sub' }, sub.join(' · '))),
        c.unread ? h('span', { class: 'pill', 'aria-hidden': 'true' }, String(c.unread)) : null));
    }
    return h('div', { class: 'app' },
      topBar('Channels', sv.name + ' · ' + sv.me, [serverBtn(), settingsBtn()]),
      connBanner(),
      h('div', { class: 'content' },
        h('h2', { class: 'group-h' }, 'Public'), h('ul', { class: 'list' }, pub.map(row)),
        h('h2', { class: 'group-h' }, 'Private'), h('ul', { class: 'list' }, priv.map(row)),
        h('p', { class: 'muted pad' }, 'Choosing a channel shows its text. It does not join its voice.')),
      h('div', { class: 'dock' }, stripView(), navBar()));
  }

  function bodyNodes(m) {
    var out = [];
    var text = m.body || '';
    var re = /(https?:\/\/\S+|@\w+)/g, last = 0, mm;
    while ((mm = re.exec(text))) {
      if (mm.index > last) out.push(text.slice(last, mm.index));
      if (mm[0].charAt(0) === '@') out.push(h('span', { class: 'mention' }, mm[0]));
      else out.push(h('a', { href: '#/' + s.route, on: { click: function (e) { e.preventDefault(); toast('A link. It would open in the system browser after saying where it goes.'); } } }, mm[0]));
      last = mm.index + mm[0].length;
    }
    if (last < text.length) out.push(text.slice(last));
    return out;
  }
  function messageView(m) {
    var status = m.status;
    var stateLine = null;
    if (m.mine && status === 'sending') stateLine = h('span', { class: 'state' }, 'Sending…');
    else if (m.mine && status === 'failed') stateLine = h('span', { class: 'state bad' }, 'Failed. Not sent.', btn('Retry', { 'aria-label': 'Retry sending message', on: { click: function () { retry(m); } } }), btn('Remove', { 'aria-label': 'Remove failed message', on: { click: function () { remove(m); } } }, 'quiet'));
    else if (m.mine && status === 'unsure') stateLine = h('span', { class: 'state bad' }, 'Not confirmed: check the conversation before retrying.', btn('Retry', { 'aria-label': 'Retry sending message', on: { click: function () { retry(m); } } }));
    var more = h('button', { type: 'button', class: 'ibtn', fk: 'more-' + m.id, 'aria-label': 'More for message from ' + m.who, on: { click: function () { s.msgSheet = m.id; openSheet('message', 'more-' + m.id); } } }, h('span', { 'aria-hidden': 'true' }, '⋯'));
    return h('li', { class: 'msg' + (m.mine ? ' mine' : ''), 'aria-label': null },
      h('div', { class: 'who' }, h('b', { title: m.who }, m.who), m.agent ? h('span', { class: 'badge' }, 'Agent') : null, more),
      m.replyTo ? h('p', { class: 'reply-ctx' }, 'In reply to ' + m.replyTo) : null,
      m.body ? h('p', { class: 'body' }, bodyNodes(m)) : null,
      m.file ? h('p', { class: 'chip' }, '📎 ' + m.file) : null,
      stateLine);
  }
  function allMessages() {
    var key = server().id + '/' + s.ch;
    return sampleMessages(server(), s.ch).concat(s.sent[key] || [], s.extra);
  }
  function retry(m) { m.status = 'sending'; s.focus = null; render(); window.setTimeout(function () { m.status = 'sent'; render(); say('Message sent.'); }, 600); }
  function remove(m) { var key = server().id + '/' + s.ch; s.sent[key] = (s.sent[key] || []).filter(function (x) { return x !== m; }); s.extra = s.extra.filter(function (x) { return x !== m; }); s.focus = 'fk:ta'; render(); }
  function doSend() {
    var key = server().id + '/' + s.ch;
    var body = (s.drafts[key] || '').trim();
    if (!body || env.conn !== 'connected') return;
    var reply = s.replyTo;
    var failing = env.failnext;
    var m = { id: nextId++, who: server().me, mine: true, status: failing ? 'failed' : 'sending', body: body, replyTo: reply };
    (s.sent[key] = s.sent[key] || []).push(m);
    s.drafts[key] = ''; s.replyTo = null; s.stick = true; s.focus = 'fk:ta';
    if (failing) { env.failnext = false; $('failnext').checked = false; }
    render();
    if (failing) alertSay('Message failed. Not sent.');
    else window.setTimeout(function () { m.status = 'sent'; s.focus = null; render(); say('Message sent.'); }, 600);
  }

  function screenChat() {
    var sv = server(); var c = chan(s.ch) || { vis: 'public' };
    var key = sv.id + '/' + s.ch;
    var offline = env.conn !== 'connected';
    var ta = h('textarea', { id: 'f-ta', rows: '1', fk: 'ta', 'aria-label': 'Message #' + s.ch, placeholder: 'Message #' + s.ch, enterkeyhint: 'enter', autocapitalize: 'sentences',
      'aria-describedby': offline ? 'send-why' : null,
      on: { input: function (e) { s.drafts[key] = e.target.value; var sb = document.querySelector('[data-role="send"]'); if (sb) sb.setAttribute('aria-disabled', (!e.target.value.trim() || offline) ? 'true' : 'false'); },
        keydown: function (e) { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); doSend(); } } } });
    ta.value = s.drafts[key] || '';
    var empty = !(s.drafts[key] || '').trim();
    var voiceNote = (s.joined || s.joining) ? h('span', { class: 'vo-aud' }, ' · Voice: ' + (s.voiceCh || s.ch)) : null;
    var aud = h('p', { class: 'aud' },
      (s.voiceCh && s.joined && s.voiceCh !== s.ch) ? 'Message #' + s.ch + '. Voice stays in #' + s.voiceCh + '.' : 'Message #' + s.ch + ' (' + (c.vis === 'private' ? 'private channel' : 'public channel') + ').', voiceNote);
    return h('div', { class: 'app' },
      topBar('#' + s.ch, (c.vis === 'private' ? 'Private channel' : 'Public channel') + ' · ' + sv.name, [h('button', { type: 'button', class: 'ibtn', fk: 'members', 'aria-label': 'Members of #' + s.ch, on: { click: function () { openSheet('members', 'members'); } } }, h('span', { 'aria-hidden': 'true', class: 'glyph' }, '☺')), settingsBtn()]),
      connBanner(),
      h('div', { class: 'content', id: 'log-wrap' }, h('ul', { class: 'log', 'aria-label': 'Messages in #' + s.ch }, allMessages().map(messageView))),
      h('div', { class: 'dock' },
        h('form', { class: 'composer', 'aria-label': 'Write a message', on: { submit: function (e) { e.preventDefault(); } } },
          s.replyTo ? h('p', { class: 'reply-ctx' }, 'Replying to ' + s.replyTo + ' ', btn('Cancel reply', { on: { click: function () { s.replyTo = null; s.focus = 'fk:ta'; render(); } } }, 'quiet')) : null,
          aud,
          h('div', { class: 'line' },
            h('button', { type: 'button', class: 'btn ibtn', 'aria-label': 'Attach a file', fk: 'attach', on: { click: function () { openSheet('attach', 'attach'); } } }, h('span', { 'aria-hidden': 'true', class: 'glyph' }, '+')),
            ta,
            h('button', { type: 'button', class: 'btn primary send', 'data-role': 'send', fk: 'send', 'aria-disabled': (empty || offline) ? 'true' : 'false', on: { click: function () { doSend(); } } }, 'Send')),
          offline ? h('p', { id: 'send-why', class: 'aud' }, 'Sending is paused until Conch is connected. Your draft is kept.') : null),
        stripView(), navBar()));
  }

  function rosterView() {
    var ch = s.joined ? s.voiceCh : s.ch;
    var people = ch === 'lounge' ? ROSTER_LOUNGE.slice() : [];
    var out = people.map(function (p) { return { n: p.n, tx: p.tx }; });
    if (s.joined && ch === s.voiceCh) out.unshift({ n: server().me + ' (you)', you: true });
    if (!out.length) return h('p', { class: 'muted' }, 'Nobody is in voice in #' + ch + '.');
    return h('ul', { class: 'roster', 'aria-label': 'In voice in #' + ch }, out.map(function (p) {
      var st = p.you ? (captured() ? (transmitting() ? 'Transmitting' : 'Microphone in use') : 'Listening') : (p.tx ? 'Speaking' : 'Listening');
      return h('li', {}, h('span', { class: 'nm' }, p.n), h('span', { class: 'badge' + (p.tx || (p.you && transmitting()) ? ' hot' : '') }, st));
    }));
  }

  function screenVoice() {
    var here = s.ch, vch = s.voiceCh;
    var offline = env.conn === 'offline';
    var dest, status, talk, toggles, tail = [];
    if (s.joining) {
      dest = h('div', { class: 'card' }, h('h2', { fk: 'status', tabindex: '-1', id: 'dest' }, 'Connecting to #' + here + '…'), h('p', { class: 'muted', role: 'status' }, 'Not in voice yet.'));
    } else if (!s.joined) {
      var why = offline ? 'Cannot join voice while offline.' : null;
      dest = h('div', { class: 'card' },
        h('h2', { id: 'dest' }, 'Not in voice'),
        h('p', {}, 'You are viewing #' + here + '. Opening a channel never joins voice.'),
        rosterView(),
        h('button', { type: 'button', class: 'btn primary block', fk: 'join', 'aria-disabled': why ? 'true' : null, 'aria-describedby': why ? 'join-why' : null, on: { click: function () { if (why) { say(why); return; } tryJoin(false); } } }, 'Join voice in #' + here),
        h('button', { type: 'button', class: 'btn block', fk: 'join-nomic', 'aria-disabled': why ? 'true' : null, 'aria-describedby': why ? 'join-why' : null, on: { click: function () { if (why) { say(why); return; } tryJoin(true); } } }, 'Join without microphone in #' + here),
        why ? h('p', { id: 'join-why', class: 'err' }, why) : null);
    } else {
      var elsewhere = vch !== here;
      dest = h('div', { class: 'card' },
        h('h2', { id: 'dest' }, (env.conn === 'connected' ? 'In voice in #' : 'Reconnecting to #') + vch),
        elsewhere ? h('p', {}, 'You are viewing #' + here + '. Voice stays in #' + vch + ' until you leave. To talk in #' + here + ', leave #' + vch + ' first, then join.') : null,
        rosterView());
    }
    if (s.joined) {
      var cap = captured();
      var capWhy = cap ? 'Microphone in use' : env.grant === 'listen' ? 'Listen only: this server gave you no microphone here' : s.noMic ? 'Microphone off: you joined without it' : 'Microphone off';
      status = h('div', { class: 'status-lines card', 'aria-label': 'Voice state' },
        h('p', { class: 'l' }, h('span', { class: 'ring', 'data-on': cap ? 'true' : 'false', 'aria-hidden': 'true' }), h('span', {}, capWhy)),
        cap ? h('p', { class: 'l' }, h('span', { class: 'ring pulse', 'data-role': 'tx-ring', 'data-on': transmitting() ? 'true' : 'false', 'aria-hidden': 'true' }), h('span', { 'data-role': 'tx-text' }, transmitting() ? 'Transmitting' : 'Not transmitting')) : null,
        s.deafened ? h('p', {}, 'Deafened. You hear nothing and send nothing.') : null,
        s.muted && !s.deafened ? h('p', {}, 'Muted. Nothing is sent.') : null,
        (env.mic === 'denied' && s.noMic) ? h('p', {}, 'The microphone is off in system settings. ', btn('Open system settings', { on: { click: function () { toast('Opens the system settings (not in this prototype).'); } } }, 'quiet')) : null);
      if (cap) {
        var block = talkBlock();
        if (s.mode === 'ptt') {
          talk = [talkButton('big', 'Hold to talk', 'Hold to talk, sends to #' + vch, block),
            h('p', { class: 'muted' }, s.style === 'toggle' ? 'Tap to start sending, tap again to stop. Sends only while this app is on screen.' : 'Hold to send. Let go to stop. Sends only while this app is on screen and stops if it leaves.')];
        } else {
          talk = [h('div', { class: 'card' }, h('h2', {}, 'Open microphone'), h('p', {}, 'Sends when you speak (illustrative: no audio here). Mute stops it. Nothing to hold.'))];
        }
        toggles = h('div', { class: 'toggles' },
          btn(s.muted ? 'Unmute' : 'Mute', { fk: 'mute', on: { click: toggleMute } }),
          btn(s.deafened ? 'Undeafen' : 'Deafen', { fk: 'deafen', on: { click: toggleDeafen } }));
      } else {
        talk = [h('p', { class: 'muted' }, env.grant === 'listen' ? 'Listen only. You hear the room; there is no microphone to use here.' : 'You hear the room. To talk, leave and join with the microphone.')];
        toggles = h('div', { class: 'toggles' }, btn(s.deafened ? 'Undeafen' : 'Deafen', { fk: 'deafen', on: { click: toggleDeafen } }));
      }
      tail.push(btn('Leave voice', { fk: 'leave', on: { click: leave } }, 'danger block'));
    }
    tail.push(btn('Voice options', { fk: 'vopts', on: { click: function () { openSheet('voiceopts', 'vopts'); } } }, 'block quiet'));
    var left = [dest, status];
    var right = [talk, toggles, tail];
    var land = isLand();
    return h('div', { class: 'app' },
      topBar('Voice', server().name + ' · ' + server().me, [serverBtn(), settingsBtn()]),
      connBanner(),
      h('div', { class: 'content' }, land
        ? h('div', { class: 'pad voice-land' }, h('div', { class: 'pad' }, left), h('div', { class: 'pad' }, right))
        : h('div', { class: 'pad' }, left, right)),
      h('div', { class: 'dock' }, navBar()));
  }

  function screenSettings() {
    var n = env.notif;
    return h('div', { class: 'app' },
      topBar('Settings', server().name, null, h('button', { type: 'button', class: 'ibtn', fk: 'back', 'aria-label': 'Back', on: { click: function () { view('chat'); } } }, h('span', { 'aria-hidden': 'true', class: 'glyph' }, '‹'))),
      connBanner(),
      h('div', { class: 'content' }, h('div', { class: 'pad' },
        h('div', { class: 'card' }, h('h2', {}, 'Account'),
          h('dl', { class: 'kv' }, h('dt', {}, 'Signed in as'), h('dd', {}, server().me), h('dt', {}, 'Server'), h('dd', { class: 'mono' }, server().addr))),
        h('div', { class: 'card' }, h('h2', {}, 'Notifications'),
          n === 'ask' ? h('p', {}, 'Off. Conch works without them: you see new messages when you open it.') : null,
          n === 'allowed' ? h('p', {}, 'On. What a notification shows is decided in the notification design (M3).') : null,
          n === 'denied' ? h('p', {}, 'Off in system settings. Conch cannot turn them on; it works without them.') : null,
          n === 'ask' ? btn('Turn on notifications', { fk: 'notif', on: { click: function () { openSheet('osnotif', 'notif'); } } }) : null,
          n === 'denied' ? btn('Open system settings', { on: { click: function () { toast('Opens the system settings (not in this prototype).'); } } }) : null),
        btn('Voice options', { fk: 'vopts2', on: { click: function () { openSheet('voiceopts', 'vopts2'); } } }, 'block'),
        btn('Servers and accounts', { fk: 'servers2', on: { click: function () { openSheet('servers', 'servers2'); } } }, 'block'),
        btn('Sign out on this phone', { fk: 'signout', on: { click: function () { openSheet('signout', 'signout'); } } }, 'block'),
        h('p', { class: 'muted' }, 'Prototype: illustrative sample data; nothing is stored.'))));
  }

  function screenGone() {
    return h('div', { class: 'app' },
      topBar('This channel is not available', null, null),
      h('div', { class: 'content' }, h('div', { class: 'pad' },
        h('p', {}, 'You followed a link or notification to #ops-old. It may have been renamed, or you may no longer have access. Conch cannot tell which, and does not say.'),
        s.joined ? h('p', {}, 'Voice is not affected unless you lost access to its channel.') : null,
        h('button', { type: 'button', class: 'btn primary block', fk: 'tochannels', on: { click: function () { view('channels'); } } }, 'Go to Channels'))),
      h('div', { class: 'dock' }, stripView(), navBar()));
  }
  function screenRevoked() {
    return h('div', { class: 'app' },
      topBar('You were signed out', server().name, null),
      h('div', { class: 'content' }, h('div', { class: 'pad' },
        h('p', { role: 'status' }, 'The server ended this sign-in. Voice stopped. Messages you were still typing stay on this phone.'),
        h('button', { type: 'button', class: 'btn primary block', fk: 'again', on: { click: function () { s.joined = false; s.voiceCh = null; go('signin'); } } }, 'Sign in again'),
        btn('Choose another server', { on: { click: function () { openSheet('servers', null); } } }, 'block'))));
  }

  /* ---------- sheets ---------- */

  function sheet(title, kids, extraRole) {
    return h('div', { class: 'scrim', on: { click: function (e) { if (e.target === e.currentTarget) closeSheet(); } } },
      h('div', { class: 'sheet', role: extraRole || 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'sheet-h' },
        h('h2', { id: 'sheet-h', tabindex: '-1' }, title), kids));
  }
  function sheetView() {
    var k = s.sheet;
    if (k === 'servers') {
      return sheet('Servers and accounts', h('div', { class: 'acts' },
        SERVERS.map(function (sv, i) {
          var cur = i === s.server;
          return h('button', { type: 'button', class: 'btn block', 'aria-current': cur ? 'true' : null, fk: 'srv-' + sv.id, style: null,
            'aria-label': sv.name + ', ' + sv.addr + ', signed in as ' + sv.me + (cur ? ', current' : ''),
            on: { click: function () { if (cur) { closeSheet(); return; } if (s.joined) { s.pendingSwitch = i; openSheet('switchwarn', 'servers'); } else switchServer(i); } } },
            h('span', {}, sv.name, h('br'), h('span', { class: 'muted' }, sv.addr + ' · ' + sv.me + (cur ? ' · current' : ''))));
        }),
        btn('Add a server', { on: { click: function () { s.sheet = null; go('paste'); } } }, 'primary block'),
        btn('Close', { on: { click: closeSheet } }, 'block quiet'),
        h('p', { class: 'muted' }, 'Each server is signed in on its own. One voice call at a time, on one server.')));
    }
    if (k === 'switchwarn') {
      var to = SERVERS[s.pendingSwitch];
      return sheet('Switch to ' + to.name + '?', h('div', { class: 'acts' },
        h('p', {}, 'You are in voice in #' + s.voiceCh + ' on ' + server().name + '. Switching leaves that call.'),
        btn('Leave voice and switch', { on: { click: function () { var i = s.pendingSwitch; leaveQuiet(); switchServer(i); } } }, 'danger block'),
        btn('Stay here', { on: { click: closeSheet } }, 'block')), 'alertdialog');
    }
    if (k === 'signout') {
      return sheet('Sign out on this phone?', h('div', { class: 'acts' },
        h('p', {}, 'This ends the sign-in on this phone only. Your other devices stay signed in.' + (s.joined ? ' You will leave voice.' : '')),
        btn('Sign out', { on: { click: function () { leaveQuiet(); s.sheet = null; go('signin'); } } }, 'danger block'),
        btn('Cancel', { on: { click: closeSheet } }, 'block')), 'alertdialog');
    }
    if (k === 'osmic') {
      return sheet('“Conch” would like to use the microphone', h('div', { class: 'acts' },
        h('p', { class: 'muted' }, 'A stand-in for the system dialog. The real one is drawn by the OS, with the OS wording.'),
        btn('Allow', { on: { click: function () { env.mic = 'allowed'; $('mic').value = 'allowed'; beginJoin(s.pendingJoin || s.ch, false); } } }, 'primary block'),
        btn('Don’t allow', { on: { click: function () { env.mic = 'denied'; $('mic').value = 'denied'; s.sheet = 'micdenied'; s.focus = 'sheet'; render(); } } }, 'block')), 'alertdialog');
    }
    if (k === 'micdenied') {
      return sheet('The microphone is off for Conch', h('div', { class: 'acts' },
        h('p', {}, 'Conch cannot send your voice. You can still listen to #' + (s.pendingJoin || s.ch) + '.'),
        btn('Open system settings', { on: { click: function () { toast('Opens the system settings (not in this prototype).'); } } }, 'block'),
        btn('Join without microphone', { on: { click: function () { beginJoin(s.pendingJoin || s.ch, true); } } }, 'primary block'),
        btn('Cancel', { on: { click: closeSheet } }, 'block quiet')), 'alertdialog');
    }
    if (k === 'osnotif') {
      return sheet('“Conch” would like to send you notifications', h('div', { class: 'acts' },
        h('p', { class: 'muted' }, 'A stand-in for the system dialog.'),
        btn('Allow', { on: { click: function () { env.notif = 'allowed'; $('notif').value = 'allowed'; closeSheet(); say('Notifications are on.'); } } }, 'primary block'),
        btn('Don’t allow', { on: { click: function () { env.notif = 'denied'; $('notif').value = 'denied'; closeSheet(); say('Notifications stay off. Conch works without them.'); } } }, 'block')), 'alertdialog');
    }
    if (k === 'voiceopts') {
      function radio(group, val, label, cur, set) {
        var r = h('input', { type: 'radio', name: group, value: val, id: group + '-' + val, on: { change: function () { set(val); } } });
        if (cur === val) r.setAttribute('checked', '');
        r.checked = cur === val;
        return h('label', { 'for': group + '-' + val, class: 'check', style: null }, r, ' ' + label);
      }
      return sheet('Voice options', h('div', { class: 'acts' },
        h('fieldset', {}, h('legend', {}, 'How you talk'),
          radio('mode', 'ptt', 'Hold to talk', s.mode, function (v) { s.mode = v; s.talking = false; }),
          radio('mode', 'open', 'Open microphone', s.mode, function (v) { s.mode = v; s.talking = false; })),
        h('fieldset', {}, h('legend', {}, 'Hold to talk works by'),
          radio('style', 'hold', 'Holding the button', s.style, function (v) { s.style = v; }),
          radio('style', 'toggle', 'Tapping to start and again to stop', s.style, function (v) { s.style = v; })),
        h('p', { class: 'muted' }, 'Sound goes to the phone’s current output (earpiece, speaker, headset or Bluetooth) as the system chooses; Conch shows which. Illustrative.'),
        btn('Done', { on: { click: function () { closeSheet(); } } }, 'primary block')));
    }
    if (k === 'members') {
      return sheet('Members of #' + s.ch, h('div', { class: 'acts' },
        h('ul', { class: 'roster' }, [['Ada', ''], [longName, ''], ['build-bot', 'Agent'], ['Sam (you)', '']].map(function (p) { return h('li', {}, h('span', { class: 'nm' }, p[0]), p[1] ? h('span', { class: 'badge' }, p[1]) : null); })),
        btn('Close', { on: { click: closeSheet } }, 'block')));
    }
    if (k === 'attach') {
      return sheet('Attach a file', h('div', { class: 'acts' },
        btn('Photo library', { on: { click: function () { closeSheet(); toast('The system picker opens here; Conch gets only what you pick (illustrative).'); } } }, 'block'),
        btn('Files', { on: { click: function () { closeSheet(); toast('The system file picker opens here (illustrative).'); } } }, 'block'),
        btn('Cancel', { on: { click: closeSheet } }, 'block quiet')));
    }
    if (k === 'message') {
      var m = allMessages().filter(function (x) { return x.id === s.msgSheet; })[0];
      if (!m) return null;
      return sheet('Message from ' + m.who, h('div', { class: 'acts' },
        btn('Reply', { on: { click: function () { s.replyTo = m.who + ': ' + (m.body || m.file || '').slice(0, 40); s.sheet = null; s.msgSheet = null; s.opener = null; s.focus = 'fk:ta'; render(); } } }, 'block'),
        btn('Copy text', { on: { click: function () { closeSheet(); toast('Copied (illustrative).'); } } }, 'block'),
        btn('Cancel', { on: { click: closeSheet } }, 'block quiet')));
    }
    return null;
  }
  function leaveQuiet() { s.joined = false; s.joining = false; s.voiceCh = null; s.talking = false; s.noMic = false; s.muted = false; s.deafened = false; }
  function switchServer(i) {
    s.server = i; s.ch = SERVERS[i].channels[0].n; s.sheet = null; s.opener = null; s.extra = [];
    view('channels'); toast('Now on ' + SERVERS[i].name + ' as ' + SERVERS[i].me + '.');
  }

  /* ---------- render ---------- */

  function isLand() { var z = SIZES[env.size]; return z.w > z.h; }

  function render() {
    var ph = $('phone');
    var active = document.activeElement;
    var prevFk = active && ph.contains(active) ? active.getAttribute('data-fk') : null;
    var prevScroll = null;
    var oldContent = ph.querySelector('.content');
    if (oldContent) prevScroll = oldContent.scrollTop;
    var prevBase = ph.getAttribute('data-base');

    var z = SIZES[env.size];
    var ins = env.safe ? z.safe : [0, 0, 0, 0];
    ph.style.setProperty('--w', z.w + 'px'); ph.style.setProperty('--h', z.h + 'px');
    ph.style.setProperty('--sat', ins[0] + 'px'); ph.style.setProperty('--sar', ins[1] + 'px');
    ph.style.setProperty('--sab', ins[2] + 'px'); ph.style.setProperty('--sal', ins[3] + 'px');
    ph.style.setProperty('--scale', String(env.scale));
    ph.toggleAttribute('data-land', isLand());
    ph.toggleAttribute('data-kb', env.kb);
    ph.toggleAttribute('data-large', env.scale >= 1.6);
    ph.toggleAttribute('data-safe-show', env.safeShow);
    ph.setAttribute('data-motion', env.motion === 'system' ? (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'reduce' : 'allow') : env.motion);
    ph.setAttribute('data-base', s.base);

    var screens = { link: screenLink, paste: screenPaste, confirm: screenConfirm, account: screenAccount, signin: screenSignin, channels: screenChannels, chat: screenChat, voice: screenVoice, settings: screenSettings, gone: screenGone, revoked: screenRevoked };
    var app = (screens[s.base] || screenChannels)();
    var sh = sheetView();
    if (sh) app.setAttribute('inert', '');
    if (isLand() && app.querySelector('.nav')) {
      // Landscape: the views are a rail, first in reading order.
      var rail = app.querySelector('.nav'); rail.remove();
      var main = h('div', { class: 'main' });
      while (app.firstChild) main.appendChild(app.firstChild);
      app.appendChild(rail); app.appendChild(main); app.classList.add('land');
    }
    var overlay = h('div', { class: 'safe-overlay', 'aria-hidden': 'true' });
    overlay.style.borderWidth = ins[0] + 'px ' + ins[1] + 'px ' + ins[2] + 'px ' + ins[3] + 'px';

    ph.textContent = '';
    ph.appendChild(app);
    if (env.kb) ph.appendChild(h('div', { class: 'keyboard', 'aria-hidden': 'true', style: null }, 'On-screen keyboard (stand-in)'));
    var kb = ph.querySelector('.keyboard');
    if (kb) kb.style.height = (isLand() ? 150 : 290) + 'px';
    if (sh) ph.appendChild(sh);
    ph.appendChild(overlay);
    ph.appendChild(h('div', { class: 'home-bar', 'aria-hidden': 'true' }));
    paintToast();
    $('caption').textContent = (byId[s.route] ? byId[s.route].label : s.route) + ' — ' + z.w + '×' + z.h + (isLand() ? ' landscape' : ' portrait') + ', text ' + Math.round(env.scale * 100) + '%';

    // Scroll: chat starts at the newest message; others keep their place.
    var content = ph.querySelector('.content');
    if (isLand() && env.scale >= 1.6 && ph.querySelector('.main')) content = ph.querySelector('.main');
    if (content) {
      if (s.stick || prevBase !== s.base) content.scrollTop = s.base === 'chat' ? content.scrollHeight : 0;
      else if (prevScroll !== null) content.scrollTop = prevScroll;
    }
    s.stick = false;

    // Focus: where the last action said, else the control that had it.
    var want = s.focus; s.focus = null;
    var target = null;
    if (want === 'heading') target = ph.querySelector('#heading');
    else if (want === 'sheet') target = ph.querySelector('#sheet-h');
    else if (want && want.indexOf('fk:') === 0) target = ph.querySelector('[data-fk="' + want.slice(3) + '"]');
    else if (prevFk) target = ph.querySelector('[data-fk="' + prevFk + '"]');
    if (target && typeof target.focus === 'function') target.focus({ preventScroll: true });
  }

  /* ---------- the bench controls ---------- */

  function fillScreens() {
    var sel = $('screen');
    SCREENS.forEach(function (x) { sel.appendChild(h('option', { value: x.id }, x.label)); });
  }
  function bind() {
    $('controls').addEventListener('submit', function (e) { e.preventDefault(); });
    $('screen').addEventListener('change', function (e) { go(e.target.value); });
    $('size').addEventListener('change', function (e) { env.size = e.target.value; s.focus = null; render(); });
    $('scale').addEventListener('change', function (e) { env.scale = parseFloat(e.target.value); s.focus = null; render(); });
    $('motion').addEventListener('change', function (e) { env.motion = e.target.value; s.focus = null; render(); });
    $('safe').addEventListener('change', function (e) { env.safe = e.target.checked; s.focus = null; render(); });
    $('safe-show').addEventListener('change', function (e) { env.safeShow = e.target.checked; s.focus = null; render(); });
    $('kb').addEventListener('change', function (e) { env.kb = e.target.checked; s.focus = null; render(); });
    $('installed').addEventListener('change', function (e) { env.installed = e.target.checked; s.linkTried = false; s.focus = null; render(); });
    $('conn').addEventListener('change', function (e) {
      env.conn = e.target.value; if (env.conn !== 'connected') s.talking = false; s.focus = null; render();
      say(env.conn === 'connected' ? 'Connected.' : env.conn === 'offline' ? 'Offline.' : 'Reconnecting.');
    });
    $('mic').addEventListener('change', function (e) { env.mic = e.target.value; s.focus = null; render(); });
    $('notif').addEventListener('change', function (e) { env.notif = e.target.value; s.focus = null; render(); });
    $('grant').addEventListener('change', function (e) { env.grant = e.target.value; s.talking = false; s.focus = null; render(); });
    $('failnext').addEventListener('change', function (e) { env.failnext = e.target.checked; });
    $('interrupt').addEventListener('click', interrupt);
    $('reset').addEventListener('click', function () {
      window.clearTimeout(joinTimer);
      SERVERS.forEach(function (sv) { sv.channels.forEach(function (c) { if (c.n === 'general' && sv.id === 'a') c.unread = 2; }); });
      s = fresh(); go(initialRoute());
    });
    // Escape closes a sheet; releasing anywhere ends Hold to talk.
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && s.sheet) { e.preventDefault(); closeSheet(); } });
    ['pointerup', 'pointercancel'].forEach(function (ev) { window.addEventListener(ev, function () { if (s.style !== 'toggle') setTalking(false); }); });
    // Leaving the window or hiding the page ends talking in both styles.
    window.addEventListener('blur', function () { setTalking(false); });
    document.addEventListener('visibilitychange', function () { if (document.hidden) setTalking(false); });
    // A sheet is modal: Tab and Shift+Tab wrap inside it.
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Tab' || !s.sheet) return;
      var sh = $('phone').querySelector('.sheet'); if (!sh) return;
      var f = Array.prototype.filter.call(sh.querySelectorAll('button, input'), function (x) { return !x.disabled && x.offsetParent !== null && !(x.type === 'radio' && !x.checked); });
      if (!f.length) return;
      var first = f[0], last = f[f.length - 1], a = document.activeElement;
      var inside = sh.contains(a);
      if (!inside || (e.shiftKey && (a === first || a.id === 'sheet-h')) ) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && (a === last || a.id === 'sheet-h')) { e.preventDefault(); first.focus(); }
    });
    window.addEventListener('hashchange', function () { var r = initialRoute(); if (r !== s.route) go(r); });
  }
  function initialRoute() {
    var r = window.location.hash.replace(/^#\/?/, '');
    return byId[r] ? r : 'link';
  }

  fillScreens();
  bind();
  go(initialRoute());
}());
