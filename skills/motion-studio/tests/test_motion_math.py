"""Closed-form spring and beat grid (Python mirror of lib/motion.js)."""
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
from motion_math import beat_frame, spring  # noqa: E402

SAMPLES = [i / 300 for i in range(0, 1201)]  # 0..4 s


class SpringTest(unittest.TestCase):
    def test_starts_at_rest(self):
        for zeta in (0.3, 1.0, 1.6):
            self.assertEqual(spring(0, zeta, 12), 0)
            self.assertEqual(spring(-1, zeta, 12), 0)

    def test_settles_on_target(self):
        for zeta in (0.3, 0.7, 1.0, 1.6):
            self.assertAlmostEqual(spring(6, zeta, 12), 1, places=3)

    def test_underdamped_overshoots(self):
        self.assertGreater(max(spring(t, 0.4, 12) for t in SAMPLES), 1.05)

    def test_critical_and_overdamped_never_overshoot(self):
        for zeta in (1.0, 1.5):
            values = [spring(t, zeta, 12) for t in SAMPLES]
            self.assertTrue(all(b >= a - 1e-12 for a, b in zip(values, values[1:])), zeta)
            self.assertLessEqual(max(values), 1 + 1e-9)

    def test_rejects_out_of_range_parameters(self):
        for zeta, omega in ((0, 12), (-1, 12), (0.5, 0)):
            with self.assertRaises(ValueError):
                spring(0.5, zeta, omega)


class BeatGridTest(unittest.TestCase):
    def test_one_beat_at_120_bpm_30_fps_is_15_frames(self):
        self.assertEqual(beat_frame(1, 120, 30), 15)

    def test_fractional_beats_round_without_drift(self):
        # 128 BPM at 30 fps = 14.0625 frames per beat; beat 16 lands on 225 exactly
        self.assertEqual(beat_frame(4, 128, 30), 56)
        self.assertEqual(beat_frame(16, 128, 30), 225)


if __name__ == "__main__":
    unittest.main()
