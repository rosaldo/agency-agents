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

## Layer (b): `scripts/contact-sheet scene.json`

Only after layer (a) exits 0. It writes `contact-sheet.png`: one grid, four
cells per second, never wider or taller than 2048 px, and prints which cells
belong to each scene. That image is the only visual you look at per pass; do
not open frames or the video.

Score every scene 0–10 by adding the five lines below (0, 1 or 2 each):

| Line | 2 | 1 | 0 |
|---|---|---|---|
| Read | the message is clear in the cells where the scene settles | readable, but crowded or late | cannot be read in time |
| Hierarchy | one focal point, the hero or the key number | two elements compete | no clear focus |
| Motion | entrances differ cell to cell and settle before the cut | movement exists but looks uniform | nothing moves, or it never settles |
| Fit | palette, type and composition follow `style.md` and the approved plan | one visible drift | ignores the plan |
| Craft | aligned, nothing clipped, consistent spacing | one small flaw | broken layout or clipped text |

## The loop

1. Scenes scoring 8 or more are done.
2. Fix the `scene.json` entries of every scene below 8, run `validate-scene`,
   then `render --scenes <ids>`, `synth-audio`, `encode`, and both layers again.
3. `render` allows two such passes and refuses a third. When it refuses, stop
   and tell the user, scene by scene, the score and what still fails.
4. At the end, append one line per lesson to `lessons.md` in the video's working
   directory (never in the skill directory): what failed, and the rule that
   avoids it next time. Read `lessons.md` before planning the next video.
