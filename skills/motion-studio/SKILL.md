---
name: motion-studio
description: Make a short motion-graphics video from code — explainer, product video without footage, animated numbers, title cards — rendered frame by frame in a headless browser and delivered as out.mp4 (H.264) plus preview.webm (VP9). Use when the user asks for a motion graphic, an animated explainer, a product or feature video built from text, numbers and real screenshots, or a vertical/square/landscape social clip without filmed material. Not for editing recorded footage.
argument-hint: "[type] [duration] [aspect]"
allowed-tools: Bash(${CLAUDE_SKILL_DIR}/scripts/*)
metadata: { agent: short-video-editing-coach }
---

# Motion Studio

You write one file per video, `scene.json`; scripts do everything else and
print one short JSON line each. Run the scripts, never read them. Work in the
video's own directory (the working directory): `scene.json`, `assets/`,
`style.md`, `lessons.md` and every output live there, never in the skill directory.

## 1. Check the tools

Run `${CLAUDE_SKILL_DIR}/scripts/check-deps`. Exit 1 lists what is missing and
how to install it; stop and tell the user. It installs Chromium by itself.

## 2. Gather

- Brief: type (explainer, product video, motion graphics), duration, format
  (`9:16`, `16:9`, `1:1`, `4:5`, `5:4`, `2:3`, `3:2`; quality `low`/`medium`/`high`).
- Real assets go in `assets/` (screenshots, logos, photos). Never invent a
  product image; a scene without a real asset has no hero.
- If they exist, read `style.md` and `lessons.md` in the working directory.
- Reference images, if given: note palette (hex), type (weight, case, scale)
  and composition. Notes only; copy no layout or text.

## 3. Plan, then STOP for approval

Show the user a plan and wait. Nothing renders without an explicit approval.

- BPM and duration (a whole number of beats), format and size.
- A table, one row per scene: start–end in seconds and beats, message, elements
  (component per `references/components.md`), entrance beat, any `hold`.
- Style notes: palette hex, font sizes (at most two per scene), the hero asset.

Read `references/components.md` while planning. If the user changes the plan,
show it again. Proceed only on approval.

## 4. Write and validate `scene.json`

Read `references/scene-schema.md`, write `scene.json`, then run
`${CLAUDE_SKILL_DIR}/scripts/validate-scene scene.json` until it prints
`"ok": true`. Every error names the rule it broke; fix the scene, not the rule.

Everything moves on a spring and keeps a quiet idle motion once settled, so no
frame freezes by accident. When a scene must stand still on purpose, add
`"hold": <beats>` to it: its last beats repeat one frame. Use it rarely; any
other still stretch of 0.5 s or more fails the review.

## 5. Render and encode

```
${CLAUDE_SKILL_DIR}/scripts/render scene.json
${CLAUDE_SKILL_DIR}/scripts/synth-audio scene.json
${CLAUDE_SKILL_DIR}/scripts/encode scene.json
```

`render` writes `frames/` and `frames/layout.json`; `synth-audio` writes
`audio.wav` on the beat grid; `encode` writes `out.mp4` and `preview.webm`.

## 6. Review

Read `references/review.md` now.

1. Layer (a): `${CLAUDE_SKILL_DIR}/scripts/check-video scene.json`. Fix and
   re-render until it exits 0. It costs no tokens; never skip it.
2. Layer (b), only after (a) passes:
   `${CLAUDE_SKILL_DIR}/scripts/contact-sheet scene.json`. Look at
   `contact-sheet.png` (the only image you open) and score each scene 0–10
   with the rubric.
3. Scenes below 8: fix them in `scene.json`, then
   `${CLAUDE_SKILL_DIR}/scripts/render scene.json --scenes <ids>`, synth-audio,
   encode, and both layers again. Two such passes at most; `render` refuses a
   third. Then stop and explain, scene by scene, what still falls short.
4. Append one line per lesson to `lessons.md` in the working directory.

## 7. Deliver

Give the user both paths: `preview.webm` to watch, `out.mp4` to publish, with
the final score per scene.
