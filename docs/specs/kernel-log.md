# Kernel log

## What it does
A caller reads the last lines of the guest kernel log of one sandbox. Factoree uses it to prove that the guest kernel killed a check because the check used too much memory. Rootless Docker in the Factoree image runs without cgroups, so `docker inspect` never shows `OOMKilled`, and an exec cannot read `/dev/kmsg` without root (#33).

## Decisions
- `GET /v1/sandboxes/{id}/console?lines=N` returns `200 {"lines": [...]}`, oldest first. The path is the one the Factoree contract names. The docs, the CLI and the code say "kernel log", because AGENTS.md reserves the last word of the path for the terminal.
- `lines` defaults to 200. A value outside 1 to 2000, or a value that is not a number, returns 400.
- The route reads `console.log` in the sandbox directory. Firecracker writes the guest serial output there. The guest kernel command line sends its output to `ttyS0` and has no `quiet`, so the kernel prints `KERN_ERR` lines, and "Out of memory: Killed process" is one of them. kilninit writes its own log lines to the same file.
- Each VM start truncates the file. A fork, a wake and a rewind start a new VM, so they start with no lines. Factoree needs only the lines of the current VM.
- A sleeping sandbox does not wake. It returns the lines of the VM that slept, because the directory stays during sleep.
- A sandbox with no file returns `{"lines": []}`.
- A template with an empty user runs everything as root, and root in the guest can write to the serial port without end. Two bounds stop it from filling the host disk:
  - Firecracker limits the serial output to 1 MiB per 10 minutes (`rate_limiter` on `PUT /serial`), and it drops the bytes above that. A boot and an out-of-memory report fit in the limit many times. The setting is not in a snapshot, so a restore sends it again before the load.
  - Firecracker writes the file with `O_APPEND`. A read that finds the file larger than 8 MiB cuts it to its last 1 MiB, from a line start. A line that the guest writes during the cut can be lost.
- The route reads at most the last 4 MiB of the file.
- The route needs `sandbox:read`. The gateway applies it from the apispec operation, so a reader sees the kernel log.
- An unknown sandbox returns 404. A destroyed sandbox also returns 404, on this route and on every call that needs a live sandbox, such as exec, file reads, logs, snapshot, fork and the timeline. Before this change those calls returned 409, and a caller could not tell a missing file from a missing sandbox (#8). `GET /v1/sandboxes/{id}` still returns the destroyed row.
- `kiln kernel-log <id> --lines N` prints one line of the log on each output line.
- The release is v0.18.0, because this is a new route.

## Out
- No stream of new lines. A caller reads again.
- No kernel log of a template build. The build error already carries the last lines.
- No history of the parent VM in a fork.

## How I know it works
- In a running sandbox of a 512 MiB template, a python process that adds 64 MiB of written bytes in a loop exits with a kill, and `GET /v1/sandboxes/{id}/console?lines=2000` shows the "Out of memory: Killed process <pid> (python…)" line. The sec gate check C10 does this.
- A fork of that sandbox returns no kill line.
- An unknown or a destroyed sandbox returns 404, and exec on a destroyed sandbox returns 404. `TestKernelLog` pins this.
