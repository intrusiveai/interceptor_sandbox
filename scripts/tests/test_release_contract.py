"""Contract metadata must agree with executable output, not only source inputs."""

import json
from pathlib import Path
import sys
import tarfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import build_release as builder
import publish_release as publisher
import verify_release_contract as verifier
import test_release_archives


class ContractTest(unittest.TestCase):
    def setUp(self):
        self.fixture = test_release_archives.ReleaseArchivesTest()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)

    def test_contract_version_format(self):
        for value in (None, 1, "", "01.1.0", "v0.1.0", "0.1", "0.1.0-rc.1", "0.1.0+build", "0.1.0\n", "^0.1.0", "1"*129+".0.0"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                builder.contract_version(value)
        self.assertEqual(builder.contract_version("0.1.0"), "0.1.0")

    def test_native_archive_commands_and_disagreement(self):
        path = self.fixture.build("darwin/arm64")
        calls = []
        def command(args, **kwargs):
            self.assertFalse(Path(kwargs["env"]["PATH"]).joinpath("docker").exists())
            self.assertNotEqual(kwargs["cwd"], Path.cwd())
            calls.append(args[1:])
            if args[1:] == ["contract", "--json"]:
                return json.dumps({"openapi":"3.1.0", "info":{"version":"0.1.0"}, "paths":{"/example":{}}, "components":{"schemas":{"Example":{}}}})
            if args[1:] == ["version", "--json"]:
                return json.dumps({"version":"0.1.2", "source_commit":"a"*40, "interceptor_contract_version":"0.1.0"})
            return "0.1.2\n"
        with patch.object(verifier.platform, "system", return_value="Darwin"), patch.object(verifier.platform, "machine", return_value="arm64"), patch.object(verifier.subprocess, "check_output", side_effect=command):
            report = verifier.verify(path, "a"*40, "0.1.2")
        self.assertEqual(calls, [["contract", "--json"], ["version", "--json"], ["version"]])
        self.assertEqual(report["interceptor_contract_version"], "0.1.0")
        self.assertEqual(report["archive_sha256"], publisher.checksum(path)[7:])

        def wrong(args, **kwargs):
            return command(args, **kwargs).replace('"0.1.0"', '"0.2.0"')
        with patch.object(verifier.platform, "system", return_value="Darwin"), patch.object(verifier.platform, "machine", return_value="arm64"), patch.object(verifier.subprocess, "check_output", side_effect=wrong):
            with self.assertRaisesRegex(ValueError, "disagree"):
                verifier.verify(path, "a"*40, "0.1.2")
        with patch.object(verifier.platform, "system", return_value="Linux"), patch.object(verifier.platform, "machine", return_value="arm64"), patch.object(verifier.subprocess, "check_output", side_effect=command):
            with self.assertRaisesRegex(ValueError, "native runner"):
                verifier.verify(path, "a"*40, "0.1.2")
            report = verifier.verify(path, "a"*40, "0.1.2", companion=True)
            self.assertEqual(report["executable"], "interceptor-linux")

    def test_missing_or_invalid_archive_metadata(self):
        path = self.fixture.build("linux/amd64")
        with tarfile.open(path) as archive:
            files = [(m.name, archive.extractfile(m).read(), m.mode) for m in archive.getmembers()]
        for value in (None, "", "0.1.0+build"):
            changed = []
            for name, data, mode in files:
                if name.endswith("/release.json"):
                    metadata = json.loads(data)
                    if value is None: metadata.pop("interceptor_contract_version")
                    else: metadata["interceptor_contract_version"] = value
                    data = json.dumps(metadata).encode()
                changed.append((name, data, mode))
            builder.archive(path, changed)
            with self.assertRaisesRegex(ValueError, "interceptor_contract_version"):
                publisher.read_archive(path)
