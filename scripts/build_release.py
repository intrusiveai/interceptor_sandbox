#!/usr/bin/env python3
"""Build a deterministic Interceptor archive from a clean committed checkout."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

TOOLCHAIN = "go1.26.5"
PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")
VERSION = r"(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})"


def run(args, root, env=None):
    return subprocess.check_output(args, cwd=root, env=env, text=True).strip()


def archive(output, files):
    with output.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w") as bundle:
                for name, content, mode in files:
                    info = tarfile.TarInfo(name)
                    info.size = len(content)
                    info.mode = mode
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    info.mtime = 0
                    bundle.addfile(info, io.BytesIO(content))


def build(root, output, version, platform):
    if not re.fullmatch(VERSION, version):
        raise ValueError("release version must be numeric major.minor.patch")
    if platform not in PLATFORMS:
        raise ValueError("unsupported release platform")
    if output.exists():
        raise ValueError("output must be a new directory")
    if run(["git", "status", "--porcelain", "--untracked-files=all"], root):
        raise ValueError("release source must be a clean committed checkout")

    commit = run(["git", "rev-parse", "HEAD"], root)
    goos, goarch = platform.split("/")
    env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOFLAGS="", CGO_ENABLED="0",
               GOOS=goos, GOARCH=goarch, GOAMD64="v1", GOARM64="v8.0")
    if run(["go", "env", "GOVERSION"], root, env) != TOOLCHAIN:
        raise ValueError("release builds require " + TOOLCHAIN)
    run(["go", "mod", "verify"], root, env)

    output.mkdir(mode=0o700, parents=True)
    with tempfile.TemporaryDirectory(prefix="interceptor-release-") as temporary:
        binary = Path(temporary) / "interceptor"
        flags = " ".join(["-s", "-w", "-buildid=", "-X",
                          "github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo.Version=" + version,
                          "-X", "github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo.SourceCommit=" + commit])
        run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", flags,
             "-o", str(binary), "./cmd/interceptor"], root, env)
        executable = binary.read_bytes()
        metadata = {
            "version": version,
            "source_commit": commit,
            "platform": platform,
            "binary_sha256": hashlib.sha256(executable).hexdigest(),
            "toolchain": TOOLCHAIN,
            "cgo_enabled": False,
        }
        files = [("interceptor", executable, 0o755)]
        if goos == "darwin":
            companion = Path(temporary) / "interceptor-linux"
            run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", flags,
                 "-o", str(companion), "./cmd/interceptor"], root, dict(env, GOOS="linux"))
            companion_bytes = companion.read_bytes()
            metadata["linux_companion"] = {
                "filename": "interceptor-linux",
                "platform": "linux/" + goarch,
                "sha256": hashlib.sha256(companion_bytes).hexdigest(),
            }
            files.append(("interceptor-linux", companion_bytes, 0o755))
        release_json = (json.dumps(metadata, sort_keys=True, indent=2) + "\n").encode()
        license_text = (root / "LICENSE").read_bytes()
        files.extend((
            ("release.json", release_json, 0o644),
            ("LICENSE", license_text, 0o644),
        ))
        archive(output / (platform.replace("/", "-") + ".tar.gz"), files)
    print(json.dumps({"status": "built-not-qualified", **metadata}, sort_keys=True))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--platform", choices=PLATFORMS, required=True)
    args = parser.parse_args()
    build(args.source.resolve(), args.output.resolve(), args.version, args.platform)


if __name__ == "__main__":
    main()
