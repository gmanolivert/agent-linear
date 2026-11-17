# Single agent for Linear issues

One fresh Codex or OpenCode CLI session handles one issue through all six source roles.
Feature PRs always target **`staging`**. Linear integration and local cron
activation are deferred until the owner configures project routes. The Linear
API key is supplied through the local `LINEAR_API_KEY` environment variable.
No live issue, PR, deployment, or cron job has been created by this setup.

- [AGENT.md](AGENT.md): reusable, sequential agent directives.
- [config.example.json](config.example.json): trusted Linear-project → repository
  mapping and deployment authority. Multiple projects may use the same agent.
- [main.go](main.go): paginated Linear polling, fresh isolated checkouts,
  serial launches, persistent issue claims, and GitHub PR verification.
- [result.schema.json](result.schema.json): acceptance evidence and final result.
- [.agents/skills/progressive-issue/SKILL.md](.agents/skills/progressive-issue/SKILL.md):
  standalone progressive-role workflow for ad hoc issue work.

## Ad hoc issue skill

Use `progressive-issue` for issue work in a normal agent session. It walks one
agent through CEO intake, Architect, Security design, Builder, Security review,
QA, delivery, DevOps handoff, and CEO closure. It uses the target repository's
instructions and the user's delivery scope; it grants no additional permission
to commit, push, change issue status, or deploy.

After building, install the same skill into another Git checkout:

```bash
mise run build
./agent-linear install-skill /absolute/path/to/another-repo
```

The destination must be the checkout root (linked worktrees are supported).
The command writes only `.agents/skills/progressive-issue/SKILL.md`, creates its
parent directories if needed, and preserves existing directives and other skills.
An identical installation is left untouched; a different existing skill or a
symlink destination is rejected. Review customized copies before replacing them.
The skill is embedded in the executable: rebuild after editing its source, and
the installer works without access to this source checkout.

In an agent session that supports repository skills, invoke:

```text
Use $progressive-issue to implement this issue: [issue description or link].
```

The skill is self-contained. Installing or using it requires no project routing
configuration, Linear credentials, runner state, or scheduled job. It does not
install cron, copy `AGENT.md`, or impose the runner's `staging`/PR-only policy.

## Use the directives now

In a fresh session in the target repository, supply `AGENT.md`, the complete
Linear issue body, and this trusted context separately from the issue:

```text
Handle only this issue using the attached AGENT.md sequential workflow.
Target: OWNER/REPO; base: staging; checkout: ABSOLUTE_PATH
Feature branch: UNIQUE_BRANCH; baseline SHA: STAGING_SHA
Scratch directory outside product checkout: ABSOLUTE_PATH
Signed commits required: true
Deployment: pr-only (owner handles releases; do not merge or deploy)
Requirements and acceptance criteria: [complete issue body]
Return the final criterion evidence, checks, security/QA verdict, branch/SHA,
PR URL, deployment state, and any blockers. Do not merge.
```

The runner supplies this context automatically. Do not paste API keys into it.
A single session's self-review does not replace independent review on the PR.

## Goals: turning prompts into issues

`goal` turns a natural-language directive into concrete, well-scoped Linear issues.
A read-only agent investigates the repository, then the runner creates a parent
**goal** issue with one sub-issue per finding:

```bash
./agent-linear goal --project PROJECT_UUID --prompt "Diagnose N+1 queries"
./agent-linear goal --project PROJECT_UUID --prompt-file goals/n1.txt --dry-run
```

The investigation never commits, pushes, or opens a PR; only the runner writes to
Linear. Each generated sub-issue carries requirements and acceptance criteria, so
a human can later move it to the ready state and the normal poll/run engine
implements it. By default new issues land in the configured **triage** state;
`--autostart` instead places them directly in ready with the `agent-ready` label so
the next cron tick picks them up without approval. `--dry-run` prints the plan
without any Linear writes. Re-running the same prompt for the same project is
detected and skipped rather than creating duplicate goals.

A planned issue can declare prerequisites with `depends_on` (a list of the
titles of issues that must finish first). The runner writes a Linear `blocks`
relation from each prerequisite to the dependent issue, so the dependent is
marked blocked until each prerequisite's pull request is merged into `staging`
(or reaches a terminal state when it has no pull request); the ready-queue gate
then skips it until then. `depends_on` applies to issues created at goal time
and to follow-ups and refinements applied during `goal review`; an empty array
means no prerequisites. Dependencies must only point at earlier issues — a plan
that depends on a later issue, on itself, or that forms a cycle is rejected
before anything is written, so background processing can never deadlock.

### Feedback loop

`goal review` reads a goal, its sub-issues, and their comments, then turns team
feedback into concrete action:

```bash
./agent-linear goal review --goal GOAL_UUID
./agent-linear goal review --goal GOAL_UUID --dry-run
```

A read-only agent re-examines the goal and produces three kinds of change, all
applied by the runner:

- **new issues** — genuinely new work from feedback, created in triage under the
  goal;
- **refinements** — edits to existing sub-issues still in **triage**, updated in
  place; a refinement against an issue that has already started becomes a
  follow-up instead, because its requirements are frozen;
- **flags** — stale, duplicate or superseded issues, listed in the review comment
  for a human to close; the runner never closes them itself.

Comments authored by the runner (claim/finish/summary bookkeeping) are ignored;
only team comments drive the review. `--dry-run` prints the revised plan without
any Linear writes.

Review every goal that has new feedback since its last review:

```bash
./agent-linear goal review --all
./agent-linear goal review --all --dry-run
```

The sweep is idempotent: each goal records a high-water mark of the newest comment
already acted on, and `--all` only reviews goals with feedback newer than that
mark. It reuses the same 15-minute provider cooldown as `poll`, so a rate limit
pauses the sweep. The scheduled `poll` job also runs this sweep after processing
the issue queue, so goals with new feedback are reviewed automatically on the
next tick; goal *creation* remains manual.

Routes need a `triage_state_id` to use goals; it is optional for plain issue
processing. A route without it still polls and implements ready issues, but
`goal` and `goal review` require it and otherwise fail with a clear message.

## Configure later

Needs Linux, the built `agent-linear` executable, git, an authenticated Codex or
OpenCode CLI, authenticated GitHub CLI, access to each GitHub repository, and its
remote `staging` branch. Every feature
commit must use the local Git profile's author and signing key. The runner reads
effective `user.name`, `user.email`, `user.signingkey`, and signing settings from
the mapped local repository, honoring repository-local overrides and conditional
includes. It requires all three profile values and resolves the key fingerprint.
It applies the captured settings only to the isolated checkout, enables commit
signing there, and supplies the same author/committer identity to the session.
It does not change the source checkout's or global Git configuration or copy
private keys. All routes require `require_signed_commits: true`; disabling it
is rejected. Unavailable signing keys or failed verification block the run.

Build from this checkout with [mise](https://mise.jdx.dev/):

```bash
cd /home/noah/Documents/blank-agent-linear
mise install
mise exec -- go version
mise run build
./agent-linear --help
```

`mise.toml` pins Go to the `1.27` release line (tested with Go 1.27.2).
The CLI uses [Cobra](https://cobra.dev/) and a pure-Go SQLite driver; module versions
and checksums are pinned in `go.mod` and `go.sum`. Building needs access to those
modules initially. The resulting executable needs no language runtime installed.
`mise run test` runs the race-enabled tests and `mise run check` runs `go vet`.
Race tests require a C compiler; normal builds use `CGO_ENABLED=0`.

Build `agent-linear` in the repository root as shown. Configuration defaults to
`config.json` **beside the executable**, and claims/artifacts stay in `.runs`
beside it, regardless of the current working directory. `--config` can appear
before or after a command and changes only the configuration path. Moving the
executable moves its default state location: keep the existing `.runs` with it
to preserve duplicate suppression. The directives, example config, and result
schema are embedded at build time; rebuild after changing those sources.

For a new installation:

```bash
cp config.example.json config.json
chmod 600 config.json
```

Edit `config.json`: use real project/team/ready-state UUIDs, the existing local
repository path and matching GitHub `OWNER/REPO`. Replace every placeholder.
Find UUIDs using Linear's command menu → “Copy model UUID”. A ready-state plus
the optional `agent-ready` label opts issues into processing; remove `label`
if readiness should depend only on status. Polling includes all currently
eligible issues, including existing ones, rather than a creation-time cutoff.

An issue that is `blocked by` another issue is skipped until it is unblocked:
a ready issue with an active Linear "blocks" relation against it is not claimed,
commented on, or changed, and no agent session or provider request starts. A
blocker resolves once its pull request is merged into `staging`; when the
blocker has no recorded pull request, its terminal workflow state (`Done`,
`Canceled`, or `Duplicate`) resolves the block instead. The issue is then picked
up on a later tick with no manual reset. This gate is always on for every route,
and checking it adds no extra Linear requests.

Supply the Linear API key through the local `LINEAR_API_KEY` environment variable.
The runner does not load credential files. Credentials are not stored in config,
issue snapshots, prompts, or the state database, and the Linear key is removed
from the agent's environment. The runner posts claim and finish comments through
Linear's API and changes issue status; the key must allow comment creation,
workflow-state/user reads, and issue updates. The Linear assignee owns the issue.
Polling includes unassigned issues and issues assigned to the authenticated user;
issues assigned to anyone else are excluded. The launcher also checks live
ownership/readiness before provider availability and again before claiming work,
including when given a saved snapshot. Existing assignments are preserved: the
start mutation changes only status for an owned issue and confirms its assignee.
An unassigned issue is assigned to the authenticated user returned by Linear's
`viewer` query using that runner's `LINEAR_API_KEY`. Each teammate should use their
own personal key; the MCP connection and runner key can represent different users.
No user ID is hardcoded or added to config. Each routed
team must have exactly one status named `In Progress` and one named `In Review`.
The runner resolves these by name within that team; no new config is needed.
Smoke tests and dry runs remain read-only against Linear.

All projects use `pr-only` with environment `none`; the runner rejects preview
and direct deployment policies. The agent creates a PR into `staging`, then
DevOps documents release requirements for the owner. The agent does not merge
or deploy. Deploy-dependent acceptance criteria remain blocked for owner
release/verification. Existing repository CI may run automatically on a PR;
that does not authorize the agent to trigger a separate deployment.

## Update an existing installation

### Reserve disk space before starting work

Set the top-level `min_free_bytes` to `8000000000` to require 8 GB of free space
on the filesystem containing `.runs` before starting work. Omitted or zero
preserves the prior behavior. The check uses space available to the runner user,
excluding blocks reserved for root.

The launcher checks each new issue, including between issues within one poll,
and checks again after provider preflight before creating its claim. Low space
or an unreadable capacity measurement defers the poll without claiming the next
issue or starting an agent. Manual runs/resumes return a capacity error. Pending
automatic continuations stay eligible and retain their checkout/session until
space is available. Goal investigations and goal reviews also require the
reserve, including their dry-run modes because those modes launch an agent.
Read-only status, reconciliation, and issue-queue dry runs remain available.

This gate does not delete files, invoke cleanup, interrupt active agents, or
replace the worker lock. It is a dispatch check, not a quota on an active job.
Configure cleanup separately and recheck actual free space afterward.
Once enough space is available, the next normal poll can
continue. Install the new binary before enabling this setting on a host;
older binaries ignore it. Test a low-space deferral and healthy dispatch with
synthetic fixtures before activation.

Existing JSON configuration fields and the SQLite `runs` table remain compatible.
`model_reasoning_effort` is an optional Codex setting; older configurations can
continue omitting it.
Provider cooldown metadata and versioned recovery context are added automatically
in the same database, preserving the existing five-column claims table.
Existing claims, scratch directories, sessions, and lessons remain usable; there
is no manual database migration. Older started runs remain manual; newly recorded
provider-limit interruptions can resume automatically after availability succeeds.
Keep `config.json` and `.runs` in
the same checkout, build the executable there, and replace the old marked cron
entry only when ready to activate the Go runner.

After building, stop future polling before switching the scheduled executable:

```bash
./agent-linear uninstall-cron
./agent-linear smoke-test
./agent-linear cron-line
# Activate only when ready for the next tick to start work:
./agent-linear install-cron
```

Already running agents continue; let them finish before switching the scheduled
executable. The installer replaces the existing `# blank-agent-linear` entry and
preserves other jobs. Keep only one installation polling the same queue.

## Choose the CLI

Codex is the default, including for existing configs without `engine`. Pin its
model and reasoning at the top level of this runner's `config.json`, alongside
`gh` and `projects`:

```json
{
  "engine": "codex",
  "model": "gpt-6.1-sol",
  "model_reasoning_effort": "medium"
}
```

The checked-in example pins these values. Both settings are optional in existing
configs: omitted values use the selected CLI's native settings. Overrides are
passed explicitly to availability checks and issue sessions using `--model` and
`-c model_reasoning_effort=...`, following the
[official OpenAI configuration precedence](https://learn.chatgpt.com/docs/developer-settings).
The same explicit settings are captured in trusted run context; managed resume
keeps the recorded model/reasoning overrides when repository defaults change.
Older runs with no recorded reasoning override keep that unspecified behavior.
Changing these fields affects new jobs on the next poll; no cron reinstall is needed.

`model_reasoning_effort` accepts a nonempty string for Codex only. Use a level
advertised by your selected model/client (for example `medium` or `high`);
Codex validates support during the availability request before issue pickup.
The runner does not maintain a separate model-specific level list. A malformed,
null or blank setting is rejected. Remove this field when switching to OpenCode;
OpenCode configs containing it fail validation rather than silently ignoring it.

To use OpenCode, set these top-level fields in `config.json`:

```json
{
  "engine": "opencode",
  "opencode": "/home/noah/.opencode/bin/opencode",
  "model": "provider/model"
}
```

Replace `provider/model` with an entry from `opencode models`. `model` is optional;
when omitted, the selected CLI uses its own default. Set `engine` back to `codex`
to use the existing `codex` executable setting. Remove or change `model` when
switching engines because their model names may differ. Each poll uses the
configured engine for new work across all project routes. Provider-limit recovery
uses the interrupted run's recorded engine/model, even after these defaults change.
The cron entry itself does not change. Prefer absolute executable paths for cron.

Configure provider access once as the OS user that runs cron (`opencode auth login`
for OpenCode). The cron process must have access to that user's credentials,
signing agent/key, and any required provider environment variables. Do not put
API keys in this runner's config. `smoke-test` checks Codex login status, or reads
OpenCode's credential/model inventory and checks an explicitly selected model.
An OpenCode inventory check does not prove a provider accepts requests; that
requires a live model run. Neither smoke test launches issue work.

OpenCode support uses `opencode run --format json` with the prompt on stdin,
verified against version 1.18.14. The runner extracts the final completed text
from its events into `result.json`; both engines' reports must match
`result.schema.json` before PR verification. Missing, malformed, interrupted,
or error results fail the run. There is no engine fallback. Recognized provider
limits can schedule automatic continuation of the recorded session; other failures
require manual recovery. Untouched deferred issues can be tried later.

### Codex CLI sandbox and the product checkout

Recent Codex CLI releases (0.16x and later) mark `.git` inside every writable
root as read-only under `--sandbox workspace-write`; this is documented as a
protected path and cannot be relaxed without `danger-full-access`. If the run
folder is passed as an extra writable root and the product checkout is the
session's working directory, the agent can edit files but every `git add`,
`commit` and `push` fails with "Read-only file system" and the run ends
blocked with no PR. The launcher therefore starts Codex with the **run folder**
as the working directory and no extra writable roots: the product checkout
lives one level down (`.runs/ISSUE_UUID/repo`), so its `.git` is nested
inside the writable root and stays writable while the sandbox still contains the
whole run. Verified 2026-11-04 on Codex CLI 0.166 (Ubuntu 26.04 under WSL2):
the same issue that blocked before the change produced a signed commit and a
pull request after it. Two related notes from that run: `~/.npm` is also
read-only inside the sandbox, so Node projects may need `npm_config_cache=/tmp`
or an equivalent; and a run that opened its PR but could not observe the
launcher-owned "In Review" transition before finishing reports `blocked` even
though the pull request exists (the PR URL is in `result.json`).

## Provider capacity and limits

Before claiming an eligible issue or making any Linear mutation, the runner sends
a small availability prompt through the selected CLI and model, using the target
repository's native provider configuration. It requires an exact `READY` reply.
This is a real model request and consumes provider usage. The check requests no
tools or issue work, strips `LINEAR_API_KEY`, uses a read-only Codex sandbox with
shell/web tools disabled, or an OpenCode agent with tool permissions denied. It
has a 90-second deadline. Smoke tests and dry runs do not make this request.

A recognized rate, usage, credit, or quota limit causes a **15-minute local
cooldown** persisted in `.runs/state.sqlite3`, shared by all project routes using
the same engine, executable path, and configured model (or native default). The
current tick stops, and later ticks skip pickup during that cooldown without
contacting Linear or the model. Cron stays installed. After expiry, the next
eligible issue receives a new availability check; another limit renews cooldown.
This is a polling delay, not a promise that provider capacity resets in 15 minutes.

If the availability check reports a limit, the issue is recorded locally as
`deferred`: its assignee, status, and comments remain untouched, and no feature
checkout is created. A deferred row does not claim the issue; after cooldown it
can become eligible work again, subject to a fresh ready-state/label check for
polling. Polling refreshes eligibility again after availability is confirmed and
before claiming, so withdrawal during the check does not start stale work.
Authentication errors, timeouts, or unexpected availability responses
also stop pickup before any claim or Linear mutation, but remain errors rather
than being classified as provider limits.

If a limit is reported **after launch**, the CLI process group is stopped, the
local issue becomes `blocked`, and a finish comment explains the interruption
and provides the recorded session's resume command when available. The issue
stays In Progress, the existing assignee and partial checkout/commits are retained,
and no In Review transition occurs. The provider cooldown also stops other work.
New provider-limit interruptions also record explicit recovery eligibility in
SQLite. Before new issue pickup, cron handles one pending recovery per tick.
After cooldown, it probes using the saved engine, executable, model and reasoning
settings in the retained checkout. An exact `READY` reply triggers managed resume
of the same session, preserving edits, commits, signing profile and checkout.
Another provider limit renews cooldown; repeated limits can keep deferring recovery
until capacity returns. Recovery takes priority over new issues while waiting.

All managed-resume checks still apply: original owner still assigned, unchanged
scope/route and In Progress state, inactive checkout, original branch/origin and
signing profile, recorded session ID, and no prior finish-state request. The worker
lock covers the entire check and continuation. A failed availability check other
than a recognized limit, failed validation, ordinary continuation failure, or
pending/failed completion checks stops automatic recovery and requires manual
resume/reconciliation. Older blocked runs are not automatically opted in.

Eligibility is consumed before checking/continuing, so a crash during an attempt
requires inspection rather than automatic replay. Manual resume/reconcile consumes it too;
only a freshly recognized provider limit schedules another attempt. `status`
shows `provider_recovery`, `next_probe_at` while waiting, and a manual-action reason
when automatic recovery stops; `status --all` includes older runs. Detailed failures
remain in the cron log and retained run artifacts. After inspecting partial work
and the remote branch/PR, use `resume` or `reconcile` as appropriate.

The first command that opens writable runner state upgrades SQLite to schema version 2 without
queuing older runs. Earlier binaries reject this version; rollback requires a
compatible binary or a reviewed state migration. Never delete claims to roll back.

Detection watches provider error/retry JSON events and diagnostic stderr for
recognized limit codes/messages (including HTTP 429). It ignores ordinary
assistant text and tool-result events. Reported limits interrupt CLI retries;
unrecognized errors keep the normal failure behavior. A successful check cannot
guarantee capacity for an entire issue. Feature sessions have a two-hour deadline;
git, GitHub, signing and crontab commands have five-minute deadlines. Interrupts
and deadlines stop the subprocess group, preserve partial work and require
explicit recovery, except for the recorded provider-limit continuation above.

Availability logs are retained under `.runs/preflight/check-*/`; deferred evidence
includes `deferred.json`. Started interruptions retain `provider-limit.json`,
`failure.txt`, events, stderr, and the checkout in the issue's scratch directory.

The runner gives OpenCode a per-run `linear-issue` agent: shell work is allowed,
subagents and questions are denied, and edits are restricted to the checkout
and scratch directory. Prior lesson files are readable. Permission prompts are
automatically rejected in noninteractive mode; the runner does not use `--auto`.
Sharing and automatic updates are disabled for the run. The settings are saved
as `opencode.json` in scratch for recovery. Native provider configuration is
still loaded; the runner supplies its own `OPENCODE_CONFIG_CONTENT` overrides.
These are OpenCode tool permissions, **not an OS filesystem sandbox**. Shell
commands run with the cron user's access. Use an isolated OS account/container
if OS-level confinement is required. Codex retains its `workspace-write` sandbox.

## Verify and run

The first live test is a setup smoke test, not feature implementation. With
`LINEAR_API_KEY` supplied in the environment, collect mappings without scheduling
anything, then check access:

```bash
go test -race ./...
./agent-linear setup
./agent-linear smoke-test
```

`smoke-test` validates the configuration/local origins and PR-only policy,
checks git/crontab/selected-agent/GitHub executables, CLI preflight, GitHub write
permission, read access to each remote `staging` branch, Git profile/key resolution,
and the filtered Linear queues. It prints counts instead of issue bodies.
It does not launch an agent, claim issues, create SQLite state, change product
files, create commits/branches/PRs, or install cron. An empty queue does not prove
the project/team/status IDs belong together; verify those in Linear. The smoke
test does not exercise signing, a model request, Linear comment-write access,
or the cron job's environment.

Feature processing is enabled later, only when explicitly requested:

```bash
./agent-linear poll --dry-run
./agent-linear poll
./agent-linear status
```

`--dry-run` queries Linear and reports eligible/previously claimed issues. It
does not start an agent, clone, push, create a PR, or deploy. It creates only the
local lock/state files. Real polling refreshes each issue immediately before
launching, so a withdrawn ready label/state does not start stale work. A missing
title or body is reported and skipped without claiming or mutating that issue;
other eligible issues continue, and a corrected issue can run on a later tick.
Route mismatches and shared provider/authentication failures still stop pickup.

`status` reads the database without taking the worker lock or writing state.
By default it shows the five most recently started issues, newest first.
Deferred issues have not started implementation and appear only with `--all`:

```bash
./agent-linear status
./agent-linear status --all
```

Ordering uses each run's original `activity.json` start time; later activity or
reconciliation does not move an old run to the top. Records without a valid start
time follow dated runs, ordered by local database insertion order, newest first.
Health indicators and blocked/failed/interrupted totals always include all history,
even when an issue is outside the five displayed. This limits display only:
every claim and artifact remains available for duplicate prevention and recovery.
It emits the existing four-field run arrays, plus JSON objects for worker-lock
activity and available per-run phase, launcher PID, host, start time and last
log activity. A saved PID/claim alone does not prove a process is still alive.

Real polls atomically record `.runs/poll-attempt.json` on every invocation,
including ticks skipped while the worker lock is held. `.runs/poll-health.json`
records the last started/completed poll, last successful completion, outcome and
a sanitized failure phase. Dry runs do not update health. Provider cooldowns
are `deferred`, not successful polls. `status` reports missing or older-than-15-minute
attempts as `polling_stale`, and separately exposes `last_success_stale`. It flags
failed polls and polls left running without a worker lock, plus counts of blocked,
failed and interrupted runs needing attention. Health remains readable when
configuration/authentication fails or the state database is absent.
These are local health signals available through `status` and the existing cron
log; they do not send external alerts or prove that cron is installed. A monitor
must invoke `status` to detect a stopped scheduler. A long active run may have an
old successful-completion timestamp while later cron ticks continue arriving.

To run one issue directly without the scheduled poller, pass its Linear
identifier (for example `INT-3`) or UUID. The runner fetches the issue live,
derives its project route from `config.json`, and proceeds through the normal
claim/checkout/PR/finish flow:

```bash
./agent-linear run --issue INT-3 --dry-run
./agent-linear run --issue INT-3
```

The fetched issue supplies the task body and its `project.id`, `team.id`, and
`state.id`; the project must be a configured route. Current ownership and
readiness are still checked live, so `run` cannot take another user's issue. A
live target session still needs access to the selected CLI, its provider, and
GitHub.

After availability is confirmed and the local claim is committed, the runner resolves the authenticated user and
both team statuses, then posts
`Run claimed on HOSTNAME. Starting work on IDENTIFIER.` before cloning or launching
the agent. If that comment cannot be confirmed, the run fails without starting feature
work. It then changes the issue to **In Progress**, confirming the returned issue
and status IDs before cloning or launching the agent. Missing/ambiguous statuses or
an unconfirmed start update fail the run before feature work starts.
For an unassigned issue, the same In Progress mutation assigns the authenticated
user. For an already owned issue it leaves the assignment intact. The response must
confirm both the status and expected assignee before work launches. A failed user lookup or
unconfirmed assignment fails the run without launching feature work. The In Review
update does not change the assignee.
The comment announces ownership; SQLite remains the local duplicate guard,
and the comment is not a lock across machines.

After verifying a `ready` result (including its PR, required CI and signed commits
for the default completion path),
the runner reads the current Linear issue and checks its owner, project/team,
title, body, relations and workflow state. A conflict preserves human changes
and the checkout/PR for inspection. An unchanged In Progress issue moves to
**In Review** before recording `ready`. If it is already In Review, the same
verified completion is recorded without another status mutation. An issue already
in a completed Linear state is likewise recognized after full verification,
without moving it back to review. A malformed or
conflicting saved finish request still requires inspection. Other status changes
are never overwritten. The launcher owns assignments and workflow transitions;
the agent returns a report and performs only task-authorized comments/actions.
This final read is not an atomic
cross-machine lock; [Linear's issue update schema](https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql)
has no expected-version parameter, leaving a read/update race with concurrent edits.
Blocked, failed, and
invalid completion results do not trigger this transition. An unconfirmed review
update fails the run and preserves the result/PR for manual recovery. Failed or
blocked work otherwise stays In Progress; the runner never moves issues to Done.
Status mutation requests and confirmed responses are retained as `start-state*`
and `finish-state*` in scratch. No mutations are automatically retried; inspect
Linear before recovering from a timeout, which may have applied the update.

After saving the final outcome, the runner posts:

```text
Run finished on <hostname> with status <run status>. View pull request at <GitHub PR URL>. Recorded <engine> session: <session id>. Continue as the assigned owner with `<runner> resume --issue <issue UUID>`; use reconcile when only completion checks remain.
```

Finish-comment statuses are `ready`, `blocked`, or `failed`; untouched `deferred`
issues receive no Linear comment. Failed runs and blocked runs that will not
auto-recover prepend an `@mention` of the assignee so manual resolution is
surfaced; provider-limit blocks that resume automatically are not mentioned.
Session IDs come from CLI events:
Codex's `thread.started.thread_id` or OpenCode's `sessionID`, not the agent report.
The comment supplies the managed resume command for both engines. Resume on the
named machine under the original OS user, from outside the feature checkout.
Supply `--config PATH` when using a nondefault routing configuration. When no PR URL is recorded,
the comment says it is unavailable; when no session ID is recorded, it omits the
resume command and explains that the session is unavailable. A finish-comment
failure preserves the run outcome and logs delivery evidence in the issue's
scratch directory. Comment bodies, confirmed IDs, and delivery errors are retained
as `start-comment.*` and `finish-comment.*`. There are no automatic mutation retries:
after a timeout, check Linear before manually reposting. A killed runner may leave
only a start comment. A successful run uses nine Linear API requests for user,
workflow-state, two ownership/readiness and final-issue reads, two status updates
and two comments.
Polling also queries the queue and refreshes eligibility before
and after the model availability check.

Every launched issue is claimed in `.runs/state.sqlite3` after its model
availability check and before issue mutations, cloning, or feature work starts.
The existing repository is cloned with separate git metadata; only committed
source is copied, then the actual remote `staging` SHA is fetched as the base.
The user's uncommitted checkout stays intact. Branches use
`linear-IDENTIFIER-UUID8` to avoid collisions. Polling starts fresh sessions for
new issues and uses managed resume for scheduled provider-limit recovery.
Other continuation requires explicit managed resume. Roles run sequentially in that
session. The agent creates the PR; the runner independently queries GitHub to
check its repository, base/head, SHA, open/non-draft state, and every feature
commit's raw author, signature validity, and signing-key fingerprint against the
profile captured before launch. A valid signature from another key is rejected.
The agent checks the same requirements before pushing.

The runner freezes the issue title and every nonblank-line-separated body block
as `R1`, `R2`, etc. in `requirements.json` and the database's trusted launch context.
Final criteria must contain each ID exactly once with identical source text,
passed status and evidence. Compound blocks still need all their conditions
verified; complete IDs do not prove the quality of the agent's evidence.

The agent selects `completion: "pr"` or `completion: "no-code"` from the frozen
issue requirements and explains that choice in its report summary. Most tasks
need a PR: repository changes or an explicit PR requirement use the PR path.
A comment-only, investigation or other task that can satisfy all requirements
without product changes uses no-code completion. No special header or label is
required, and the absence of a PR alone never proves a task is finished.

Both paths require passed original criteria and relevant checks with evidence,
security APPROVED, QA passed, no blockers and deployment `not-requested`.
Repository tests can be inapplicable for a no-code task, but verification of its
actual outcome remains mandatory. Record verification commands/tools and evidence
in `checks`; unmet requirements still block completion.

No-code reports set `completion: "no-code"`, `status: "ready"`, an empty `pr_url`
and `commit_sha` equal to the captured base SHA. The runner verifies the assigned
branch and origin, a clean checkout including staged/untracked files, and HEAD
exactly equal to that base. New commits (even empty ones) or product edits prevent
no-code completion. PR/signature/CI gates apply to PR reports; the runner does not
manufacture empty commits or PRs for no-code work. Normal launcher prerequisites
and the configured repository/staging base still apply to both paths.

A successful task becomes locally `ready` and moves to **In Review** for human
review on either path; the runner never automatically marks it Done.
Finish/recovery comments explicitly identify successful no-code completion.

Older reports without a `completion` field remain readable: the runner derives
the path from their existing PR URL while preserving status, blockers and evidence.
A previously blocked result is not silently promoted to ready. Explicit managed
resume can reassess an eligible original task using current runner instructions,
without repeating completed external actions. Prior instructions are archived
with the other attempt evidence; issue requirements/context stay frozen. Changed
scope, ownership, status or cancellation still require inspection and are never
overwritten. Do not delete claims to force retries.

For default PR completion, required CI is checked with `gh pr checks --required` and compared with the named
requirements from classic branch protection and all active branch rulesets.
Every declared check must report pass or GitHub's skipped result at the verified
PR head; pending, missing, failed, cancelled and unreadable checks prevent ready.
The PR head is checked again afterwards, and `ci-checks.json` retains evidence.
Repositories must configure at least one named required status check on staging;
an empty or inaccessible policy is not treated as passing. The agent must never
change protection or rulesets to bypass this gate. Set `skip_ci_checks: true` on
a route to disable this gate for repositories that do not run required CI; the
PR is then accepted without a required status check. These reads follow
[GitHub's required-check CLI](https://cli.github.com/manual/gh_pr_checks) and
[effective branch rules API](https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch).

The process holds a nonblocking `flock` for the entire tick; the agent child
inherits the lock descriptor, so an orphaned session retains the worker lock.
This is a **single-host** queue. Do not run another installation against the
same issues: local claims do not coordinate multiple machines.

## Install cron after the smoke test and explicit activation

The agreed interval is five minutes. The host is assumed to have its OS-provided
`crontab` command on PATH and its cron service running. The installer uses that
command to manage the current user's crontab; no OS-specific package installation
or service management is needed in this workflow.
The cron job must receive `LINEAR_API_KEY` through its own environment; an export
in an interactive shell does not configure cron. The installer does not copy
the key into the generated cron entry.

Use `./agent-linear setup` to collect mappings for the smoke test without
scheduling. Later, to supply or update mappings while explicitly enabling cron:

```bash
./agent-linear install-cron --setup
```

The prompts collect each Linear project UUID, team UUID, ready-status UUID,
local repository path, and optional label. The GitHub repository is read from
the local repository's origin. After each route, `Add another project? [y/N]`
defaults to finishing: press Enter or enter `n` to save and exit, or enter `y`
to configure another project. Existing routes and deployment policies are
preserved; new routes default to signed commits, PRs into `staging`, and
`pr-only` deployment. Validated mappings are saved privately to `config.json`,
then the installer checks Linear access and installs the single five-minute job.
Existing routes with preview/direct policies must be changed to `pr-only`,
environment `none`, and owner-release instructions before setup can save them.
The installer starts no issue sessions itself, but the installed polling job
can start feature work on its next tick. If installation fails, saved mappings
remain available for the next attempt.

For mappings already configured in `config.json`:

```bash
./agent-linear smoke-test
./agent-linear cron-line
./agent-linear install-cron
crontab -l
```

The installer verifies config and read-only Linear access, reads the existing
crontab, backs it up at `.runs/crontab.before`, and replaces only its marked
`# blank-agent-linear` entry. It preserves other entries and uses absolute
executable/config paths plus the current PATH. Logs go to `.runs/cron.log`.
Cron directly invokes the compiled Go executable; no mise activation or Bash
wrapper is required at runtime. Install under the same OS user whose
agent/provider and GitHub authentication and git signing setup
will be used by scheduled sessions.
To stop future polling, run under the same OS user:

```bash
./agent-linear uninstall-cron
```

It removes only this workflow's marked entry, preserves other jobs, saves the
previous crontab to `.runs/crontab.before-uninstall`, and verifies removal.
It works without project config or credentials and while a worker is active.
Already-running sessions continue; re-enable polling with `install-cron`.
Keep the host awake; cron does not replay missed ticks. Inspect logs and local
status for failures; Linear comments provide per-run notifications.

## Evidence and recovery

`.runs/ISSUE_UUID/` retains issue/context/prompt snapshots, product checkout,
agent JSONL events, selected engine/model and explicit reasoning override, stderr, final structured result, worklog/lessons, and failure
information. It is ignored by git and created private to the current user.
Checkpoint files are replaced atomically and synced before later external effects;
status readers see complete activity records during updates.
Do not commit it. Retain it until review/deployment is finished, then archive or
remove it deliberately; there is no automatic cleanup.

Preview deletion of recorded Codex/OpenCode sessions whose **current Linear
status name** is exactly `Done`, `Canceled`, or `Duplicate`:

```bash
./agent-linear prune-sessions
./agent-linear prune-sessions --apply
```

The first command only previews; `--apply` permanently deletes eligible sessions
using the recorded CLI executable. It requires `LINEAR_API_KEY` and checks Linear
again on each invocation. `In Review` and all other status names are excluded,
regardless of the local run result or Linear state type. There is no age cutoff.
Running/deferred runs and waiting provider recoveries are excluded, and the worker
lock and checkout process checks prevent pruning during managed work. Missing or
conflicting context, changed routes, API errors, and CLI failures retain the
affected session and return an error; other eligible records can still be pruned.

Successful deletions are recorded in `session-pruned.json` so subsequent commands
skip them. Deletion removes the ability to resume the session; OpenCode also
deletes its child sessions. Use a Codex version supporting `delete --force` and
an OpenCode version supporting `session delete`. Run records, checkouts, event
logs, reports and lessons under `.runs/` are retained and need separate cleanup.
This command does not schedule cleanup or change Linear issues.

Only explicitly recorded provider-limit interruptions receive automatic session
continuation after a successful availability check. Other blocked, failed, and
interrupted claims are **never retried by cron**, even if the issue body changes.
A `running` database row is a claim, not proof of a live
process: inspect the event stream/session ID and current agent process/session
before deciding it died. For recovery, inspect the checkout and actual remote
branch/PR first. Continue implementation with managed resume as described below.
When only verification remains, explicitly verify and finish the recorded run:

```bash
./agent-linear reconcile --issue ISSUE_UUID_OR_IDENTIFIER
```

The command takes the worker lock, refuses processes still using the checkout,
and checks the original database context against the current route. It requires
the authenticated Linear user to be the current assignee. The original user can
continue while still assigned; a different user must first receive an explicit
reassignment in Linear. Reassignment authorizes recovery of the existing run,
preserving its original snapshot and captured Git author/signing profile. It
does not move session files, change Git identity or start a new implementation.
Run it from outside the feature checkout and close shells using that checkout
first. It
reuses the report, checkout, captured signing profile and PR; it does not launch
a model, push work or create another PR. It reruns schema, requirement, Git,
and final Linear checks, plus signature/PR/CI gates for default PR completion,
before saving the outcome. Pending CI can
be reconciled after it passes. Repeated successful reconciliation does not repeat
the status update or its reconciliation comment.

A fully verified, unchanged owned issue already In Review or in a completed
Linear state can be confirmed
without replaying a transition, including one moved by an agent or integration
before the launcher finalized. All requirement, ownership, repository and
applicable PR/CI gates still run. On verified ready, stale active `failure.txt`,
`reconcile-error.txt` and `provider-limit.json` diagnostics are archived under
`resolved-errors-*`; their original evidence is retained. Archival-only problems
are reported without changing a successful task back to failed. Historical logs
remain an audit of earlier attempts; the saved outcome and current phase reflect
the completed task. An
issue still In Progress with a prior finish request remains ambiguous: inspect
Linear and the retained request before explicitly resolving that checkpoint.
A human reopening after confirmed completion is likewise preserved rather than
automatically moved back to review.
An unconfirmed reconciliation comment is also never automatically reposted.
Runs created before trusted database context was captured, or interrupted before
that capture, require manual inspection; the command does not invent a snapshot.
Existing claims remain preserved. Deleting state to force retries can duplicate
external work.

To abandon a stuck run and start the issue over from scratch:

```bash
./agent-linear reset --issue INT-3
./agent-linear reset --issue INT-3 --dry-run
```

`reset` moves the issue back to the ready state, clears its local claim, recovery
context and provider-recovery rows, and removes the `.runs/ISSUE_UUID/` directory,
so the next poll picks it up fresh. It accepts an identifier or a UUID. It refuses
to run when a pull request was recorded or a remote feature branch still exists,
so it cannot duplicate external work; it also refuses when the issue is assigned
to another user or the worker lock is held. `--dry-run` previews the action without
mutating Linear or local state. This is the deliberate, safe alternative to
hand-editing the state database to clear a stuck claim.

To continue implementation as the original owner while still assigned:

```bash
./agent-linear resume --issue ISSUE_UUID_OR_IDENTIFIER
```

Run this outside the feature checkout and close shells using that checkout first.
The command holds the same worker lock as cron for the entire continuation,
checks live ownership, In Progress status, unchanged scope/route, recorded branch,
origin and Git signing profile, and refuses an active checkout or missing session.
Handoffs are not supported by managed resume. It continues the exact recorded
Codex/OpenCode session, engine, model and explicit reasoning setting, preserves partial edits and commits,
and uses the original signing profile and sandbox/permissions. The Linear API key
is removed from the agent environment. No replacement session or checkout is created.
The underlying noninteractive continuation commands follow the
[Codex command reference](https://learn.chatgpt.com/docs/developer-commands#codex-exec)
and [OpenCode CLI reference](https://opencode.ai/docs/cli/#run).

Before continuing, previous events, stderr, report and failure evidence are copied
to `.runs/ISSUE_UUID/resumes/attempt-*/`. Current output is captured afresh and the
session ID is checkpointed so a failed continuation can be resumed again. A failed
agent cannot leave its previous report eligible for reconciliation. Successful
agent output goes through the normal schema, criteria, Git and final Linear checks,
plus signatures/PR/CI gates for default PR completion, before updating the saved outcome. Completion-check failures
retain the fresh report for explicit reconciliation. Only recognized provider
limits schedule another automatic continuation. A recorded finish-state request
requires inspection/reconciliation rather than another implementation continuation.

Native CLI access remains outside runner enforcement; use managed resume to keep
ownership checks and the worker lock in effect.

Untouched `deferred` issues also remain eligible: their availability check may run
again after cooldown, because no feature session or Linear mutation started.

The Go tests exercise real throwaway git repositories with executable Codex/OpenCode
fixtures, a local HTTP Linear server, and simulated GitHub/crontab commands. They
also verify existing SQLite claims survive reopening, inherited worker
locks after the launcher is killed, pagination, API errors, completion gates, signing rejection,
duplicate suppression, failed-run retention, crontab preservation, and the smoke
test's lack of issue-work/scheduling effects. They also cover claim/finish comments,
recorded session IDs, current-user assignment, status transitions and failures, and notification failures
without changing run outcomes. They do not prove live Linear auth, status updates, or comment delivery,
model behavior, real GitHub delivery, or cron execution. The first live check
is only the smoke test; model/signing/issue-to-PR and scheduled execution evidence
can be collected later when feature processing is authorized. Deployment is
outside this workflow.

Reference: [official Codex non-interactive documentation](https://developers.openai.com/codex/noninteractive),
[OpenCode CLI](https://opencode.ai/docs/cli/),
[OpenCode permissions](https://opencode.ai/docs/permissions/),
[Linear GraphQL authentication/errors](https://linear.app/developers/graphql),
and [Linear filtering](https://linear.app/developers/filtering).

Issue attachments are copied into `<run>/attachments/` before the session
starts, next to the checkout rather than inside it. Linear-hosted uploads are
fetched with the API key; other URLs are fetched without credentials when they
serve a file and listed as links when they serve an HTML page.
`attachments/MANIFEST.txt` records each attachment's size, sha256 and URL, and
the prompt carries the same list so the agent can locate the files. Downloads
stream under a per-run budget, `attachment_limit_bytes` on the route (default
256 MiB), with a ten-minute deadline per file; changing the budget later does
not invalidate a run. A download failure never fails the run; the manifest
records what was not fetched.
