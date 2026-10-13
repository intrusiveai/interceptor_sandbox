#!/usr/bin/env python3
"""File-backed S3 fixture for executing the real publication/deployment scripts."""
import hashlib
import json
import os
from pathlib import Path
import sys

args = sys.argv[1:]
root = Path(os.environ["TEST_BUCKET"])
metadata = Path(os.environ["TEST_METADATA"])
def opt(name):
    return args[args.index(name) + 1]
def etag(path):
    return '"' + hashlib.sha256(path.read_bytes()).hexdigest() + '"'
def error(code):
    print(code, file=sys.stderr)
    sys.exit(1)
def meta_path(key):
    return metadata / hashlib.sha256(key.encode()).hexdigest()
def put(key, body):
    path = root / key
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(body)
    meta_path(key).write_text(json.dumps({
        "ContentType": opt("--content-type"), "CacheControl": opt("--cache-control"),
        "ContentLength": len(body), "ETag": etag(path), "VersionId": "fixture",
    }))

key = opt("--key") if "--key" in args else ""
path = root / key
if args[:2] == ["s3api", "list-objects-v2"]:
    if os.environ.get("TEST_ACCESS_DENIED"): error("AccessDenied")
    keys = sorted(str(p.relative_to(root)) for p in root.rglob("*") if p.is_file()
                  and str(p.relative_to(root)).startswith(opt("--prefix")))
    if "--output" in args and opt("--output") == "text":
        print("\t".join(keys))
    else:
        print(json.dumps({"Contents": [{"Key": k} for k in keys]}))
elif args[:2] == ["s3api", "put-object"]:
    if key in {f"docs/releases/{p}.json" for p in ["operator_sandbox", "attack_harness", "interceptor_sandbox"]}:
        if os.environ.get("TEST_SITE_ROLE"): error("site role cannot overwrite catalog")
        assert "--if-none-match" in args or "--if-match" in args
        race = os.environ.get("TEST_RACE_BODY")
        if race and Path(race).exists():
            put(key, Path(race).read_bytes())
            Path(race).unlink()
        if os.environ.get("TEST_ALWAYS_CONFLICT"): error("PreconditionFailed")
    if "--if-none-match" in args and path.exists(): error("PreconditionFailed")
    if "--if-match" in args and (not path.exists() or opt("--if-match") != etag(path)):
        error("PreconditionFailed")
    put(key, Path(opt("--body")).read_bytes())
    print(json.dumps({"ChecksumSHA256": "fixture", "VersionId": "fixture", "ETag": etag(path)}))
elif args[:2] == ["s3api", "get-object"]:
    if not path.exists(): error("NoSuchKey")
    Path(args[-1]).write_bytes(path.read_bytes())
    print(meta_path(key).read_text())
elif args[:2] == ["s3api", "head-object"]:
    print(meta_path(key).read_text())
elif args[:2] == ["s3api", "delete-object"]:
    if key in {f"docs/releases/{p}.json" for p in ["operator_sandbox", "attack_harness", "interceptor_sandbox"]} or key.startswith("sha256/"):
        error("publisher cannot delete product data")
    path.unlink()
    print("{}")
elif args[:2] == ["cloudfront", "create-invalidation"]:
    print("{}")
else:
    error("unexpected AWS invocation: " + " ".join(args))
