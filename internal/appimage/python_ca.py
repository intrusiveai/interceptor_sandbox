"""Embedded build helper; executed only by declared image interpreters with -I."""

import json
from pathlib import Path
import ssl
import stat
import sys

import certifi


def certificates(data):
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.load_verify_locations(cadata=data.decode("ascii"))
    return set(context.get_ca_certs(binary_form=True))


def main():
    mode, ca_file, mounts_json = sys.argv[1:]
    roots = [Path(root) for root in json.loads(mounts_json)]
    bundle = Path(certifi.where())
    if not bundle.is_absolute():
        raise ValueError("certifi.where() must return an absolute persistent bundle path")
    resolved = bundle.resolve(strict=True)
    # Check both logical and resolved paths: a symlink may cross a mount boundary.
    for candidate in (bundle, resolved, Path(sys.executable), Path(sys.executable).resolve(),
                      Path(sys.prefix), Path(certifi.__file__).resolve()):
        for root in roots:
            for mount in (root, root.resolve()):
                if candidate == mount or mount in candidate.parents:
                    raise ValueError(f"{candidate} conflicts with runtime/image mount {root}; move the Python environment outside mounted paths")
    if not stat.S_ISREG(resolved.stat().st_mode):
        raise ValueError(f"Certifi bundle is not a regular file: {resolved}")
    existing = resolved.read_bytes()
    before = certificates(existing)
    ca = Path(ca_file).read_bytes()
    added = certificates(ca)
    if len(added) != 1:
        raise ValueError("sandbox CA must contain one CA certificate")
    if not added <= before:
        if mode != "install":
            raise ValueError("sandbox CA missing after installation; bundle must persist between interpreter launches")
        combined = existing + b"\n" + ca
        if certificates(combined) != before | added:
            raise ValueError("combined bundle did not preserve original certificates")
        # In-place writing preserves the resolved file's owner and permissions.
        with resolved.open("r+b") as stream:
            stream.write(combined)
            stream.truncate()
    if not before | added <= certificates(resolved.read_bytes()):
        raise ValueError("installed Certifi bundle failed verification")
    print(str(resolved))


if __name__ == "__main__":
    main()
