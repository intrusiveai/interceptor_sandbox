"""Exercise candidate and stable release catalog publication."""

import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import publish_release as release
import test_candidate_workflow as workflow_fixture
from test_candidate_workflow import FIRST, GitHubFixture, pull


class CatalogGitHub(GitHubFixture):
    def __init__(self):
        super().__init__()
        self.request = None
        self.conclusion = "success"
        self.dispatched = []
        self.public_catalog = None
        self.never_started = False
        self.status = "completed"
        self.cancelled = False

    def download_catalog(self, tag):
        return json.loads(self.uploaded[release.CATALOG_ASSET])

    def api(self, path, method="GET", data=None, missing=False):
        if path.endswith("publish-catalog.yml/dispatches"):
            self.request = data["inputs"]["request_id"]
            self.dispatched.append(data)
            entry = release.catalog_context(self, data["ref"], self.tag)
            if self.conclusion == "success":
                self.public_catalog = entry
            return None
        if "/runs?event=" in path:
            return {"workflow_runs": [] if self.never_started else [self.run()]}
        if path == "actions/runs/123":
            return self.run()
        if path == "actions/runs/123/cancel":
            self.cancelled = True
            return None
        return super().api(path, method, data, missing)

    def run(self):
        return {
            "id": 123,
            "display_title": self.request,
            "head_sha": self.tag,
            "status": self.status,
            "conclusion": self.conclusion,
            "html_url": (
                "https://github.com/intrusiveai/interceptor_sandbox/actions/runs/123"
            ),
        }


class CatalogTest(unittest.TestCase):
    def setUp(self):
        self.fixture = workflow_fixture.CandidateWorkflowTest()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.github = CatalogGitHub()
        self.fixture.github = self.github
        self.fixture.publish()

    def test_candidate_then_stable_catalog_preserves_release_archives(self):
        release_files = dict(self.github.uploaded)
        result = release.sync_catalog(self.github, "v1.2.3", FIRST)
        self.assertTrue(result.endswith("/123"))
        candidate = self.github.public_catalog
        self.assertEqual(candidate["tags"], ["rc"])
        self.assertEqual(
            {artifact["platform"] for artifact in candidate["artifacts"]},
            release.PLATFORMS,
        )
        for artifact in candidate["artifacts"]:
            self.assertEqual(
                artifact["sha256"],
                release.hashlib.sha256(release_files[artifact["filename"]]).hexdigest(),
            )

        self.github.pr = pull(FIRST)
        release.promote(self.github, 42)
        release.sync_catalog(self.github, "v1.2.3", FIRST)
        stable = self.github.public_catalog
        self.assertEqual(stable["tags"], ["release"])
        self.assertEqual(stable["artifacts"], candidate["artifacts"])
        actual_release_files = {
            name: data
            for name, data in self.github.uploaded.items()
            if name != release.CATALOG_ASSET
        }
        self.assertEqual(actual_release_files, release_files)
        self.assertEqual(self.github.dispatched[-1]["ref"], "v1.2.3")

        release.sync_catalog(self.github, "v1.2.3", FIRST)
        self.assertEqual(self.github.public_catalog["tags"], ["release"])

    def test_failed_or_skipped_catalog_run_is_not_success(self):
        for conclusion in ("failure", "skipped", "cancelled"):
            self.github.conclusion = conclusion
            with self.assertRaisesRegex(RuntimeError, "publication failed"):
                release.sync_catalog(self.github, "v1.2.3", FIRST)
        self.github.conclusion = "success"
        release.sync_catalog(self.github, "v1.2.3", FIRST)
        self.assertEqual(self.github.public_catalog["tags"], ["rc"])

    def test_stale_source_entry_and_wrong_product_do_not_reach_service(self):
        with self.assertRaisesRegex(ValueError, "source/tag"):
            release.sync_catalog(self.github, "v1.2.3", "b" * 40)
        release.sync_catalog(self.github, "v1.2.3", FIRST)
        entry = self.github.download_catalog("v1.2.3")
        entry["tags"] = ["release"]
        self.github.uploaded[release.CATALOG_ASSET] = json.dumps(entry).encode()
        with self.assertRaisesRegex(ValueError, "stale"):
            release.catalog_context(self.github, "v1.2.3", FIRST)
        self.github.repository = "intrusiveai/operator_sandbox"
        with self.assertRaisesRegex(ValueError, "restricted"):
            release.catalog_entry(self.github, "v1.2.3", FIRST)

    def test_no_observed_run_is_not_publication_success(self):
        self.github.never_started = True
        with patch.object(release.time, "monotonic", side_effect=[0, 0, 2]), patch.object(
            release.time, "sleep"
        ):
            with self.assertRaisesRegex(RuntimeError, "timed out"):
                release.sync_catalog(self.github, "v1.2.3", FIRST, timeout=1)

    def test_stable_catalog_requires_verified_candidate_promotion(self):
        self.github.current["prerelease"] = False
        with self.assertRaisesRegex(ValueError, "verified candidate PR"):
            release.catalog_entry(self.github, "v1.2.3", FIRST)
        self.github.pr = pull(FIRST)
        release.promote(self.github, 42)
        self.assertEqual(
            release.catalog_entry(self.github, "v1.2.3", FIRST)["tags"],
            ["release"],
        )

    def test_known_running_job_is_cancelled_on_timeout(self):
        self.github.status = "in_progress"
        with patch.object(release.time, "monotonic", side_effect=[0, 0, 2]), patch.object(
            release.time, "sleep"
        ):
            with self.assertRaisesRegex(RuntimeError, "timed out"):
                release.sync_catalog(self.github, "v1.2.3", FIRST, timeout=1)
        self.assertTrue(self.github.cancelled)

    def test_cross_product_asset_url_is_rejected(self):
        original = self.github.assets

        def altered(item):
            assets = original(item)
            for asset in assets:
                if asset["name"] == "darwin-amd64.tar.gz":
                    asset["browser_download_url"] = "https://example.invalid/archive.tar.gz"
            return assets

        self.github.assets = altered
        with self.assertRaisesRegex(ValueError, "URL"):
            release.catalog_entry(self.github, "v1.2.3", FIRST)


if __name__ == "__main__":
    unittest.main()
