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

  root.Components = Components;
  root.TEXT_SIZES = TEXT_SIZES;
})(typeof window !== "undefined" ? window : globalThis);
