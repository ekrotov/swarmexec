/* swarmexec — site-next: the stage and the three toys.
   Every scene re-enacts a documented mechanism, and every string printed is the
   client's own (see NOTES.md for the source of each). No library: requestAnimationFrame
   for packets, CSS for everything else. Without JS the page shows the health scene's
   end state and the toys' default outputs, statically. Under prefers-reduced-motion
   every scene and toy jumps to its end state. */
(function () {
  'use strict';

  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
  var SVGNS = 'http://www.w3.org/2000/svg';
  var CANCEL = { cancelled: true };

  function $(sel, root) { return (root || document).querySelector(sel); }
  function $$(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  /* parts: array of strings or [cls, text] pairs — never HTML */
  function rich(tag, cls, parts) {
    var e = el(tag, cls);
    parts.forEach(function (p) {
      if (typeof p === 'string') e.appendChild(document.createTextNode(p));
      else e.appendChild(el('span', p[0], p[1]));
    });
    return e;
  }
  function later(ms) { return new Promise(function (r) { setTimeout(r, reduce ? 0 : ms); }); }
  function restartAnim(node, cls) { node.classList.remove(cls); void node.offsetWidth; node.classList.add(cls); }
  function onceVisible(node, threshold, fn) {
    if (!('IntersectionObserver' in window) || reduce) { fn(); return; }
    var io = new IntersectionObserver(function (es) {
      if (es.some(function (x) { return x.isIntersecting; })) { io.disconnect(); fn(); }
    }, { threshold: threshold });
    io.observe(node);
  }

  /* ================================================================ stage */
  var stage = $('#stage');
  if (stage) initStage();

  function initStage() {
    var board = $('#board'), svg = $('#wires'), labelsEl = $('#labels');
    var con = $('#console'), cap = $('#stage-cap'), hint = $('#stage-hint');
    var tabs = $$('.scenes [role="tab"]'), panel = $('#stage-body');
    var wires = {}, labels = [], token = 0, current = 'health', started = false, visible = !('IntersectionObserver' in window);

    function anchor(n) { return board.querySelector('[data-anchor="' + n + '"]'); }
    function pod(n) { return board.querySelector('[data-pod="' + n + '"]'); }
    function rel(e) {
      var b = board.getBoundingClientRect(), r = e.getBoundingClientRect();
      return { x: r.left - b.left, y: r.top - b.top, w: r.width, h: r.height };
    }

    /* ---- wires, drawn from measured geometry so they follow any reflow */
    /* a second layer above the cards carries the packets that travel inside node-4 */
    var top = document.createElementNS(SVGNS, 'svg');
    top.setAttribute('class', 'wires wires-top'); top.setAttribute('aria-hidden', 'true'); top.setAttribute('focusable', 'false');
    board.appendChild(top);
    ['mgr', 'node-2', 'node-3', 'node-4', 'fwd-in'].forEach(function (id) {
      var p = document.createElementNS(SVGNS, 'path');
      p.setAttribute('class', id === 'fwd-in' ? 'fwd-inner' : 'wire');
      (id === 'fwd-in' ? top : svg).appendChild(p);
      wires[id] = p;
    });

    function draw() {
      var bw = board.clientWidth, bh = board.clientHeight;
      svg.setAttribute('viewBox', '0 0 ' + bw + ' ' + bh);
      top.setAttribute('viewBox', '0 0 ' + bw + ' ' + bh);
      var h = rel(anchor('host')), m = rel(anchor('mgr'));
      wires.mgr.setAttribute('d', 'M' + (h.x + h.w) + ' ' + (h.y + h.h / 2) + ' L' + m.x + ' ' + (m.y + m.h / 2));
      ['node-2', 'node-3', 'node-4'].forEach(function (id, i) {
        var n = rel(anchor(id));
        var sx = h.x + h.w * (0.28 + 0.22 * i), sy = h.y + h.h;
        var ex = n.x + n.w / 2, ey = n.y;
        var k = (ey - sy) * 0.6;
        wires[id].setAttribute('d', 'M' + sx + ' ' + sy + ' C' + sx + ' ' + (sy + k) + ' ' + ex + ' ' + (ey - k) + ' ' + ex + ' ' + ey);
      });
      /* inside node-4: from where the wire lands, down the card's left edge, into the
         sidecar, and across to db.1 — the sidecar sits in db.1's network namespace */
      var side = $('.sidecar', board);
      if (!side.hidden) {
        var n4 = rel(anchor('node-4')), sc = rel(side), db = rel(pod('db.1'));
        var gx = n4.x + 5;
        wires['fwd-in'].setAttribute('d',
          'M' + (n4.x + n4.w / 2) + ' ' + n4.y +
          ' L' + gx + ' ' + (n4.y + 14) +
          ' L' + gx + ' ' + (sc.y + sc.h / 2) +
          ' L' + sc.x + ' ' + (sc.y + sc.h / 2) +
          ' M' + (sc.x + 14) + ' ' + sc.y + ' L' + (sc.x + 14) + ' ' + (db.y + db.h));
      } else wires['fwd-in'].setAttribute('d', '');
      labels.forEach(placeLabel);
    }

    function placeLabel(L) {
      var x, y;
      if (L.wire === 'mgr') {
        var m = rel(anchor('mgr'));
        x = m.x + m.w / 2; y = m.y + m.h + 8;
        L.node.style.transform = 'translate(-50%, 0)';
      } else {
        var p = wires[L.wire], len = p.getTotalLength();
        var pt = p.getPointAtLength(len * 0.5);
        x = pt.x; y = pt.y;
        L.node.style.transform = '';
      }
      var bw = board.clientWidth, half = L.node.offsetWidth / 2;
      x = Math.max(half + 4, Math.min(bw - half - 4, x));
      L.node.style.left = x + 'px';
      L.node.style.top = y + 'px';
    }

    function clearLabels() { labels.forEach(function (L) { L.node.remove(); }); labels = []; }
    function label(wire, text, back) {
      clearLabels();
      var n = el('div', 'wlabel' + (back ? ' back' : ''), text);
      labelsEl.appendChild(n);
      var L = { wire: wire, node: n };
      labels.push(L);
      placeLabel(L);
    }

    /* with motion on, the terminal starts empty and the scene types it; the static
       end state in the HTML stays for no-JS and reduced-motion readers */
    if (!reduce) {
      con.textContent = '';
      var idle = el('p', 'con-line cmd'); idle.appendChild(el('span', 'cursor')); con.appendChild(idle);
    }

    if (window.ResizeObserver) new ResizeObserver(draw).observe(board);
    window.addEventListener('resize', draw);
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(draw);
    draw();

    /* ---- boot: the wires draw themselves once the cards have landed (CSS does the cards) */
    var bootDone = reduce ? Promise.resolve() : later(1150).then(function () {
      draw();
      ['mgr', 'node-2', 'node-3', 'node-4'].forEach(function (id, i) {
        var p = wires[id];
        p.setAttribute('pathLength', '1');
        p.style.animationDelay = (i * 90) + 'ms';
        p.classList.add('drawing');
        setTimeout(function () {
          p.classList.remove('drawing'); p.removeAttribute('pathLength'); p.style.animationDelay = '';
        }, 700 + i * 90 + 60);
      });
      return later(1000);
    });

    /* hovering a node card traces its wire, when no scene is using it */
    ['node-2', 'node-3', 'node-4'].forEach(function (id) {
      var c = anchor(id);
      c.addEventListener('mouseenter', function () { if (!wires[id].classList.contains('is-on')) wires[id].classList.add('is-hover'); });
      c.addEventListener('mouseleave', function () { wires[id].classList.remove('is-hover'); });
    });

    /* ---- the scene runner: every await re-checks that the scene is still current */
    function runner(my) {
      function live() { if (my !== token) throw CANCEL; }
      function packetOn(p, back, d, cls) {
        if (reduce) return Promise.resolve().then(live);
        var len = p.getTotalLength(), t0 = null;
        if (!len) return Promise.resolve().then(live);
        var c = document.createElementNS(SVGNS, 'circle');
        c.setAttribute('r', 4.5);
        c.setAttribute('class', 'packet' + (back ? ' back' : '') + (cls ? ' ' + cls : ''));
        c.setAttribute('cx', -20);
        svg.appendChild(c);
        return new Promise(function (res) {
          var done = false;
          function end() { if (!done) { done = true; c.remove(); res(); } }
          /* rAF does not tick in a hidden tab (or some headless setups); never let that stall a scene */
          setTimeout(end, d + 400);
          function f(ts) {
            if (done) return;
            if (my !== token) return end();
            if (t0 === null) t0 = ts;
            var k = Math.min(1, (ts - t0) / d);
            var e = k < 0.5 ? 2 * k * k : 1 - Math.pow(-2 * k + 2, 2) / 2;
            var pt = p.getPointAtLength(len * (back ? 1 - e : e));
            c.setAttribute('cx', pt.x); c.setAttribute('cy', pt.y);
            if (k < 1) requestAnimationFrame(f); else end();
          }
          requestAnimationFrame(f);
        }).then(live);
      }
      return {
        live: live,
        wait: function (ms) { return later(ms).then(live); },
        packet: function (wire, back, dur) { return packetOn(wires[wire], back, dur || 650); },
        /* type text into a console line, a few characters per tick, with a caret */
        type: function (line, text, cps) {
          if (reduce) { line.appendChild(document.createTextNode(text)); return Promise.resolve().then(live); }
          var node = document.createTextNode(''), caret = el('span', 'caret');
          line.appendChild(node); line.appendChild(caret);
          var i = 0, step = Math.max(1, Math.round((cps || 38) / 30));
          return new Promise(function (res) {
            function tick() {
              if (my !== token) { caret.remove(); return res(); }
              i = Math.min(text.length, i + step);
              node.data = text.slice(0, i);
              if (i < text.length) setTimeout(tick, 33 + Math.random() * 18);
              else { caret.remove(); res(); }
            }
            tick();
          }).then(live);
        }
      };
    }

    function on(wire, cls) { wires[wire].classList.remove('is-hover'); wires[wire].classList.add(cls || 'is-on'); }
    function off(wire) { wires[wire].classList.remove('is-on', 'is-warn'); }
    function card(n, v) { anchor(n).classList.toggle('is-on', v !== false); }
    function spawn(node) { node.classList.remove('spawn'); void node.offsetWidth; node.classList.add('spawn'); }

    function say(parts, cls) {
      var line = Array.isArray(parts) ? rich('p', 'con-line ' + (cls || ''), parts) : el('p', 'con-line ' + (cls || ''), parts);
      con.appendChild(line);
      return line;
    }
    async function cmd(r, text) {
      var line = el('p', 'con-line cmd');
      con.appendChild(line);
      await r.wait(120);
      await r.type(line, text, 40);
      await r.wait(180);
      return line;
    }
    function setCap(nodes) {
      cap.textContent = '';
      nodes.forEach(function (n) {
        if (typeof n === 'string') cap.appendChild(document.createTextNode(n));
        else cap.appendChild(el('code', null, n[0]));
      });
    }

    function reset() {
      clearLabels();
      $$('circle', board).forEach(function (c) { c.remove(); });
      Object.keys(wires).forEach(function (w) { wires[w].setAttribute('class', w === 'fwd-in' ? 'fwd-inner' : 'wire'); });
      $$('.card', board).forEach(function (c) { c.classList.remove('is-on'); });
      $$('.pod', board).forEach(function (p) {
        p.classList.remove('is-on', 'is-gone', 'is-stopping', 'spawn', 'flip');
        p.disabled = true;
        p.removeAttribute('aria-label');
      });
      pod('web.1').dataset.health = 'healthy';
      pod('web.2').dataset.health = 'healthy';
      pod('web.3').dataset.health = 'healthy';
      pod('db.1').dataset.health = 'none';
      pod('web.2b').dataset.health = 'starting';
      $('.pod-late', board).hidden = true;
      $('.sidecar', board).hidden = true;
      board.classList.remove('hp', 'probing');
      con.textContent = '';
      hint.hidden = true;
      draw();
    }

    /* ---- health: the rule is healthColor()/healthBadge() in client/internal/cli/stats.go */
    var STATUS = {
      healthy:   ['Up 3 days ', ['c-aqua', '(healthy)']],
      unhealthy: ['Up 2 hours ', ['c-red', '(unhealthy)']],
      starting:  ['Up 8 seconds ', ['c-yellow', '(health: starting)']],
      none:      ['Up 3 days', ['c-dim', '   no healthcheck declared']]
    };
    var CYCLE = { healthy: 'unhealthy', unhealthy: 'starting', starting: 'none', none: 'healthy' };
    var WEB = [['web.1', 'node-2'], ['web.2', 'node-3'], ['web.3', 'node-4']];
    var H = {};

    function verdict() {
      var c = { healthy: 0, unhealthy: 0, starting: 0, none: 0 };
      WEB.forEach(function (w) { c[pod(w[0]).dataset.health]++; });
      var checked = c.healthy + c.unhealthy + c.starting, colour, why, badge = null;
      if (c.unhealthy === 0) colour = 'c-aqua';
      else if (c.unhealthy >= checked) colour = 'c-red';
      else colour = 'c-amber';
      if (c.unhealthy > 0) badge = ['c-red', '✖ ' + c.unhealthy + ' unhealthy'];
      else if (c.starting > 0) badge = ['c-yellow', '◌ ' + c.starting + ' starting'];

      if (checked === 0) why = 'No container declares a healthcheck. Nothing is known, so the row is not recoloured: aqua, from 3/3.';
      else if (c.unhealthy > 0 && c.unhealthy >= checked)
        why = (c.none ? 'Every container that declares a healthcheck fails it' : 'Every container fails its healthcheck') + ': red, as bad as none running.';
      else if (c.unhealthy > 0) why = c.unhealthy + ' of ' + checked + ' checked containers unhealthy: orange, like a partial rollout.';
      else if (c.starting > 0) why = 'Probes not passed yet: a yellow marker, and the colour stays what 3/3 gave it.';
      else why = 'Every checked container healthy: the row keeps the colour the replica count gave it.';
      if (c.none && checked && !(c.unhealthy > 0 && c.unhealthy >= checked)) why += ' A container without a healthcheck is not counted either way.';
      return { colour: colour, badge: badge, why: why };
    }

    function renderHealth(flash) {
      if (!H.agents) return;
      H.agents.textContent = '';
      WEB.forEach(function (w) {
        var st = STATUS[pod(w[0]).dataset.health];
        H.agents.appendChild(rich('p', 'con-line', [w[1] + '  '].concat(st)));
      });
      var v = verdict();
      H.row.textContent = '';
      H.row.appendChild(el('span', v.colour, '▾ web  replicated  3/3  nginx:1.27.2'));
      if (v.badge) { H.row.appendChild(document.createTextNode('  ')); H.row.appendChild(el('span', v.badge[0], v.badge[1])); }
      H.why.textContent = v.why;
      if (flash) restartAnim(H.row, 'flash');
      WEB.forEach(function (w) {
        var p = pod(w[0]);
        p.setAttribute('aria-label', w[0] + ' on ' + w[1] + ', healthcheck ' + (p.dataset.health === 'none' ? 'not declared' : p.dataset.health) + '. Activate to change it.');
      });
    }

    /* a tap sends the new verdict up the wire the way the agent would report it */
    board.addEventListener('click', function (e) {
      var p = e.target.closest('.pod');
      if (!p || p.disabled || current !== 'health') return;
      p.dataset.health = CYCLE[p.dataset.health];
      restartAnim(p, 'flip');
      var node = WEB.filter(function (w) { return w[0] === p.dataset.pod; })[0][1];
      var r = runner(token);
      on(node);
      r.packet(node, true, 520).then(function () { off(node); renderHealth(true); }, function () {});
      if (reduce) renderHealth(true);
    });

    var scenes = {};

    scenes.health = async function (r) {
      setCap(['Swarm’s task state reads ', ['running'], ' while its container fails every probe, so the verdict has to come from the node. It rides along with the status line the agent already reads for resource usage — no extra request.']);
      pod('web.2').dataset.health = 'unhealthy';
      board.classList.add('probing');
      var b1 = el('div', 'con-block'); var h1 = el('p', 'con-h'); b1.appendChild(h1); con.appendChild(b1);
      await r.type(h1, 'manager · task list for web', 90);
      on('mgr'); card('mgr');
      label('mgr', 'TaskList');
      await r.packet('mgr', false);
      label('mgr', 'every task: running', true);
      await r.packet('mgr', true);
      for (var i = 0; i < WEB.length; i++) {
        b1.appendChild(rich('p', 'con-line', [WEB[i][0] + '   ' + WEB[i][1] + '   ', ['c-aqua', 'running']]));
        await r.wait(140);
      }
      off('mgr'); card('mgr', false); clearLabels();
      await r.wait(450);

      var b2 = el('div', 'con-block'); var h2 = el('p', 'con-h'); b2.appendChild(h2); con.appendChild(b2);
      await r.type(h2, 'agents · the status line each node already reads', 110);
      H.agents = el('div'); b2.appendChild(H.agents);
      ['node-2', 'node-3', 'node-4'].forEach(function (n) { on(n); card(n); });
      await Promise.all([r.packet('node-2', true, 800), r.packet('node-3', true, 950), r.packet('node-4', true, 1100)]);
      WEB.forEach(function (w) { pod(w[0]).classList.add('is-on'); });
      H.row = el('pre', 'tree-row'); H.why = el('p', 'con-why');
      renderHealth();
      await r.wait(650);

      var b3 = el('div', 'con-block'); var h3 = el('p', 'con-h'); b3.appendChild(h3);
      con.appendChild(b3);
      await r.type(h3, 'swarmexec ui · the tree row', 90);
      b3.appendChild(H.row); b3.appendChild(H.why);
      ['node-2', 'node-3', 'node-4'].forEach(function (n) { off(n); card(n, false); });
      WEB.forEach(function (w) { var p = pod(w[0]); p.classList.remove('is-on'); p.disabled = false; });
      board.classList.add('hp');
      renderHealth(true);
      hint.textContent = 'Your turn: tap web.1, web.2 or web.3 to change its healthcheck. The manager’s answer never changes; the row follows the tree’s own rule.';
      hint.hidden = false;
    };

    scenes.exec = async function (r) {
      setCap(['The client asks the manager which node runs the task, then dials that node’s agent directly on :9443. No SSH to node-3, no helper container per command, and the remote exit code comes back verbatim. There is no agent-to-agent mesh.']);
      await cmd(r, 'swarmexec exec web.2 -- sh');
      on('mgr'); card('mgr'); label('mgr', 'TaskList · NodeInspect');
      await r.packet('mgr', false);
      label('mgr', 'web.2 → node-3, 3f9a2b1c7d4e', true);
      await r.packet('mgr', true);
      await r.wait(450);
      off('mgr'); wires.mgr.classList.add('is-off'); card('mgr', false);
      on('node-3'); card('node-3'); label('node-3', 'gRPC · mTLS · :9443');
      await r.packet('node-3', false, 800);
      pod('web.2').classList.add('is-on');
      label('node-3', 'agent execs via its node’s docker.sock', true);
      await r.packet('node-3', true, 800);
      clearLabels();
      var p1 = say([['c-dim', '/ '], ['c-green', '# ']]);
      await r.wait(250);
      await r.type(p1, 'hostname', 30);
      await r.packet('node-3', false, 420);
      await r.packet('node-3', true, 420);
      say('3f9a2b1c7d4e');
      var p2 = say([['c-dim', '/ '], ['c-green', '# ']]);
      await r.wait(400);
      await r.type(p2, 'cat /etc/hostname', 30);
      await r.packet('node-3', false, 380);
      await r.packet('node-3', true, 380);
      say('3f9a2b1c7d4e');
      var last = say([['c-dim', '/ '], ['c-green', '# ']]);
      last.appendChild(el('span', 'cursor'));
    };

    scenes.forward = async function (r) {
      setCap(['The agent cannot route to db.1’s overlay address — it shares no network with it. So it starts a sidecar from its own image inside db.1’s network namespace, where the port is simply 127.0.0.1:5432. One sidecar per forward, not per connection; your end binds to 127.0.0.1 only.']);
      pod('db.1').classList.add('is-on');
      await cmd(r, 'swarmexec port-forward db 5432');
      pod('db.1').classList.remove('is-on');
      on('mgr'); card('mgr'); label('mgr', 'which node runs db?');
      await r.packet('mgr', false);
      label('mgr', 'db.1 → node-4, c81e0d2a9f3b', true);
      await r.packet('mgr', true);
      await r.wait(350);
      off('mgr'); wires.mgr.classList.add('is-off'); card('mgr', false);
      on('node-4'); card('node-4'); label('node-4', 'PortForward · :9443');
      await r.packet('node-4', false, 800);
      var side = $('.sidecar', board);
      side.hidden = false; spawn(side); draw();
      wires['fwd-in'].classList.add('is-on');
      pod('db.1').classList.add('is-on');
      label('node-4', 'sidecar joins db.1’s netns → 127.0.0.1:5432', true);
      await r.wait(900);
      await r.packet('node-4', true, 700);
      clearLabels();
      say(['forwarding ', ['c-aqua', '127.0.0.1:5432'], ' -> c81e0d2a9f3b:5432 (node-4) ', ['c-dim', '— press Ctrl-C to stop']]);
      await r.wait(700);
      say('# in a second terminal', 'con-note');
      await cmd(r, 'pg_isready -h 127.0.0.1 -p 5432');
      await roundTrip(r);
      say([['c-green', '127.0.0.1:5432 - accepting connections']]);
      /* then the forward just keeps carrying traffic while the stage is on screen */
      if (reduce) return;
      var n = 0;
      while (true) {
        await r.wait(visible && !document.hidden ? 900 : 1500);
        if (!visible || document.hidden) continue;
        await roundTrip(r);
        if (++n === 2) say('# the same sidecar carries every connection of this forward', 'con-note');
      }
    };
    async function roundTrip(r) {
      await r.packet('node-4', false, 520);
      await r.packetInner(false);
      await r.packetInner(true);
      await r.packet('node-4', true, 520);
    }

    scenes.logs = async function (r) {
      setCap(['The per-node agents cannot see a swarm-level replacement, so the client watches for it through the manager it is already talking to, re-resolves the slot and reconnects. It waits about 30 s for a replacement, and stops cleanly if the service is removed.']);
      await cmd(r, 'swarmexec logs -f web.2');
      on('mgr'); card('mgr'); label('mgr', 'resolve web.2');
      await r.packet('mgr', false);
      label('mgr', 'node-3, 3f9a2b1c7d4e', true);
      await r.packet('mgr', true);
      off('mgr'); card('mgr', false); clearLabels();
      on('node-3'); card('node-3'); pod('web.2').classList.add('is-on');
      label('node-3', 'Logs stream · :9443');
      var lines = ['10.0.1.7 - - "GET /healthz HTTP/1.1" 200 2', '10.0.1.9 - - "GET /api/cart HTTP/1.1" 200 812', '10.0.1.7 - - "GET /healthz HTTP/1.1" 200 2'];
      for (var i = 0; i < lines.length; i++) { await r.packet('node-3', true, 480); say(lines[i]); }
      clearLabels();
      await r.wait(400);
      say('# meanwhile: docker service update --image nginx:1.27.2 web', 'con-note');
      await r.wait(500);
      /* the rolling update: the old task stops, its stream ends, a new task starts elsewhere */
      var old = pod('web.2');
      old.classList.remove('is-on'); old.classList.add('is-stopping');
      label('node-3', 'task shutting down', true);
      await r.wait(800);
      old.classList.remove('is-stopping'); old.classList.add('is-gone');
      wires['node-3'].classList.remove('is-on'); wires['node-3'].classList.add('is-warn');
      label('node-3', 'stream ends: container stopped', true);
      await r.wait(800);
      var late = $('.pod-late', board);
      late.hidden = false; spawn(pod('web.2b')); draw();
      await r.wait(600);
      off('node-3'); wires['node-3'].classList.add('is-off'); card('node-3', false);
      on('mgr'); card('mgr'); label('mgr', 're-resolve slot 2');
      await r.packet('mgr', false);
      label('mgr', 'node-2, 7f3a2b1c9e04', true);
      await r.packet('mgr', true);
      off('mgr'); card('mgr', false);
      on('node-2'); card('node-2');
      pod('web.2b').dataset.health = 'healthy'; pod('web.2b').classList.add('is-on');
      label('node-2', 'Logs stream · :9443');
      await r.packet('node-2', false, 700);
      clearLabels();
      say('── container replaced; reconnected to 7f3a2b1c9e04 on node-2 ──', 'c-dim');
      var more = ['10.0.1.7 - - "GET /healthz HTTP/1.1" 200 2', '10.0.1.4 - - "POST /api/cart HTTP/1.1" 201 64', '10.0.1.9 - - "GET /api/cart HTTP/1.1" 200 812'];
      for (var j = 0; j < more.length; j++) { await r.packet('node-2', true, 480); say(more[j]); await r.wait(250); }
    };

    function play(name) {
      current = name;
      var my = ++token;
      reset();
      var r = runner(my);
      r.packetInner = function (back) {
        if (reduce) return Promise.resolve().then(r.live);
        return packetInnerOn(my, back);
      };
      scenes[name](r).catch(function (e) { if (e !== CANCEL) throw e; });
    }
    function packetInnerOn(my, back) {
      /* a runner bound to the scene token, walking the in-card path */
      var p = wires['fwd-in'], len = p.getTotalLength();
      if (!len) return Promise.resolve();
      var c = document.createElementNS(SVGNS, 'circle');
      c.setAttribute('r', 3.5); c.setAttribute('class', 'packet back'); c.setAttribute('cx', -20);
      top.appendChild(c);
      /* the path is two pieces (to the sidecar, then sidecar to db.1); travel the first
         piece and hop onto the second by walking the whole length */
      var d = 650, t0 = null;
      return new Promise(function (res) {
        var done = false;
        function end() { if (!done) { done = true; c.remove(); res(); } }
        setTimeout(end, d + 400);
        function f(ts) {
          if (done) return;
          if (my !== token) return end();
          if (t0 === null) t0 = ts;
          var k = Math.min(1, (ts - t0) / d);
          var pt = p.getPointAtLength(len * (back ? 1 - k : k));
          c.setAttribute('cx', pt.x); c.setAttribute('cy', pt.y);
          if (k < 1) requestAnimationFrame(f); else end();
        }
        requestAnimationFrame(f);
      }).then(function () { if (my !== token) throw CANCEL; });
    }

    function select(name, focus) {
      started = true; /* a choice by the reader always beats the autoplay */
      tabs.forEach(function (t) {
        var on = t.dataset.scene === name;
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.tabIndex = on ? 0 : -1;
        if (on) { panel.setAttribute('aria-labelledby', t.id); if (focus) t.focus(); }
      });
      play(name);
    }

    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { select(t.dataset.scene); });
      t.addEventListener('keydown', function (e) {
        var k = e.key, n = null;
        if (k === 'ArrowRight') n = (i + 1) % tabs.length;
        else if (k === 'ArrowLeft') n = (i - 1 + tabs.length) % tabs.length;
        else if (k === 'Home') n = 0;
        else if (k === 'End') n = tabs.length - 1;
        if (n !== null) { e.preventDefault(); select(tabs[n].dataset.scene, true); }
      });
    });
    $('#replay').addEventListener('click', function () { play(current); });
    $$('[data-goto]').forEach(function (b) {
      b.addEventListener('click', function () {
        stage.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'start' });
        select(b.dataset.goto);
      });
    });

    /* track visibility (the forward loop idles off screen), and autoplay once the stage is on screen */
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(function (es) { visible = es[es.length - 1].isIntersecting; }, { threshold: 0.1 }).observe(board);
    }
    function start() {
      if (started) return;
      started = true;
      bootDone.then(function () { if (token === 0) play(current); });
    }
    onceVisible(board, 0.35, start);
  }

  /* ================================================================ diff toy */
  var DIFF = {
    sx: {
      bump: {
        exit: 1,
        lines: [
          ['d-meta', '--- deployed stack web'], ['d-meta', '+++ web.yml'], ['d-meta', '@@ -1,6 +1,6 @@'],
          [null, ' services:'], [null, '   web:'],
          ['d-del', '-    image: nginx:1.27.1'], ['d-add', '+    image: nginx:1.27.2'],
          [null, '     networks:'], [null, '       - front'], [null, '     deploy:']
        ],
        note: 'One line changed, so one line is reported.'
      },
      same: {
        exit: 0,
        lines: [[null, 'no differences between web.yml and deployed stack web']],
        note: 'In sync, and it says so — with exit 0, so CI can rely on it.'
      }
    },
    text: function (tag) {
      var L = [
        ['d-meta', '--- deployed service specs, as the engine returns them'], ['d-meta', '+++ web.yml, as written'],
        [null, ' services:'],
        ['d-del', '-  web_web:'], ['d-add', '+  web:'],
        ['d-del', '-    image: nginx:1.27.1@sha256:6af79ae5de40…'], ['d-add', '+    image: nginx:' + tag, tag === '1.27.2'],
        [null, '     networks:'],
        ['d-del', '-      - web_front'], ['d-add', '+      - front'],
        [null, '     deploy:'], [null, '       replicas: 3'],
        ['d-del', '-      restart_policy: { condition: any, max_attempts: 0 }'],
        ['d-del', '-      update_config: { parallelism: 1, failure_action: pause, order: stop-first }'],
        ['d-del', '-      rollback_config: { parallelism: 1, failure_action: pause, order: stop-first }'],
        ['d-del', '-    labels: { com.docker.stack.namespace: web }'],
        [null, ' networks:'],
        ['d-del', '-  web_front: { driver: overlay, labels: { com.docker.stack.namespace: web } }'], ['d-add', '+  front: {}']
      ];
      var n = L.filter(function (l) { return l[0] === 'd-del' || l[0] === 'd-add'; }).length;
      return {
        exit: 1, lines: L,
        note: tag === '1.27.2'
          ? n + ' lines differ. One of them is the change you made (highlighted); the rest are the digest, the daemon’s defaults and the stack prefix.'
          : n + ' lines differ, for a stack that is exactly in sync.'
      };
    }
  };

  var diffToy = $('#diff-toy');
  if (diffToy) initDiff();

  function initDiff() {
    var out = $('#diff-out'), ex = $('#diff-exit'), note = $('#diff-note'), title = $('#diff-title');
    var touched = false, busy = 0, lastMode = null;
    function state() {
      var file = $('input[name="diff-file"]:checked', diffToy).value;
      var mode = $('input[name="diff-mode"]:checked', diffToy).value;
      return { file: file, mode: mode, d: mode === 'sx' ? DIFF.sx[file] : DIFF.text(file === 'bump' ? '1.27.2' : '1.27.1') };
    }
    function paint(s, animateIn) {
      out.textContent = '';
      s.d.lines.forEach(function (l, i) {
        var sp = el('span', l[2] ? (l[0] + ' d-hl') : l[0], l[1]);
        if (animateIn) { sp.classList.add('in'); sp.style.setProperty('--k', i); }
        out.appendChild(sp);
      });
      var code = 'exit ' + s.d.exit;
      if (ex.textContent !== code) restartAnim(ex, 'pop');
      ex.textContent = code;
      ex.className = 'exit ' + (s.d.exit ? 'bad' : 'ok') + (ex.classList.contains('pop') ? ' pop' : '');
      title.textContent = s.mode === 'sx' ? 'swarmexec stack diff web.yml' : 'a plain text diff of the same two';
      note.textContent = s.d.note;
      lastMode = s.mode;
    }
    /* text -> swarmexec: the noise collapses away first, so you see what normalising removed */
    function render() {
      var s = state(), my = ++busy;
      if (!reduce && lastMode === 'text' && s.mode === 'sx') {
        var spans = $$('span', out), k = 0;
        spans.forEach(function (sp) {
          if (!sp.classList.contains('d-hl')) { sp.classList.add('out'); sp.style.setProperty('--k', k++); }
        });
        setTimeout(function () { if (my === busy) paint(s, true); }, 420 + k * 22);
      } else paint(s, !reduce && lastMode !== null);
    }
    $$('input', diffToy).forEach(function (i) {
      i.addEventListener('change', function () { touched = true; render(); });
    });
    if (reduce) { paint(state(), false); return; }
    /* start as text, and flip to swarmexec's view once the reader is looking at it */
    $('input[name="diff-mode"][value="text"]', diffToy).checked = true;
    paint(state(), false);
    onceVisible($('.term', diffToy), 0.6, function () {
      setTimeout(function () {
        if (touched) return;
        $('input[name="diff-mode"][value="sx"]', diffToy).checked = true;
        render();
      }, 1400);
    });
  }

  /* ================================================================ scan toy
     Findings, titles and details are the analyzers' own (client/internal/secscan);
     the output layout is writePlanReport() in client/internal/cli/stackimport.go. */
  var scanToy = $('#scan-toy');
  if (scanToy) initScan();

  function initScan() {
    var CHECKED = 'Docker socket mounted in, added Linux capabilities, host network, seccomp / AppArmor disabled, container user, secrets in environment variables, missing resource limits, unpinned image';
    var RULES = {
      cap:    { rule: 'added-capability', sev: 'high', title: 'capability NET_ADMIN added', detail: 'the container is granted CAP_NET_ADMIN — full control of the node’s networking' },
      sock:   { rule: 'docker-socket', sev: 'high', title: 'Docker socket mounted in', detail: 'the Docker socket is bind-mounted at /var/run/docker.sock; anything in this container can start a privileged container on the node, i.e. it is root on the host (read-only does not help — the socket is an API, not a file)' },
      user:   { rule: 'root-user', sev: 'high', title: 'runs as root', detail: 'the service explicitly runs its container as root (User=0)' },
      env:    { rule: 'secret-in-env', sev: 'high', title: 'secret in environment variable', detail: 'the env var DB_PASSWORD holds a literal value in the service spec; use a Docker secret or a *_FILE reference instead' },
      limits: { rule: 'no-resource-limits', sev: 'low', title: 'no resource limits' },
      image:  { rule: 'unpinned-image', sev: 'low', title: 'image not pinned' }
    };
    var sOut = $('#scan-out'), sExit = $('#scan-exit'), sNote = $('#scan-note'), gate = $('#gate'), gLabel = $('#gate-label');
    var seen = null;
    var pad = function (s, n) { while (s.length < n) s += ' '; return s; };
    var lines = $$('.y-line', scanToy);
    lines.forEach(function (b) { b.appendChild(el('span', 'y-hint')); });

    function render() {
      var found = lines.filter(function (b) { return b.getAttribute('aria-pressed') === 'true'; })
        .map(function (b) { return RULES[b.dataset.rule]; });
      var order = { high: 0, medium: 1, low: 2 };
      found.sort(function (a, b) { return (order[a.sev] - order[b.sev]) || (a.rule < b.rule ? -1 : a.rule > b.rule ? 1 : 0); });
      var loud = found.filter(function (f) { return f.sev !== 'low'; });
      var quiet = found.filter(function (f) { return f.sev === 'low'; });
      sOut.textContent = '';
      var t = function (s) { sOut.appendChild(document.createTextNode(s)); };
      if (loud.length) {
        t('1 of 1 service(s) flagged in web.yml:\n\n  api\n');
        loud.forEach(function (f) {
          var row = el('span', seen && seen.indexOf(f.rule) < 0 && !reduce ? 'fresh' : null);
          row.appendChild(document.createTextNode('    '));
          row.appendChild(el('span', 'sev-t ' + f.sev, f.sev));
          row.appendChild(document.createTextNode(pad('', 8 - f.sev.length) + f.title + ' — ' + f.detail + '\n'));
          sOut.appendChild(row);
        });
      } else {
        t('1 service(s) checked, nothing above informational.\nChecked: ' + CHECKED + '.\n');
      }
      var code = loud.length ? 'exit 1' : 'exit 0';
      if (seen && sExit.textContent !== code && !reduce) restartAnim(sExit, 'pop');
      sExit.textContent = code;
      sExit.classList.toggle('bad', !!loud.length); sExit.classList.toggle('ok', !loud.length);
      gate.classList.toggle('is-open', !loud.length); gate.classList.toggle('is-closed', !!loud.length);
      gLabel.textContent = loud.length ? 'a plain deploy stops here and asks' : 'a plain deploy goes ahead';
      var qs = quiet.map(function (f) { return f.title; }).join(', ');
      if (loud.length) sNote.textContent = qs ? 'Also found, not printed because it is informational: ' + qs + '.' : 'Nothing informational on top.';
      else sNote.textContent = qs ? 'Informational only (' + qs + '): true of nearly every service, so it does not stop a deploy.' : 'Nothing at all found.';
      lines.forEach(function (b) {
        var f = RULES[b.dataset.rule], on = b.getAttribute('aria-pressed') === 'true';
        $('.y-hint', b).textContent = on ? '← ' + f.sev + ': ' + f.title : '← tap for the risky form';
      });
      seen = loud.map(function (f) { return f.rule; });
    }
    lines.forEach(function (b) {
      b.addEventListener('click', function () {
        b.setAttribute('aria-pressed', b.getAttribute('aria-pressed') === 'true' ? 'false' : 'true');
        render();
      });
    });
    render();
  }

  /* ================================================================ logs transcript
     Replays the static transcript once it is on screen: two lines, the old stream
     ends, the dim notice sweeps in, the new stream continues. */
  var logsToy = $('#logs-toy');
  if (logsToy && !reduce) {
    var pre = $('pre', logsToy);
    var src = pre.textContent.split('\n');
    onceVisible(pre, 0.6, async function () {
      pre.textContent = '';
      for (var i = 0; i < src.length; i++) {
        var notice = src[i].indexOf('container replaced') >= 0;
        await later(notice ? 1300 : 450);
        pre.appendChild(el('span', 'logs-line ' + (notice ? 'c-dim sweep' : 'in'), src[i]));
      }
    });
  }
})();
