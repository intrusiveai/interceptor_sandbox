#!/usr/bin/env python3
"""Publish a rolling Interceptor candidate, or promote its existing assets."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import time
import uuid

from build_release import ARCHIVE_ROOT, archive_name, checksum_name, checksum_line

PLATFORMS = {"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}
VERSION = r"(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})"
MARKER = "<!-- interceptor-candidate-release/v1 -->"
CATALOG_ASSET = "release-index-entry.json"


def candidate_version(branch):
    if not re.fullmatch("candidate-v" + VERSION, branch):
        raise ValueError("candidate branch must be candidate-vMAJOR.MINOR.PATCH")
    return branch.removeprefix("candidate-v")


def event_context(event, name, ref, sha, repository):
    result = {"mode": "verify", "version": "0.1.2", "source": sha,
              "branch": ref.removeprefix("refs/heads/"), "pr": ""}
    if name == "pull_request":
        pr = event["pull_request"]
        if event["action"] == "closed":
            result["mode"] = "ignore"
            if pr["merged"] and pr["base"]["ref"] == "main" and pr["head"]["ref"].startswith("candidate-v"):
                if pr["head"]["repo"]["full_name"] != repository:
                    raise ValueError("release candidates must belong to this repository")
                result.update(mode="promote", version=candidate_version(pr["head"]["ref"]),
                              source=pr["head"]["sha"], branch=pr["head"]["ref"], pr=str(pr["number"]))
        elif pr["base"]["ref"].startswith("candidate-v"):
            result["version"] = candidate_version(pr["base"]["ref"])
        elif pr["base"]["ref"] == "main" and pr["head"]["ref"].startswith("candidate-v"):
            result["version"] = candidate_version(pr["head"]["ref"])
    elif name in ("push", "workflow_dispatch") and result["branch"].startswith("candidate-v"):
        if event.get("deleted"):
            result["mode"] = "ignore"
        else:
            result.update(mode="publish", version=candidate_version(result["branch"]))
    return result


class GitHub:
    def __init__(self, repository):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid GitHub repository")
        self.repository = repository
        self.prefix = "repos/" + repository + "/"

    def api(self, path, method="GET", data=None, missing=False):
        command = ["gh", "api", self.prefix + path, "--method", method]
        if data is not None:
            command += ["--input", "-"]
        result = subprocess.run(command, input=json.dumps(data) if data is not None else None,
                                text=True, capture_output=True)
        if result.returncode:
            if missing and "(HTTP 404)" in result.stderr:
                return None
            detail = result.stderr.strip()
            for name in ("GH_TOKEN", "GITHUB_TOKEN"):
                token = os.environ.get(name)
                if token:
                    detail = detail.replace(token, "[redacted]")
            raise RuntimeError("GitHub request failed: " + method + " " + path + ": " + detail)
        return json.loads(result.stdout) if result.stdout.strip() else None

    def upload(self, tag, files, replace=False):
        subprocess.run(["gh", "release", "upload", tag, *map(str, files),
                        "--repo", self.repository] + (["--clobber"] if replace else []), check=True)

    def download_catalog(self, tag):
        with tempfile.TemporaryDirectory(prefix="interceptor-catalog-") as directory:
            subprocess.run(["gh", "release", "download", tag, "--repo", self.repository,
                            "--pattern", CATALOG_ASSET, "--dir", directory], check=True)
            return json.loads((Path(directory) / CATALOG_ASSET).read_text())

    def release(self, tag):
        return self.api("releases/tags/" + tag, missing=True)

    def assets(self, release):
        return self.api(f"releases/{release['id']}/assets?per_page=100")


def checksum(path):
    with path.open("rb") as stream:
        return "sha256:" + hashlib.file_digest(stream, "sha256").hexdigest()


def read_archive(path):
    with tarfile.open(path, "r:gz") as archive:
        members = archive.getmembers()
        if any(not member.isfile() for member in members):
            raise ValueError("release archive contains a non-file member")
        prefix = ARCHIVE_ROOT + "/"
        if sum(member.name == prefix + "release.json" for member in members) != 1:
            raise ValueError("release archive requires one release.json")
        metadata = json.load(archive.extractfile(prefix + "release.json"))
        platform = metadata.get("platform")
        if platform not in PLATFORMS or path.name != archive_name(metadata.get("version"), platform):
            raise ValueError("release archive platform/filename differs")
        names = ["interceptor", "release.json", "LICENSE"]
        if platform.startswith("darwin/"):
            names.insert(1, "interceptor-linux")
        elif "linux_companion" in metadata:
            raise ValueError("Linux releases must not declare a companion")
        if [member.name for member in members] != [prefix + name for name in names]:
            raise ValueError("release archive has an unexpected file inventory")
        executable = archive.extractfile(prefix + "interceptor").read()
        if metadata.get("binary_sha256") != hashlib.sha256(executable).hexdigest():
            raise ValueError("release archive binary digest differs")
        if archive.getmember(prefix + "interceptor").mode != 0o755:
            raise ValueError("release CLI must be executable")
        if platform.startswith("darwin/"):
            companion = archive.extractfile(prefix + "interceptor-linux").read()
            expected = {"filename": "interceptor-linux", "platform": platform.replace("darwin/", "linux/"),
                        "sha256": hashlib.sha256(companion).hexdigest()}
            if metadata.get("linux_companion") != expected:
                raise ValueError("release Linux companion metadata/digest differs")
            if archive.getmember(prefix + "interceptor-linux").mode != 0o755:
                raise ValueError("release Linux companion must be executable")
    return metadata


def verify_archive_checksum(path, metadata):
    sums = path.with_name(checksum_name(metadata["version"], metadata["platform"]))
    if not sums.is_file() or sums.read_text() != checksum_line(path):
        raise ValueError("release archive checksum file is missing or differs")


def assemble(directory, output):
    archives = sorted(directory.glob("*.tar.gz"))
    if len(archives) != len(PLATFORMS):
        raise ValueError("four distinct platform archives are required")
    metadata = [read_archive(path) for path in archives]
    identity = {(item.get("version"), item.get("source_commit")) for item in metadata}
    platforms = {item.get("platform") for item in metadata}
    if len(identity) != 1 or platforms != PLATFORMS:
        raise ValueError("release source, version or platform identities differ")
    version, source = identity.pop()
    if {path.name for path in directory.iterdir()} != expected_assets(version) - {"candidate.json"}:
        raise ValueError("four platform archives and matching checksum files are required")
    for path, item in zip(archives, metadata):
        verify_archive_checksum(path, item)
    output.mkdir(mode=0o700)
    for path in sorted(directory.iterdir()):
        destination = output / path.name
        destination.write_bytes(path.read_bytes())
    inventory = {"version": version, "source_commit": source,
                 "archives": {path.name: checksum(path) for path in archives}}
    record = output / "candidate.json"
    record.write_text(json.dumps(inventory, sort_keys=True, indent=2) + "\n")
    return inventory, sorted(output.iterdir())


def expected_assets(version):
    return ({archive_name(version, platform) for platform in PLATFORMS}
            | {checksum_name(version, platform) for platform in PLATFORMS}
            | {"candidate.json"})


def verify_uploaded(github, release, digests):
    # Catalog metadata can change from rc to release without changing the binaries.
    assets = [asset for asset in github.assets(release) if asset["name"] != CATALOG_ASSET]
    if len(assets) != len(digests) or {asset["name"] for asset in assets} != set(digests):
        raise ValueError("release asset inventory is incomplete")
    for asset in assets:
        if asset["state"] != "uploaded" or asset.get("digest") != digests[asset["name"]]:
            raise ValueError("uploaded release asset digest differs")


def catalog_entry(github, tag, source):
    if github.repository != "intrusiveai/interceptor_sandbox":
        raise ValueError("catalog publication is restricted to Interceptor's repository")
    version = candidate_version("candidate-" + tag)
    release = github.release(tag)
    body = (release or {}).get("body") or ""
    if not release or release["draft"] or not body.startswith(MARKER + "\n"):
        raise ValueError("catalog requires a complete managed GitHub release")
    record = json.loads(body.removeprefix(MARKER + "\n"))
    if (record["version"] != version or record["source_commit"] != source
            or github.api("commits/" + tag)["sha"] != source):
        raise ValueError("catalog source/tag differs from the published candidate")
    if set(record["assets"]) != expected_assets(version):
        raise ValueError("catalog requires all release assets")
    verify_uploaded(github, release, record["assets"])
    if not release["prerelease"]:
        number = record.get("promotion_pr")
        if type(number) is not int or number < 1:
            raise ValueError("stable catalog requires verified candidate PR promotion; rerun promotion")
        pr = github.api("pulls/" + str(number))
        if pr["head"]["ref"] != "candidate-" + tag or pr["head"]["sha"] != source:
            raise ValueError("stable catalog promotion PR differs from the release")
        promote(github, number)
    assets = {asset["name"]: asset for asset in github.assets(release)}
    artifacts = []
    for platform in sorted(PLATFORMS):
        name = archive_name(version, platform)
        asset = assets[name]
        url = f"https://github.com/{github.repository}/releases/download/{tag}/{name}"
        if asset.get("browser_download_url") != url:
            raise ValueError("catalog download URL differs from the product release")
        artifacts.append({"platform": platform, "filename": name,
                          "url": asset["browser_download_url"],
                          "sha256": asset["digest"].removeprefix("sha256:")})
    return {"version": tag, "latest": True,
            "tags": ["rc" if release["prerelease"] else "release"],
            "artifacts": artifacts}


def catalog_context(github, tag, source):
    expected = catalog_entry(github, tag, source)
    if github.download_catalog(tag) != expected:
        raise ValueError("catalog asset is stale or differs from the actual release")
    return expected


def sync_catalog(github, tag, source, timeout=1800):
    entry = catalog_entry(github, tag, source)
    with tempfile.TemporaryDirectory(prefix="interceptor-catalog-") as directory:
        path = Path(directory) / CATALOG_ASSET
        path.write_text(json.dumps(entry, sort_keys=True, indent=2) + "\n")
        github.upload(tag, [path], replace=True)
    catalog_context(github, tag, source)

    request = "interceptor-catalog-" + uuid.uuid4().hex
    workflow = "actions/workflows/publish-catalog.yml"
    github.api(workflow + "/dispatches", "POST",
               {"ref": tag, "inputs": {"request_id": request}})
    deadline = time.monotonic() + timeout
    run = None
    while time.monotonic() < deadline:
        if run is None:
            runs = github.api(workflow + "/runs?event=workflow_dispatch&head_sha="
                              + source + "&per_page=100")["workflow_runs"]
            matches = [item for item in runs
                       if item["display_title"] == request and item["head_sha"] == source]
            if len(matches) > 1:
                raise RuntimeError("ambiguous catalog workflow dispatch")
            run = matches[0] if matches else None
        else:
            run = github.api(f"actions/runs/{run['id']}")
        if run and run["status"] == "completed":
            if run["conclusion"] != "success":
                raise RuntimeError("catalog publication failed: " + run["html_url"])
            return run["html_url"]
        time.sleep(10)
    if run:
        github.api(f"actions/runs/{run['id']}/cancel", "POST")
    raise RuntimeError("catalog publication timed out; inspect the dispatched run before retrying")


def publish(github, branch, source, inventory, files):
    version = candidate_version(branch)
    if inventory["version"] != version or inventory["source_commit"] != source:
        raise ValueError("build does not match candidate event")
    if not re.fullmatch(r"[a-f0-9]{40}", source):
        raise ValueError("invalid source commit")
    if github.api("git/ref/heads/" + branch)["object"]["sha"] != source:
        return "superseded"
    tag = "v" + version
    release = github.release(tag)
    if release and (not release["prerelease"] or release.get("immutable") or MARKER not in (release.get("body") or "")):
        raise ValueError("cannot replace a stable, immutable or unmanaged release")
    digests = {path.name: checksum(path) for path in files}
    if set(digests) != expected_assets(version):
        raise ValueError("candidate file set is incomplete")
    body = MARKER + "\n" + json.dumps({"version": version, "source_commit": source,
                                         "assets": digests}, sort_keys=True)
    if release:
        github.api(f"releases/{release['id']}", "PATCH", {"draft": True})
        for asset in github.assets(release):
            github.api(f"releases/assets/{asset['id']}", "DELETE")
    ref = github.api("git/ref/tags/" + tag, missing=True)
    if ref and not release and ref["object"]["sha"] != source:
        raise ValueError("refusing to move a tag without a managed candidate release")
    if ref:
        github.api("git/refs/tags/" + tag, "PATCH", {"sha": source, "force": True})
    else:
        github.api("git/refs", "POST", {"ref": "refs/tags/" + tag, "sha": source})
    release_metadata = {"tag_name": tag, "target_commitish": source,
                        "name": "Interceptor Sandbox " + version, "body": body,
                        "draft": True, "prerelease": True, "make_latest": "false"}
    if release:
        github.api(f"releases/{release['id']}", "PATCH", release_metadata)
    else:
        release = github.api("releases", "POST", release_metadata)
    github.upload(tag, files)
    verify_uploaded(github, release, digests)
    if github.api("git/ref/heads/" + branch)["object"]["sha"] != source:
        return "superseded"
    github.api(f"releases/{release['id']}", "PATCH",
               {"draft": False, "prerelease": True, "make_latest": "false"})
    return "prerelease-published"


def promote(github, number):
    pr = github.api("pulls/" + str(number))
    if not pr["merged"] or pr["base"]["ref"] != "main" or pr["head"]["repo"]["full_name"] != github.repository:
        raise ValueError("promotion requires a merged same-repository candidate PR into main")
    version = candidate_version(pr["head"]["ref"])
    source = pr["head"]["sha"]
    release = github.release("v" + version)
    if not release or release["draft"]:
        raise ValueError("candidate prerelease is missing or incomplete; finish publication and rerun promotion")
    body = release.get("body") or ""
    if not body.startswith(MARKER + "\n"):
        raise ValueError("release is not managed by the candidate workflow")
    record = json.loads(body.removeprefix(MARKER + "\n"))
    if record["source_commit"] != source or record["version"] != version:
        raise ValueError("prerelease was not built from the merged candidate head")
    if set(record["assets"]) != expected_assets(version):
        raise ValueError("prerelease is missing required artifacts")
    if github.api("commits/v" + version)["sha"] != source:
        raise ValueError("release tag differs from built candidate")
    candidate_tree = github.api("git/commits/" + source)["tree"]["sha"]
    merged_tree = github.api("git/commits/" + pr["merge_commit_sha"])["tree"]["sha"]
    if candidate_tree != merged_tree:
        raise ValueError("main merge changed the candidate tree; merge main into the candidate and rebuild before release")
    verify_uploaded(github, release, record["assets"])
    promoted_record = dict(record, promotion_pr=number)
    body = MARKER + "\n" + json.dumps(promoted_record, sort_keys=True)
    if not release["prerelease"]:
        if record != promoted_record:
            github.api(f"releases/{release['id']}", "PATCH", {"body": body})
        return "already-stable"
    github.api(f"releases/{release['id']}", "PATCH",
               {"prerelease": False, "make_latest": "legacy", "body": body})
    return "promoted-existing-assets"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=("context", "publish", "promote",
                                              "catalog-sync", "catalog-context", "verify-archive"))
    parser.add_argument("--directory", type=Path)
    parser.add_argument("--branch")
    parser.add_argument("--source")
    parser.add_argument("--pr", type=int)
    parser.add_argument("--tag")
    parser.add_argument("--archive", type=Path)
    parser.add_argument("--version")
    args = parser.parse_args()
    if args.operation == "verify-archive":
        if not all((args.archive, args.source, args.version)):
            parser.error("verify-archive requires --archive, --source and --version")
        metadata = read_archive(args.archive)
        if metadata.get("source_commit") != args.source or metadata.get("version") != args.version:
            raise ValueError("archive source/version differs from the requested build")
        verify_archive_checksum(args.archive, metadata)
        print(json.dumps(metadata, sort_keys=True))
        return
    if args.operation == "context":
        result = event_context(json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text()),
                               os.environ["GITHUB_EVENT_NAME"], os.environ["GITHUB_REF"],
                               os.environ["GITHUB_SHA"], os.environ["GITHUB_REPOSITORY"])
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for key, value in result.items():
                if "\n" in value or "\r" in value:
                    raise ValueError("invalid workflow output")
                output.write(f"{key}={value}\n")
        return
    github = GitHub(os.environ["GITHUB_REPOSITORY"])
    if args.operation in ("catalog-sync", "catalog-context"):
        if not args.tag or not args.source:
            parser.error("catalog operations require --tag and --source")
        if args.operation == "catalog-context":
            result = catalog_context(github, args.tag, args.source)
            with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                output.write("json=" + json.dumps(result, separators=(",", ":")) + "\n")
        else:
            result = sync_catalog(github, args.tag, args.source)
    elif args.operation == "promote":
        if not args.pr or args.pr < 1:
            parser.error("--pr is required")
        result = promote(github, args.pr)
    else:
        if not all((args.directory, args.branch, args.source)):
            parser.error("publish requires directory, branch and source")
        with tempfile.TemporaryDirectory(prefix="interceptor-publish-") as temporary:
            inventory, files = assemble(args.directory.resolve(), Path(temporary) / "assets")
            result = publish(github, args.branch, args.source, inventory, files)
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                output.write("published=" + str(result == "prerelease-published").lower() + "\n")
    print(json.dumps({"status": result}))


if __name__ == "__main__":
    main()
