#!/usr/bin/env python3
"""Check the release handoff without running Jenkins or installing anything."""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("handoff", Path(__file__).with_name("jenkins-delivery.py"))
handoff = importlib.util.module_from_spec(spec)
spec.loader.exec_module(handoff)


class HandoffTests(unittest.TestCase):
    def test_exact_main_head_must_match_origin(self):
        revision = "a" * 40
        with patch.object(handoff.subprocess, "check_output", side_effect=["main", revision, f"{revision}\trefs/heads/main\n"]) as git:
            self.assertEqual(handoff.pushed_main_revision(Path("/repo")), ("main", revision))
        self.assertEqual(git.call_count, 3)
        self.assertEqual(git.call_args_list[-1].args[0][-4:], ["ls-remote", "--exit-code", "origin", "refs/heads/main"])
        self.assertEqual(git.call_args_list[-1].kwargs["timeout"], 30)

    def test_stale_remote_tip_is_rejected(self):
        with patch.object(handoff.subprocess, "check_output", side_effect=["main", "a" * 40, f"{'b' * 40}\trefs/heads/main\n"]):
            with self.assertRaisesRegex(SystemExit, "origin/main"):
                handoff.pushed_main_revision(Path("/repo"))

    def test_non_main_branch_is_rejected(self):
        with patch.object(handoff.subprocess, "check_output", return_value="feature"):
            with self.assertRaisesRegex(SystemExit, "main branch"):
                handoff.pushed_main_revision(Path("/repo"))

    def test_exact_pushed_revision_is_submitted_without_local_release(self):
        revision = "a" * 40
        with patch.object(handoff, "pushed_main_revision", return_value=("main", revision)), \
                patch.object(handoff.Path, "is_file", return_value=True), \
                patch.object(handoff.subprocess, "run") as execute:
            execute.return_value.returncode = 0
            with self.assertRaises(SystemExit) as result:
                handoff.main()
            self.assertEqual(result.exception.code, 0)
            argv = execute.call_args.args[0]
            self.assertEqual(argv[-5:], ["deploy", "--profile", "one-shot-tally", "--revision", revision])
            self.assertTrue(argv[1].endswith("/jenkins-local/one-shot-tally-delivery.py"))
            self.assertEqual(execute.call_args.kwargs["timeout"], 295)
            self.assertEqual(execute.call_args.kwargs["cwd"], handoff.Path(handoff.__file__).resolve().parents[1])
            self.assertEqual(execute.call_args.kwargs["env"]["DEPLOY_IT_COMMIT"], revision)
            self.assertEqual(execute.call_args.kwargs["env"]["DEPLOY_IT_BRANCH"], "main")
            self.assertEqual(execute.call_args.kwargs["env"]["DEPLOY_IT_ENVIRONMENT"], "local-mac")

    def test_branch_mismatch_cannot_trigger_delivery(self):
        with patch.object(handoff, "pushed_main_revision", side_effect=SystemExit("main branch")), \
                patch.object(handoff.subprocess, "run") as execute:
            with self.assertRaisesRegex(SystemExit, "main branch"):
                handoff.main()
            execute.assert_not_called()

    def test_remote_mismatch_cannot_trigger_delivery(self):
        with patch.object(handoff, "pushed_main_revision", side_effect=SystemExit("origin/main")), \
                patch.object(handoff.subprocess, "run") as execute:
            with self.assertRaisesRegex(SystemExit, "origin/main"):
                handoff.main()
            execute.assert_not_called()

    def test_failure_is_not_reported_as_deployment(self):
        with patch.object(handoff, "pushed_main_revision", return_value=("main", "b" * 40)), \
                patch.object(handoff.Path, "is_file", return_value=True), \
                patch.object(handoff.subprocess, "run") as execute:
            execute.return_value.returncode = 7
            with self.assertRaises(SystemExit) as result:
                handoff.main()
            self.assertEqual(result.exception.code, 7)


if __name__ == "__main__":
    unittest.main()
