"""Archive integrity and shared host/companion build inputs, without publication."""

import json
import io
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import build_release as builder
import publish_release as publisher


class ReleaseArchivesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "LICENSE").write_text("license")
        self.calls = []

    def run_build_command(self, args, root, env=None):
        self.calls.append((args, env))
        if args[:2] == ["git", "status"]:
            return ""
        if args[:2] == ["git", "rev-parse"]:
            return "a" * 40
        if args[:2] == ["go", "env"]:
            return builder.TOOLCHAIN
        if args[:2] == ["go", "build"]:
            Path(args[args.index("-o") + 1]).write_bytes((env["GOOS"] + env["GOARCH"]).encode())
        return ""

    def build(self, platform):
        output = self.root / platform.replace("/", "-")
        with patch.object(builder, "run", side_effect=self.run_build_command):
            builder.build(self.root, output, "0.1.0", platform)
        return output / builder.archive_name(platform)

    def test_contents_integrity_and_identical_build_identity(self):
        for platform in builder.PLATFORMS:
            with self.subTest(platform=platform):
                self.calls.clear()
                path = self.build(platform)
                self.assertEqual(path.name, "interceptor_sandbox-" + platform.replace("/", "_") + ".tar.gz")
                metadata = publisher.read_archive(path)
                names = ["interceptor", "release.json", "LICENSE"]
                if platform.startswith("darwin/"):
                    names.insert(1, "interceptor-linux")
                with tarfile.open(path) as archive:
                    self.assertEqual(archive.getnames(), ["interceptor_sandbox/" + name for name in names])
                    install = self.root / ("extracted-" + platform.replace("/", "-"))
                    archive.extractall(install, filter="data")
                    self.assertEqual([entry.name for entry in install.iterdir()], ["interceptor_sandbox"])
                    self.assertEqual(sorted(entry.name for entry in (install / "interceptor_sandbox").iterdir()),
                                     sorted(names))
                repeated = self.root / ("repeat-" + platform.replace("/", "-"))
                with patch.object(builder, "run", side_effect=self.run_build_command):
                    builder.build(self.root, repeated, "0.1.0", platform)
                self.assertEqual(path.read_bytes(), (repeated / path.name).read_bytes())
                self.assertEqual(metadata["version"], "0.1.0")
                self.assertEqual(metadata["source_commit"], "a" * 40)
                builds = [(args, env) for args, env in self.calls if args[:2] == ["go", "build"]]
                self.assertEqual(len(builds), 4 if platform.startswith("darwin/") else 2)
                for args, env in builds:
                    flags = args[args.index("-ldflags") + 1]
                    self.assertIn("buildinfo.Version=0.1.0", flags)
                    self.assertIn("buildinfo.SourceCommit=" + "a" * 40, flags)
                    self.assertEqual(env["GOARCH"], platform.split("/")[1])
                    self.assertEqual(env["CGO_ENABLED"], "0")
                if platform.startswith("darwin/"):
                    self.assertEqual(builds[1][1]["GOOS"], "linux")
                    self.assertEqual(builds[0][0][builds[0][0].index("-ldflags")+1], builds[1][0][builds[1][0].index("-ldflags")+1])
                    self.assertEqual(metadata["linux_companion"]["platform"], platform.replace("darwin/", "linux/"))
                else:
                    self.assertNotIn("linux_companion", metadata)

    def test_both_macos_archives_reject_incomplete_or_changed_companion(self):
        for platform in ("darwin/amd64", "darwin/arm64"):
            path = self.build(platform)
            original = path.read_bytes()
            with tarfile.open(path) as archive:
                files = [(item.name, archive.extractfile(item).read(), item.mode) for item in archive.getmembers()]
            for failure in ("missing", "changed", "mode", "metadata", "platform", "extra", "duplicate"):
                with self.subTest(platform=platform, failure=failure):
                    changed = []
                    for name, data, mode in files:
                        if name == "interceptor_sandbox/interceptor-linux":
                            if failure == "missing": continue
                            if failure == "changed": data = b"wrong bytes"
                            if failure == "mode": mode = 0o644
                        if name == "interceptor_sandbox/release.json" and failure in ("metadata", "platform"):
                            record = json.loads(data)
                            if failure == "metadata": del record["linux_companion"]
                            else: record["linux_companion"]["platform"] = "linux/wrong"
                            data = json.dumps(record).encode()
                        changed.append((name, data, mode))
                    if failure == "extra": changed.append(("unexpected", b"", 0o644))
                    if failure == "duplicate": changed.append(files[1])
                    builder.archive(path, changed)
                    with self.assertRaises(ValueError): publisher.read_archive(path)
            path.write_bytes(original)

    def test_rejects_wrong_names_and_unsafe_or_flat_layouts(self):
        for platform in builder.PLATFORMS:
            path = self.build(platform)
            original = path.read_bytes()
            with tarfile.open(path) as archive:
                files = [(item.name, archive.extractfile(item).read(), item.mode) for item in archive.getmembers()]
            for filename in (platform.replace("/", "-") + ".tar.gz",
                             "interceptor_sandbox-wrong_arch.tar.gz"):
                with self.subTest(platform=platform, filename=filename):
                    renamed = path.with_name(filename)
                    renamed.write_bytes(original)
                    with self.assertRaisesRegex(ValueError, "filename"):
                        publisher.read_archive(renamed)
            for replacement in ("interceptor", "../interceptor", "/interceptor",
                                "other/interceptor", "interceptor_sandbox/../interceptor"):
                with self.subTest(platform=platform, replacement=replacement):
                    builder.archive(path, [(replacement, files[0][1], files[0][2]), *files[1:]])
                    with self.assertRaisesRegex(ValueError, "inventory"):
                        publisher.read_archive(path)
            for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.DIRTYPE):
                with self.subTest(platform=platform, kind=kind):
                    with tarfile.open(path, "w:gz") as archive:
                        for index, (name, data, mode) in enumerate(files):
                            member = tarfile.TarInfo(name)
                            member.mode = mode
                            if index == 0:
                                member.type = kind
                                member.linkname = "/tmp/outside"
                                archive.addfile(member)
                            else:
                                member.size = len(data)
                                archive.addfile(member, io.BytesIO(data))
                    with self.assertRaisesRegex(ValueError, "non-file"):
                        publisher.read_archive(path)

    def test_non_candidate_dispatch_cannot_publish(self):
        context = publisher.event_context({}, "workflow_dispatch", "refs/heads/fix/macos-wrapper-companion", "a"*40, "intrusiveai/interceptor_sandbox")
        self.assertEqual(context["mode"], "verify")
        self.assertEqual(context["version"], "0.1.1")


if __name__ == "__main__":
    unittest.main()
