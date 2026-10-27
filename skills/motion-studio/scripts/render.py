"""render.py <scene.json> [--out DIR] [--scenes LIST] — frames + layout.json.

Builds the page from lib/, steps it with MotionStudio.renderFrame(n) and saves
frame_NNNNN.png per frame plus layout.json (each scene's frame range and the
text boxes measured on its last frame). --scenes re-renders only those scenes
(ids or 1-based numbers) over an existing full render. Prints one JSON line.
"""
import argparse
import json
import sys
from pathlib import Path

from playwright.sync_api import sync_playwright

from scene_schema import resolve_size, validate

LIB = Path(__file__).resolve().parents[1] / "lib"
LIB_FILES = ("motion.js", "components.js", "runtime.js")


def page_html(base_dir):
    css = (LIB / "stage.css").read_text()
    js = "\n".join((LIB / name).read_text() for name in LIB_FILES)
    return (f'<!doctype html><html><head><meta charset="utf-8"><base href="{base_dir.as_uri()}/">'
            f"<style>{css}</style></head><body><div id=\"stage\"></div><script>{js}</script></body></html>")


def pick(scenes, tokens):
    """Scene indices named by id or 1-based number; raises ValueError on an unknown one."""
    chosen = set()
    for token in filter(None, (t.strip() for t in tokens.split(","))):
        ids = [i for i, s in enumerate(scenes) if s["id"] == token]
        if not ids and token.isdigit() and 1 <= int(token) <= len(scenes):
            ids = [int(token) - 1]
        if not ids:
            raise ValueError(f"no scene {token!r}; scenes are {', '.join(s['id'] for s in scenes)}")
        chosen.update(ids)
    return chosen


def fail(message, **extra):
    print(json.dumps({"ok": False, "error": message, **extra}))
    return 1


def main(argv):
    ap = argparse.ArgumentParser(prog="render")
    ap.add_argument("scene")
    ap.add_argument("--out", help="frames directory (default: frames/ next to scene.json)")
    ap.add_argument("--scenes", help="re-render only these scenes, e.g. 2,4 or hook,close")
    args = ap.parse_args(argv)
    scene_path = Path(args.scene).resolve()
    try:
        scene = json.loads(scene_path.read_text())
    except (OSError, ValueError) as exc:
        return fail(str(exc))
    errors = validate(scene, scene_path.parent)
    if errors:
        print(json.dumps({"ok": False, "errors": errors[:20]}))
        return 1
    width, height = resolve_size(scene)
    out = Path(args.out).resolve() if args.out else scene_path.parent / "frames"
    layout_path = out / "layout.json"
    old_layout = json.loads(layout_path.read_text()) if args.scenes and layout_path.is_file() else None
    if args.scenes and old_layout is None:
        return fail("--scenes needs a full render first; run render without --scenes")
    out.mkdir(parents=True, exist_ok=True)
    page_file = out / ".page.html"
    page_file.write_text(page_html(scene_path.parent))

    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": width, "height": height}, device_scale_factor=1)
        page.goto(page_file.as_uri())
        total = page.evaluate("([s, w, h]) => MotionStudio.load(s, {width: w, height: h})", [scene, width, height])
        page.evaluate("MotionStudio.ready().then(() => document.fonts.ready).then(() => true)")
        ranges = page.evaluate("MotionStudio.scenes()")
        try:
            chosen = pick(ranges, args.scenes) if args.scenes else set(range(len(ranges)))
        except ValueError as exc:
            browser.close()
            return fail(str(exc))
        if old_layout and len(list(out.glob("frame_*.png"))) != total:
            browser.close()
            return fail("existing frames do not match this scene.json; run a full render")
        boxes = {}
        rendered = 0
        for i in sorted(chosen):
            sc = ranges[i]
            for n in range(sc["start"], sc["end"]):
                page.evaluate("n => MotionStudio.renderFrame(n)", n)
                page.screenshot(path=str(out / f"frame_{n:05d}.png"), clip={"x": 0, "y": 0, "width": width, "height": height})
                rendered += 1
            boxes[i] = page.evaluate("MotionStudio.textBoxes()")
        browser.close()

    previous = {s["id"]: s["boxes"] for s in (old_layout or {}).get("scenes", [])}
    layout = {"width": width, "height": height, "fps": scene.get("fps", 30), "frames": total,
              "scenes": [{**sc, "boxes": boxes[i] if i in boxes else previous.get(sc["id"], [])}
                         for i, sc in enumerate(ranges)]}
    layout_path.write_text(json.dumps(layout))
    page_file.unlink()
    print(json.dumps({"ok": True, "frames": total, "rendered": rendered, "width": width, "height": height,
                      "out": str(out)}))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
