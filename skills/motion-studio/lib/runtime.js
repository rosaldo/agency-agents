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
  function paint(layer, bg) {
    var first = bg.color || bg.gradient[0];
    layer.style.background = bg.color || "linear-gradient(180deg, " + bg.gradient.join(", ") + ")";
    layer.style.color = luminance(first) > 0.55 ? "#111418" : "#f5f5f2";
  }

  var state = null;

  function load(scene, size) {
    var stage = document.getElementById("stage");
    var fps = scene.fps || 30, bpm = scene.bpm;
    stage.style.width = size.width + "px";     // the frame itself, from scene.json
    stage.style.height = size.height + "px";
    stage.textContent = "";
    var beatOf = function (t) { return Math.round(t * bpm / 60); };
    var scenes = scene.scenes.map(function (sc, si) {
      var layer = document.createElement("section");
      layer.className = "ms-scene";
      layer.dataset.scene = sc.id || String(si);
      paint(layer, sc.background || scene.background || { color: "#111418" });
      stage.appendChild(layer);
      var startBeat = beatOf(sc.start), stagger = sc.stagger || 3;
      var first = M.beatFrame(startBeat + (sc.enter_beat || 0), bpm, fps);
      var elements = sc.elements.map(function (el, i) {
        var comp = C[el.component], st = comp.mount(el);
        st.root.classList.add("ms-el", "ms-align-" + (el.align || "start"));
        if (el.caps) st.root.classList.add("ms-caps");
        if (el.box) {
          st.root.classList.add("has-box");
          st.root.style.left = el.box[0] * 100 + "%";
          st.root.style.top = el.box[1] * 100 + "%";
          st.root.style.width = el.box[2] * 100 + "%";
          st.root.style.height = el.box[3] * 100 + "%";
        }
        layer.appendChild(st.root);
        return { el: el, comp: comp, state: st, enter: first + i * stagger, spring: el.spring || comp.spring };
      });
      return { layer: layer, start: M.beatFrame(startBeat, bpm, fps),
               end: M.beatFrame(beatOf(sc.end), bpm, fps), stagger: stagger, elements: elements };
    });
    state = { fps: fps, scenes: scenes, frames: Math.round(scene.duration * fps) };
    return state.frames;
  }

  function renderFrame(frame) {
    state.scenes.forEach(function (sc) {
      var live = frame >= sc.start && frame < sc.end;
      sc.layer.classList.toggle("is-live", live);
      if (!live) return;
      sc.elements.forEach(function (e) {
        e.comp.update(e.state, {
          stagger: sc.stagger,
          progress: function (delay, zeta) {
            return M.spring((frame - e.enter - (delay || 0)) / state.fps, zeta || e.spring.zeta, e.spring.omega);
          }
        });
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
        var walk = document.createTreeWalker(e.state.root, NodeFilter.SHOW_TEXT), node, u = null;
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
