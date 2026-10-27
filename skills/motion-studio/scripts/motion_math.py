"""Closed-form spring and beat grid — mirror of lib/motion.js.

The page animates with lib/motion.js; the Python scripts (validator, video
checks) use this mirror so both sides agree on the same numbers.
"""
import math


def spring(t, zeta, omega):
    """Position of a unit spring released from 0 at rest, at time t seconds.

    zeta is the damping ratio (<1 overshoots, >=1 does not), omega the natural
    angular frequency in rad/s. Returns 0 for t <= 0 and tends to 1.
    """
    if zeta <= 0 or omega <= 0:
        raise ValueError(f"spring needs zeta > 0 and omega > 0, got zeta={zeta} omega={omega}")
    if t <= 0:
        return 0.0
    if zeta < 1:
        wd = omega * math.sqrt(1 - zeta * zeta)
        decay = math.exp(-zeta * omega * t)
        return 1 - decay * (math.cos(wd * t) + (zeta * omega / wd) * math.sin(wd * t))
    if zeta == 1:
        return 1 - math.exp(-omega * t) * (1 + omega * t)
    root = omega * math.sqrt(zeta * zeta - 1)
    r1, r2 = -zeta * omega + root, -zeta * omega - root
    return 1 - (r2 * math.exp(r1 * t) - r1 * math.exp(r2 * t)) / (r2 - r1)


def frames_per_beat(bpm, fps):
    return 60 * fps / bpm


def beat_frame(n, bpm, fps):
    """Frame of beat n (beat 0 = frame 0). Rounded per beat, so no drift accumulates."""
    return round(n * frames_per_beat(bpm, fps))
