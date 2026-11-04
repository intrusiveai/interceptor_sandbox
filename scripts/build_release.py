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
from native_build import native_environment, static_guest_environment, inspect_binary

TOOLCHAIN = "go1.26.5"
ARCHIVE_ROOT = "interceptor_sandbox"
PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")
VERSION = r"(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})"


def contract_version(value):
    if (not isinstance(value, str) or len(value) > 128
            or not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", value)):
        raise ValueError("interceptor_contract_version must be exact major.minor.patch (at most 128 characters)")
    return value


def archive_name(version, platform):
    if not isinstance(version, str) or not re.fullmatch(VERSION, version):
        raise ValueError("release version must be numeric major.minor.patch")
    if platform not in PLATFORMS:
        raise ValueError("unsupported release platform")
    return ARCHIVE_ROOT + "-v" + version + "-" + platform.replace("/", "_") + ".tar.gz"


def checksum_name(version, platform):
    return archive_name(version, platform).removesuffix(".tar.gz") + "-SHA256SUMS"


def checksum_line(path):
    with path.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    return f"{digest}  {path.name}\n"


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


def build(root, output, version, platform, linux_archive=None):
    if not re.fullmatch(VERSION, version):
        raise ValueError("release version must be numeric major.minor.patch")
    if platform not in PLATFORMS:
        raise ValueError("unsupported release platform")
    if output.exists():
        raise ValueError("output must be a new directory")
    if run(["git", "status", "--porcelain", "--untracked-files=all"], root):
        raise ValueError("release source must be a clean committed checkout")

    commit = run(["git", "rev-parse", "HEAD"], root)
    contract = json.loads((root / "internal/integrationcontract/contract.json").read_text())
    contract_identity = contract_version(contract.get("interceptor_contract_version"))
    goos, goarch = platform.split("/")
    env, native = native_environment(platform)
    env.update(GOOS=goos, GOARCH=goarch)
    if (goos == "darwin") != (linux_archive is not None):
        raise ValueError("macOS packaging requires its matching native Linux archive; Linux builds their own companion")
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
            **native, **inspect_binary(binary, platform),
            "interceptor_contract_version": contract_identity,
        }
        files = [("interceptor", executable, 0o755)]
        if goos == "linux":
            companion = Path(temporary) / "interceptor-linux"
            guest_env = static_guest_environment(env)
            guest_env.update(GOOS="linux", GOARCH=goarch)
            run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", flags,
                 "-o", str(companion), "./cmd/interceptor"], root, guest_env)
            companion_bytes = companion.read_bytes()
        else:
            companion_bytes = imported_companion(linux_archive, version, commit, goarch, contract_identity)
        metadata["linux_companion"] = {
            "filename": "interceptor-linux", "platform": "linux/" + goarch,
            "sha256": hashlib.sha256(companion_bytes).hexdigest(),
        }
        files.append(("interceptor-linux", companion_bytes, 0o755))
        release_json = (json.dumps(metadata, sort_keys=True, indent=2) + "\n").encode()
        license_text = (root / "LICENSE").read_bytes() + b"\n\nTHIRD-PARTY COMPONENT NOTICES\n\n" + (root / "internal/appimage/postgresql-go.LICENSE").read_bytes() + b"\n\n" + (root / "docs/ONEPASSWORD_NOTICES.md").read_bytes()
        files.extend((
            ("release.json", release_json, 0o644),
            ("LICENSE", license_text, 0o644),
        ))
        path = output / archive_name(version, platform)
        archive(path,
                [(ARCHIVE_ROOT + "/" + name, content, mode) for name, content, mode in files])
        (output / checksum_name(version, platform)).write_text(checksum_line(path))
    print(json.dumps({"status": "built-not-qualified", **metadata}, sort_keys=True))


def imported_companion(path, version, source, arch, contract):
    # Import lazily: the publisher also imports archive naming helpers above.
    from publish_release import read_archive, verify_archive_checksum
    metadata = read_archive(path)
    verify_archive_checksum(path, metadata)
    if any(metadata.get(key) != value for key, value in {
        "version": version, "source_commit": source, "platform": "linux/" + arch,
        "interceptor_contract_version": contract,
    }.items()) or "linux_companion" not in metadata:
        raise ValueError("Linux companion archive identity differs from this build")
    with tarfile.open(path, "r:gz") as bundle:
        return bundle.extractfile(ARCHIVE_ROOT + "/interceptor-linux").read()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--platform", choices=PLATFORMS, required=True)
    parser.add_argument("--linux-companion-archive", type=Path)
    args = parser.parse_args()
    build(args.source.resolve(), args.output.resolve(), args.version, args.platform, args.linux_companion_archive)


if __name__ == "__main__":
    main()
