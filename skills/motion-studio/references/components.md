# Components

Read this while planning scenes. Every component is driven by the frame time
`t` and the spring below; nothing uses CSS transitions or animations.

## Spring

`Motion.spring(t, zeta, omega)` moves a value from 0 to 1, starting at rest.

| Parameter | Meaning | Useful range | Notes |
|---|---|---|---|
| `zeta` | damping ratio | 0.35 – 1.2 | below 1 overshoots once or more; 1 is the fastest settle without overshoot; above 1 feels heavy |
| `omega` | natural frequency, rad/s | 8 – 30 | higher is snappier; 12 settles in about half a second at `zeta` 0.7 |

Values outside `zeta > 0`, `omega > 0` throw.

Suggested feels: snappy UI `0.7 / 18`, soft landing `1.0 / 10`, playful pop `0.45 / 16`.

## Beat grid

`Motion.beatFrame(n, bpm, fps)` is the frame of beat `n` (beat 0 is frame 0),
rounded per beat so long videos do not drift. At 120 BPM and 30 fps one beat
is 15 frames.
