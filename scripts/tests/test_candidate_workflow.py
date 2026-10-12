"""Exercise candidate push -> rolling release -> merged PR promotion."""

import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))
import build_release as builder
import publish_release as release

REPO = "intrusiveai/interceptor_sandbox"
FIRST, SECOND, MERGE = "a" * 40, "b" * 40, "c" * 40
BRANCH = "candidate-v1.2.3"


def pull(source=SECOND, merged=True):
    return {
        "number": 42,
        "merged": merged,
        "head": {"ref": BRANCH, "sha": source, "repo": {"full_name": REPO}},
        "base": {"ref": "main"},
        "merge_commit_sha": MERGE,
    }


class GitHubFixture:
    repository = REPO

    def __init__(self, version="1.2.3"):
        self.version = version
        self.branch = "candidate-v" + version
        self.head = FIRST
        self.tag = None
        self.current = None
        self.uploaded = {}
        self.pr = pull()
        self.tree_matches = True
        self.fail_upload = False
        self.calls = []

    def release(self, tag):
        return copy.deepcopy(self.current)

    def assets(self, item):
        return [
            {
                "id": index,
                "name": name,
                "state": "uploaded",
                "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
            }
            for index, (name, data) in enumerate(sorted(self.uploaded.items()))
        ]

    def upload(self, tag, files):
        for index, path in enumerate(files):
            self.uploaded[path.name] = path.read_bytes()
            if self.fail_upload and index == 1:
                raise RuntimeError("simulated upload interruption")

    def api(self, path, method="GET", data=None, missing=False):
        self.calls.append((path, method, copy.deepcopy(data)))
        if path == "git/ref/heads/" + self.branch:
            return {"object": {"sha": self.head}}
        if path == "git/ref/tags/v" + self.version:
            return {"object": {"sha": self.tag}} if self.tag else None
        if path == "git/refs" and method == "POST":
            self.tag = data["sha"]
            return {}
        if path == "git/refs/tags/v" + self.version and method == "PATCH":
            self.tag = data["sha"]
            return {}
        if path == "releases" and method == "POST":
            self.current = dict(data, id=1)
            return copy.deepcopy(self.current)
        if path == "releases/1" and method == "PATCH":
            self.current.update(data)
            return copy.deepcopy(self.current)
        if path.startswith("releases/assets/") and method == "DELETE":
            self.uploaded.pop(self.deletion_names[int(path.rsplit("/", 1)[1])])
            return None
        if path == "pulls/42":
            return self.pr
        if path == "commits/v" + self.version:
            return {"sha": self.tag}
        if path.startswith("git/commits/"):
            tree = "same-tree"
            if not self.tree_matches and path == "git/commits/" + MERGE:
                tree = "different-tree"
            return {"tree": {"sha": tree}}
        raise AssertionError((path, method, data))

    def prepare_replacement(self):
        self.deletion_names = sorted(self.uploaded)


class CandidateWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.github = GitHubFixture()

    def publish(self, source=FIRST):
        context = release.event_context({}, "push", "refs/heads/" + BRANCH, source, REPO)
        self.assertEqual(context["mode"], "publish")
        files = []
        for name in sorted(release.expected_assets()):
            path = self.root / name
            path.write_bytes((name + ":" + source).encode())
            files.append(path)
        self.github.prepare_replacement()
        return release.publish(
            self.github,
            context["branch"],
            context["source"],
            {"version": context["version"], "source_commit": source},
            files,
        )

    def test_assemble_validates_and_records_all_platform_archives(self):
        candidates = self.root / "candidates"
        candidates.mkdir()
        executable = b"interceptor executable"
        for platform in sorted(builder.PLATFORMS):
            metadata = {
                "version": "1.2.3",
                "source_commit": FIRST,
                "platform": platform,
                "binary_sha256": hashlib.sha256(executable).hexdigest(),
                "toolchain": builder.TOOLCHAIN,
                "cgo_enabled": False,
            }
            builder.archive(
                candidates / (platform.replace("/", "-") + ".tar.gz"),
                (
                    ("interceptor", executable, 0o755),
                    ("release.json", (json.dumps(metadata) + "\n").encode(), 0o644),
                    ("LICENSE", b"license", 0o644),
                ),
            )
        inventory, files = release.assemble(candidates, self.root / "assembled")
        self.assertEqual(inventory["version"], "1.2.3")
        self.assertEqual(inventory["source_commit"], FIRST)
        self.assertEqual({path.name for path in files}, release.expected_assets())

    def test_push_replace_merge_promotes_exact_same_bytes_and_tag(self):
        self.assertEqual(self.publish(), "prerelease-published")
        self.assertTrue(self.github.current["prerelease"])
        self.assertFalse(self.github.current["draft"])
        self.github.head = SECOND
        self.assertEqual(self.publish(SECOND), "prerelease-published")
        self.assertEqual(self.github.tag, SECOND)
        before = dict(self.github.uploaded)
        context = release.event_context(
            {"action": "closed", "pull_request": pull()},
            "pull_request",
            "refs/heads/main",
            MERGE,
            REPO,
        )
        self.assertEqual(context["mode"], "promote")
        self.assertEqual(
            release.promote(self.github, int(context["pr"])),
            "promoted-existing-assets",
        )
        self.assertFalse(self.github.current["prerelease"])
        self.assertEqual(self.github.uploaded, before)
        self.assertEqual(self.github.tag, SECOND)
        self.assertEqual(release.promote(self.github, 42), "already-stable")
        with self.assertRaisesRegex(ValueError, "stable"):
            self.publish(SECOND)

    def test_old_push_never_replaces_newer_branch_head(self):
        self.github.head = SECOND
        self.assertEqual(self.publish(), "superseded")
        self.assertIsNone(self.github.current)

    def test_interrupted_upload_is_draft_and_retry_repairs_it(self):
        self.github.fail_upload = True
        with self.assertRaises(RuntimeError):
            self.publish()
        self.assertTrue(self.github.current["draft"])
        with self.assertRaisesRegex(ValueError, "incomplete"):
            release.promote(self.github, 42)
        self.github.fail_upload = False
        self.assertEqual(self.publish(), "prerelease-published")
        self.assertEqual(set(self.github.uploaded), release.expected_assets())

    def test_main_merge_must_match_candidate_source_and_tree(self):
        self.publish()
        with self.assertRaisesRegex(ValueError, "head"):
            release.promote(self.github, 42)
        self.github.pr = pull(FIRST)
        self.github.tree_matches = False
        with self.assertRaisesRegex(ValueError, "tree"):
            release.promote(self.github, 42)
        self.assertTrue(self.github.current["prerelease"])

    def test_changed_missing_or_extra_asset_blocks_promotion(self):
        self.publish()
        self.github.pr = pull(FIRST)
        original = dict(self.github.uploaded)
        for change in ("changed", "missing", "extra"):
            self.github.uploaded = dict(original)
            if change == "changed":
                self.github.uploaded["linux-amd64.tar.gz"] = b"unexpected bytes"
            elif change == "missing":
                self.github.uploaded.pop("linux-amd64.tar.gz")
            else:
                self.github.uploaded["unexpected.exe"] = b"extra"
            with self.assertRaises(ValueError):
                release.promote(self.github, 42)
            self.assertTrue(self.github.current["prerelease"])

    def test_wrong_tag_unmerged_fork_and_immutable_are_rejected(self):
        self.publish()
        self.github.pr = pull(FIRST)
        self.github.tag = SECOND
        with self.assertRaisesRegex(ValueError, "tag"):
            release.promote(self.github, 42)
        self.github.tag = FIRST
        self.github.pr["merged"] = False
        with self.assertRaises(ValueError):
            release.promote(self.github, 42)
        self.github.pr = pull(FIRST)
        self.github.pr["head"]["repo"]["full_name"] = "someone/fork"
        with self.assertRaises(ValueError):
            release.promote(self.github, 42)
        self.github.current["immutable"] = True
        with self.assertRaisesRegex(ValueError, "immutable"):
            self.publish()

    def test_candidate_version_and_event_routing(self):
        for branch in (
            "candidate-v1.2",
            "candidate-v01.2.3",
            "candidate-v1.2.3-rc1",
            "candidate-v1.2.3/x",
        ):
            with self.assertRaises(ValueError):
                release.candidate_version(branch)
        self.assertEqual(
            release.event_context({}, "push", "refs/heads/main", MERGE, REPO)["mode"],
            "verify",
        )
        self.assertEqual(
            release.event_context(
                {}, "workflow_dispatch", "refs/heads/" + BRANCH, FIRST, REPO
            )["mode"],
            "publish",
        )
        self.assertEqual(
            release.event_context(
                {"deleted": True}, "push", "refs/heads/" + BRANCH, FIRST, REPO
            )["mode"],
            "ignore",
        )
        pr = pull(merged=False)
        self.assertEqual(
            release.event_context(
                {"action": "closed", "pull_request": pr},
                "pull_request",
                "refs/heads/main",
                FIRST,
                REPO,
            )["mode"],
            "ignore",
        )
        self.assertEqual(
            release.event_context(
                {"action": "opened", "pull_request": pr},
                "pull_request",
                "refs/pull/42/merge",
                FIRST,
                REPO,
            )["version"],
            "1.2.3",
        )
        pr["base"]["ref"] = BRANCH
        context = release.event_context(
            {"action": "synchronize", "pull_request": pr},
            "pull_request",
            "refs/pull/42/merge",
            FIRST,
            REPO,
        )
        self.assertEqual((context["mode"], context["version"]), ("verify", "1.2.3"))

    def test_context_cli_produces_actions_outputs(self):
        event = self.root / "event.json"
        event.write_text("{}")
        output = self.root / "output"
        subprocess.run(
            [sys.executable, str(ROOT / "scripts/publish_release.py"), "context"],
            check=True,
            env=dict(
                os.environ,
                GITHUB_EVENT_PATH=str(event),
                GITHUB_EVENT_NAME="push",
                GITHUB_REF="refs/heads/" + BRANCH,
                GITHUB_SHA=FIRST,
                GITHUB_REPOSITORY=REPO,
                GITHUB_OUTPUT=str(output),
            ),
        )
        self.assertIn("mode=publish\nversion=1.2.3\n", output.read_text())

    def test_github_api_does_not_mistake_auth_errors_for_missing_release(self):
        github = release.GitHub(REPO)
        missing = subprocess.CompletedProcess([], 1, "", "gh: Not Found (HTTP 404)")
        with patch.object(subprocess, "run", return_value=missing):
            self.assertIsNone(github.release("v1.2.3"))
        denied = subprocess.CompletedProcess([], 1, "", "gh: Bad credentials (HTTP 401)")
        with patch.object(subprocess, "run", return_value=denied):
            with self.assertRaises(RuntimeError):
                github.release("v1.2.3")


if __name__ == "__main__":
    unittest.main()
