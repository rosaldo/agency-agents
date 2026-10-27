"""lib/ animates only through the spring and the frame time, in frame-relative units.

A static scan: CSS transitions/animations/easing curves, real-time clocks and
fixed pixel sizes are forbidden in lib/. The forbidden fixture proves the scan
catches each of them.
"""
import re
import sys
import unittest
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
LIB = SKILL / "lib"
FORBIDDEN_FIXTURE = Path(__file__).resolve().parent / "fixtures" / "forbidden"

sys.path.insert(0, str(SKILL / "scripts"))
from scene_schema import COMPONENTS, TEXT_SIZES  # noqa: E402

RULES = {
    "css-transition": re.compile(r"\btransition"),
    "css-animation": re.compile(r"\banimation|@keyframes"),
    "easing-curve": re.compile(r"\bease|cubic-bezier|\blinear\b(?!-gradient)"),
    "frame-callback": re.compile(r"requestAnimationFrame"),
    "real-time-clock": re.compile(r"Date\.now|performance\.now|new Date\b"),
    "fixed-pixels": re.compile(r"\b\d+(\.\d+)?px\b"),
}


def scan(directory):
    hits = []
    for path in sorted(directory.rglob("*")):
        if path.suffix not in (".js", ".css", ".html"):
            continue
        for no, line in enumerate(path.read_text().splitlines(), 1):
            hits += [(rule, f"{path.name}:{no}") for rule, rx in RULES.items() if rx.search(line)]
    return hits


class LibRulesTest(unittest.TestCase):
    def test_scan_catches_the_forbidden_fixture(self):
        found = {rule for rule, _ in scan(FORBIDDEN_FIXTURE)}
        self.assertEqual(found, {"css-transition", "easing-curve", "real-time-clock", "fixed-pixels"})

    def test_lib_is_clean(self):
        self.assertEqual(scan(LIB), [])

    def test_lib_registers_every_schema_component(self):
        source = (LIB / "components.js").read_text()
        registered = set(re.findall(r'^\s*Components\["([a-z-]+)"\]', source, re.M))
        self.assertEqual(registered, set(COMPONENTS))

    def test_text_sizes_mirror_the_schema(self):
        source = (LIB / "components.js").read_text()
        block = re.search(r"TEXT_SIZES = \{([^}]*)\}", source).group(1)
        mirrored = {k: float(v) for k, v in re.findall(r"([a-z]+):\s*([0-9.]+)", block)}
        self.assertEqual(mirrored, TEXT_SIZES)


if __name__ == "__main__":
    unittest.main()
