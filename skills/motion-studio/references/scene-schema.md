# scene.json

Read this when writing `scene.json`, the only file you write per video.
Check it with `scripts/validate-scene scene.json` before rendering; the output
names the broken rule for every error.

## Frame

Either a preset or a free size:

| Field | Type | Default | Rule |
|---|---|---|---|
| `preset` | `"1:1"` `"9:16"` `"16:9"` `"4:5"` `"5:4"` `"2:3"` `"3:2"` | — | shortcut for width/height |
| `quality` | `"low"` `"medium"` `"high"` | `"medium"` | short side 720 / 1080 / 1440 |
| `width`, `height` | integers | — | used when there is no `preset`; even, 160–4096 each |
| `fps` | integer | 30 | 12–60 |
| `bpm` | number | — | 40–240; sets the beat grid |
| `duration` | seconds | — | 1–300 and a whole number of beats |
| `background` | `{"color": "#rrggbb"}` or `{"gradient": ["#rrggbb", "#rrggbb"]}` | — | default for every scene |

Preset sizes (width × height):

| Preset | low | medium | high |
|---|---|---|---|
| 9:16 | 720×1280 | 1080×1920 | 1440×2560 |
| 16:9 | 1280×720 | 1920×1080 | 2560×1440 |
| 1:1 | 720×720 | 1080×1080 | 1440×1440 |
| 4:5 | 720×900 | 1080×1350 | 1440×1800 |
| 5:4 | 900×720 | 1350×1080 | 1800×1440 |
| 2:3 | 720×1080 | 1080×1620 | 1440×2160 |
| 3:2 | 1080×720 | 1620×1080 | 2160×1440 |

A duration between beats is rejected, and the error names the two nearest
valid durations. Pick one, or change `bpm`.

## Scenes

`scenes` is a list that tiles the timeline: the first starts at 0, each starts
where the previous ended, and the last ends at `duration`.

| Field | Type | Default | Rule |
|---|---|---|---|
| `id` | string | — | short name used in reviews |
| `start`, `end` | seconds | — | both on a beat (cuts land on the beat) |
| `enter_beat` | integer | 0 | beat, counted from the scene start, at which entrances begin |
| `stagger` | integer | 3 | frames between consecutive entrances, 2–4 |
| `background` | as above | top-level | overrides the default |
| `elements` | list | — | components, entering in list order |

## Elements

Every element has `component` (see `components.md`) plus:

| Field | Type | Default | Rule |
|---|---|---|---|
| `size` | `"xs"` `"s"` `"m"` `"l"` `"xl"` | per component | fraction of the short side; at most 2 sizes per scene |
| `align` | `"start"` `"center"` `"end"` | `"start"` | |
| `box` | `[x, y, w, h]` | component layout | fractions 0–1 of the frame, inside it |
| `hero` | boolean | false | only on an `asset-card` whose `asset` exists |
| `spring` | `{"zeta", "omega"}` | component feel | zeta 0.2–2, omega 4–40 |
| `caps` | boolean | false | renders the text in capitals |

Assets are paths under `assets/` next to `scene.json`, and they must exist.

## Rules the validator enforces

`format`, `even-size`, `size-limits`, `fps`, `bpm`, `duration`, `beat-grid`,
`timeline`, `cut-on-beat`, `stagger`, `enter-timing`, `font-sizes`,
`caps-center-gradient` (capitals + centered + gradient background together),
`hero-asset`, `asset`, `component`, `relative-box`, `background`, `spring`.
