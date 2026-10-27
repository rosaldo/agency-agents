// components.js — the component catalog (references/components.md). Each
// component mounts its DOM once and then sets styles from the spring progress
// the runtime hands it for the current frame; nothing reads a clock.
(function (root) {
  "use strict";

  // Mirror of TEXT_SIZES in scripts/scene_schema.py: fraction of the short side.
  var TEXT_SIZES = { xs: 0.028, s: 0.036, m: 0.048, l: 0.072, xl: 0.11 };

  function clamp01(v) { return v < 0 ? 0 : v > 1 ? 1 : v; }
  function size(token) { return TEXT_SIZES[token] * 100 + "cqmin"; }
  function make(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }
  // rise: fade in while travelling up by `dist` (fraction of the short side).
  function rise(n, p, dist) {
    n.style.opacity = clamp01(p * 1.6);
    n.style.transform = "translateY(" + (1 - p) * dist * 100 + "cqmin)";
  }
  function slide(n, p, dist) {
    n.style.opacity = clamp01(p * 1.6);
    n.style.transform = "translateX(" + (1 - p) * -dist * 100 + "cqmin)";
  }

  var Components = {};

  // ctx.progress(delayFrames[, zeta]) is the spring value for this element,
  // started delayFrames after its entrance; ctx.stagger is the scene stagger.
  Components["title"] = {
    spring: { zeta: 0.7, omega: 14 },
    mount: function (el) {
      var box = make("div");
      var h = make("h1", "ms-title", el.text);
      h.style.fontSize = size(el.size || "xl");
      box.appendChild(h);
      var sub = null;
      if (el.subtitle) {
        sub = make("p", "ms-subtitle", el.subtitle);
        sub.style.fontSize = size("m");
        box.appendChild(sub);
      }
      return { root: box, title: h, subtitle: sub };
    },
    update: function (s, ctx) {
      rise(s.title, ctx.progress(0), 0.05);
      if (s.subtitle) rise(s.subtitle, ctx.progress(ctx.stagger), 0.03);
    }
  };

  Components["text"] = {
    spring: { zeta: 0.8, omega: 12 },
    mount: function (el) {
      var p = make("p", "ms-text", el.text);
      p.style.fontSize = size(el.size || "m");
      return { root: p };
    },
    update: function (s, ctx) { rise(s.root, ctx.progress(0), 0.035); }
  };

  Components["counter"] = {
    spring: { zeta: 0.75, omega: 12 },
    mount: function (el) {
      var n = make("div", "ms-counter");
      n.style.fontSize = size(el.size || "xl");
      return { root: n, el: el };
    },
    update: function (s, ctx) {
      var el = s.el, digits = el.decimals || 0;
      // the value settles without overshoot so it never shows past `to`
      var v = el.from + (el.to - el.from) * clamp01(ctx.progress(0, 1));
      s.root.textContent = (el.prefix || "") + v.toFixed(digits) + (el.suffix || "");
      rise(s.root, ctx.progress(0), 0.04);
    }
  };

  Components["list"] = {
    spring: { zeta: 0.7, omega: 16 },
    mount: function (el) {
      var ul = make("ul", "ms-list"), items = [];
      ul.style.fontSize = size(el.size || "m");
      el.items.forEach(function (text) { var li = make("li", null, text); ul.appendChild(li); items.push(li); });
      return { root: ul, items: items };
    },
    update: function (s, ctx) {
      s.items.forEach(function (li, i) { slide(li, ctx.progress(i * ctx.stagger), 0.05); });
    }
  };

  Components["asset-card"] = {
    spring: { zeta: 0.6, omega: 12 },
    mount: function (el) {
      var fig = make("figure", "ms-card");
      fig.style.margin = "0";
      var img = make("img");
      img.src = el.asset;
      img.alt = el.caption || "";
      fig.appendChild(img);
      var cap = null;
      if (el.caption) {
        cap = make("figcaption", null, el.caption);
        cap.style.fontSize = size(el.size || "s");
        fig.appendChild(cap);
      }
      return { root: fig, img: img, caption: cap };
    },
    update: function (s, ctx) {
      var p = ctx.progress(0);
      s.img.style.opacity = clamp01(p * 1.4);
      s.img.style.transform = "scale(" + (0.9 + 0.1 * p) + ")";
      if (s.caption) rise(s.caption, ctx.progress(ctx.stagger), 0.03);
    }
  };

  Components["bar-chart"] = {
    spring: { zeta: 0.8, omega: 10 },
    mount: function (el) {
      var wrap = make("div", "ms-bars"), rows = [];
      var max = Math.max.apply(null, el.bars.map(function (b) { return b.value; })) || 1;
      wrap.style.fontSize = size(el.size || "s");
      el.bars.forEach(function (b) {
        var row = make("div"), label = make("div", "ms-bar-label");
        label.appendChild(make("span", null, b.label));
        label.appendChild(make("span", null, b.value));
        var track = make("div", "ms-bar-track"), fill = make("div", "ms-bar-fill");
        track.appendChild(fill);
        row.appendChild(label);
        row.appendChild(track);
        wrap.appendChild(row);
        rows.push({ row: row, fill: fill, share: b.value / max });
      });
      return { root: wrap, rows: rows };
    },
    update: function (s, ctx) {
      s.rows.forEach(function (r, i) {
        // bars settle without overshoot so the longest never leaves its track
        var p = ctx.progress(i * ctx.stagger, 1);
        r.row.style.opacity = clamp01(ctx.progress(i * ctx.stagger) * 2);
        r.fill.style.transform = "scaleX(" + clamp01(p) * r.share + ")";
      });
    }
  };


  // ---- graphic components: SVG and shapes drawn in code, no images ----
  var S = root.Shapes, NS = "http://www.w3.org/2000/svg";
  // Mirror of GRAPHIC_SIZES in scripts/scene_schema.py: height of a graphic in
  // the default column layout, as a fraction of the short side. A `box` overrides it.
  var GRAPHIC_SIZES = { xs: 0.1, s: 0.16, m: 0.24, l: 0.34, xl: 0.46 };
  var ASPECT = { start: "xMinYMid meet", center: "xMidYMid meet", end: "xMaxYMid meet" };

  function svgEl(tag, attrs) {
    var n = document.createElementNS(NS, tag);
    Object.keys(attrs || {}).forEach(function (k) { n.setAttribute(k, attrs[k]); });
    return n;
  }
  // graphic(el, viewBox, aspect) — the element's root div (sized by `size`
  // unless it has a box) holding one SVG that fills it.
  function graphic(el, viewBox, aspect) {
    var box = make("div", "ms-graphic");
    if (!el.box) box.style.height = GRAPHIC_SIZES[el.size || "m"] * 100 + "cqmin";
    var svg = svgEl("svg", { viewBox: viewBox, preserveAspectRatio: aspect || ASPECT[el.align || "start"] });
    box.appendChild(svg);
    return { root: box, svg: svg };
  }
  function colorOf(el, env, k) { return el.color || env.palette[(env.index + (k || 0)) % env.palette.length]; }
  // a stroke traced from 0 to p of its length (pathLength 1), p clamped so a
  // spring overshoot never wraps the dash
  function trace(path, p) { path.style.strokeDashoffset = String(1 - clamp01(p)); }
  function traced(d, color, width) {
    return svgEl("path", { d: d, pathLength: "1", fill: "none", stroke: color, "stroke-width": width,
                           "stroke-linecap": "round", "stroke-linejoin": "round", "stroke-dasharray": "1 2" });
  }
  var FROM = { left: [-1, 0], right: [1, 0], top: [0, -1], bottom: [0, 1] };
  // stretch(s) — once the element is laid out, map its 100 x 100 strokes onto
  // the box in pixels, so a stroke never thins or thickens with the box shape
  function stretch(s) {
    if (s.fitted) return;
    var w = s.root.offsetWidth, h = s.root.offsetHeight;
    s.svg.setAttribute("viewBox", "0 0 " + w + " " + h);
    s.paths.forEach(function (q) { q.node.setAttribute("d", S.fit(q.d, w / 100, h / 100)); });
    s.fitted = true;
  }

  Components["shape"] = {
    spring: { zeta: 0.55, omega: 11 },
    mount: function (el, env) {
      var name = el.shape || "circle", stretch = name === "rect" && el.box;
      var g = graphic(el, "-1 -1 2 2", stretch ? "none" : null);
      var path = svgEl("path", { fill: colorOf(el, env) });
      if (el.opacity !== undefined) path.setAttribute("fill-opacity", el.opacity);
      g.svg.appendChild(path);
      return { root: g.root, svg: g.svg, path: path, el: el };
    },
    update: function (s, ctx) {
      var el = s.el, p = ctx.progress(0), dir = FROM[el.from] || [0, 0];
      s.path.setAttribute("d", S.pathOf(S.outline(el.shape || "circle", ctx.t)));
      // slides in from a side, or pops in place; spins from `spin` degrees to rest
      s.root.style.transform = "translate(" + (1 - p) * dir[0] * 60 + "cqmin, " + (1 - p) * dir[1] * 60 + "cqmin)";
      s.path.style.transform = "rotate(" + (1 - p) * (el.spin || 0) + "deg) scale(" + (dir[0] || dir[1] ? 1 : Math.max(0, p)) + ")";
      s.root.style.opacity = clamp01(p * 3);
    }
  };

  Components["draw"] = {
    spring: { zeta: 1, omega: 9 },
    mount: function (el, env) {
      var g = graphic(el, "0 0 100 100", "none"), d = S.STROKES[el.stroke];
      var path = traced(d, colorOf(el, env), "1.1cqmin");
      g.svg.appendChild(path);
      return { root: g.root, svg: g.svg, path: path, paths: [{ node: path, d: d }] };
    },
    update: function (s, ctx) {
      stretch(s);
      trace(s.path, ctx.progress(0, 1));
    }
  };

  Components["morph"] = {
    spring: { zeta: 0.6, omega: 10 },
    mount: function (el, env) {
      var g = graphic(el, "-1 -1 2 2");
      var path = svgEl("path", { fill: colorOf(el, env) });
      g.svg.appendChild(path);
      return { root: g.root, svg: g.svg, path: path, el: el };
    },
    update: function (s, ctx) {
      var el = s.el, pop = ctx.progress(0);
      // the morph starts `after` beats past the entrance (default 2)
      var m = ctx.progress(ctx.beat * (el.after === undefined ? 2 : el.after));
      s.path.setAttribute("d", S.pathOf(S.lerpPoints(S.outline(el.from, ctx.t), S.outline(el.to, ctx.t), m)));
      s.path.style.transform = "scale(" + Math.max(0, pop) + ") rotate(" + (1 - clamp01(m)) * -20 + "deg)";
    }
  };

  Components["icon"] = {
    spring: { zeta: 0.5, omega: 13 },
    mount: function (el, env) {
      var g = graphic(el, "-2 -2 28 28"), color = colorOf(el, env), disc = null;
      if (el.disc) {
        disc = svgEl("circle", { cx: 12, cy: 12, r: 13, fill: color });
        g.svg.appendChild(disc);
      }
      var path = traced(S.ICONS[el.icon], el.disc ? env.paper : color, "2");
      g.svg.appendChild(path);
      return { root: g.root, svg: g.svg, disc: disc, path: path, el: el };
    },
    update: function (s, ctx) {
      var pop = ctx.progress(0);
      if (s.disc) s.disc.style.transform = "scale(" + Math.max(0, pop) + ")";
      if (s.el.mode === "pop") {
        trace(s.path, 1);
        s.path.style.transform = "scale(" + Math.max(0, ctx.progress(s.disc ? ctx.stagger : 0)) + ")";
      } else {
        trace(s.path, ctx.progress(s.disc ? ctx.stagger : 0, 1)); // the stroke draws itself in
      }
    }
  };

  Components["ring"] = {
    spring: { zeta: 1, omega: 7 },
    mount: function (el, env) {
      var g = graphic(el, "-1.1 -1.1 2.2 2.2"), color = colorOf(el, env);
      var track = svgEl("circle", { r: 0.9, fill: "none", stroke: "currentColor", "stroke-opacity": 0.14, "stroke-width": 0.16 });
      var arc = svgEl("circle", { r: 0.9, fill: "none", stroke: color, "stroke-width": 0.16, "stroke-linecap": "round",
                                   pathLength: "1", "stroke-dasharray": "1 2", transform: "rotate(-90)" });
      var label = svgEl("text", { "text-anchor": "middle", "dominant-baseline": "central", fill: "currentColor",
                                  "font-size": 0.5, "font-weight": 800 });
      g.svg.appendChild(track);
      g.svg.appendChild(arc);
      g.svg.appendChild(label);
      return { root: g.root, svg: g.svg, arc: arc, label: label, el: el };
    },
    update: function (s, ctx) {
      var el = s.el, p = clamp01(ctx.progress(0, 1)), v = el.value * p;
      s.arc.style.strokeDashoffset = String(1 - v / 100);
      s.label.textContent = (el.prefix || "") + v.toFixed(el.decimals || 0) + (el.suffix === undefined ? "%" : el.suffix);
      s.svg.style.opacity = clamp01(ctx.progress(0) * 3);
    }
  };

  Components["line-chart"] = {
    spring: { zeta: 1, omega: 6 },
    mount: function (el, env) {
      var g = graphic(el, "0 0 100 100", "none"), color = colorOf(el, env);
      var lo = Math.min.apply(null, el.points), hi = Math.max.apply(null, el.points), span = hi - lo || 1;
      var pts = el.points.map(function (v, i) { return [4 + 92 * i / (el.points.length - 1), 90 - 76 * (v - lo) / span]; });
      var d = "M" + pts.map(function (q) { return q[0] + " " + q[1]; }).join("L");
      var area = svgEl("path", { d: d + "L96 100L4 100Z", fill: color, "fill-opacity": 0.18 });
      var line = traced(d, color, "1.1cqmin");
      g.svg.appendChild(area);
      g.svg.appendChild(line);
      // the end dot is HTML so it stays round whatever the box shape
      var dot = make("div", "ms-dot"), last = pts[pts.length - 1];
      dot.style.left = last[0] + "%";
      dot.style.top = last[1] + "%";
      dot.style.background = color;
      g.root.appendChild(dot);
      return { root: g.root, svg: g.svg, area: area, line: line, dot: dot,
               paths: [{ node: area, d: d + "L96 100L4 100Z" }, { node: line, d: d }] };
    },
    update: function (s, ctx) {
      stretch(s);
      var p = ctx.progress(0, 1);
      trace(s.line, p);
      s.area.style.opacity = clamp01(p * p);
      s.dot.style.transform = "translate(-50%, -50%) scale(" + Math.max(0, ctx.progress(ctx.beat * 2, 0.45)) + ")";
    }
  };

  // burst — seeded confetti from the centre of its box (the whole frame without
  // one). Each particle flies out on a critically damped spring, falls under a
  // closed-form gravity and fades, so frame n is the same on every render.
  Components["burst"] = {
    spring: { zeta: 1, omega: 6 },
    mount: function (el, env) {
      var wrap = make("div", "ms-burst"), rand = S.rng(el.seed || 1), parts = [];
      for (var i = 0; i < (el.count || 36); i++) {
        var n = make("div", "ms-particle"), round = rand() < 0.4, side = 0.9 + rand() * 1.4;
        n.style.width = side + "cqmin";
        n.style.height = (round ? side : side * 0.45) + "cqmin";
        n.style.borderRadius = round ? "50%" : "0.2cqmin";
        n.style.background = env.palette[i % env.palette.length];
        wrap.appendChild(n);
        parts.push({ n: n, a: rand() * 2 * Math.PI, reach: 14 + rand() * 30, spin: (rand() - 0.5) * 900,
                     life: 1.1 + rand() * 0.9, fall: 8 + rand() * 10 });
      }
      return { root: wrap, parts: parts };
    },
    update: function (s, ctx) {
      var out = ctx.progress(0), t = Math.max(0, ctx.since);
      s.parts.forEach(function (q) {
        var x = Math.cos(q.a) * q.reach * out, y = Math.sin(q.a) * q.reach * out + q.fall * t * t;
        q.n.style.transform = "translate(-50%, -50%) translate(" + x + "cqmin, " + y + "cqmin) rotate(" + q.spin * t + "deg)";
        q.n.style.opacity = t > 0 ? clamp01((q.life - t) * 2.5) : 0;
      });
    }
  };

  root.GRAPHIC_SIZES = GRAPHIC_SIZES;
  root.Components = Components;
  root.TEXT_SIZES = TEXT_SIZES;
})(typeof window !== "undefined" ? window : globalThis);
