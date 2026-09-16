# recipe: check
# Run every check in checks/, then the Go stack gates.
check:
    #!/usr/bin/env bash
    set -uo pipefail
    fail=0
    # The kiln binary embeds the guest agent, so the agent is built first.
    # A stale one would install an agent nobody wrote.
    bash scripts/build-kilninit.sh > /dev/null || fail=1
    for c in checks/*.sh; do
        [ -e "$c" ] || continue
        if ! bash "$c"; then
            echo "FAIL $c" >&2
            fail=1
        fi
    done
    unformatted="$(gofmt -l . 2>/dev/null)"
    if [ -n "$unformatted" ]; then
        echo "FAIL gofmt" >&2
        echo "$unformatted" >&2
        fail=1
    fi
    go vet ./... || fail=1
    go build ./... || fail=1
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./guest/kilninit || fail=1
    go test ./... || fail=1
    go test -tags=kvm -run '^$' ./tests/... || fail=1
    (cd infra && go build -o /dev/null ./...) || fail=1
    exit "$fail"

# recipe: agents
# Sync global agent files and skills, then verify this repo's agent files.
agents:
    bash ~/dotfiles/agents/sync.sh
    bash checks/agents-md.sh
    bash checks/check-headers.sh

# recipe: check-slow
# Run the checks too slow for every `just check`. Today: run the read-only commands quoted in AGENTS.md.
check-slow:
    bash checks/agents-md.sh --run

# recipe: preflight
# Assert the host can run VM work. Fails closed. Never skips.
preflight:
    bash scripts/preflight.sh

# recipe: status
# List the gates and name the first gate without a tag.
status:
    #!/usr/bin/env bash
    set -euo pipefail
    next=""
    for g in P0 P1 P2 P3 P4 P5 P6; do
        case "$g" in
            P0|P3|P6) note=" (attended)" ;;
            *) note="" ;;
        esac
        if git rev-parse -q --verify "refs/tags/gate/$g" >/dev/null 2>&1; then
            echo "$g closed $(git rev-parse --short "gate/$g^{commit}")"
        else
            echo "$g open$note"
            [ -n "$next" ] || next="$g"
        fi
    done
    echo "next: ${next:-none}"

# recipe: gate
# Run one gate under scripts/leak.sh, then tag gate/<phase>. Gates: P0..P6, or sec.
gate phase:
    #!/usr/bin/env bash
    set -euo pipefail
    phase="{{phase}}"
    case "$phase" in
        P[0-6]) pattern="^TestGate${phase}$" ;;
        sec)    pattern="^TestSec" ;;
        *) echo "gate: unknown gate $phase. Use P0..P6 or sec." >&2; exit 2 ;;
    esac
    tmp="$(mktemp)"
    trap 'rm -f "$tmp"' EXIT
    rc=0
    scripts/leak.sh go test -tags=kvm -count=1 -timeout 20m -v -run "$pattern" ./tests/... 2>&1 | tee "$tmp" || rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "gate $phase: tests failed" >&2
        exit "$rc"
    fi
    if [ "$phase" = sec ]; then
        pass="--- PASS: TestSec"
    else
        pass="--- PASS: TestGate$phase"
    fi
    if ! grep -qF -- "$pass" "$tmp"; then
        echo "gate $phase: $pass did not appear; the gate test did not run" >&2
        exit 1
    fi
    git tag -f "gate/$phase"
    echo "gate $phase: tagged"

# recipe: loop
# Run the unattended gates in order. Stop at the first open attended gate.
loop:
    #!/usr/bin/env bash
    set -euo pipefail
    for g in P0 P1 P2 P3 P4 P5 P6; do
        if git rev-parse -q --verify "refs/tags/gate/$g" >/dev/null 2>&1; then
            continue
        fi
        case "$g" in
            P0|P3|P6)
                echo "loop: $g is attended. Stop."
                exit 0
                ;;
        esac
        just gate "$g"
    done
    echo "loop: every gate is closed"

# recipe: demo
# Run the v1 done-line. The script refuses until P6 exists.
demo:
    bash scripts/demo.sh

# recipe: review
# Review the branch diff against the default branch on two axes, each in a fresh read-only pi session.
# Axis 1, correctness: sees the diff and the repo. Runs on a model family that did not write the branch. REVIEW_MODEL overrides the model.
# Axis 2, spec compliance: sees docs/specs/<slug>.md and the diff, nothing else. <slug> is the branch name after its last /,
# or the one spec file the branch adds or changes. No spec: axis 2 says so in one line and no model runs.
review base="": check-slow
    #!/usr/bin/env bash
    set -euo pipefail
    base="{{base}}"
    if [ -z "$base" ]; then
        base="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || echo main)"
    fi
    mb="$(git merge-base "$base" HEAD)"
    tmp="$(mktemp -d -t review.XXXXXX)"
    trap 'rm -rf "$tmp"' EXIT
    git diff "$mb" > "$tmp/branch.diff"
    [ -s "$tmp/branch.diff" ] || { echo "no diff against $base"; exit 0; }

    family() {
        case "$(tr '[:upper:]' '[:lower:]' <<< "$1")" in
            *anthropic*|*claude*) echo anthropic ;;
            *openai*|*gpt*) echo openai ;;
            *deepseek*|ds/*) echo deepseek ;;
            *qwen*) echo qwen ;;
            *gemini*|*google*) echo google ;;
            *) echo "review: unknown model family: $1. Add it to family() in the review recipe." >&2; return 1 ;;
        esac
    }
    model="${REVIEW_MODEL:-or/openai/gpt-5.6-sol}"
    rf="$(family "$model")"
    # Builder families: commit authors and trailers on the branch, and the local pi default model when one is set.
    builders="$(git log "$mb"..HEAD --format='%an %ae %B' | grep -Eio 'claude|anthropic|gpt|openai|deepseek|qwen|gemini' | sort -u || true)"
    pidefault="$(jq -r '(.defaultProvider // "") + "/" + (.defaultModel // "")' "$HOME/.pi/agent/settings.json" 2>/dev/null || true)"
    for b in $builders "${pidefault:-}"; do
        [ -n "$b" ] && [ "$b" != "/" ] || continue
        bf="$(family "$b" 2>/dev/null || true)"
        [ "$bf" != "$rf" ] || { echo "review: $model is family $rf, which wrote this branch ($b). Set REVIEW_MODEL to another family." >&2; exit 1; }
    done

    branch="$(git rev-parse --abbrev-ref HEAD)"
    slug="${branch##*/}"
    spec=""
    if [ -f "docs/specs/$slug.md" ]; then
        spec="docs/specs/$slug.md"
    else
        touched="$(git diff --name-only --diff-filter=AM "$mb" -- 'docs/specs/*.md')"
        [ "$(grep -c . <<< "$touched" || true)" = 1 ] && { spec="$touched"; slug="$(basename "$spec" .md)"; }
    fi

    ro=(--no-session --no-extensions --no-skills --no-prompt-templates --tools read,grep,find,ls --model "$model")
    status=0

    echo "== Axis 1: correctness ($model)"
    pi "${ro[@]}" -p @"$tmp/branch.diff" "Review this diff for correctness bugs only. Read AGENTS.md and the touched files for context. You are read-only. A finding is a test: for each bug give the path tests/review/$slug/<name> in the repo's test framework, the full test file, the failing input, and the wrong result the test shows on HEAD. The test must fail on HEAD. A bug you cannot express as a test that fails on HEAD goes under a 'Nits' heading. No style notes. No praise. If there are no bugs, say so in one line." || status=1

    echo
    echo "== Axis 2: spec compliance"
    if [ -z "$spec" ]; then
        echo "No spec for branch $branch: docs/specs/$slug.md does not exist and the branch changes no single spec. Axis 2 stops."
    else
        mkdir "$tmp/axis2"
        cp "$spec" "$tmp/axis2/spec.md"
        git diff "$mb" -- . ':(exclude)docs/specs' ':(exclude)tests/review' > "$tmp/axis2/branch.diff"
        echo "spec: $spec"
        (cd "$tmp/axis2" && pi "${ro[@]}" --no-context-files -p @spec.md @branch.diff "You check spec compliance. You have exactly two inputs: spec.md and branch.diff. Read nothing else. For each requirement in spec.md, in spec order, output one line: the requirement, then met, not met, or built differently. For built differently, state what the spec asks for and what the code does. Do not judge which is better. Then a heading 'Unrequested changes': list each change in the diff outside the spec's scope, as file and one line, without judgement. Do not judge code quality or correctness.") || status=1
    fi
    exit "$status"

# recipe: dev
# Start the app. The command is derived from the files in the repo.
dev:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -f go.mod ]; then exec go run .
    elif [ -f package.json ] && jq -e '.scripts.dev' package.json > /dev/null; then exec bun run dev
    elif ls *.sln *.csproj > /dev/null 2>&1; then exec dotnet watch run
    else echo "dev: no go.mod, package.json dev script or .NET project. Replace this recipe." >&2; exit 1
    fi

# recipe: model
# Set the pi default provider. The model is the first model of that provider in models.json.
model name:
    #!/usr/bin/env bash
    set -euo pipefail
    models="$HOME/.pi/agent/models.json"
    settings="$HOME/.pi/agent/settings.json"
    id="$(jq -er --arg p "{{name}}" '.providers[$p].models[0].id' "$models")" || {
        echo "model: no provider '{{name}}' in $models. Valid: $(jq -r '.providers | keys | join(", ")' "$models")" >&2
        exit 1
    }
    tmp="$(mktemp)"
    jq --arg p "{{name}}" --arg m "$id" '.defaultProvider = $p | .defaultModel = $m' "$settings" > "$tmp"
    mv "$tmp" "$settings"
    echo "pi default: {{name}}/$id"

# Write the generated clients, the OpenAPI document and the agent files.
sdk:
    go run ./cmd/kiln-sdk
