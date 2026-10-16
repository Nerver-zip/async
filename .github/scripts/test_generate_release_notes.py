"""Tests for automatic GitHub release-note generation."""

import importlib.util
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("generate_release_notes.py")
SPEC = importlib.util.spec_from_file_location("generate_release_notes", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
release_notes = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release_notes)


class ReleaseNotesTests(unittest.TestCase):
    def test_categorizes_commits_and_links_short_sha_and_comparison(self):
        sha_feature = "a" * 40
        sha_security = "b" * 40
        sha_fix = "c" * 40
        sha_other = "d" * 40
        log = "\n".join(
            (
                f"{sha_feature}\tfeat(cli): add live sync",
                f"{sha_security}\tfix(security)!: escape release subject",
                f"{sha_fix}\tfix: keep the native fallback",
                f"{sha_other}\tunknown title [check]",
            )
        )

        with patch.object(release_notes, "git", return_value=log) as git:
            rendered = release_notes.render(
                "v0.1.1", "v0.1.0", "Nerver-zip/async"
            )

        git.assert_called_once_with(
            "log",
            "--first-parent",
            "--no-merges",
            "--format=%H%x09%s",
            "v0.1.0..v0.1.1",
        )
        self.assertIn(
            "Changes since [v0.1.0](https://github.com/Nerver-zip/async/compare/v0.1.0...v0.1.1)",
            rendered,
        )
        self.assertIn("## 🚀 Features\n\n- add live sync ([aaaaaaa]", rendered)
        self.assertIn("## 🔒 Security\n\n- escape release subject (breaking change)", rendered)
        self.assertIn("[bbbbbbb](https://github.com/Nerver-zip/async/commit/", rendered)
        self.assertIn("## 🐛 Bug Fixes\n\n- keep the native fallback ([ccccccc]", rendered)
        self.assertIn(r"unknown title \[check\] ([ddddddd]", rendered)

    def test_first_release_omits_compare_link_and_handles_empty_history(self):
        with patch.object(release_notes, "git", return_value="") as git:
            rendered = release_notes.render("v0.1.0", None, "Nerver-zip/async")

        git.assert_called_once_with(
            "log",
            "--first-parent",
            "--no-merges",
            "--format=%H%x09%s",
            "v0.1.0",
        )
        self.assertIn("# v0.1.0", rendered)
        self.assertNotIn("Changes since", rendered)
        self.assertIn("No categorized commits were found.", rendered)

    def test_missing_predecessor_tag_is_treated_as_first_release(self):
        failure = subprocess.CalledProcessError(128, ["git", "describe"])
        with patch.object(release_notes, "git", side_effect=failure):
            self.assertIsNone(release_notes.previous_tag("v0.1.0"))


if __name__ == "__main__":
    unittest.main()
