# Components

Read this while planning scenes. Every component is driven by the frame number
and the spring below; the library has no CSS-driven motion and no clock. All
sizes are relative to the frame, so one scene works in every format.

## Catalog

| Component | Fields | Use it for |
|---|---|---|
| `title` | `text`, optional `subtitle` | the hook or a section opener; subtitle enters one stagger later |
| `text` | `text` | one short statement that supports the scene |
| `counter` | `from`, `to`, optional `prefix`, `suffix`, `decimals` | a number that matters; settles on `to` without overshoot |
| `list` | `items` (2-6 short strings) | steps or features; items slide in one stagger apart |
| `asset-card` | `asset`, optional `caption`, `hero` | a real screenshot, photo or logo from `assets/`; the scene's hero |
| `bar-chart` | `bars`: `[{"label", "value"}]` | a before/after or a small comparison; bars grow in order |

Shared fields (`size`, `align`, `box`, `spring`, `caps`) are in `scene-schema.md`.
Without `box`, elements stack in a column inside the default margins; with
`box`, they sit at those frame fractions. A cut between scenes is the only
scene change: scenes switch on their start beat, with no cross-fade.

Nothing is ever frozen by accident: once an element settles it keeps a quiet
idle motion (a slow drift of about half a percent of the short side, at a
different period per element, plus a faint breath of scale), and a soft light
wanders across every background. Both are pure functions of the frame. To
stop on purpose, declare a `hold` on the scene: its last beats repeat one frame.

Default springs (zeta / omega): title 0.7/14, text 0.8/12, counter 0.75/12,
list 0.7/16, asset-card 0.6/12, bar-chart 0.8/10.

Write a new component only when none of these fits; it goes in
`lib/components.js`, registers in `scripts/scene_schema.py`, and must pass
`tests/test_lib_rules.py`.

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
