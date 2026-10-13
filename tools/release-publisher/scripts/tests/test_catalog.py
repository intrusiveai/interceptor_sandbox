import functools
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import unittest
import urllib.request

ROOT = Path(__file__).resolve().parents[2]


class CatalogTests(unittest.TestCase):
    product = "attack_harness"
    kind = "oci_image"
    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory()
        cls.binary = Path(cls.build.name) / "releasectl"
        subprocess.run(["go", "build", "-o", str(cls.binary), "./cmd/catalog-publisher"], cwd=ROOT, check=True)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def setUp(self):
        self.key = f"docs/releases/{self.product}.json"
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.path = Path(self.work.name)
        self.bucket = self.path / "bucket"
        self.bucket.mkdir()
        metadata = self.path / "metadata"
        metadata.mkdir()
        self.stub = self.path / "bin"
        self.stub.mkdir()
        shutil.copy2(ROOT / "scripts/tests/fake_s3.py", self.stub / "aws")
        (self.stub / "aws").chmod(0o755)
        (self.stub / "go").write_text('#!/usr/bin/env bash\nset -eu\nshift 2\nexec "$TEST_BINARY" "$@"\n')
        (self.stub / "go").chmod(0o755)
        class Handler(http.server.SimpleHTTPRequestHandler):
            def log_message(self, *args):
                pass
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(Handler, directory=str(self.bucket)))
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.origin = f"http://127.0.0.1:{self.server.server_port}"
        self.env = dict(os.environ, PATH=f"{self.stub}:{os.environ['PATH']}", TEST_BINARY=str(self.binary),
                        TEST_BUCKET=str(self.bucket), TEST_METADATA=str(metadata), RELEASE_ORIGIN=self.origin,
                        CONTENT_BUCKET="fixture", CLOUDFRONT_DISTRIBUTION_ID="fixture")

    def entry(self, version="v1.0.0", tag="rc", digest="a"):
        result = {"version": version, "latest": True, "tags": [tag]}
        if self.kind == "oci_image":
            result.update(image=f"ghcr.io/intrusiveai/attack_harness:{version}", digest="sha256:" + digest * 64)
        else:
            result["artifacts"] = [{"platform": "linux/amd64", "filename": "product.tar.gz",
                "url": f"https://github.com/intrusiveai/{self.product}/releases/download/{version}/product.tar.gz",
                "sha256": digest * 64}]
        return result

    def publish(self, entry, success=True):
        path = self.path / "entry.json"
        path.write_text(json.dumps(entry))
        result = subprocess.run(["bash", str(ROOT / "scripts/publish-catalog.sh"), str(path), self.product],
                                cwd=ROOT, env=self.env, capture_output=True, text=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def get(self, key):
        with urllib.request.urlopen(f"{self.origin}/{key}") as response:
            return response.read()

    def test_candidate_replacement_promotion_and_stale_retries(self):
        self.publish(self.entry("v0.9.0", tag="release", digest="c"))
        self.publish(self.entry())
        self.assertEqual(json.loads(self.get(self.key))["releases"][0]["tags"], ["rc"])
        self.publish(self.entry(digest="b"))  # A rebuilt, still-unqualified candidate.
        self.publish(self.entry(tag="release", digest="b"))
        promoted = self.get(self.key)
        self.publish(self.entry(tag="release", digest="b"))
        self.assertEqual(self.get(self.key), promoted)
        for entry in [self.entry(digest="b"), self.entry(), self.entry(tag="release", digest="a")]:
            self.publish(entry, success=False)
            self.assertEqual(self.get(self.key), promoted)
        entries = json.loads(promoted)["releases"]
        self.assertEqual([e["version"] for e in entries], ["v1.0.0", "v0.9.0"])
        self.assertEqual([e["latest"] for e in entries], [True, False])
        self.assertEqual(entries[0]["tags"], ["release"])

    def test_conflicting_writer_is_merged_instead_of_lost(self):
        self.publish(self.entry())
        concurrent = {"schema_version": 1, "product": self.product, "kind": self.kind,
                      "releases": [self.entry("v2.0.0", digest="b"), dict(self.entry(), latest=False)]}
        race = self.path / "race.json"
        race.write_text(json.dumps(concurrent))
        self.env["TEST_RACE_BODY"] = str(race)
        self.publish(self.entry(tag="release"))
        entries = json.loads(self.get(self.key))["releases"]
        self.assertEqual([e["version"] for e in entries], ["v2.0.0", "v1.0.0"])
        self.assertEqual(entries[1]["tags"], ["release"])

    def test_rejects_invalid_input_and_access_errors(self):
        self.publish(dict(self.entry(), image="ghcr.io/other/image:v1.0.0"), success=False)
        self.assertFalse((self.bucket / self.key).exists())
        self.env["TEST_ACCESS_DENIED"] = "1"
        self.publish(self.entry(), success=False)
        self.assertFalse((self.bucket / self.key).exists())

    def test_conflicts_are_bounded_and_promotion_cannot_change_image(self):
        self.publish(self.entry())
        before = self.get(self.key)
        self.publish(self.entry(tag="release", digest="b"), success=False)
        self.env["TEST_ALWAYS_CONFLICT"] = "1"
        result = self.publish(self.entry(tag="release"), success=False)
        self.assertIn("all five attempts", result.stderr)
        self.assertEqual(self.get(self.key), before)


class OperatorCatalogTests(CatalogTests):
    product = "operator_sandbox"
    kind = "github_release"


class InterceptorCatalogTests(CatalogTests):
    product = "interceptor_sandbox"
    kind = "github_release"
