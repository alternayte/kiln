#!/usr/bin/env python3
"""Build the request bodies and read one field of an answer.

The shell owns the calls; this owns the JSON. A quote in a setup command or a
space in a hostname breaks a body a shell assembles by hand.
"""
import json
import os
import shlex
import sys


def words(value):
    """A space separated list. Empty means an empty list, which for egress is
    deny everything, and that is the default for every template."""
    return value.split()


def lines(value):
    return [line for line in value.splitlines() if line.strip()]


def main(argv):
    what = argv[1] if len(argv) > 1 else ""
    env = os.environ

    if what == "template":
        print(json.dumps({
            "name": env["NAME"],
            "image": env["IMAGE"],
            "vcpus": int(env["VCPUS"]),
            "memory_mb": int(env["MEMORY_MB"]),
            "disk_mb": int(env["DISK_MB"]),
            "ttl_seconds": int(env["TTL_SECONDS"]),
            "egress_allow": words(env.get("EGRESS", "")),
            "setup": lines(env.get("SETUP", "")),
            # Kiln ignores what the image says to run. Without these the
            # sandbox holds the application's files and serves nothing.
            "start": shlex.split(env["START"]),
            "port": int(env["PORT"]),
        }))
        return 0

    if what == "sandbox":
        print(json.dumps({
            "template": env["NAME"],
            "lifecycle": "persistent",
            "idle_seconds": int(env["IDLE_SECONDS"]),
            # The Action finds the previous preview of one pull request from
            # these, so it keeps no state of its own.
            "metadata": {
                "repository": env["REPO"],
                "pull_request": env["PR"],
                "commit": env["SHA"],
            },
        }))
        return 0

    if what == "publish":
        print(json.dumps({"port": int(env["PORT"]), "visibility": "public"}))
        return 0

    if what == "get":
        body = json.load(sys.stdin)
        print(body.get(argv[2], "") if isinstance(body, dict) else "")
        return 0

    if what == "previews":
        # The sandboxes of this pull request, newest first, as
        # "<id> <template>" lines.
        for row in json.load(sys.stdin):
            meta = row.get("metadata") or {}
            if str(meta.get("pull_request", "")) != env["PR"]:
                continue
            if meta.get("repository", "") != env.get("REPO", meta.get("repository", "")):
                continue
            if row.get("destroyed_at"):
                continue
            print(row["id"], row.get("template", ""))
        return 0

    print(f"unknown request {what!r}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
