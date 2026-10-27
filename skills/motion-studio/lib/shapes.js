// shapes.js — geometry drawn in code: outlines sampled as points (so any two
// can morph), the built-in icon set, the stroke paths `draw` traces, and a
// seeded random generator. Pure functions only; motion lives in components.js.
(function (root) {
  "use strict";
  var TAU = 2 * Math.PI;
  var N = 96; // points per outline: every shape has the same count, so any pair morphs

  function polar(r) {
    var pts = [];
    for (var i = 0; i < N; i++) {
      var a = -Math.PI / 2 + TAU * i / N; // start at the top, go clockwise
      var k = r(a);
      pts.push([k * Math.cos(a), k * Math.sin(a)]);
    }
    return pts;
  }
  // resample a closed polygon to N points evenly spaced along its perimeter
  function polygon(vs) {
    var segs = [], total = 0;
    vs.forEach(function (v, i) {
      var w = vs[(i + 1) % vs.length], len = Math.hypot(w[0] - v[0], w[1] - v[1]);
      segs.push({ a: v, b: w, len: len });
      total += len;
    });
    var pts = [], s = 0, j = 0, acc = 0;
    for (var i = 0; i < N; i++) {
      s = total * i / N;
      while (acc + segs[j].len < s) { acc += segs[j].len; j++; }
      var u = (s - acc) / segs[j].len, g = segs[j];
      pts.push([g.a[0] + (g.b[0] - g.a[0]) * u, g.a[1] + (g.b[1] - g.a[1]) * u]);
    }
    return pts;
  }
  function regular(n, inner) {
    var vs = [], m = inner ? n * 2 : n;
    for (var i = 0; i < m; i++) {
      var a = -Math.PI / 2 + TAU * i / m, r = inner && i % 2 ? inner : 1;
      vs.push([r * Math.cos(a), r * Math.sin(a)]);
    }
    return polygon(vs);
  }

  // Outlines in [-1, 1]; `t` (seconds) only moves the blob, which keeps wobbling.
  var OUTLINES = {
    circle: function () { return polar(function () { return 1; }); },
    blob: function (t) {
      return polar(function (a) {
        return 0.86 + 0.08 * Math.sin(3 * a + 0.9 * t) + 0.05 * Math.sin(5 * a - 1.3 * t + 1) + 0.03 * Math.cos(2 * a + 0.5 * t);
      });
    },
    // a superellipse: in a square box a squircle, stretched over a wide box a rounded rect
    rect: function () {
      return polar(function (a) { return Math.pow(Math.pow(Math.abs(Math.cos(a)), 6) + Math.pow(Math.abs(Math.sin(a)), 6), -1 / 6); });
    },
    triangle: function () { return regular(3).map(function (p) { return [p[0], p[1] + 0.2]; }); },
    hexagon: function () { return regular(6); },
    star: function () { return regular(5, 0.45); },
    heart: function () {
      var pts = [];
      for (var i = 0; i < N; i++) {
        var a = Math.PI + TAU * i / N; // starts at the top notch
        var x = 16 * Math.pow(Math.sin(a), 3);
        var y = -(13 * Math.cos(a) - 5 * Math.cos(2 * a) - 2 * Math.cos(3 * a) - Math.cos(4 * a));
        pts.push([x / 17, y / 17 + 0.1]);
      }
      return pts;
    }
  };

  function outline(name, t) { return OUTLINES[name](t || 0); }
  function lerpPoints(a, b, p) {
    return a.map(function (q, i) { return [q[0] + (b[i][0] - q[0]) * p, q[1] + (b[i][1] - q[1]) * p]; });
  }
  function pathOf(pts) {
    return "M" + pts.map(function (q) { return q[0].toFixed(4) + " " + q[1].toFixed(4); }).join("L") + "Z";
  }

  // Icons: strokes on a 24-unit grid, drawn in one pass along the path.
  var ICONS = {
    check: "M5 12.5L10 17.5L19 7",
    clock: "M12 3A9 9 0 1 1 11.99 3M12 7V12L15.5 14.5",
    chart: "M4 4V20H20M8.5 16V12M12.5 16V8M16.5 16V10.5",
    cart: "M3 4.5H6L8.5 15H18L20 7.5H7M9.5 19.5A1.2 1.2 0 1 1 9.49 19.5M17 19.5A1.2 1.2 0 1 1 16.99 19.5",
    star: "M12 3L14.6 9L21 9.3L16 13.4L17.8 20L12 16.3L6.2 20L8 13.4L3 9.3L9.4 9Z",
    heart: "M12 20C6 15.5 3 12.5 3 8.8C3 6.2 5 4.5 7.3 4.5C9.3 4.5 11 5.8 12 7.6C13 5.8 14.7 4.5 16.7 4.5C19 4.5 21 6.2 21 8.8C21 12.5 18 15.5 12 20Z",
    arrow: "M4 12H19M13 6L19 12L13 18",
    user: "M16 8A4 4 0 1 1 8 8A4 4 0 1 1 16 8M4.5 20C5.5 15.5 8.5 13.5 12 13.5C15.5 13.5 18.5 15.5 19.5 20",
    bolt: "M13 2.5L5 13.5H11.5L10.5 21.5L19 10H12.5Z",
    gift: "M4 10H20V20H4ZM3 7H21V10H3ZM12 7V20M12 7C10 3.5 6.5 4 7.5 6.2M12 7C14 3.5 17.5 4 16.5 6.2",
    bell: "M6 17V11C6 7.5 8.5 5 12 5C15.5 5 18 7.5 18 11V17L19.5 18.5H4.5L6 17M10 21H14",
    calendar: "M4 6H20V20H4ZM4 10H20M8 3.5V7.5M16 3.5V7.5"
  };

  // Strokes `draw` traces in a 100 x 100 box stretched over the element's box.
  var STROKES = {
    underline: "M3 62C30 52 70 50 97 58",
    arrow: "M4 50H94M80 34L95 50L80 66",
    connector: "M4 12C55 12 45 88 96 88",
    outline: "M10 3H90Q97 3 97 10V90Q97 97 90 97H10Q3 97 3 90V10Q3 3 10 3Z",
    circle: "M50 6C80 4 97 25 96 52C95 80 72 96 46 95C20 94 3 74 4 48C5 22 26 7 56 9",
    wave: "M3 50C12 30 21 30 30 50S48 70 57 50S75 30 84 50S93 66 97 56"
  };

  // fit(d, kx, ky) — scale an absolute path (M L C S Q H V Z only) by kx, ky,
  // so a stroke drawn in a 100 x 100 box keeps an even width in any box shape
  function fit(d, kx, ky) {
    return d.replace(/([MLCSQHVZ])([^MLCSQHVZ]*)/g, function (all, cmd, args) {
      var n = 0;
      return cmd + args.replace(/-?[\d.]+/g, function (v) {
        var k = cmd === "H" ? kx : cmd === "V" ? ky : n++ % 2 ? ky : kx;
        return (parseFloat(v) * k).toFixed(2);
      });
    });
  }

  // mulberry32: a tiny seeded generator, so a burst looks the same on every render
  function rng(seed) {
    var s = seed >>> 0;
    return function () {
      s = (s + 0x6d2b79f5) >>> 0;
      var x = s;
      x = Math.imul(x ^ (x >>> 15), x | 1);
      x ^= x + Math.imul(x ^ (x >>> 7), x | 61);
      return ((x ^ (x >>> 14)) >>> 0) / 4294967296;
    };
  }

  root.Shapes = { outline: outline, lerpPoints: lerpPoints, pathOf: pathOf, fit: fit, ICONS: ICONS, STROKES: STROKES,
                  OUTLINES: Object.keys(OUTLINES), rng: rng };
})(typeof window !== "undefined" ? window : globalThis);
