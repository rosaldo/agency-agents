"""encode muxes frames + WAV into out.mp4 (H.264/yuv420p/AAC) and preview.webm (VP9/Opus).

Frames and audio are synthetic (lavfi), so no Chromium or network is needed.
"""
import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
SCRIPT = SKILL / "scripts" / "encode"
SCENE = {"width": 320, "height": 240, "fps": 30, "bpm": 120, "duration": 1,
         "scenes": [{"id": "a", "text_only": True, "start": 0, "end": 1, "elements": [{"component": "text", "text": "Hi"}]}]}


def ffmpeg(*args):
    subprocess.run(["ffmpeg", "-v", "error", "-y", *args], check=True)


def probe(path):
    r = subprocess.run(["ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", str(path)],
                       capture_output=True, text=True, check=True)
    data = json.loads(r.stdout)
    streams = {s["codec_type"]: s for s in data["streams"]}
    return streams, float(data["format"]["duration"])


class EncodeTest(unittest.TestCase):
    def setUp(self):
        self.work = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.work)
        (self.work / "scene.json").write_text(json.dumps(SCENE))
        (self.work / "frames").mkdir()
        ffmpeg("-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "30", "-start_number", "0",
               str(self.work / "frames" / "frame_%05d.png"))
        ffmpeg("-f", "lavfi", "-i", "sine=f=440:sample_rate=48000:d=1", str(self.work / "audio.wav"))

    def encode(self):
        r = subprocess.run([str(SCRIPT), str(self.work / "scene.json")], capture_output=True, text=True)
        self.assertEqual(len(r.stdout.strip().splitlines()), 1, r.stdout + r.stderr)
        return r.returncode, json.loads(r.stdout)

    def test_both_files_have_the_planned_codecs_size_fps_and_duration(self):
        code, out = self.encode()
        self.assertEqual(code, 0, out)
        mp4, mp4_len = probe(self.work / "out.mp4")
        webm, webm_len = probe(self.work / "preview.webm")
        self.assertEqual((mp4["video"]["codec_name"], mp4["video"]["pix_fmt"], mp4["audio"]["codec_name"]),
                         ("h264", "yuv420p", "aac"))
        self.assertEqual((webm["video"]["codec_name"], webm["audio"]["codec_name"]), ("vp9", "opus"))
        for v in (mp4["video"], webm["video"]):
            self.assertEqual((v["width"], v["height"], v["r_frame_rate"]), (320, 240, "30/1"))
        self.assertAlmostEqual(mp4_len, 1.0, delta=1 / 30)
        self.assertAlmostEqual(webm_len, mp4_len, delta=1 / 30)

    def test_missing_frames_fail_fast(self):
        (self.work / "frames" / "frame_00029.png").unlink()
        code, out = self.encode()
        self.assertNotEqual(code, 0)
        self.assertIn("frames", out["error"])


if __name__ == "__main__":
    unittest.main()
