"""Release notes must carry reviewed upgrade notices into the published body."""

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("release_metadata.py")
NOTICE = "### 升级必读\n\n- 新版本首次启动会迁移旧持久化文件。\n"


class ReleaseNotesTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = self.root / "release-notes.md"
        self.manifest = {
            "version": "0.2.6",
            "packages": [{"identifier": "ghcr.io/open-mcp-ai/termcp:0.2.6"}],
        }
        self.write_fixture()

    def write_fixture(self, pending="", release_body=NOTICE):
        (self.root / "server.json").write_text(
            json.dumps(self.manifest), encoding="utf-8"
        )
        (self.root / "CHANGELOG.md").write_text(
            "# Changelog\n\n## Unreleased\n\n"
            + pending
            + "\n## v0.2.6 — 2026-10-07\n\n"
            + release_body
            + "\n## v0.2.5 — 2026-10-01\n\n- Previous release only.\n",
            encoding="utf-8",
        )

    def run_script(self, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--root", str(self.root), *args],
            capture_output=True,
            encoding="utf-8",
            timeout=30,
        )

    def export(self, version="v0.2.6"):
        return self.run_script(
            "notes", "--expected", version, "--output", str(self.output)
        )

    def test_notes_preserve_notice_and_exclude_other_sections(self):
        result = self.export()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_text(encoding="utf-8"), NOTICE)

    def test_wrong_tag_does_not_overwrite_existing_output(self):
        self.output.write_text("keep existing notes", encoding="utf-8")
        result = self.export("v0.3.0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("files describe v0.2.6", result.stderr)
        self.assertEqual(
            self.output.read_text(encoding="utf-8"), "keep existing notes"
        )

    def test_unreleased_changes_block_notes_export(self):
        self.write_fixture(pending="- Pending change.\n")
        result = self.export()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("move Unreleased notes", result.stderr)
        self.assertFalse(self.output.exists())

    def test_invalid_metadata_blocks_notes_export(self):
        self.manifest["version"] = "0.2.5"
        self.write_fixture()
        result = self.export()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("server.json version", result.stderr)
        self.assertFalse(self.output.exists())

    def test_prepare_and_export_keep_migration_notice(self):
        self.write_fixture(pending=NOTICE, release_body="- Previous current note.\n")
        prepared = self.run_script("prepare", "v0.3.0", "--date", "2026-10-15")
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        exported = self.export("v0.3.0")
        self.assertEqual(exported.returncode, 0, exported.stderr)
        self.assertEqual(self.output.read_text(encoding="utf-8"), NOTICE)


if __name__ == "__main__":
    unittest.main()
