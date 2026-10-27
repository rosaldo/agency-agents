"""SKILL.md stays short, uses only documented frontmatter, and gates the render on approval."""
import re
import unittest
from pathlib import Path

SKILL_MD = Path(__file__).resolve().parents[1] / "SKILL.md"
ALLOWED = {"name", "description", "argument-hint", "allowed-tools", "metadata"}


class SkillMdTest(unittest.TestCase):
    def setUp(self):
        self.text = SKILL_MD.read_text()
        self.front, self.body = re.match(r"---\n(.*?)\n---\n(.*)", self.text, re.S).groups()
        self.fields = dict(re.findall(r"^([a-z-]+):\s*(.*)$", self.front, re.M))

    def test_short(self):
        self.assertLessEqual(len(self.text.splitlines()), 150)

    def test_frontmatter_fields(self):
        self.assertTrue(set(self.fields) <= ALLOWED, set(self.fields) - ALLOWED)
        self.assertEqual(self.fields["name"], "motion-studio")
        self.assertIn("agent: short-video-editing-coach", self.fields["metadata"])
        self.assertIn("${CLAUDE_SKILL_DIR}/scripts/", self.fields["allowed-tools"])
        self.assertLessEqual(len(self.fields["description"]) + len(self.fields.get("argument-hint", "")), 1536)

    def test_approval_comes_before_render(self):
        approval = re.search(r"(?i)approv", self.body)
        render = re.search(r"scripts/render\b", self.body)
        self.assertTrue(approval and render, "needs an approval step and the render step")
        self.assertLess(approval.start(), render.start())

    def test_flow_order(self):
        steps = [f"scripts/{s}" for s in ("check-deps", "validate-scene", "render", "synth-audio", "encode",
                                          "check-video", "contact-sheet")]
        positions = [self.body.find(s) for s in steps]
        self.assertNotIn(-1, positions, dict(zip(steps, positions)))
        self.assertEqual(positions, sorted(positions))

    def test_links_every_reference(self):
        for ref in sorted((SKILL_MD.parent / "references").glob("*.md")):
            self.assertIn(f"references/{ref.name}", self.body)


if __name__ == "__main__":
    unittest.main()
