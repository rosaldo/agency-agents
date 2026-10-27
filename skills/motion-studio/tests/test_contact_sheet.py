"""contact-sheet makes one small grid image with a frame->scene index; re-render passes stop at 2."""
import json
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
SCRIPT = SKILL / "scripts" / "contact-sheet"
sys.path.insert(0, str(SKILL / "scripts"))

SCENE = {"width": 320, "height": 240, "fps": 30, "bpm": 120, "duration": 6,
         "scenes": [{"id": "a", "text_only": True, "start": 0, "end": 2, "elements": [{"component": "text", "text": "One"}]},
                    {"id": "b", "text_only": True, "start": 2, "end": 6, "elements": [{"component": "text", "text": "Two"}]}]}


class ContactSheetTest(unittest.TestCase):
    def setUp(self):
        self.work = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.work)
        (self.work / "scene.json").write_text(json.dumps(SCENE))
        subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=320x240:r=30:d=6",
                        "-c:v", "libx264", "-pix_fmt", "yuv420p", str(self.work / "out.mp4")], check=True)

    def test_one_small_sheet_with_a_cell_per_quarter_second(self):
        from review_loop import MAX_SIDE
        r = subprocess.run([str(SCRIPT), str(self.work / "scene.json")], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(len(r.stdout.strip().splitlines()), 1)
        out = json.loads(r.stdout)
        self.assertEqual(out["cells"], 24)
        self.assertEqual(out["scenes"], {"a": [0, 7], "b": [8, 23]})
        sheet = Path(out["sheet"])
        self.assertEqual([p.name for p in self.work.glob("*.png")], [sheet.name])
        w, h = struct.unpack(">II", sheet.read_bytes()[16:24])
        self.assertEqual((w, h), (out["cols"] * out["cell"][0], out["rows"] * out["cell"][1]))
        self.assertGreaterEqual(out["cols"] * out["rows"], 24)
        self.assertLessEqual(max(w, h), MAX_SIDE)

    def test_grid_shrinks_cells_to_stay_under_the_limit(self):
        from review_loop import MAX_SIDE, grid
        cols, rows, (cw, ch) = grid(1200, 1080, 1920)  # 5 minutes of 9:16
        self.assertLessEqual(max(cols * cw, rows * ch), MAX_SIDE)
        self.assertEqual((cw % 2, ch % 2), (0, 0))

    def test_review_sheet_is_cheap_but_still_readable(self):
        from review_loop import grid
        cols, rows, (cw, ch) = grid(48, 1080, 1920)  # 12 s of 9:16, the usual short
        self.assertLessEqual(max(cols * cw, rows * ch), 1280)
        self.assertGreaterEqual(cw, 120)  # below this a cell no longer shows a scene's layout


class PassCounterTest(unittest.TestCase):
    def test_third_rerender_pass_is_refused(self):
        from review_loop import next_pass
        self.assertEqual(next_pass(0), 1)
        self.assertEqual(next_pass(1), 2)
        with self.assertRaises(ValueError):
            next_pass(2)


if __name__ == "__main__":
    unittest.main()
