#!/usr/bin/env bash
# Terminal-Bench 4.0: observed 5/5 success and 0/5 failure for Codex 0.159.0
# with GPT-6.1-Sol / max. Evidence and comparison caveats: docs/terminal-bench.md.
set -euo pipefail
umask 077

usage() {
    cat <<'EOF'
Usage, from the TTC checkout root:
  ./prepare.sh [1|2]        Prepare/resume the task and open interactive TTC
  ./prepare.sh shell 1|2    Open another shell to observe the task
  ./prepare.sh grade 1|2    Grade in a fresh verifier; save results and workspace
  ./prepare.sh cleanup 1|2  Remove task containers and archive the run for a retry

1 = mvcc-lsm-compaction (default; Codex 5/5)
2 = bun-sourcemap-leak (Codex 0/5; known instruction ambiguity)
In TTC: use /model to select GPT-6.1-Sol / max, then type your own prompt.
Original task instructions: /task.md inside the container.

Requires local Podman, Git, Make, Go 1.26.5+, and Internet access.
Copies only TTC's credential file, never your whole home or history.
New runs live in term-bench/v4/; existing 2.0 runs are left untouched.
Exit TTC and any grader before cleanup. Images and source downloads are retained.
No automatic agent time limit (official: 8 hours); no storage quota.
See docs/terminal-bench.md for evidence, limits, and fresh-run instructions.
EOF
}

die() { printf 'Error: %s\n' "$*" >&2; exit 1; }
ACTION=run
case "${1:-1}" in
    -h|--help) usage; exit 0 ;;
    shell|grade|cleanup) ACTION=$1; shift ;;
esac
[[ $# -le 1 ]] || { usage >&2; exit 1; }
[[ "$ACTION" != cleanup || $# -eq 1 ]] || { usage >&2; exit 1; }
case "${1:-1}" in
    1)
        TASK=mvcc-lsm-compaction
        TASK_REF=370406dd79cc164e50b73568de37c34eba1b0e30782d7dc03f87c38c68123137
        AGENT_IMAGE=docker.io/harborframework/terminal-bench:mvcc-lsm-compaction-environment-3a74f5fdb94f7d43@sha256:4f86b06046a0d4def8b779cf0f3ed0ac61165e3cd8ef9ae82d89a8075888bd7f
        VERIFIER_IMAGE=docker.io/harborframework/terminal-bench:mvcc-lsm-compaction-verifier-1cb69b730abb8ada@sha256:d65f351fece158955ff26b320988f1349c54aaeae607e34064495fc4ab7fd1d9
        VERIFY_TIMEOUT=900
        ARTIFACTS=(Makefile crash_report.txt
            include/mvcc_lsm/{flush_builder,lsm_db,snapshot_context,types,version,version_store}.h
            src/{flush_builder,lsm_db,snapshot_context,version_store}.cc
            tests/{regression_test,stress_repro}.cc)
        ;;
    2)
        TASK=bun-sourcemap-leak
        TASK_REF=e55ae542f338f50f4468d940c7d7c93a45d34fd418dc6cc5883c1fedc753603a
        AGENT_IMAGE=docker.io/harborframework/terminal-bench:bun-sourcemap-leak-environment-af9a682ece894ebd@sha256:63c69d9bc2334b93f3e17662670ee3d283238029ef94542aa84eb0c91207518e
        VERIFIER_IMAGE=docker.io/harborframework/terminal-bench:bun-sourcemap-leak-verifier-4f21b4a5a4e1a334@sha256:5757c422d338c7b9ef9d7f008d5c8e5b98572379713aeee88cf3b3d5bdf67ea3
        VERIFY_TIMEOUT=600
        ARTIFACTS=(package.json tsconfig.json visibility.json scripts src)
        ;;
    *) usage >&2; exit 1 ;;
esac
ROOT=$(pwd -P)
[[ -f go.mod && -f cmd/ttc/main_linux.go ]] || die 'Start in the TTC checkout root.'
WORK="$ROOT/term-bench"
BENCH="$WORK/source-v4"
SAVE="$WORK/v4/$TASK"
C="ttc-bench-v4-$TASK"
V="$C-verifier"
REV=452bf305c6daa62fc59061d22133a7cbc7c1572e
[[ ! -L "$WORK" ]] || die "$WORK must not be a symlink."
command -v podman >/dev/null || die 'Install Podman first.'
podman info >/dev/null

if [[ "$ACTION" == cleanup ]]; then
    [[ ! -L "$WORK/v4" && ! -L "$SAVE" ]] || die 'Refusing cleanup through a symlink.'
    [[ ! -e "$SAVE" || -d "$SAVE" ]] || die "Unexpected run path: $SAVE"
    # Validate both names before removing either container.
    containers=()
    for container in "$C" "$V"; do
        if podman container exists "$container"; then
            [[ "$(podman inspect --format '{{index .Config.Labels "ttc-bench.root"}}' "$container")" == "$ROOT" ]] ||
                die "Container $container belongs to another checkout; refusing cleanup."
            containers+=("$container")
        fi
    done
    # Graded runs already have a frozen workspace. Preserve ungraded work too.
    if [[ -d "$SAVE" && ! -e "$SAVE/app" && ! -L "$SAVE/app" ]] && podman container exists "$C"; then
        snapshot=$(mktemp -d "$SAVE/snapshot.XXXXXX")
        podman cp "$C:/app" "$snapshot/app"
        mv "$snapshot/app" "$SAVE/app"
        rmdir "$snapshot"
    fi
    for container in "${containers[@]}"; do
        podman rm -f "$container" >/dev/null
    done
    if [[ -d "$SAVE" ]]; then
        archive=$(mktemp -d "$SAVE.saved-$(date +%Y%m%d-%H%M%S).XXXXXX")
        # Replace only our reserved empty directory; never nest into an old run.
        mv -T -- "$SAVE" "$archive"
        printf 'Archived run: %s\n' "$archive"
    else
        printf 'No saved run to archive for %s.\n' "$TASK"
    fi
    printf 'Ready for a fresh attempt: ./prepare.sh %s\n' "$1"
    exit 0
fi

mkdir -p "$WORK"
chmod 700 "$WORK"

if ! podman container exists "$C"; then
    [[ "$ACTION" == run ]] || die "Prepare the task first: ./prepare.sh ${1:-1}"
    [[ ! -e "$SAVE" ]] || die "Previous/incomplete run at $SAVE; move it aside before preparing again."
    for cmd in git make go install; do
        command -v "$cmd" >/dev/null || die "Required command not found: $cmd"
    done
    # Match TTC's data-root resolution; never print or parse the credential file.
    DATA=${TTC_DATA_DIR:-}
    if [[ -z "$DATA" ]]; then
        if [[ ${XDG_DATA_HOME:-} == /* ]]; then DATA="$XDG_DATA_HOME/ttc"
        else DATA="$HOME/.local/share/ttc"; fi
    fi
    [[ "$DATA" == /* ]] || die 'TTC_DATA_DIR must be absolute.'
    [[ -s "$DATA/openai-auth.json" ]] || die "No credentials at $DATA/openai-auth.json; log in to TTC on the host first."

    if [[ ! -e "$BENCH" ]]; then
        git clone --depth 1 --filter=blob:none --sparse --branch v4.0.0 \
            https://github.com/harbor-framework/terminal-bench.git "$BENCH"
        git -C "$BENCH" sparse-checkout set tasks/mvcc-lsm-compaction tasks/bun-sourcemap-leak
    fi
    [[ "$(git -C "$BENCH" rev-parse HEAD)" == "$REV" ]] || die 'Unexpected benchmark revision.'
    [[ -z "$(git -C "$BENCH" status --porcelain)" ]] || die 'Benchmark checkout has local changes.'
    CGO_ENABLED=0 make build
    mkdir -p "$SAVE/state"
    install -m 755 "$ROOT/ttc" "$SAVE/ttc"
    install -m 600 "$DATA/openai-auth.json" "$SAVE/state/openai-auth.json"
    printf '%s\n' "$REV" > "$SAVE/benchmark-revision.txt"
    git rev-parse HEAD > "$SAVE/ttc-revision.txt"
    printf 'model=openai/gpt-6.1-sol\nreasoning=max\ntask=sha256:%s\nagent_image=%s\nverifier_image=%s\n' \
        "$TASK_REF" "$AGENT_IMAGE" "$VERIFIER_IMAGE" > "$SAVE/baseline.txt"

    # These digest-pinned upstream images come from the published run's exact
    # task packages. Add terminal tools only to the agent, never to the verifier.
    mkdir -p "$SAVE/build"
    printf 'FROM %s\n' "$AGENT_IMAGE" > "$SAVE/build/Containerfile"
    cat >> "$SAVE/build/Containerfile" <<'EOF'
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl ripgrep diffutils tmux git \
    && rm -rf /var/lib/apt/lists/*
EOF
    podman build -t "localhost/ttc-bench-v4:$TASK" -f "$SAVE/build/Containerfile" "$SAVE/build"
    podman run -d --name "$C" --label "ttc-bench.root=$ROOT" \
        --label "ttc-bench.revision=$REV" --init --cpus=2 --memory=4g \
        -v "$SAVE/ttc:/usr/local/bin/ttc:ro,Z" \
        -v "$SAVE/state:/ttc-state:Z" \
        -e TTC_DATA_DIR=/ttc-state -w /app \
        "localhost/ttc-bench-v4:$TASK" sleep infinity
fi

[[ "$(podman inspect --format '{{index .Config.Labels "ttc-bench.root"}}' "$C")" == "$ROOT" ]] ||
    die "Container $C belongs to another checkout; refusing to use it."
[[ "$(podman inspect --format '{{index .Config.Labels "ttc-bench.revision"}}' "$C")" == "$REV" ]] ||
    die "Container $C has a different benchmark revision."
if [[ "$(podman inspect --format '{{.State.Running}}' "$C")" != true ]]; then
    podman start "$C" >/dev/null
fi

case "$ACTION" in
    run)
        [[ ! -e "$SAVE/grading-started" ]] || die 'Already submitted for grading; use a fresh run before solving again.'
        # Repair an interrupted initial prompt upload on resume as well.
        podman cp "$BENCH/tasks/$TASK/instruction.md" "$C:/task.md"
        printf '\nTask: %s\nUse /model to select GPT-6.1-Sol / max, then type your prompt.\n' "$TASK"
        printf 'Task instructions: /task.md\nHost results/state: %s\n\n' "$SAVE"
        exec podman exec -it -e "TERM=${TERM:-xterm-256color}" "$C" ttc --workdir /app
        ;;
    shell)
        exec podman exec -it -e "TERM=${TERM:-xterm-256color}" "$C" bash
        ;;
    grade)
        [[ ! -e "$SAVE/grading-complete" ]] || die "Already graded; see $SAVE/grade.*/verifier.log."
        printf 'Exit TTC first. Grade now (retries reuse the frozen snapshot)? [y/N] '
        read -r answer
        [[ "$answer" == y || "$answer" == Y ]] || exit 0
        touch "$SAVE/grading-started"
        if [[ ! -e "$SAVE/app" ]]; then
            snapshot=$(mktemp -d "$SAVE/snapshot.XXXXXX")
            podman cp "$C:/app" "$snapshot/app"
            mv "$snapshot/app" "$SAVE/app"
            rmdir "$snapshot"
        fi
        # Local-source podman cp follows symlinks. Reject links (including any
        # artifact ancestor) and special files before they can resolve on HOST.
        [[ -d "$SAVE/app" && ! -L "$SAVE/app" ]] || die 'Unsafe workspace snapshot.'
        unsafe=$(find -P "$SAVE/app" ! -type d ! -type f -print -quit) || die 'Could not validate the snapshot.'
        [[ -z "$unsafe" ]] || die 'Snapshot contains symlinks or special files; refusing host-source upload.'
        if podman container exists "$V"; then
            [[ "$(podman inspect --format '{{index .Config.Labels "ttc-bench.root"}}' "$V")" == "$ROOT" ]] ||
                die "Verifier $V belongs to another checkout."
            podman rm -f "$V" >/dev/null
        fi
        RESULTS=$(mktemp -d "$SAVE/grade.XXXXXX")
        # No TTC state/credentials, host volumes, or agent processes enter this
        # fresh verifier. Upload only task.toml's declared artifact paths.
        podman run -d --name "$V" --label "ttc-bench.root=$ROOT" \
            --init --cpus=2 --memory=4g -w /app "$VERIFIER_IMAGE" sleep infinity
        trap 'podman stop "$V" >/dev/null 2>&1 || true' EXIT
        trap 'exit 130' INT
        trap 'exit 143' TERM
        for path in "${ARTIFACTS[@]}"; do
            podman cp "$SAVE/app/$path" "$V:/app/$path"
        done
        printf 'Running verifier; output saved to %s/verifier.log ...\n' "$RESULTS"
        status=0
        podman exec -w /app "$V" timeout --kill-after=10s "$VERIFY_TIMEOUT" \
            bash /tests/test.sh > "$RESULTS/verifier.log" 2>&1 || status=$?
        cat "$RESULTS/verifier.log"
        if ! podman cp "$V:/logs/verifier" "$RESULTS/verifier"; then
            die "Could not collect verifier output; see $RESULTS/verifier.log."
        fi
        [[ "$status" != 124 && "$status" != 137 ]] || die 'Verifier timed out; inspect the log or retry grade.'
        [[ -f "$RESULTS/verifier/reward.txt" ]] || die "Verifier produced no reward (exit $status); inspect the log or retry grade."
        case "$(cat "$RESULTS/verifier/reward.txt")" in
            1)
                [[ "$status" == 0 ]] || die "Verifier returned reward 1 but exited $status."
                touch "$SAVE/grading-complete"
                printf '\nPASS: %s\nResults: %s\n' "$TASK" "$RESULTS"
                ;;
            0)
                touch "$SAVE/grading-complete"
                printf '\nFAIL: %s (verifier exit %s)\nResults: %s\n' "$TASK" "$status" "$RESULTS"
                exit 1
                ;;
            *) die 'Invalid verifier reward; inspect the log or retry grade.' ;;
        esac
        ;;
esac
