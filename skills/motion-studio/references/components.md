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

### Graphics (drawn in code, no image files)

| Component | Fields | Use it for |
|---|---|---|
| `shape` | `shape` (`circle` `blob` `rect` `triangle` `hexagon` `star` `heart`), optional `from` (`left` `right` `top` `bottom`), `spin` (degrees), `opacity` | a coloured form that pops in (or slides in from a side) and turns to rest; `blob` keeps wobbling; `rect` stretches into a rounded panel when it has a `box` |
| `draw` | `stroke` (`underline` `arrow` `connector` `outline` `circle` `wave`) | a line that draws itself across its box: underline a word, point with an arrow, link two things, ring or frame a number |
| `morph` | `from`, `to` (shape names), optional `after` (beats, default 2) | one shape pops in, then becomes another |
| `icon` | `icon` (`check` `clock` `chart` `cart` `star` `heart` `arrow` `user` `bolt` `gift` `bell` `calendar`), optional `mode` (`draw` default, `pop`), `disc` | a pictogram that draws its stroke in or pops; `disc: true` puts it on a filled circle |
| `ring` | `value` (0–100), optional `prefix`, `suffix` (default `%`), `decimals` | progress or a share: the arc grows while the number counts up |
| `line-chart` | `points` (2–12 numbers) | a trend: the line draws left to right, the area fades in, the end dot pops |
| `burst` | optional `seed`, `count` (8–80, default 36) | a confetti accent at the climax; flies out from the centre of its box, or of the frame without one |

Graphics take `color` (`#rrggbb`); without it they cycle through the top-level
`palette`. Their `size` is the height of the graphic in the column layout
(`xs` 0.1 to `xl` 0.46 of the short side) and is not a font size. Most graphics
read best with a `box`. Elements paint in list order, so a shape listed first
sits behind what follows.

Any element, text included, can enter through a mask with `reveal`: `wipe`
(left to right), `iris` (a circle opening from the centre) or `slide` (rises
from behind its own edge).

Every background is layered: soft shapes in the palette drift at three depths
(parallax), and a fixed grain dithers gradients so the encoder shows no bands.

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
list 0.7/16, asset-card 0.6/12, bar-chart 0.8/10, shape 0.55/11, draw 1/9,
morph 0.6/10, icon 0.5/13, ring 1/7, line-chart 1/6, burst 1/6.

Write a new component only when none of these fits; it goes in
`lib/components.js` (geometry in `lib/shapes.js`), registers in `scripts/scene_schema.py`, and must pass
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
