"""A settled scene keeps a quiet idle motion: no still stretch of 0.5 s without a declared hold.

Renders for real (offline Chromium, skipped without it), then encodes and runs check-video.
"""
import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from test_render import ENV, chromium_ready

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
# 3 s, two scenes, no hold: springs settle in well under a second, so the
# rest of each scene is still unless the library keeps it moving.
SCENE = {"preset": "9:16", "quality": "low", "fps": 30, "bpm": 120, "duration": 3,
         "scenes": [{"id": "a", "text_only": True, "start": 0, "end": 1.5, "elements": [{"component": "title", "text": "Quiet", "subtitle": "still alive"}]},
                    {"id": "b", "text_only": True, "start": 1.5, "end": 3, "elements": [{"component": "counter", "from": 0, "to": 7},
                                                                     {"component": "text", "text": "and settled"}]}]}


@unittest.skipUnless(chromium_ready(), "Playwright Chromium not available offline")
class IdleMotionTest(unittest.TestCase):
    def test_no_dead_air_without_hold(self):
        work = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, work)
        scene = work / "scene.json"
        scene.write_text(json.dumps(SCENE))
        for step in ("render", "synth-audio", "encode"):
            r = subprocess.run([str(SCRIPTS / step), str(scene)], env=ENV, capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, (step, r.stdout, r.stderr))
        r = subprocess.run([str(SCRIPTS / "check-video"), str(scene)], env=ENV, capture_output=True, text=True)
        out = json.loads(r.stdout)
        self.assertTrue(out["freeze"]["ok"], out)
        self.assertTrue(out["safe_area"]["ok"], out)
        frames = work / "frames"  # a declared hold, by contrast, is a true still
        scene.write_text(json.dumps({**SCENE, "scenes": [SCENE["scenes"][0], {**SCENE["scenes"][1], "hold": 2}]}))
        subprocess.run([str(SCRIPTS / "render"), str(scene)], env=ENV, capture_output=True, check=True)
        held = {(frames / f"frame_{n:05d}.png").read_bytes() for n in range(60, 90)}
        self.assertEqual(len(held), 1)


if __name__ == "__main__":
    unittest.main()
