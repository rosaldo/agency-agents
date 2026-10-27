# Review

Read this after `encode`, before showing anything to the user.

## Layer (a): `scripts/check-video scene.json`

Zero tokens: run it and read the one JSON line. Exit 0 means every check
passed; fix the scene and re-render before moving on otherwise.

| Check | Passes when | Usual fix |
|---|---|---|
| `probe` | size, fps and duration (±1 frame) match the plan | re-run `render` and `encode`; never patch the video |
| `freeze` | no still stretch of 0.5 s or more outside a declared `hold` | add motion, shorten the scene, or declare the still |
| `cuts` | every rendered cut is exactly on its planned beat (`max_error_frames` 0) | re-render; the layout is stale |
| `safe_area` | every text box sits inside the margins | move or resize the element, or set `safe_area` for the target |

### Intentional holds

A scene that should sit still on purpose (a statement left on screen to be
read) says so with `"hold": <beats>`: the last that many beats of the scene may
be completely still. Any still stretch outside a hold that lasts 0.5 s or more
is reported as dead air. Declaring the hold in beats, at the end of the scene,
keeps it on the beat grid and makes the still part of the plan the user approves.

### Safe area

Margins are fractions of the frame, so one value works at every quality.
Defaults are a house layout rule, not any platform's specification:

| Format | top | right | bottom | left |
|---|---|---|---|---|
| tall (height / width ≥ 1.5: 9:16, 2:3) | 0.08 | 0.06 | 0.12 | 0.06 |
| every other format | 0.05 | 0.05 | 0.05 | 0.05 |

Tall formats keep more room top and bottom because vertical feeds draw their
own interface over the frame. When the video targets a specific platform,
check that platform's current guide and set `safe_area` in `scene.json`; any
side you give replaces the default for that side only. Only text is measured
(the glyphs, on the last frame of each scene), so a hero image may run to the edge.
