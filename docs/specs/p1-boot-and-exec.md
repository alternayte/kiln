# P1 boot and exec

## What it does
Boots one Firecracker microVM under jailer from a rootfs, runs a command inside it through a vsock agent, prints the output, and shuts down clean. The gate repeats the cycle under `scripts/leak.sh`.

## Decisions
- `internal/runtime` owns Firecracker and is the only package that names it. Start, pause, resume, snapshot and stop are its methods.
- Every VM runs under `jailer`: its own chroot, its own cgroup, seccomp on, privileges dropped, no host path outside the jail. Running `firecracker` directly is allowed only in a unit test that touches no network.
- `guest/kilninit` is PID 1. It mounts `/proc`, `/sys` and `/dev`, then serves exec requests on a vsock port.
- One exec request opens one vsock connection and one process group. Execs run concurrently, so a background server does not block the next command.
- The agent kills the whole process group when a timeout expires.
- `timeout_seconds` defaults to 60 and caps at 3600. A larger value returns `invalid`.
- `cwd` defaults to `/`. A missing directory returns `invalid`.
- The environment is a fixed base (`PATH`, `HOME=/root`) plus the request `env`, with the request winning.
- The exit code is the child's status. A killed or timed-out child returns `-1` with `timed_out` set.
- Shutdown asks Firecracker to stop, waits for the process, and removes the jail directory and the cgroup.
- P1 VMs have no network device. The TAP and the egress chain arrive in P4.
- Every VM test runs inside `scripts/leak.sh`.

## Out
- No API, no database, no snapshots, no image import, no template.
- No published ports and no ingress.

## How I know it works
- `just gate P1` boots a VM from the test rootfs, execs `echo hello`, reads `hello`, and shuts down.
- The cycle runs 20 times inside `scripts/leak.sh`, and the before and after counts are equal.
