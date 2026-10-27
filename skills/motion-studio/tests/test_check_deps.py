"""check-deps fails fast on a missing tool or codec and installs Chromium when absent.

Every external command is faked inside a private PATH, so no case needs the
network or the real ffmpeg/uv.
"""
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "check-deps"
SYSTEM_TOOLS = ("bash", "env", "uname", "grep", "cat", "dirname", "ls", "mkdir", "printf")
ENCODERS = " V....D libx264  H.264\n V....D libvpx-vp9  VP9\n A....D libopus  Opus\n A....D aac  AAC\n"


class CheckDepsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        self.browsers = self.tmp / "browsers"
        self.uv_log = self.tmp / "uv.log"
        for tool in SYSTEM_TOOLS:
            os.symlink(shutil.which(tool), self.bin / tool)

    def fake(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/bash\n" + body + "\n")
        path.chmod(0o755)

    def fake_ffmpeg(self, encoders=ENCODERS):
        self.fake("ffmpeg", f"printf '%s' '{encoders}'")
        self.fake("ffprobe", "exit 0")

    def fake_uv(self, installs=True):
        make = f'mkdir -p "{self.browsers}/chromium_headless_shell-1"' if installs else ":"
        self.fake("uv", f'echo "$*" >> "{self.uv_log}"\n{make}')

    def run_script(self):
        env = {"PATH": str(self.bin), "HOME": str(self.tmp), "PLAYWRIGHT_BROWSERS_PATH": str(self.browsers)}
        return subprocess.run([str(SCRIPT)], env=env, capture_output=True, text=True)

    def test_missing_ffmpeg_fails_with_instruction(self):
        self.fake_uv()
        self.fake("ffprobe", "exit 0")
        r = self.run_script()
        self.assertNotEqual(r.returncode, 0)
        self.assertRegex(r.stdout + r.stderr, r"ffmpeg.*install")

    def test_ffmpeg_without_vp9_fails(self):
        self.fake_uv()
        self.fake_ffmpeg(ENCODERS.replace("libvpx-vp9", "libvpx"))
        r = self.run_script()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("libvpx-vp9", r.stdout + r.stderr)

    def test_missing_chromium_is_installed_through_uv(self):
        self.fake_uv()
        self.fake_ffmpeg()
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        args = self.uv_log.read_text()
        self.assertRegex(args, r"run .*--with playwright==\d+\.\d+\.\d+ playwright install chromium")

    def test_chromium_still_missing_after_install_fails(self):
        self.fake_uv(installs=False)
        self.fake_ffmpeg()
        self.assertNotEqual(self.run_script().returncode, 0)

    def test_present_chromium_skips_install(self):
        self.fake_uv()
        self.fake_ffmpeg()
        (self.browsers / "chromium-1").mkdir(parents=True)
        self.assertEqual(self.run_script().returncode, 0)
        self.assertFalse(self.uv_log.exists())

    def test_never_mentions_pip(self):
        self.assertNotRegex(SCRIPT.read_text(), r"\bpip\b")


if __name__ == "__main__":
    unittest.main()
