// motion.js — closed-form spring and beat grid. Every animated value on the
// page is a pure function of the frame time t, so frame N always looks the
// same. Mirrored in scripts/motion_math.py; keep the two in step.
(function (root) {
  "use strict";

  // spring(t, zeta, omega) — unit spring released from 0 at rest. t in
  // seconds; zeta < 1 overshoots, zeta >= 1 does not; omega in rad/s.
  function spring(t, zeta, omega) {
    if (!(zeta > 0) || !(omega > 0)) {
      throw new RangeError("spring needs zeta > 0 and omega > 0, got " + zeta + ", " + omega);
    }
    if (t <= 0) return 0;
    if (zeta < 1) {
      var wd = omega * Math.sqrt(1 - zeta * zeta);
      var decay = Math.exp(-zeta * omega * t);
      return 1 - decay * (Math.cos(wd * t) + (zeta * omega / wd) * Math.sin(wd * t));
    }
    if (zeta === 1) return 1 - Math.exp(-omega * t) * (1 + omega * t);
    var r = omega * Math.sqrt(zeta * zeta - 1);
    var r1 = -zeta * omega + r, r2 = -zeta * omega - r;
    return 1 - (r2 * Math.exp(r1 * t) - r1 * Math.exp(r2 * t)) / (r2 - r1);
  }

  function framesPerBeat(bpm, fps) { return 60 * fps / bpm; }

  // beatFrame(n, bpm, fps) — frame of beat n, rounded per beat (no drift).
  function beatFrame(n, bpm, fps) { return Math.round(n * framesPerBeat(bpm, fps)); }

  // mix(a, b, p) — interpolate a -> b by progress p (p may overshoot 1).
  function mix(a, b, p) { return a + (b - a) * p; }

  // idle(t, k) — the quiet motion every element keeps after it settles, so no
  // scene ever freezes: a slow drift (x, y in cqmin) and a faint breath of
  // scale. k is the element's index; each one drifts at its own period, which
  // reads as parallax. Every term is 0 at t = 0, so the entrance never jumps.
  function idle(t, k) {
    if (t <= 0) return { x: 0, y: 0, s: 1 };
    var tau = 2 * Math.PI;
    return {
      x: IDLE_DRIFT * Math.sin(tau * t / (5.2 + 0.9 * k)),
      y: IDLE_DRIFT * 0.6 * Math.sin(tau * t / (3.9 + 0.7 * k)),
      s: 1 + IDLE_BREATH * (1 - Math.cos(tau * t / (4.6 + 0.5 * k))) / 2
    };
  }
  // glow(t) — centre of the soft light that wanders across every scene's
  // background, in % of the frame. Text alone covers too little of the frame
  // to register as motion; the light moves most of the pixels a little.
  function glow(t) {
    var tau = 2 * Math.PI;
    return { x: 50 + 18 * Math.sin(tau * t / 7), y: 42 + 12 * Math.sin(tau * t / 5.5) };
  }
  var IDLE_DRIFT = 0.5;    // cqmin: about 5 px on a 1080 short side
  var IDLE_BREATH = 0.012; // at most 1.2% larger

  root.Motion = { spring: spring, framesPerBeat: framesPerBeat, beatFrame: beatFrame, mix: mix, idle: idle, glow: glow };
})(typeof window !== "undefined" ? window : globalThis);
