#!/usr/bin/env bash
# Dispatch an ad-hoc E2E Chaos run with a random seed and a random
# profile, on demand, from a checkout.
#
# Why this exists: the chaos runner's exploration budget is the nightly
# matrix — six profiles, seeded from the run id, once a day. Touching the
# failover, drain or chassis-cleanup paths means wanting that exploration
# *now*, and the only ad-hoc path is the Actions tab, where a human picks
# the seed and the profile by hand. Picking either by hand is the problem:
# a hand-picked seed is almost always 42 (the dispatch default) and a
# hand-picked profile almost always `everything-on`, so the ad-hoc runs
# re-walk a sequence the nightly already covers. This draws both from
# /dev/urandom instead, and prints what it drew.
#
# It does not weaken the replay contract — it only makes what there is to
# replay worth replaying. The seed and profile are echoed here, recorded
# in the run's `run-start` journal event and in `summary.json`, and
# printed as a copy-pasteable replay line for CI and for a local lab, so
# a run that goes red is reproducible either way.
#
# The duration is fixed at 10 minutes: the nightly window, the length
# every recovery budget and every recorded comparison baseline was
# calibrated against, and comfortably inside the workflow's 40-minute job
# timeout.
#
# The profile list is not duplicated here. It is read out of the dispatch
# input's `choice` options in .github/workflows/e2e-chaos.yml — the list
# `TestChaosWorkflowSweepsEveryProfile` (test/e2e/chaos/profiles_test.go)
# already guards against drift from the registry in
# test/e2e/chaos/profiles.go — so a profile added to the registry becomes
# reachable here as soon as that test passes.
#
# Usage:
#   test/e2e/chaos-random.sh                    # one run, random seed, random profile
#   test/e2e/chaos-random.sh -n 3               # three independent runs at once
#   test/e2e/chaos-random.sh --profile pf-only  # pin the profile, keep the random seed
#   test/e2e/chaos-random.sh --seed 12345       # pin the seed, keep the random profile
#   test/e2e/chaos-random.sh --ref my-branch    # dispatch against a pushed branch
#   test/e2e/chaos-random.sh --watch            # wait for the run, then render its report
#   test/e2e/chaos-random.sh --dry-run          # draw and print, dispatch nothing
#
# Requires `gh`, authenticated (`gh auth status`). `--watch` waits for
# every dispatched run and renders it through
# `go run ./test/e2e/chaos -report`, exiting non-zero if any run did; with
# no Go toolchain on PATH it prints the report command instead.
#
# Environment overrides:
#   WORKFLOW     workflow file to dispatch (default e2e-chaos.yml)
#   DURATION     fault-injection window (default 10m)
#   REMOTE       git remote the ref must exist on (default origin)
#   RUN_TIMEOUT  seconds to wait for a dispatched run to appear (default 60)

set -euo pipefail

WORKFLOW="${WORKFLOW:-e2e-chaos.yml}"
DURATION="${DURATION:-10m}"
REMOTE="${REMOTE:-origin}"
RUN_TIMEOUT="${RUN_TIMEOUT:-60}"

RUNS=1
REF=""
PIN_SEED=""
PIN_PROFILE=""
WATCH=0
DRY_RUN=0

REPO=""
PROFILES=()
DISPATCHED=()

log() { printf '[chaos-random] %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

usage() {
    cat >&2 <<'EOF'
Dispatch an E2E Chaos run with a random seed and a random profile.

  test/e2e/chaos-random.sh [options]

  -n, --runs N        dispatch N runs, each with its own draw (default 1)
      --profile NAME  pin the profile instead of drawing one
      --seed N        pin the seed instead of drawing one
      --ref REF       branch to dispatch against (default: current branch)
      --watch         wait for each run and render its report
      --dry-run       draw and print the dispatch command, dispatch nothing
  -h, --help          this text

The duration is always 10m. See the file header for the rationale.
EOF
}

parse_args() {
    while (( $# )); do
        case "$1" in
        -n|--runs) RUNS="${2:-}"; shift 2 ;;
        --profile) PIN_PROFILE="${2:-}"; shift 2 ;;
        --seed)    PIN_SEED="${2:-}"; shift 2 ;;
        --ref)     REF="${2:-}"; shift 2 ;;
        --watch)   WATCH=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *)         log "unknown argument: $1"; usage; exit 1 ;;
        esac
    done

    case "${RUNS}" in
    ''|*[!0-9]*) die "-n takes a positive integer, got: ${RUNS}" ;;
    esac
    (( RUNS >= 1 )) || die "-n takes a positive integer, got: ${RUNS}"

    # The workflow rejects anything but a bare non-negative integer seed
    # (it fans one out through GITHUB_OUTPUT into a matrix). Fail here
    # rather than burn a dispatch on a value it will reject.
    if [ -n "${PIN_SEED}" ]; then
        case "${PIN_SEED}" in
        *[!0-9]*) die "--seed takes a non-negative integer, got: ${PIN_SEED}" ;;
        esac
    fi
}

# The dispatch input's choice options, in registry order. Read from the
# workflow rather than hard-coded, so this script can never become the
# place a new profile is missing from. Anchored on the `profile:` input
# key rather than on the first `options:` block, so a second choice input
# added to the workflow later cannot silently redirect the scan; the
# matrix's `profile: ${{ … }}` has a value on the same line and does not
# match.
workflow_profiles() {
    awk '
        /^[[:space:]]*profile:[[:space:]]*$/ { inprofile = 1; next }
        inprofile && /^[[:space:]]*options:[[:space:]]*$/ { inblock = 1; next }
        inblock && /^[[:space:]]*-[[:space:]]/ {
            sub(/^[[:space:]]*-[[:space:]]*/, "")
            gsub(/["'"'"']/, "")
            sub(/[[:space:]]*(#.*)?$/, "")
            if (length($0)) print
            next
        }
        inblock { exit }
    ' "$1"
}

# A 32-bit draw from /dev/urandom. $RANDOM is 15 bits and seeded per
# shell, which makes two invocations in the same second collide often
# enough to matter when a fresh sequence is the entire point.
random_uint32() {
    od -An -N4 -tu4 < /dev/urandom | tr -d '[:space:]'
}

# The modulo bias of 2^32 over a handful of profiles is far below the
# noise of a randomized fault sequence.
random_profile() {
    local index
    index=$(( $(random_uint32) % ${#PROFILES[@]} ))
    printf '%s\n' "${PROFILES[${index}]}"
}

preflight() {
    command -v gh >/dev/null 2>&1 || die "gh is not installed — see https://cli.github.com"
    gh auth status >/dev/null 2>&1 || die "gh is not authenticated — run: gh auth login"

    REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
    [ -n "${REPO}" ] || die "could not resolve the repository — run gh repo set-default"

    # A ref that exists only locally produces an opaque 422 from the
    # dispatch API, so check it here and say what to do about it.
    if ! git ls-remote --exit-code --heads "${REMOTE}" "${REF}" >/dev/null 2>&1; then
        die "branch ${REF} does not exist on ${REMOTE} — push it first, or pass --ref main"
    fi
}

newest_run_id() {
    gh run list -R "${REPO}" --workflow "${WORKFLOW}" --event workflow_dispatch \
        --limit 1 --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null || true
}

# Poll for the run the dispatch just created: `gh workflow run` returns no
# id, so the newest dispatch run whose id differs from the one seen just
# before is ours. Runs appear within a second or two; the deadline only
# bounds the wait when the API lags.
resolve_run_id() {
    local before="$1" deadline newest
    deadline=$(( $(date +%s) + RUN_TIMEOUT ))
    while (( $(date +%s) < deadline )); do
        newest="$(newest_run_id)"
        if [ -n "${newest}" ] && [ "${newest}" != "${before}" ]; then
            printf '%s\n' "${newest}"
            return 0
        fi
        sleep 2
    done
    return 1
}

dispatch_one() {
    local seed profile before run_id
    seed="${PIN_SEED:-$(random_uint32)}"
    profile="${PIN_PROFILE:-$(random_profile)}"

    log "seed=${seed} profile=${profile} duration=${DURATION} ref=${REF}"

    if (( DRY_RUN )); then
        printf 'gh workflow run %s --ref %s -f seed=%s -f profile=%s -f duration=%s\n' \
            "${WORKFLOW}" "${REF}" "${seed}" "${profile}" "${DURATION}"
        return 0
    fi

    before="$(newest_run_id)"
    gh workflow run "${WORKFLOW}" -R "${REPO}" --ref "${REF}" \
        -f "seed=${seed}" \
        -f "profile=${profile}" \
        -f "duration=${DURATION}" >/dev/null

    if ! run_id="$(resolve_run_id "${before}")"; then
        log "dispatched, but the run did not appear within ${RUN_TIMEOUT}s — find it with:"
        log "  gh run list -R ${REPO} --workflow ${WORKFLOW} --event workflow_dispatch"
        DISPATCHED+=("?:${seed}:${profile}")
        return 0
    fi

    log "run ${run_id}: https://github.com/${REPO}/actions/runs/${run_id}"
    DISPATCHED+=("${run_id}:${seed}:${profile}")
}

summarize() {
    local entry run_id seed profile
    log "dispatched ${#DISPATCHED[@]} run(s); replay any of them with:"
    for entry in "${DISPATCHED[@]}"; do
        IFS=: read -r run_id seed profile <<<"${entry}"
        log "  CI:    gh workflow run ${WORKFLOW} --ref ${REF} -f seed=${seed} -f profile=${profile} -f duration=${DURATION}"
        log "  local: make e2e-up && make e2e-chaos CHAOS_FLAGS=\"-seed ${seed} -profile ${profile} -duration ${DURATION}\""
    done
}

# Wait for each dispatched run and render its record — the verdict, the
# recovery durations, the loss windows attributed to their faults — the
# same way each job renders its own summary. It reads the uploaded
# artifacts, so a red run reports too; only a run that died before the
# runner wrote a record has nothing to show.
watch_and_report() {
    local entry run_id seed profile url status=0
    for entry in "${DISPATCHED[@]}"; do
        IFS=: read -r run_id seed profile <<<"${entry}"
        [ "${run_id}" != "?" ] || continue

        log "waiting for run ${run_id} (seed ${seed}, profile ${profile})"
        gh run watch "${run_id}" -R "${REPO}" --exit-status >/dev/null || status=1

        url="https://github.com/${REPO}/actions/runs/${run_id}"
        if command -v go >/dev/null 2>&1; then
            go run ./test/e2e/chaos -report "${url}" ||
                log "the report could not be rendered for run ${run_id}"
        else
            log "no Go toolchain on PATH — render the report with:"
            log "  make e2e-chaos-report CHAOS_RUN=${url}"
        fi
    done
    return "${status}"
}

main() {
    parse_args "$@"

    command -v git >/dev/null 2>&1 || die "git is not installed"
    local repo_root workflow_file profile known
    repo_root="$(git rev-parse --show-toplevel)"
    # `--watch` renders the report with `go run ./test/e2e/chaos`, which
    # is a repo-relative package path, so run from the root regardless of
    # where the script was invoked.
    cd "${repo_root}"
    workflow_file="${repo_root}/.github/workflows/${WORKFLOW}"
    [ -f "${workflow_file}" ] || die "no such workflow: ${workflow_file}"

    # `+=` rather than mapfile: /bin/bash on macOS is 3.2, where mapfile
    # does not exist and expanding an empty array under `set -u` is an
    # error.
    while IFS= read -r profile; do
        PROFILES+=("${profile}")
    done < <(workflow_profiles "${workflow_file}")
    (( ${#PROFILES[@]} )) || die "no profiles in ${workflow_file} — has the dispatch input changed?"

    if [ -n "${PIN_PROFILE}" ]; then
        known=0
        for profile in "${PROFILES[@]}"; do
            [ "${profile}" != "${PIN_PROFILE}" ] || known=1
        done
        (( known )) || die "unknown profile: ${PIN_PROFILE} (known: ${PROFILES[*]})"
    fi

    if [ -z "${REF}" ]; then
        REF="$(git rev-parse --abbrev-ref HEAD)"
        [ "${REF}" != "HEAD" ] || die "detached HEAD — pass an explicit --ref"
    fi

    log "profiles: ${PROFILES[*]}"
    (( DRY_RUN )) || preflight

    local i
    for (( i = 0; i < RUNS; i++ )); do
        dispatch_one
    done

    if (( DRY_RUN )); then
        return 0
    fi

    summarize

    # The exit code carries the runs' verdict only when we waited for it;
    # a plain dispatch is done the moment the runs exist.
    if (( WATCH )); then
        watch_and_report
        return $?
    fi
    return 0
}

main "$@"
