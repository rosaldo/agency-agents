"""render draws every frame in Chromium, the same way twice, and re-renders single scenes.

Needs the Playwright Chromium and the pinned playwright wheel already in the uv
cache; runs offline (UV_OFFLINE=1) and is skipped when either is missing.
"""
import hashlib
import json
import os
import shutil
import struct
import subprocess
import tempfile
import unittest
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
RENDER = SKILL / "scripts" / "render"
PIN = (SKILL / "scripts" / "playwright-version").read_text().strip()
ENV = {**os.environ, "UV_OFFLINE": "1"}

# 1 s of 16:9 at 120 BPM: two half-second scenes, so a partial render has a neighbour to keep.
SCENE = {
    "preset": "16:9", "quality": "low", "fps": 30, "bpm": 120, "duration": 1,
    "background": {"color": "#101418"},
    "scenes": [
        {"id": "a", "start": 0, "end": 0.5, "elements": [{"component": "title", "text": "Alpha"}]},
        {"id": "b", "start": 0.5, "end": 1, "elements": [{"component": "counter", "from": 0, "to": 9}]},
    ],
}


def chromium_ready():
    if not shutil.which("uv"):
        return False
    probe = "from playwright.sync_api import sync_playwright as s\nwith s() as p: p.chromium.launch().close()"
    r = subprocess.run(["uv", "run", "--no-project", "--quiet", "--with", f"playwright=={PIN}", "python", "-c", probe],
                       env=ENV, capture_output=True)
    return r.returncode == 0


def png_size(path):
    return struct.unpack(">II", path.read_bytes()[16:24])


def digests(frames):
    return {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(frames.glob("frame_*.png"))}


@unittest.skipUnless(chromium_ready(), "Playwright Chromium not available offline")
class RenderTest(unittest.TestCase):
    def setUp(self):
        self.work = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.work)
        self.scene = self.work / "scene.json"
        self.scene.write_text(json.dumps(SCENE))

    def render(self, *args):
        r = subprocess.run([str(RENDER), str(self.scene), *args], env=ENV, capture_output=True, text=True)
        lines = r.stdout.strip().splitlines()
        self.assertEqual(len(lines), 1, r.stdout + r.stderr)  # one summary line, no per-frame log
        return r.returncode, json.loads(lines[0])

    def test_full_render_is_complete_and_deterministic(self):
        code, out = self.render()
        self.assertEqual(code, 0, out)
        frames = self.work / "frames"
        first = digests(frames)
        self.assertEqual(len(first), 30)
        self.assertEqual(png_size(frames / "frame_00000.png"), (1280, 720))
        layout = json.loads((frames / "layout.json").read_text())
        self.assertEqual([(s["id"], s["start"], s["end"]) for s in layout["scenes"]], [("a", 0, 15), ("b", 15, 30)])
        self.assertTrue(all(len(s["boxes"]) == 1 for s in layout["scenes"]))
        self.assertEqual(self.render()[0], 0)
        self.assertEqual(digests(frames), first)

    def test_partial_render_rewrites_only_the_chosen_scene(self):
        self.assertEqual(self.render()[0], 0)
        frames = self.work / "frames"
        before = {p.name: p.stat().st_mtime_ns for p in frames.glob("frame_*.png")}
        code, out = self.render("--scenes", "2")
        self.assertEqual((code, out["rendered"]), (0, 15), out)
        changed = {n for n, t in before.items() if (frames / n).stat().st_mtime_ns != t}
        self.assertEqual(changed, {f"frame_{i:05d}.png" for i in range(15, 30)})

    def test_partial_render_needs_a_full_render_first(self):
        code, out = self.render("--scenes", "b")
        self.assertNotEqual(code, 0)
        self.assertFalse(out["ok"])

    def test_invalid_scene_is_refused_before_launching(self):
        self.scene.write_text(json.dumps({**SCENE, "duration": 1.2}))
        code, out = self.render()
        self.assertNotEqual(code, 0)
        self.assertEqual(out["errors"][0]["rule"], "beat-grid")


if __name__ == "__main__":
    unittest.main()
