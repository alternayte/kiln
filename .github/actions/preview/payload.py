#!/usr/bin/env python3
"""Build the request bodies and read one field of an answer.

The shell owns the calls; this owns the JSON. A quote in a setup command or a
space in a hostname breaks a body a shell assembles by hand.

Not named json.py: the script's own directory comes first on sys.path, so
`import json` would import this file and every dumps call would fail.
"""
import json
import os
import re
import shlex
import sys

KEY = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def words(value):
    """A space separated list. Empty means an empty list, which for egress is
    deny everything, and that is the default for every template."""
    return value.split()


def lines(value):
    return [line for line in value.splitlines() if line.strip()]


def env_pairs(value):
    """One KEY=value per line, split on the first =. A blank line is skipped.
    Anything else is a caller error, reported before any call to Kiln."""
    pairs = {}
    for number, line in enumerate(value.splitlines(), 1):
        if not line.strip():
            continue
        if "=" not in line:
            raise ValueError(f"env line {number} has no '='")
        key, val = line.split("=", 1)
        key = key.strip()
        if not KEY.match(key):
            raise ValueError(f"env line {number}: key {key!r} must match {KEY.pattern}")
        if key in pairs:
            raise ValueError(f"env line {number}: key {key} appears twice")
        pairs[key] = val
    return pairs


def extra_ports(value, primary):
    ports = []
    for word in words(value):
        if not word.isdigit() or not 1 <= int(word) <= 65535:
            raise ValueError(f"publish entry {word!r} is not a port from 1 to 65535")
        port = int(word)
        if port == primary:
            raise ValueError(f"publish entry {port} is the same as port")
        if port in ports:
            raise ValueError(f"publish entry {port} appears twice")
        ports.append(port)
    return ports


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

    if what == "check":
        # Every caller error fails here, before the template is built. The
        # masks go out first, so no later line of the log shows a value.
        try:
            pairs = env_pairs(env.get("ENV_LINES", ""))
            extra_ports(env.get("PUBLISH", ""), int(env["PORT"]))
        except ValueError as err:
            print(err, file=sys.stderr)
            return 1
        for val in pairs.values():
            if val:
                print(f"::add-mask::{val}")
        return 0

    if what == "ports":
        for port in extra_ports(env.get("PUBLISH", ""), int(env["PORT"])):
            print(port)
        return 0

    if what == "urls":
        # "<port> <url>" lines in publish order, as one JSON object.
        urls = {}
        for line in lines(sys.stdin.read()):
            port, url = line.split(" ", 1)
            urls[port] = url
        print(json.dumps(urls))
        return 0

    if what == "sandbox":
        body = {
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
        }
        # A fork's commit never sees env: a label approves the code, not what
        # the code can reach, and a value can leave through the published port.
        pairs = env_pairs(env.get("ENV_LINES", ""))
        if pairs and env.get("FORK") != "true":
            body["env"] = pairs
        print(json.dumps(body))
        return 0

    if what == "publish":
        print(json.dumps({"port": int(argv[2]), "visibility": "public"}))
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
