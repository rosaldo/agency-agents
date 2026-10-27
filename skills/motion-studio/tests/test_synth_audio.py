"""synth-audio writes a stdlib-only 48 kHz WAV as long as the video, loudest on the cuts."""
import array
import json
import shutil
import subprocess
import tempfile
import unittest
import wave
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
SCRIPT = SKILL / "scripts" / "synth-audio"
FPS, BPM = 30, 120
SCENE = {"preset": "1:1", "fps": FPS, "bpm": BPM, "duration": 3,
         "scenes": [{"id": "a", "start": 0, "end": 1.5, "elements": [{"component": "text", "text": "One"}]},
                    {"id": "b", "start": 1.5, "end": 3, "elements": [{"component": "text", "text": "Two"}]}]}


class SynthAudioTest(unittest.TestCase):
    def setUp(self):
        self.work = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.work)
        (self.work / "scene.json").write_text(json.dumps(SCENE))
        r = subprocess.run([str(SCRIPT), str(self.work / "scene.json")], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(len(r.stdout.strip().splitlines()), 1)
        with wave.open(str(self.work / "audio.wav")) as w:
            self.rate, self.channels, self.width = w.getframerate(), w.getnchannels(), w.getsampwidth()
            self.samples = array.array("h", w.readframes(w.getnframes()))

    def test_format_and_exact_duration(self):
        self.assertEqual((self.rate, self.width), (48000, 2))
        self.assertEqual(len(self.samples), 3 * 48000 * self.channels)

    def test_cut_beat_is_the_loudest_peak_near_its_frame(self):
        mono = self.samples[::self.channels]
        beat = 48000 * 60 // BPM            # samples per beat
        cut = 3 * beat                       # scene b starts on beat 3 (1.5 s)
        window = mono[cut - beat // 2: cut + beat // 2]
        peak = cut - beat // 2 + max(range(len(window)), key=lambda i: abs(window[i]))
        self.assertLessEqual(abs(peak - cut), 48000 // FPS)          # within one frame
        plain = mono[2 * beat - beat // 2: 2 * beat + beat // 2]     # beat 2 is not a cut
        self.assertGreater(max(map(abs, window)), max(map(abs, plain)))

    def test_stdlib_only(self):
        text = SCRIPT.read_text()
        self.assertNotIn("dependencies", text)
        self.assertNotRegex(text, r"(?m)^(import|from) (numpy|scipy)")


if __name__ == "__main__":
    unittest.main()
