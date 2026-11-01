---
name: kiln
description: Run code in Kiln sandboxes and put a change on a preview environment. Use when a task touches the kiln command, a Kiln sandbox, template, snapshot or fork, or asks to prove that a change runs.
---

# Kiln

Kiln {{VERSION}} runs isolated Linux microVMs. A template is an OCI image built once into a snapshot. A sandbox restores from it, and a fork restores it several times.

`kiln` reads `KILN_URL` and `KILN_API_KEY` from the environment. Never write the key to a file. Add `--json` to any command to read typed output.

## Work loop for a change

1. Build and push the application image. Kiln builds no application image.
2. Run `kiln preview up --image <image> --port <port> --start '<command>'`. It prints the URL when the application listens.
3. On a failure, read the error and its `next` line. Run `kiln logs <id>` for the start command output and the build result. Run `kiln egress <id>` when a network call fails: it lists each destination the host allowed or refused, and `kiln egress <id> --propose` prints a list to send to `kiln sandboxes set-egress`.
4. Fix the cause and run step 2 again. The new preview replaces the previous one of this directory.
5. Give the URL to the person. Run `kiln preview down` when the work ends.

A preview from an image that names its own start command still needs `--start`.

## Work loop for code in a sandbox

```sh
kiln templates create --name py312 --image docker.io/library/python:3.12-slim \
  --vcpus 1 --memory-mb 512 --disk-mb 2048 --egress-allow pypi.org --wait
kiln sandboxes create --template py312 --lifecycle ephemeral --idle-seconds 600 --json
kiln exec <id> -- python3 -c 'print(2+2)'
kiln sandboxes fork <id> --count 3 --json
kiln sandboxes delete <id>
```

`kiln help` lists every command, and `kiln <command> --help` names its flags.

## Rules

- Egress is deny by default. A sandbox reaches only the hostnames in `egress_allow`. A failed download usually means a missing hostname, not a broken network.
- A value in `env` makes a sandbox secret-bearing, so a fork then needs `--allow-secret-fork`.
- `kiln exec` returns the exit code of the command, so branch on it.
- Do not poll a build by hand. `--wait` does it, and `--timeout` limits it.
- Do not parse text output. Use `--json`.

## Exit codes

{{EXIT_CODES}}
