"""validate-scene accepts a good scene.json and names the rule each bad one breaks."""
import copy
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
SCRIPT = SKILL / "scripts" / "validate-scene"
VALID_DIR = Path(__file__).resolve().parent / "fixtures" / "valid"
VALID = json.loads((VALID_DIR / "scene.json").read_text())

sys.path.insert(0, str(SKILL / "scripts"))
from scene_schema import PRESETS, QUALITIES, resolve_size  # noqa: E402

EXPECTED_SIZES = {  # ratio: (low, medium, high) — the short side is 720 / 1080 / 1440
    "9:16": ((720, 1280), (1080, 1920), (1440, 2560)),
    "16:9": ((1280, 720), (1920, 1080), (2560, 1440)),
    "1:1": ((720, 720), (1080, 1080), (1440, 1440)),
    "4:5": ((720, 900), (1080, 1350), (1440, 1800)),
    "5:4": ((900, 720), (1350, 1080), (1800, 1440)),
    "2:3": ((720, 1080), (1080, 1620), (1440, 2160)),
    "3:2": ((1080, 720), (1620, 1080), (2160, 1440)),
}


def validate(scene):
    """Run the script on <scene> written next to a copy of the fixture assets."""
    with tempfile.TemporaryDirectory() as tmp:
        shutil.copytree(VALID_DIR / "assets", Path(tmp) / "assets")
        path = Path(tmp) / "scene.json"
        path.write_text(json.dumps(scene))
        r = subprocess.run([str(SCRIPT), str(path)], capture_output=True, text=True)
    return r.returncode, json.loads(r.stdout)


def mutated(fn):
    scene = copy.deepcopy(VALID)
    fn(scene)
    return scene


class PresetTableTest(unittest.TestCase):
    def test_all_21_preset_quality_sizes(self):
        self.assertEqual(sorted(PRESETS), sorted(EXPECTED_SIZES))
        self.assertEqual(QUALITIES, ("low", "medium", "high"))
        for ratio, sizes in EXPECTED_SIZES.items():
            for quality, size in zip(QUALITIES, sizes):
                self.assertEqual(resolve_size({"preset": ratio, "quality": quality}), size, (ratio, quality))

    def test_sizes_are_even_and_never_repeat(self):
        sizes = [resolve_size({"preset": r, "quality": q}) for r in PRESETS for q in QUALITIES]
        self.assertEqual(len(sizes), 21)
        self.assertEqual(len(set(sizes)), 21)
        self.assertTrue(all(w % 2 == 0 and h % 2 == 0 for w, h in sizes))

    def test_quality_defaults_to_medium(self):
        self.assertEqual(resolve_size({"preset": "4:5"}), (1080, 1350))


class ValidateSceneTest(unittest.TestCase):
    def assertRule(self, scene, rule):
        code, out = validate(scene)
        self.assertNotEqual(code, 0, out)
        self.assertFalse(out["ok"])
        self.assertIn(rule, [e["rule"] for e in out["errors"]], out)

    def test_valid_scene_passes_in_several_formats(self):
        def free_size(s):
            del s["preset"], s["quality"]
            s["width"], s["height"] = 1000, 600

        cases = {
            "9:16": (VALID, (1080, 1920)),
            "16:9 low": (mutated(lambda s: s.update(preset="16:9", quality="low")), (1280, 720)),
            "1:1 high": (mutated(lambda s: s.update(preset="1:1", quality="high")), (1440, 1440)),
            "free": (mutated(free_size), (1000, 600)),
        }
        for name, (scene, size) in cases.items():
            code, out = validate(scene)
            self.assertEqual(code, 0, (name, out))
            self.assertEqual((out["width"], out["height"]), size, name)
            self.assertEqual(out["frames"], 180, name)

    def test_odd_or_absurd_free_sizes_are_rejected(self):
        def size(w, h):
            def fn(s):
                del s["preset"]
                s["width"], s["height"] = w, h
            return fn
        self.assertRule(mutated(size(1081, 1920)), "even-size")
        self.assertRule(mutated(size(64, 64)), "size-limits")
        self.assertRule(mutated(size(8000, 1080)), "size-limits")

    def test_unknown_preset_or_quality(self):
        self.assertRule(mutated(lambda s: s.update(preset="7:3")), "format")
        self.assertRule(mutated(lambda s: s.update(quality="ultra")), "format")

    def test_fps_and_duration_limits(self):
        self.assertRule(mutated(lambda s: s.update(fps=500)), "fps")
        self.assertRule(mutated(lambda s: s.update(duration=0)), "duration")

    def test_duration_off_the_beat_grid_is_rejected_with_nearest_values(self):
        def off(s):
            s["duration"] = 6.2
            s["scenes"][-1]["end"] = 6.2
        code, out = validate(mutated(off))
        self.assertNotEqual(code, 0)
        grid = [e for e in out["errors"] if e["rule"] == "beat-grid"]
        self.assertTrue(grid, out)
        self.assertIn("6", grid[0]["message"])
        self.assertIn("6.5", grid[0]["message"])

    def test_cut_off_the_beat(self):
        def off(s):
            s["scenes"][0]["end"] = 2.1
            s["scenes"][1]["start"] = 2.1
        self.assertRule(mutated(off), "cut-on-beat")

    def test_scenes_must_tile_the_timeline(self):
        self.assertRule(mutated(lambda s: s["scenes"][1].update(start=2.5)), "timeline")

    def test_more_than_two_font_sizes(self):
        third = {"component": "text", "text": "and calmer weekends", "size": "l"}  # scene has xl + m already
        self.assertRule(mutated(lambda s: s["scenes"][1]["elements"].append(third)), "font-sizes")

    def test_hold_is_a_beat_count_inside_the_scene(self):
        self.assertEqual(validate(mutated(lambda s: s["scenes"][0].update(hold=2)))[0], 0)
        self.assertRule(mutated(lambda s: s["scenes"][0].update(hold=0)), "hold")
        self.assertRule(mutated(lambda s: s["scenes"][0].update(hold=9)), "hold")  # scene is 4 beats

    def test_safe_area_overrides_are_relative_margins(self):
        self.assertEqual(validate(mutated(lambda s: s.update(safe_area={"bottom": 0.2})))[0], 0)
        self.assertRule(mutated(lambda s: s.update(safe_area={"bottom": 60})), "safe-area")
        self.assertRule(mutated(lambda s: s.update(safe_area={"middle": 0.1})), "safe-area")

    def test_caps_centered_on_gradient(self):
        self.assertRule(
            mutated(lambda s: s["scenes"][1]["elements"][1].update(text="FEWER TICKETS", align="center")),
            "caps-center-gradient")

    def test_stagger_outside_2_to_4_frames(self):
        self.assertRule(mutated(lambda s: s["scenes"][0].update(stagger=1)), "stagger")
        self.assertRule(mutated(lambda s: s["scenes"][0].update(stagger=5)), "stagger")

    def test_hero_without_a_real_asset(self):
        self.assertRule(mutated(lambda s: s["scenes"][0]["elements"][1].update(asset="assets/missing.png")),
                        "hero-asset")
        self.assertRule(mutated(lambda s: s["scenes"][0]["elements"][0].update(hero=True)), "hero-asset")

    def test_unknown_component(self):
        self.assertRule(mutated(lambda s: s["scenes"][0]["elements"].append({"component": "sparkles"})),
                        "component")

    def test_box_must_be_relative_to_the_frame(self):
        self.assertRule(mutated(lambda s: s["scenes"][0]["elements"][1].update(box=[100, 900, 800, 400])),
                        "relative-box")

    def test_graphic_components_validate_their_fields(self):
        graphics = [{"component": "shape", "shape": "blob", "from": "left", "spin": 90, "opacity": 0.5},
                    {"component": "draw", "stroke": "underline", "reveal": "wipe"},
                    {"component": "morph", "from": "circle", "to": "heart", "after": 1},
                    {"component": "icon", "icon": "check", "disc": True, "color": "#ffb547"},
                    {"component": "ring", "value": 72},
                    {"component": "line-chart", "points": [1, 3, 2, 6]},
                    {"component": "burst", "seed": 4, "count": 30, "box": [0.2, 0.2, 0.6, 0.6]}]

        def add(s):
            s["palette"] = ["#ffb547", "#4cc9a6"]
            s["background"] = {"color": "#101418", "shapes": 4, "grain": True}
            s["scenes"][2]["elements"] = graphics
        self.assertEqual(validate(mutated(add))[0], 0, validate(mutated(add))[1])
        bad = {"icon": {"component": "icon", "icon": "rocket"},
               "stroke": {"component": "draw", "stroke": "zigzag"},
               "color": {"component": "shape", "color": "red"},
               "reveal": {"component": "text", "text": "x", "reveal": "fade"},
               "points": {"component": "line-chart", "points": [1]},
               "count": {"component": "burst", "count": 500},
               "ring": {"component": "ring", "value": 140},
               "morph": {"component": "morph", "from": "circle", "to": "cube"}}
        for name, el in bad.items():
            with self.subTest(name):
                self.assertRule(mutated(lambda s: s["scenes"][2]["elements"].append(el)), "component")
        self.assertRule(mutated(lambda s: s.update(palette=["#fff"])), "palette")
        self.assertRule(mutated(lambda s: s.update(background={"color": "#101418", "shapes": 9})), "background")

    def test_every_scene_needs_a_graphic_unless_marked_text_only(self):
        text_only = [{"component": "title", "text": "Only words"}, {"component": "text", "text": "and more words"}]
        self.assertRule(mutated(lambda s: s["scenes"][2].update(elements=text_only)), "graphic-richness")
        self.assertEqual(validate(mutated(lambda s: s["scenes"][2].update(elements=text_only, text_only=True)))[0], 0)
        self.assertRule(mutated(lambda s: s["scenes"][2].update(text_only="yes")), "graphic-richness")


if __name__ == "__main__":
    unittest.main()
