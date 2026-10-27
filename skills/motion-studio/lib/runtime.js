// runtime.js — builds the stage from a validated scene.json and draws any
// frame on demand: MotionStudio.load(scene, size), then renderFrame(n).
// The frame number is the only input, so frame n is identical on every render.
(function (root) {
  "use strict";
  var M = root.Motion, C = root.Components;

  function luminance(hex) {
    var n = parseInt(hex.slice(1), 16);
    return (0.2126 * (n >> 16 & 255) + 0.7152 * (n >> 8 & 255) + 0.0722 * (n & 255)) / 255;
  }
  // paint() returns the scene's base background; light() lays the wandering
  // glow over it each frame (a dark glow on light backgrounds).
  function paint(layer, bg) {
    var light = luminance(bg.color || bg.gradient[0]) > 0.55;
    layer.style.color = light ? "#111418" : "#f5f5f2";
    return { base: bg.color || "linear-gradient(180deg, " + bg.gradient.join(", ") + ")",
             tint: light ? "rgba(0, 0, 0, 0.06)" : "rgba(255, 255, 255, 0.08)",
             paper: bg.color || bg.gradient[0] };
  }
  function light(sc, t) {
    var g = M.glow(t);
    sc.layer.style.background = "radial-gradient(circle at " + g.x + "% " + g.y + "%, " + sc.paint.tint +
      ", transparent 60%), " + sc.paint.base;
  }

  // Default palette for graphics when scene.json gives none: warm, mint, blue, coral.
  var PALETTE = ["#ffb547", "#4cc9a6", "#6c8cff", "#ff6b6b"];
  var grainTile = null;

  // grain() — a seeded noise tile laid over every background: it breaks the
  // gradient into fine dither, so the encoder shows no bands, and never moves.
  function grain() {
    var c = document.createElement("canvas"), side = 192, g = c.getContext("2d");
    c.width = c.height = side;
    var img = g.createImageData(side, side), rand = root.Shapes.rng(7);
    for (var i = 0; i < img.data.length; i += 4) {
      var v = rand() < 0.5 ? 0 : 255;
      img.data[i] = img.data[i + 1] = img.data[i + 2] = v;
      img.data[i + 3] = Math.round(rand() * 16);
    }
    g.putImageData(img, 0, 0);
    return c.toDataURL("image/png");
  }

  // decorate() — the layered background: soft shapes in the palette at three
  // depths (placed by a seed per scene) plus the grain. Returns what drift() moves.
  function decorate(layer, bg, palette, si) {
    var back = document.createElement("div"), rand = root.Shapes.rng(101 + si * 7919), shapes = [];
    back.className = "ms-backdrop";
    for (var i = 0; i < (bg.shapes === undefined ? 3 : bg.shapes); i++) {
      var n = document.createElement("div"), depth = 1 + i % 3, side = 28 + rand() * 30;
      n.className = "ms-orb";
      n.style.width = n.style.height = side + "cqmin";
      n.style.left = rand() * 100 + "%";
      n.style.top = rand() * 100 + "%";
      n.style.background = palette[(si + i) % palette.length];
      n.style.borderRadius = i % 2 ? "38% 62% 55% 45% / 45% 40% 60% 55%" : "50%";
      back.appendChild(n);
      shapes.push({ n: n, depth: depth, phase: rand() * 6.28 });
    }
    if (bg.grain !== false) {
      var g = document.createElement("div");
      g.className = "ms-grain";
      g.style.backgroundImage = "url(" + grainTile + ")";
      back.appendChild(g);
    }
    layer.appendChild(back);
    return shapes;
  }
  // drift(shapes, t) — nearer shapes (higher depth) move further: parallax.
  function drift(shapes, t) {
    shapes.forEach(function (o) {
      var a = 2 * Math.PI * t / 9 + o.phase;
      o.n.style.transform = "translate(-50%, -50%) translate(" + 2.2 * o.depth * Math.sin(a) + "cqmin, " +
        1.6 * o.depth * Math.cos(a * 0.8) + "cqmin) rotate(" + 6 * o.depth * Math.sin(a * 0.5) + "deg)";
    });
  }
  // unmask(e, q) — open the element's mask by q (0 shut, 1 open)
  function unmask(e, q) {
    q = Math.min(1, Math.max(0, q));
    var mask = e.node, kind = e.el.reveal;
    if (kind === "slide") e.state.root.style.translate = "0 " + (1 - q) * 110 + "%";
    if (q >= 1) {
      mask.style.clipPath = "none"; // fully open: nothing clipped, not even an overshoot
    } else if (kind === "slide") {
      mask.style.clipPath = "inset(0)";
    } else if (kind === "wipe") {
      mask.style.clipPath = "inset(-5% " + (1 - q) * 105 + "% -5% -5%)";
    } else {
      mask.style.clipPath = "circle(" + q * 75 + "% at 50% 50%)";
    }
  }

  var state = null;

  function load(scene, size) {
    var stage = document.getElementById("stage");
    var fps = scene.fps || 30, bpm = scene.bpm;
    stage.style.width = size.width + "px";     // the frame itself, from scene.json
    stage.style.height = size.height + "px";
    stage.textContent = "";
    var beatOf = function (t) { return Math.round(t * bpm / 60); };
    var palette = scene.palette || PALETTE;
    grainTile = grainTile || grain();
    var scenes = scene.scenes.map(function (sc, si) {
      var layer = document.createElement("section");
      layer.className = "ms-scene";
      layer.dataset.scene = sc.id || String(si);
      var bg = sc.background || scene.background || { color: "#111418" };
      var look = paint(layer, bg);
      look.deco = decorate(layer, bg, palette, si);
      stage.appendChild(layer);
      var startBeat = beatOf(sc.start), stagger = sc.stagger || 3;
      var first = M.beatFrame(startBeat + (sc.enter_beat || 0), bpm, fps);
      var elements = sc.elements.map(function (el, i) {
        var comp = C[el.component];
        var st = comp.mount(el, { palette: palette, index: i, paper: look.paper });
        // a reveal wraps the element in a mask that the runtime opens; the
        // component keeps animating its own nodes inside it
        var node = st.root;
        if (el.reveal) {
          node = document.createElement("div");
          node.className = "ms-mask";
          node.appendChild(st.root);
        }
        node.classList.add("ms-el", "ms-align-" + (el.align || "start"));
        if (el.caps) node.classList.add("ms-caps");
        if (el.box) {
          node.classList.add("has-box");
          node.style.left = el.box[0] * 100 + "%";
          node.style.top = el.box[1] * 100 + "%";
          node.style.width = el.box[2] * 100 + "%";
          node.style.height = el.box[3] * 100 + "%";
        }
        layer.appendChild(node);
        return { el: el, index: i, comp: comp, state: st, node: node, enter: first + i * stagger,
                 spring: el.spring || comp.spring };
      });
      // a declared hold freezes the scene on purpose: its last `hold` beats repeat one frame
      var still = sc.hold ? M.beatFrame(beatOf(sc.end) - sc.hold, bpm, fps) : Infinity;
      return { layer: layer, paint: look, still: still, start: M.beatFrame(startBeat, bpm, fps),
               end: M.beatFrame(beatOf(sc.end), bpm, fps), stagger: stagger, elements: elements };
    });
    state = { fps: fps, beat: M.framesPerBeat(bpm, fps), scenes: scenes, frames: Math.round(scene.duration * fps) };
    return state.frames;
  }

  function renderFrame(frame) {
    state.scenes.forEach(function (sc) {
      var live = frame >= sc.start && frame < sc.end;
      sc.layer.classList.toggle("is-live", live);
      if (!live) return;
      var f = Math.min(frame, sc.still), t = (f - sc.start) / state.fps;
      light(sc, t);
      drift(sc.paint.deco, t);
      sc.elements.forEach(function (e) {
        var progress = function (delay, zeta) {
          return M.spring((f - e.enter - (delay || 0)) / state.fps, zeta || e.spring.zeta, e.spring.omega);
        };
        e.comp.update(e.state, { stagger: sc.stagger, beat: state.beat, t: t, since: (f - e.enter) / state.fps,
                                 progress: progress });
        if (e.el.reveal) unmask(e, progress(0, 1));
        // idle motion uses the individual translate/scale properties, so it
        // composes with the transform each component sets for its entrance
        var idle = M.idle((f - e.enter) / state.fps, e.index), root = e.node;
        root.style.translate = idle.x + "cqmin " + idle.y + "cqmin";
        root.style.scale = String(idle.s);
      });
    });
  }

  // ready() resolves once every image is decoded, so frame 0 is never half-loaded.
  function ready() {
    var imgs = Array.prototype.slice.call(document.querySelectorAll("#stage img"));
    return Promise.all(imgs.map(function (img) { return img.decode().catch(function () {}); }));
  }

  // textBoxes() — boxes of the text drawn by the live scene's elements, as
  // fractions of the frame, for the safe-area check. Measures the glyphs, not
  // the element, so an image or a bar track never counts as text.
  function textBoxes() {
    var stage = document.getElementById("stage").getBoundingClientRect(), out = [];
    state.scenes.forEach(function (sc) {
      if (!sc.layer.classList.contains("is-live")) return;
      sc.elements.forEach(function (e, i) {
        var walk = document.createTreeWalker(e.node, NodeFilter.SHOW_TEXT), node, u = null;
        while ((node = walk.nextNode())) {
          if (!node.nodeValue.trim()) continue;
          var range = document.createRange();
          range.selectNodeContents(node);
          var r = range.getBoundingClientRect();
          u = u ? { l: Math.min(u.l, r.left), t: Math.min(u.t, r.top), r: Math.max(u.r, r.right), b: Math.max(u.b, r.bottom) }
                : { l: r.left, t: r.top, r: r.right, b: r.bottom };
        }
        if (!u) return;
        out.push({ element: i, box: [(u.l - stage.left) / stage.width, (u.t - stage.top) / stage.height,
          (u.r - u.l) / stage.width, (u.b - u.t) / stage.height] });
      });
    });
    return out;
  }

  // scenes() — each scene's id and frame range [start, end), as cut on the beat grid.
  function scenes() {
    return state.scenes.map(function (sc) { return { id: sc.layer.dataset.scene, start: sc.start, end: sc.end }; });
  }

  root.MotionStudio = { load: load, renderFrame: renderFrame, ready: ready, textBoxes: textBoxes, scenes: scenes };
})(window);
