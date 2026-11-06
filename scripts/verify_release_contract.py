#!/usr/bin/env python3
"""Verify actual archived executables offline, without Docker or a target.

Native runners verify all four host CLIs; Linux runners also verify the matching
macOS archive's companion. Reports bind these observations to the archive bytes.
"""

import argparse
import hashlib
import json
from pathlib import Path
import platform
import subprocess
import tarfile
import tempfile

from build_release import ARCHIVE_ROOT, contract_version
from publish_release import read_archive, verify_archive_checksum


def verify(path, source, version, companion=False):
    metadata = read_archive(path)
    verify_archive_checksum(path, metadata)
    if metadata["source_commit"] != source or metadata["version"] != version:
        raise ValueError("contract verification source/version differs")
    target = metadata.get("linux_companion") if companion else {
        "filename": "interceptor", "platform": metadata["platform"], "sha256": metadata["binary_sha256"]}
    if not target:
        raise ValueError("archive has no companion")
    arch = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if target["platform"] != platform.system().lower() + "/" + str(arch):
        raise ValueError("contract verification requires a compatible native runner")
    with tempfile.TemporaryDirectory(prefix="interceptor-contract-") as temporary:
        root = Path(temporary)
        with tarfile.open(path) as bundle:
            bundle.extractall(root, filter="data")
        executable = root / ARCHIVE_ROOT / target["filename"]
        outside = root / "outside"
        outside.mkdir()
        empty_path = root / "empty-path"
        empty_path.mkdir()
        # No Go, Docker or inherited user configuration is available to commands.
        env = {"PATH": str(empty_path), "HOME": str(outside), "INTERCEPTOR_DATA_DIR": str(root / "unused-state")}
        def command(*args):
            return subprocess.check_output([str(executable), *args], cwd=outside, env=env, text=True, timeout=30)
        contract = json.loads(command("contract", "--json"))
        reported = json.loads(command("version", "--json"))
        expected = contract_version(metadata.get("interceptor_contract_version"))
        if (contract.get("info", {}).get("version") != expected
                or reported != {"version": version, "source_commit": source, "interceptor_contract_version": expected}
                or command("version").strip() != version
                or contract.get("openapi") != "3.1.0"
                or not contract.get("paths") or not contract.get("components", {}).get("schemas")):
            raise ValueError("archived contract, version reporting and release metadata disagree")
        if (root / "unused-state").exists():
            raise ValueError("contract commands created runtime state")
    return {"archive_sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
            "source_commit": source, "version": version, "platform": metadata["platform"],
            "interceptor_contract_version": expected, "executable": target["filename"],
            "binary_sha256": target["sha256"], "execution_platform": target["platform"],
            "checks": ["contract-json", "version-json", "plain-version", "no-runtime-state"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--companion", action="store_true")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = verify(args.archive.resolve(), args.source, args.version, args.companion)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, sort_keys=True, indent=2) + "\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
