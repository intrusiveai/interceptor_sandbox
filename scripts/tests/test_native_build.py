"""Native build selection must not silently produce a different host artifact."""
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import native_build as native


class NativeBuildTest(unittest.TestCase):
    def test_matching_hosts_and_cgo(self):
        for system, machine, target in (("Linux", "x86_64", "linux/amd64"), ("Linux", "aarch64", "linux/arm64"),
                                        ("Darwin", "x86_64", "darwin/amd64"), ("Darwin", "arm64", "darwin/arm64")):
            with self.subTest(target=target), patch.object(native.platform, "system", return_value=system), \
                 patch.object(native.platform, "machine", return_value=machine), \
                 patch.object(native.shutil, "which", return_value="/usr/bin/cc"), \
                 patch.object(native.subprocess, "check_output", side_effect=lambda args, **kw: "/Xcode/SDK\n" if "--show-sdk-path" in args else "15.5\n" if "--show-sdk-version" in args else "native compiler\n"), \
                 patch.dict(os.environ, {"CGO_ENABLED": "0", "GOOS": "wrong", "CC": "cross-cc", "CGO_LDFLAGS": "-wrong"}):
                env, info = native.native_environment(target)
                self.assertEqual(env["CGO_ENABLED"], "1")
                self.assertNotIn("GOOS", env)
                self.assertNotIn("CGO_LDFLAGS", env)
                self.assertEqual(env["CC"], "/usr/bin/cc")
                self.assertEqual(info["build_platform"], target)
                if system == "Darwin":
                    self.assertEqual(env["MACOSX_DEPLOYMENT_TARGET"], "15.0")
                    with self.assertRaises(ValueError): native.static_guest_environment(env)
                else:
                    self.assertEqual(native.static_guest_environment(env)["CGO_ENABLED"], "0")
                with self.assertRaises(ValueError): native.native_environment("windows/amd64")

    def test_native_dependencies_and_disabled_cgo(self):
        def result(args, **kw):
            if args[:2] == ["go", "version"]: return "build CGO_ENABLED=1\nbuild GOOS=linux\nbuild GOARCH=arm64"
            if "-d" in args: return "Shared library: [libc.so.6]"
            return "GLIBC_2.17 GLIBC_2.34"
        with patch.object(native.subprocess, "check_output", side_effect=result):
            self.assertEqual(native.inspect_binary("binary", "linux/arm64")["glibc_required"], "2.34")
        with patch.object(native.subprocess, "check_output", return_value="CGO_ENABLED=0 GOOS=linux GOARCH=arm64"):
            with self.assertRaises(ValueError): native.inspect_binary("binary", "linux/arm64")

if __name__ == "__main__": unittest.main()
