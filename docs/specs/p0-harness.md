# P0 harness

## What it does
Builds the repo skeleton every later gate trusts: the Go module, the harness scripts, the gate recipes and the version pins. It proves the harness catches the failures it exists for. No VM code exists in this slice.

## Decisions
- Module `github.com/alternayte/kiln`. `cmd/kiln` carries `serve`, `ctl` and `init` stubs. The SDK will live in `client/`.
- `scripts/versions.env` is the only version file. It pins the Firecracker release, the kernel version, the kernel URL and the SHA256 of the Firecracker tarball and the kernel. v1 is x86_64.
- `kiln init` downloads the pinned Firecracker and kernel, verifies each checksum, and installs `firecracker` and `jailer` under the Kiln root. It writes `config.json`, mode 0600, with the bearer token and the listener addresses (control `127.0.0.1:8080`, ingress `0.0.0.0:443`). Later slices add fields.
- `scripts/preflight.sh` fails closed: a missing `/dev/kvm`, a missing binary, or a version that differs from the pin stops the run. It never skips.
- `scripts/leak.sh` counts Firecracker processes, `kiln-` TAPs, `kiln_` chains, Kiln mounts, sandbox directories and live rows before and after a command. Any difference fails.
- `scripts/dev-env.sh` prepares a Linux host with KVM. It fails without `/dev/kvm`, installs the missing packages, creates the Kiln root and prints the versions. It is idempotent.
- `scripts/nuke.sh` clears a host that cannot reconcile. It kills Kiln's processes, deletes its TAPs, flushes its chains, unmounts its mounts and removes the Kiln root.
- The hook stays at `.githooks/pre-commit`, which runs every `checks/staged/*.sh` on the staged diff. `checks/staged/spec-frozen.sh` refuses a commit that touches `docs/internal/sdd.md` unless `KILN_SPEC_CHANGE=1`. `checks/staged/no-skip.sh` refuses a staged test that calls `t.Skip`.
- `just preflight` runs `scripts/preflight.sh`. `just status` lists the gate tags and names the first gate without a tag. `just gate P<n>` runs that gate under `scripts/leak.sh` and tags `gate/P<n>` on success. `just gate sec` runs every `TestSec` test. `just loop` runs the unattended gates in order and stops at an attended one. `just demo` runs `scripts/demo.sh`.
- The gate order is P0 harness, P1 boot, P2 template, P3 restore, P4 fork, P5 lifecycle, P6 ingress. P0, P3 and P6 are attended.
- The GitHub workflow runs `just check` and `just gate P0` to `P5` on `ubuntu-24.04`. P6 and `just demo` run on the VPS only.
- The dev host is a Linux box with KVM reached over SSH. The laptop runs `just check` only. No Lima VM.
- No `LOG.md`. State lives in the gate tags, the specs and the commits.
- A change that does not fit the current gate is reported and placed where it belongs. Do not distort the work to fit a gate boundary.

## Out
- No Firecracker launches, no SQLite, no HTTP API.
- No ingress and no ACME.

## How I know it works
- `just gate P0` passes: `preflight.sh` exits non-zero without `/dev/kvm`, `leak.sh` fails when its wrapped command leaves a `kiln-` TAP, and `just status` names P1 after P0 is tagged.
- A staged change to `docs/internal/sdd.md` is refused, and accepted with `KILN_SPEC_CHANGE=1`.
- `just check` passes.
