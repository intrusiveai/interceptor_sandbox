"""Carry Interceptor release outputs through the real local catalog publisher.

GitHub, S3, and public HTTP are fixtures; no production writes are performed.
"""

import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import unittest
from unittest.mock import patch

import test_candidate_workflow as candidate_fixture
import test_release_catalog as catalog_fixture

ROOT = Path(__file__).resolve().parents[2]
PUBLISHER = ROOT / "tools/release-publisher"
spec = importlib.util.spec_from_file_location(
    "publisher_fixture", PUBLISHER / "scripts/tests/test_catalog.py"
)
publisher_fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publisher_fixture)
release = catalog_fixture.release


class LocalPublisherTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Keep the vendored bundle unchanged, including its fixture helpers.
        with patch.dict(os.environ, GOWORK="off", GOPROXY="off", GOTOOLCHAIN="local"):
            publisher_fixture.InterceptorCatalogTests.setUpClass()
        cls.addClassCleanup(publisher_fixture.InterceptorCatalogTests.tearDownClass)

    def test_candidate_replacement_and_qualified_promotion(self):
        publisher = publisher_fixture.InterceptorCatalogTests()
        publisher.setUp()
        self.addCleanup(publisher.doCleanups)
        candidate = candidate_fixture.CandidateWorkflowTest()
        candidate.setUp()
        self.addCleanup(candidate.doCleanups)
        github = catalog_fixture.CatalogGitHub()
        candidate.github = github
        original_api = github.api

        def dispatch_to_local_publisher(path, method="GET", data=None, missing=False):
            result = original_api(path, method, data, missing)
            if path.endswith("publish-catalog.yml/dispatches"):
                publisher.publish(github.public_catalog)
            return result

        github.api = dispatch_to_local_publisher
        history = publisher.entry("v1.0.0", tag="release", digest="c")
        publisher.publish(history)
        candidate.publish()
        release.sync_catalog(github, "v1.2.3", candidate_fixture.FIRST)
        first = json.loads(publisher.get(publisher.key))["releases"][0]
        self.assertEqual(first["tags"], ["rc"])

        github.head = candidate_fixture.SECOND
        candidate.publish(candidate_fixture.SECOND)
        release.sync_catalog(github, "v1.2.3", candidate_fixture.SECOND)
        rebuilt = json.loads(publisher.get(publisher.key))["releases"][0]
        self.assertNotEqual(first["artifacts"], rebuilt["artifacts"])
        self.assertEqual(len(rebuilt["artifacts"]), 4)
        for artifact in rebuilt["artifacts"]:
            self.assertEqual(artifact["sha256"], hashlib.sha256(
                github.uploaded[artifact["filename"]]).hexdigest())

        archives = {name: data for name, data in github.uploaded.items()
                    if name != release.CATALOG_ASSET}
        github.pr = candidate_fixture.pull(candidate_fixture.SECOND)
        release.promote(github, 42)
        release.sync_catalog(github, "v1.2.3", candidate_fixture.SECOND)
        promoted = publisher.get(publisher.key)
        entries = json.loads(promoted)["releases"]
        self.assertEqual(entries[0]["tags"], ["release"])
        self.assertEqual(entries[0]["artifacts"], rebuilt["artifacts"])
        self.assertEqual(entries[1], dict(history, latest=False))
        self.assertEqual(github.tag, candidate_fixture.SECOND)
        self.assertEqual(archives, {name: data for name, data in github.uploaded.items()
                                    if name != release.CATALOG_ASSET})
        release.sync_catalog(github, "v1.2.3", candidate_fixture.SECOND)
        self.assertEqual(publisher.get(publisher.key), promoted)

        publisher.publish(rebuilt, success=False)  # Stale candidate cannot demote.
        changed = copy.deepcopy(entries[0])
        changed["artifacts"][0]["sha256"] = "f" * 64
        publisher.publish(changed, success=False)
        self.assertEqual(publisher.get(publisher.key), promoted)

    def test_complete_reviewed_export_matches_provenance(self):
        manifest = json.loads((PUBLISHER / "publisher-source.json").read_text())
        self.assertEqual(manifest["product"], "interceptor_sandbox")
        self.assertFalse(manifest["working_tree"])
        self.assertEqual(manifest["source_revision"],
                         "c651c5fa47346f1ca02abe33791667f838b59e03")
        self.assertEqual(len(manifest["sha256"]), 11)
        for name, digest in manifest["sha256"].items():
            self.assertEqual(hashlib.sha256((PUBLISHER / name).read_bytes()).hexdigest(),
                             digest, name)


if __name__ == "__main__":
    unittest.main()
