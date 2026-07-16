# CI pipeline

This page documents the CI gates, how `main` is protected, and the
routine maintenance a few of them need (linter version, containerlab
pin, the cross-repo PAT). The guiding contract (issue #161) is: **it
must be impossible for untested code to reach `main` or a release tag
through normal Git operations, and a dependency CVE must surface within
a week even with no PR activity.**

## Gate map

| Workflow | File | Triggers | Required check |
| --- | --- | --- | --- |
| Test | `test.yml` | PR, push `main`, weekly | **`test`** |
| Lint | `lint.yml` | PR | **`golangci-lint`, `vet`, `gofmt`, `docs-gen`** |
| Build | `build.yml` | PR | **`verify`** |
| Integration | `integration.yml` | PR (skips docs-only) | **`smoke`** |
| govulncheck | `govulncheck.yml` | PR, push `main`, weekly | no |
| E2E | `e2e.yml` | PR (skips docs-only), dispatch | no |
| E2E Chaos | `e2e-chaos.yml` | nightly, dispatch, PR label | no |
| Docs | `docs.yml` | PR touching docs | no |
| Package | `package.yml` | PR | no |
| CodeQL | `codeql.yml` | PR, push `main`, weekly | no |
| Dependency Review | `dependency-review.yml` | PR | no |
| OpenSSF Scorecard | `scorecard.yml` | push `main`, weekly | no |
| Release | `release.yml` | tag `v*` | n/a |
| Deploy Documentation | `deploy-docs.yaml` | push `main` (docs), dispatch | n/a |
| Add to project | `add-to-project.yml` | issues, PR target | n/a |

The seven **required** contexts are enforced by the branch ruleset
below. They are the workflow **job ids**, not the workflow names — a
required context named `test` is the `test:` job, `verify` is Build's
`verify:` job, and so on. **Renaming any required job id silently
detaches it from the ruleset**, so rename the ruleset context in the
same change (a linter would otherwise report the check as never
arriving).

`govulncheck`, `docs-build`, the E2E jobs, `deb-smoke`, CodeQL, and
dependency-review are deliberately **not** required: a newly published
CVE should surface loudly rather than hard-block an unrelated PR, and
the path-filtered workflows (`docs.yml`, `e2e.yml`) would leave a
required check Pending forever on PRs that do not trigger them.

## Branch ruleset on `main`

The ruleset is committed as code at
[`.github/rulesets/main.json`](https://github.com/osism/ovn-network-agent/blob/main/.github/rulesets/main.json).
It enforces:

- **`deletion`** — `main` cannot be deleted (the one rule that already
  existed).
- **`pull_request`** — changes reach `main` only through a pull request.
  Zero required approvals: the goal is to require the *PR path and the
  status checks*, not reviews, so a single maintainer is not deadlocked
  by an unapprovable self-review.
- **`required_status_checks`** with `strict_required_status_checks_policy`
  — the seven contexts above must pass **and** the branch must be up to
  date with `main` before merge. "Up to date" is what closes the
  semantic-merge-conflict gap: two PRs that each pass alone but conflict
  in behaviour cannot both merge without the second re-running against
  the first. If serialized merges become painful, a GitHub **merge
  queue** is the documented alternative — swap the strict policy for a
  `merge_queue` rule.
- **`required_signatures`** — every commit that reaches `main` must carry
  a verified signature. This is the compensating control for the
  zero-review decision (see the trust boundary below), so applying the
  ruleset requires commit signing to be configured first
  (`git config gpg.format ssh` + a `user.signingkey`, or a GPG key, plus
  `commit.gpgsign true`).

**Trust boundary.** With zero required approvals no human other than the
author inspects a change before it merges, and the required checks are
machine gates — they catch broken code, not malicious code. Because
`release.yml` builds a signed, attested release from a `v*` tag, the path
from write access to a published release has no second-party review step;
its trust boundary rests on **GitHub account security** (protect maintainer
accounts with a phishing-resistant second factor) plus the
`required_signatures` rules on `main` and on release tags — a stolen token
or hijacked session alone cannot produce a signed commit or a signed
release tag without also holding the maintainer's signing key. If the
project gains a second maintainer, raise `required_approving_review_count`
to `1` to add mandatory review.

Applying or changing the ruleset is a repository-admin action that a PR
cannot perform. Because a ruleset named `main` already exists, this is
an **update (`PUT`)**, not a create:

```bash
# List rulesets and find the id of the one named "main".
gh api repos/osism/ovn-network-agent/rulesets --jq '.[] | {id, name}'

# Update it in place from the committed JSON (replace <id>).
gh api --method PUT repos/osism/ovn-network-agent/rulesets/<id> \
  --input .github/rulesets/main.json

# If no "main" ruleset existed yet, create it instead:
gh api --method POST repos/osism/ovn-network-agent/rulesets \
  --input .github/rulesets/main.json
```

The `integration_id: 15368` on each required check pins the context to
the GitHub Actions app, so a same-named check from another app cannot
satisfy the gate. Verify the app id for this repo with:

```bash
gh api repos/osism/ovn-network-agent/commits/main/check-runs \
  --jq '[.check_runs[].app.id] | unique'
```

## Ruleset on release tags

`release.yml` triggers on `v*` tags, so the tags are a release trust
boundary in their own right — a tag can be pushed onto any commit. The
committed tag ruleset at
[`.github/rulesets/tags.json`](https://github.com/osism/ovn-network-agent/blob/main/.github/rulesets/tags.json)
targets `refs/tags/v*` and enforces:

- **`deletion`** — a published release tag cannot be removed.
- **`required_signatures`** — a `v*` tag must be signed to be created, so
  the release path inherits the same key-bound trust boundary as `main`
  (configure tag signing with `git config tag.gpgsign true`).

It has no pre-existing counterpart, so the first apply is a **create
(`POST`)**:

```bash
gh api --method POST repos/osism/ovn-network-agent/rulesets \
  --input .github/rulesets/tags.json
```

## Scheduled and push runs

The unit suite (`test.yml`) and `govulncheck.yml` run on push to `main`
and on a weekly `schedule:` (Monday, staggered around 05:00–06:00 UTC
with CodeQL and Scorecard). These non-PR runs are the backstop the
green-main contract needs:

- **push `main`** catches anything that reaches `main` outside the PR
  path and confirms the merged commit post-merge.
- **weekly cron** catches runner/kernel drift, toolchain updates, and a
  CVE published against a pinned dependency during a quiet period —
  surfacing it within a week with no PR activity.

The chaos runner (`e2e-chaos.yml`) also runs nightly, on its own
`17 3 * * *` cron — off the hour and staggered clear of the Monday
05:00–06:00 UTC burst so the two schedules never contend for runners.
Each night fans out as a `fail-fast: false` matrix over all six curated
chaos profiles, one job per profile at 10 minutes, so a fault that only
shows up under a particular gateway configuration still gets exercised.
The seed is the run id, so successive nights explore different fault
sequences rather than replaying one; it is echoed to the run log and
recorded by the runner in the uploaded `summary.json` and
`journal.jsonl`, so a failing night is fully replayable — dispatch
`e2e-chaos.yml` with the seed, profile and duration read back from that
job's artifacts and the run reproduces exactly.

A pull request opts into a short chaos smoke by carrying the
`chaos-smoke` label, which keeps chaos off the default PR gate;
applying the label needs triage permission. The label is not created
automatically — as a one-time repository-admin action (like applying
the rulesets above), create it once with:

```bash
gh label create chaos-smoke \
  --description "Run the e2e-chaos smoke on this PR"
```

Until the label exists the smoke trigger is inert.

Scheduled-run failures appear in the Actions tab and notify the
workflow file's last committer.

## Release test gate

`release.yml` runs the race-enabled unit suite in a `test` job that
`build` depends on, before any artifact is built, signed, or attested.
A tag can be pushed on any commit, so this gate is what stops an
untested commit from becoming a release. Permissions are scoped per
job: only `release` holds the write/attestation grants; `test` and
`build` are read-only.

## Docs-only pull requests

A PR that touches only `docs/**` or `*.md` skips the heavy suites:

- **E2E** is not a required check, so `e2e.yml` uses a plain
  `paths-ignore` filter.
- **Integration** *is* required (`smoke`), and a check skipped by
  `on.paths` stays Pending forever and blocks the merge. So
  `integration.yml` instead runs a `changes` job that inspects the PR
  file list, and `smoke` runs under a fail-safe
  `if: !cancelled() && needs.changes.outputs.code != 'false'`: it skips
  only when `changes` positively reports a docs-only PR, and still runs
  if `changes` itself fails. A skipped-by-`if:` job reports success to
  the required check, so the gate does not hang.
- **Docs** (`docs.yml`) builds the VitePress site so a dead internal
  link fails the PR instead of the post-merge Pages deploy.

## Integration coverage

The `smoke` job builds the agent with `make build-integration`
(`go build -cover -covermode=atomic -race`) rather than a plain binary, so
integration-only Linux code — `main()`, `Connect()`, `routing_linux.go`,
`nftables_linux.go` — is counted, and the concurrency-heavy paths
(`refreshLoop`, `drainWatchCh`, event handlers) run under the race detector.
Each agent process writes counter files to `GOCOVERDIR`; a post-test step
merges them with `go tool covdata` and prints the total in the job log. This
figure is **informational**, not a gate — the merge tolerates a run where no
counters were collected so it never fails the required check.

The enforced coverage floor (`COVERAGE_FLOOR` in `test.yml`) is deliberately
left unchanged: it gates the **unit** suite, and raising it needs the merged
unit-plus-integration numbers this instrumentation first makes visible. Revisit
the floor once those numbers have stabilised across a few runs (issue #160's
"afterwards revisit" step).

## Lint gate

[`.golangci.yml`](https://github.com/osism/ovn-network-agent/blob/main/.golangci.yml)
is the versioned contract for the lint gate: the enabled linters
(the standard set plus `gosec`, `errorlint`, `gocritic`, `revive`) and
every exclusion are recorded there, so a linter major bump cannot
silently change what the gate checks. Run it locally the way CI does —
keep the version in sync with the pin in `lint.yml`:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
golangci-lint config verify
golangci-lint run
```

`gosec` findings on this agent's audited seams — building
`nft`/`vtysh`/`ovs-vsctl` argv, procfs sysctl writes, world-readable
generated docs — are documented as categorical false positives in the
config's `exclusions`, each with a why-comment. New code is still
scanned normally.

## Containerlab pin

`make e2e-install-tools` installs a pinned containerlab `.deb` and
verifies its sha256 against the checksum committed in the `Makefile`,
instead of piping the upstream installer into a shell. To bump the
version:

1. Pick the new release at
   <https://github.com/srl-labs/containerlab/releases>.
2. Set `CONTAINERLAB_VERSION` and **both** `CONTAINERLAB_SHA256_amd64`
   and `CONTAINERLAB_SHA256_arm64` in the `Makefile` from that release's
   `checksums.txt` (the `linux_amd64.deb` / `linux_arm64.deb` lines).
3. Both CI and local installs share these values through the
   `e2e-lab-setup` composite action.

## Cross-repo automation PAT

`add-to-project.yml` runs `osism/.github`'s reusable workflow on
`pull_request_target` with the `ADD_TO_PROJECT_PAT` secret. Two
maintenance notes:

- The reusable workflow is **pinned by commit SHA**, not `@main`, so a
  moved upstream branch cannot change what runs in that privileged
  context. Bump the SHA deliberately when adopting an upstream change,
  updating the `# main as of <date>` comment.
- `ADD_TO_PROJECT_PAT` should be a **fine-grained PAT** scoped to the
  minimum: organization Projects read/write (plus the default repo
  metadata read). Rotating it to that scope is an org-admin action.
