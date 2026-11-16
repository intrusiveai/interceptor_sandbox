#!/usr/bin/env python3
"""Offline installed CLI check on a runner without the 1Password desktop app."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess


def check(binary):
    request = {"authentication": "onepassword-desktop", "account": "release-smoke",
               "reference": "op://release-smoke/missing/password", "maximum": 64}
    env = {key: value for key, value in os.environ.items() if not key.startswith("OP_")}
    process = subprocess.Popen([str(binary.resolve()), "__onepassword-read"], stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env, start_new_session=True)
    try:
        raw, _ = process.communicate(json.dumps(request).encode(), timeout=15)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.communicate()
        raise ValueError("offline SDK initialization did not return within 15 seconds") from None
    if process.returncode != 0 or len(raw) > 1024 or json.loads(raw) != {"error": "desktop_unavailable"}:
        raise ValueError("offline SDK check requires a clean runner without desktop integration")
    print("Installed SDK child returned the expected safe missing-desktop result.")

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    check(parser.parse_args().binary)
