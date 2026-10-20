#!/usr/bin/env python3
"""Exercise an extracted release with Docker, outside the checkout and without Go.

Uses the documented command application interface with /bin/cat as the target.
The supplied image must already exist locally and match the host architecture.
Only uniquely named test images/containers and an isolated data root are removed.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import uuid

from publish_release import read_archive
from build_release import ARCHIVE_ROOT


def qualify(archive, version, source, base_image, output, client):
    metadata = read_archive(archive)
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    host = platform.system().lower() + "/" + str(arch)
    if metadata["platform"] != host or metadata["version"] != version or metadata["source_commit"] != source:
        raise ValueError("archive must match the native host, requested source and release version")
    docker = shutil.which("docker")
    if not docker:
        raise ValueError("Docker must be installed and running")
    if output.exists():
        raise ValueError("output must be a new directory")
    output.mkdir(parents=True)
    output = output.resolve()
    suffix = uuid.uuid4().hex
    target, wrapper = "interceptor-release-target:"+suffix, "interceptor-release-wrapper:"+suffix
    inspection = "interceptor-release-check-"+suffix
    with tempfile.TemporaryDirectory(prefix="interceptor-release-qa-") as temporary:
        work = Path(temporary).resolve()
        install, outside, bin_dir = (work / name for name in ("install", "outside", "bin"))
        for directory in (install, outside, bin_dir): directory.mkdir()
        with tarfile.open(archive) as bundle:
            bundle.extractall(install, filter="data")  # read_archive checked the exact regular-file inventory.
        install = install / ARCHIVE_ROOT
        (bin_dir / "docker").symlink_to(Path(docker).resolve())
        env = dict(os.environ, PATH=str(bin_dir), INTERCEPTOR_DATA_DIR=str(work / "data"))
        if shutil.which("go", path=env["PATH"]) is not None:
            raise ValueError("qualification must not expose Go to the CLI")
        cli = str(install / "interceptor")
        def run(*args):
            result = subprocess.run(args, cwd=outside, env=env, text=True, capture_output=True, timeout=120)
            if result.returncode:
                raise RuntimeError(f"{args}: {result.stdout}\n{result.stderr}")
            return result.stdout.strip()
        if run(cli, "version") != version:
            raise ValueError("extracted CLI version differs")
        daemon = json.loads(run(docker, "info", "--format", "{{json .}}"))
        daemon_arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(daemon["Architecture"])
        if daemon["OSType"] != "linux" or daemon_arch != arch:
            raise ValueError("Docker daemon must run Linux with the same architecture as the CLI")
        base = json.loads(run(docker, "image", "inspect", base_image))[0]
        if base["Os"] != "linux" or base["Architecture"] != arch:
            raise ValueError("target image must be native Linux; cross-architecture execution is not qualified")
        # Reference only the inspected local image ID, never fetch application code.
        # A tag lets BuildKit resolve the local source after its identity is checked.
        run(docker, "tag", base["Id"], target)
        process = None
        try:
            blueprint = outside / "environment"
            run(cli, "environment", "init", str(blueprint))
            record = {
                "api_version": "interceptor.dev/v1alpha1", "kind": "Environment",
                "metadata": {"name": "release-qualification", "version": version,
                             "reviewed_by": "release-qualification", "reviewed_at": "2026-01-01T00:00:00Z"},
                "application": {"image": target+"@"+base["Id"],
                    "launch": {"command": ["/bin/cat"], "workdir": "/tmp",
                               "environment": {"MODEL_BASE_URL": "http://127.0.0.1:18080/v1"}},
                    "interface": {"kind": "command", "operations": [{"id": "echo"}],
                        "command": {"stdin": "raw", "output_media_type": "text/plain", "success_exit_codes": [0], "concurrency": 1},
                        "startup_timeout_ms": 30000, "turn_timeout_ms": 30000},
                    "persistence": {"snapshot_capable": False, "quiesce": {"kind": "none"}}},
                "time": {"base": "2026-01-01T00:00:00Z", "mode": "fixed"},
                "limits": {"max_turns": 10, "max_tokens": 10000, "max_wall_seconds": 120,
                           "max_snapshots": 1, "max_blob_bytes": 10485760},
            }
            (blueprint / "environment.yaml").write_text(json.dumps(record))
            run(cli, "environment", "validate", str(blueprint))
            run(cli, "environment", "seal", str(blueprint))
            lock = json.loads(run(cli, "app", "build", "--environment", str(blueprint), "--tag", wrapper, "--json"))
            run(cli, "app", "verify", "--environment", str(blueprint))
            packaged = install / ("interceptor-linux" if host.startswith("darwin/") else "interceptor")
            expected = hashlib.sha256(packaged.read_bytes()).hexdigest()
            if lock["interceptor_binary_digest"] != "sha256:"+expected or lock["interceptor_version"] != version:
                raise ValueError("wrapper lock does not identify the packaged executable")
            run(docker, "create", "--name", inspection, wrapper, "version")
            run(docker, "cp", inspection+":/usr/local/bin/interceptor", str(work / "container-interceptor"))
            if (work / "container-interceptor").read_bytes() != packaged.read_bytes():
                raise ValueError("wrapper contains different executable bytes")
            if run(docker, "start", "--attach", inspection) != version:
                raise ValueError("in-container executable version differs")
            with (output / "run.log").open("w") as log:
                process = subprocess.Popen([cli, "run", "--environment", str(blueprint),
                    "--model-provider", "litellm", "--provider-auth", "none",
                    "--provider-base-url", "http://127.0.0.1:9/v1", "--allow-model", "unused"],
                    cwd=outside, env=env, stdout=log, stderr=subprocess.STDOUT)
                deadline = time.monotonic() + 60
                session = None
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        raise RuntimeError("release exited before readiness: " + (output / "run.log").read_text())
                    sessions = json.loads(run(cli, "list", "--json"))["sessions"] or []
                    session = next((item for item in sessions if item.get("phase") == "running"), None)
                    if session and "Interceptor local API:" in (output / "run.log").read_text(): break
                    time.sleep(0.25)
                if not session: raise RuntimeError("release readiness timed out")
                run(str(client), "--command-only", "--output", str(output / "native"))
                native = json.loads((output / "native/result.json").read_text())
                if not native["passed"] or native["session_id"] != session["id"]:
                    raise ValueError("native qualification did not pass for this session")
                process.send_signal(signal.SIGINT)
                if process.wait(timeout=30) != 0:
                    raise RuntimeError("release shutdown failed: " + (output / "run.log").read_text())
                process = None
            run(cli, "logs", "verify", session["id"])
            result = {"platform": host, "source_commit": source, "version": version,
                      "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                      "container_binary_sha256": expected, "go_available": False,
                      "target_output": native["target_output"].strip(), "session_id": session["id"]}
            (output / "result.json").write_text(json.dumps(result, indent=2)+"\n")
            print(json.dumps(result))
        finally:
            if process is not None and process.poll() is None:
                process.send_signal(signal.SIGINT)
                try: process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill(); process.wait()
            # This newly created data root contains only this test's sessions.
            if (work / "data").exists():
                run(cli, "purge", "--all")
            subprocess.run([docker, "rm", "-f", inspection], capture_output=True, timeout=30)
            for image in (wrapper, target):
                subprocess.run([docker, "image", "rm", image], capture_output=True, timeout=30)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--image", required=True, help="existing native Linux image containing /bin/cat")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--client", type=Path, required=True, help="prebuilt examples/local-api-smoke qualification client")
    args = parser.parse_args()
    qualify(args.archive.resolve(), args.version, args.source, args.image, args.output, args.client.resolve())
