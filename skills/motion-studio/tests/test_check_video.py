"""check-video (review layer a) flags wrong probe values, dead air, off-beat cuts and unsafe text.

Every video is synthetic (lavfi) and every layout.json is written by hand, so
no Chromium or network is needed.
"""
import copy
import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
SCRIPT = SKILL / "scripts" / "check-video"
# 3 s at 120 BPM and 30 fps: one beat is 15 frames; the cut lands on beat 3 = frame 45.
SCENE = {"width": 320, "height": 240, "fps": 30, "bpm": 120, "duration": 3,
         "scenes": [{"id": "a", "text_only": True, "start": 0, "end": 1.5, "elements": [{"component": "text", "text": "One"}]},
                    {"id": "b", "text_only": True, "start": 1.5, "end": 3, "elements": [{"component": "text", "text": "Two"}]}]}
LAYOUT = {"width": 320, "height": 240, "fps": 30, "frames": 90, "scenes": [
    {"id": "a", "text_only": True, "start": 0, "end": 45, "boxes": [{"element": 0, "box": [0.1, 0.4, 0.5, 0.1]}]},
    {"id": "b", "text_only": True, "start": 45, "end": 90, "boxes": [{"element": 0, "box": [0.1, 0.4, 0.5, 0.1]}]}]}


class CheckVideoTest(unittest.TestCase):
    def setUp(self):
        self.work = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.work)
        (self.work / "frames").mkdir()

    def video(self, source):
        subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", source, "-c:v", "libx264",
                        "-pix_fmt", "yuv420p", str(self.work / "out.mp4")], check=True)

    def check(self, scene=SCENE, layout=LAYOUT):
        (self.work / "scene.json").write_text(json.dumps(scene))
        (self.work / "frames" / "layout.json").write_text(json.dumps(layout))
        r = subprocess.run([str(SCRIPT), str(self.work / "scene.json")], capture_output=True, text=True)
        self.assertLessEqual(len(r.stdout.strip().splitlines()), 20, r.stdout)
        return r.returncode, json.loads(r.stdout)

    def test_continuous_video_passes(self):
        self.video("testsrc2=s=320x240:r=30:d=3")
        code, out = self.check()
        self.assertEqual(code, 0, out)
        self.assertTrue(out["ok"])

    def test_one_frozen_second_is_dead_air(self):
        self.video("testsrc2=s=320x240:r=30:d=2,tpad=stop_mode=clone:stop_duration=1")
        code, out = self.check()
        self.assertEqual(code, 1)
        self.assertFalse(out["freeze"]["ok"])

    def test_a_declared_hold_excuses_the_still(self):
        self.video("testsrc2=s=320x240:r=30:d=2,tpad=stop_mode=clone:stop_duration=1")
        scene = copy.deepcopy(SCENE)
        scene["scenes"][1]["hold"] = 2  # the last two beats (1 s) are a deliberate still
        code, out = self.check(scene)
        self.assertEqual(code, 0, out)

    def test_wrong_duration_is_flagged(self):
        self.video("testsrc2=s=320x240:r=30:d=4")
        code, out = self.check()
        self.assertEqual(code, 1)
        self.assertFalse(out["probe"]["ok"])

    def test_text_outside_the_safe_area_is_flagged(self):
        self.video("testsrc2=s=320x240:r=30:d=3")
        layout = copy.deepcopy(LAYOUT)
        layout["scenes"][1]["boxes"][0]["box"] = [0.1, 0.9, 0.5, 0.08]  # bottom edge at 98%
        code, out = self.check(layout=layout)
        self.assertEqual(code, 1)
        self.assertEqual(out["safe_area"]["outside"][0]["scene"], "b")

    def test_safe_area_override_from_scene_json(self):
        self.video("testsrc2=s=320x240:r=30:d=3")
        code, out = self.check(dict(SCENE, safe_area={"left": 0.2}))
        self.assertEqual(code, 1)
        self.assertFalse(out["safe_area"]["ok"])

    def test_cut_off_the_beat_is_counted_in_frames(self):
        self.video("testsrc2=s=320x240:r=30:d=3")
        layout = copy.deepcopy(LAYOUT)
        layout["scenes"][0]["end"] = layout["scenes"][1]["start"] = 47
        code, out = self.check(layout=layout)
        self.assertEqual(code, 1)
        self.assertEqual(out["cuts"]["max_error_frames"], 2)


if __name__ == "__main__":
    unittest.main()
