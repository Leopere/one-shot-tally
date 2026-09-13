# Deployment hook audit — September 7, 2026

The repeated `Hook failed` reports combined a delivery failure with a missing result handoff. The old tally report ran before shipping and could report high activity while deployment remained incomplete. A Stop hook is an automatic end-of-turn command. It is not an approval requirement or evidence that production changed.

## Latest policy and delivery observations

At 19:42 UTC, the user clarified that Git delivery must remain mechanical:
pull once per local day, then `git add .`, automatic commit, and push everything
Git stages. Ship-it does not assess content or select files. A proposed
conflict-state veto was removed before installation. The existing integration
test confirms that deleted tracked files, staged files, and untracked files all
reach the remote. Native hooks still own this flow.

Tests are advisory evidence. The global instructions, installed ship-it skill,
canonical installer guidance, and tracked operator-workspace instructions now
say to classify failures as intentional changes, unintended defects, faulty
tests, or environment problems, then repair the relevant cause and continue.
The shared one-shot-delivery skill formerly said “Production Gate” and “no
known relevant test failure”; those requirements have been replaced. Explicit
unit and regression tests in the infrastructure source-controller deployment
selectors now log their command and failure status without stopping deployment.
A behavior test proves that a test exit of 23 continues to deployment, while an
actual deployment exit of 37 still propagates. Legacy `.ship-it.json` verification
settings are not read by the installed ship-it and do not gate its Git flow.

Tally also contained a direct outcome veto: `recordedOutcome` returned `FAILED`
for the latest failed test before considering successful native delivery.
That test-only veto is removed. A matching recorded command sequence identifies
the test result, so a later failed non-test command still counts as a failure.
The report retains failed check counts and adds advisory diagnosis guidance.

The last native infrastructure delivery exposed two more concrete failures:
Git's autostash application left conflict markers even though pull returned
zero, and an owned, empty legacy guard file had mode 0644 instead of 0600.
The working agent resolved the dispatcher conflict while preserving upstream
selectors. The lock helper now tightens that exact legacy file in place after
checking ownership, type, link count, emptiness, and inode identity. It does not
remove or replace the lock. The actual guard was repaired the same way.

Garage revision `dd65bdcd9f2e5aa770b9c50c657c98e5152e017d` was uploaded and
activated. Its deployment command exceeded the 300-second deadline during
acceptance after HTTP and DNS failures. Subsequent read-only checks show the
exact revision at `/version`, healthy `/health`, the expected 404 at `/examples`,
all seven static assets matching source hashes, and one running Swarm task with
the matching image and update state `completed`. This proves the observed live
revision; it does not turn the timed-out command into a successful receipt.
No second image upload was issued.

SMSBridge's exact-revision recheck for
`7d561352b15395fd61d1e28999ff953a711e43a9` returned exit 0 using its existing
successful workflow. CISL2's newer revision
`16a1c438074aae2a8353a1bc2ced22ffcb3ce8e4` still failed the actual backup-node
readiness check; the known host connection refusal remains separate from test
policy. Earlier observations below retain their original timestamps and revisions.

## Findings and repairs

| Finding | Evidence | Result |
| --- | --- | --- |
| Shipping ended after tally had reported. Tally did not receive the final deployment result. | Native Stop runs tally before ship-it. `internal/app/app.go` in ship-it now calls the tally reporter after each repository operation. | Native delivery records FAILED or a verified success. Failure output includes the error and diagnostic log path. |
| A failed deployment caused hook exit 1 even when the failure could be reported through the hook protocol. | The shipping cycle propagated its operation error directly to the hook runner. | A correctly recorded failure returns valid hook JSON and exit 0. The outcome remains FAILED. If delivery and its reporter both fail, the process still returns nonzero. |
| A clean checkout suppressed deployment retries. | The repeated Stop check only looked for unpublished Git changes. A successful push followed by failed deployment leaves Git clean. | One continuation retry is permitted for the failed roots. Successful roots are skipped unless they have new work. An unchanged repeat does not deploy again. |
| Blocker claims could disappear across turns or be outweighed by activity. | The former result was based on per-turn tool state; assistant terminal claims were unused. | A session/project ledger retains unresolved obligations. Abandonment gets FAILED and outcome grade 0. A bounded Stop response asks for evidence and recovery. Only observed failure followed by matching success earns RECOVERED / INCREDIBLE WIN. |
| Current native command hooks omit exit status. | A controlled local probe showed Bash PostToolUse `tool_response` containing stdout only. The structured result visible to the model belongs to another layer. | Stdout-only results remain unknown and the report explains why. Native shipping sends its actual result directly. Printed PASS or JSON is not promoted to success. |
| An older successful receipt could conceal newer work. | A successful operation can finish while the checkout changes. | Success must match a clean current HEAD before clearing delivery. Ordered native receipt IDs reject older results and malformed IDs. |
| Removal guidance pointed only to agent-file-guard recovery. | `internal/fileguard/hook.go`, `removalAdvice`. | The installed guidance recommends macOS Move to Trash, or the host OS Recycle Bin. It covers files and directories. |

## Rules and remaining constraints

These are separate from the repaired result handoff. They should be considered explicitly when deciding deployment policy.

| Source | Exact rule or behavior | Practical effect |
| --- | --- | --- |
| `~/.codex/AGENTS.md:21` | “Agents finish their work and let the hooks run. Do not invoke ship-it, poll it, or add shipping prompts.” | Routine Git delivery belongs to native hooks. An agent should repair and verify the repository command, then receive the native outcome. This rule is not permission to abandon a failed deployment. |
| `~/.codex/AGENTS.md:24` | “Report a blocker only with the attempted command, observed failure, and the specific missing access, information, or external change.” | A prior failed attempt alone does not establish a blocker. |
| `~/.codex/skills/ship-it/SKILL.md` | A tracked `.deploy-it.json` selects the deployment handoff after a successful push. | A push proves Git delivery. The repository deployment command must verify its destination before reporting deployment success. |
| `~/dev/boompay-vps-infra-l2/scripts/deployment-lock.sh:20` | Stale recovery immediately returns when the owner file is missing or empty. | An empty orphaned lock is never recovered by this path. |
| Same file, lines 97–101 | Release deletes the owner file, then suppresses failure to remove the lock directory. | A directory-removal denial can leave an empty lock. Later acquisition says another change is running without an owner record proving that claim. |
| Same file, acquisition loop | Default lock wait is 1500 seconds. | A shorter enclosing deployment timeout can terminate the command before this loop reports its own diagnosis. |
| `internal/fileguard/hook.go`, destructive-command regex | The guard scans command text for deletion tokens, including quoted text. | A read-only search or test containing a literal deletion command can be denied. The guard's permission logic was not changed by the wording update. |

The reported production lock is `~/.local/share/boompay-vps-infra-l2-production-controller/runtime/.production-deployment-lock`. The initial investigation found an empty directory with no owner record. A further check around 18:03 UTC found the same condition. This remains relevant to Partners, which still uses that controller lock. Garage and Boost have since completed deployments through their current application commands.

## Current delivery evidence and command defects

These observations were collected on September 7 between 17:40 and 18:06 UTC. Revisions can advance in other agent tabs. A passing local test does not replace a deployment receipt.

| Repository | Observed result | Cause or remaining work |
| --- | --- | --- |
| Garage | The accepted receipt for `c7f692dbd47f1a37a81dcdf3a5944b8d12f02bc8` records completion at 17:40:21 UTC. Public `/version` matched, and `/health` returned `ok: true`, scope `garage`. | This release is live. The earlier quoted `d0126f9` status is stale. |
| Boost | The accepted receipt for `820d7d725faedc128f905ed3872ab203f9fce5fb` records completion at 17:00:28 UTC. Public `/version` matched, and `/health` returned `ok: true`, scope `borrower`. | This release is live. |
| Partners | Revision `495330cea5e167869f0f7e19080a069d6539ad94` exceeded deploy-it's deadline at 16:59:53. Its image workflow succeeded at 17:02:00. A later attempt for `f5b9da358e7c91c28aca641862ed96733f7dbfa9` failed with `error connecting to api.github.com`. Public `/version` still reported `d56d9c232ffb67731475063160294644a4ff4b0f`. | The script waits up to 1,800 seconds for an Actions image inside a 300-second deployment contract. Its unguarded registry lookup also aborts on a transient API failure. Both failures precede lock acquisition. The shared empty lock remains a separate activation obstacle. |
| SMSBridge | The latest recorded deployment failed with `required deployment environment variable GH_CONFIG_DIR is not set`. | The manifest declared optional authentication overrides and repair IDs as mandatory. The repaired manifest requires `HOME` and explicitly allows those optional inputs. deploy-it now validates `optional_env`, forwards only declared values, and preserves missing-required-variable failures. Deployment after this repair still needs its own receipt. |
| CISL2 | The controller reports `cisl2-backup-01 Down Active Unreachable`; the other four nodes are ready. SSH to the configured backup host returned `Connection refused`. The cloud inventory still lists that exact host as active. | This is a verified host failure. The preflight must preserve fleet readiness. Separately, the recovery command returned 75 after each successful intermediate phase; its repair runs all remaining phases, saves each completed phase, and returns success only after final acceptance. |

The Partners image workflow continues after the local deployment times out. An old successful workflow is not permission to activate an older revision. Recheck the exact currently published revision and its signed artifact before retrying the existing activation command.

The repository delivery rule also exposes a scope gap: Stop originally received only the tab's working directory or workspace roots. Edits in another repository could remain unpublished. The repair records explicit native edit paths by session and includes their canonical repository roots at Stop. Reading a repository does not select it for publication. A pending root is cleared only after successful delivery of its exact clean revision; concurrent newer edits remain pending. Historical edits made before installation, opaque shell edits outside the known roots, and child sessions with a different native session ID cannot be inferred from this registry. The registry does not deduplicate concurrent initial Stop events; the existing repository lock serializes their execution.

After installation, one real native patch changed files in deploy-it, ship-it, SMSBridge, and CISL2. The host hook recorded all four canonical repository roots under this session, each with both PreToolUse and PostToolUse observations. This confirms compatibility with the installed host's actual edit events. No synthetic events were written to the live registry.

## Controller repair follow-up

SMSBridge's existing production run completed successfully. The exact-revision
`deploy-it` verification returned exit 0 for
`7d561352b15395fd61d1e28999ff953a711e43a9`. Both public health endpoints reported
that build. Reattaching to the existing run verified its result without another
upload. The earlier local watcher timeout had not stopped the remote workflow.

The shared controller repair now keeps the lock directory in place and uses
the same kernel advisory lock in Garage and the central controller. New owner
records identify their format and distinguish active from released ownership.
Nested commands must inherit the actual locked descriptor and matching owner
record. Fresh empty legacy acquisitions remain protected; aged empty directories
can recover in place. Garage also propagates a failed child command's exit code.

A separate bootstrap defect prevented Partners from receiving this repair:
it acquired the old cached lock before fetching the new controller. The tracked
infrastructure deployment command now obtains the lock through the shipped
archive, fast-forwards the verified clean cache, checks the exact revision and
helper bytes, and releases ownership before reporting cache acceptance. This
is a controller update; it is not evidence that the whole fleet has converged.

The infrastructure manifest previously requested 3,600 seconds, which the
installed `deploy-it check` rejected: `timeout_seconds must be between 1 and 300`.
The repaired manifest uses 300 seconds. Its former implicit Wazuh audit and
rolling reboot are now an explicit `wazuh-sca-audit` selector. Long fleet
procedures still need a resumable execution design within the valid envelope.
Keeping their selectors does not prove that those procedures meet that limit.

The native broad infrastructure test run reported 53 failures and 441 errors
across 1,225 tests, with many fixture cleanup failures. It is not
counted as a pass. Focused Linux testing exposed and helped fix an additional
BSD/GNU file-permission check mismatch and missing helper dependencies in
fixtures; the affected 91-test Linux run then passed. Generated fixture files
left in the repository root were preserved under ignored runtime storage and
removed from the publication set. No production data or Trash was inspected
or removed during that cleanup.

The final shared-lock snapshot passed all 24 focused controller/lock tests in
Linux after its source hashes were checked. Garage passed all 10 lock tests,
including real inheritance in both directions, refusal of an unlocked inherited
descriptor, fresh legacy-acquisition protection, and propagation of child exit
37 after durable release. Shell validation and diff checks passed. These are
repair checks; the controller and Garage updates still require native delivery
and destination receipts.

## Native Trash maintenance

The [prepared maintenance worker](maintenance/README.md) uses launchd for weekly runs, login catch-up, and Trash-change triggers. It requires no AI calls. It selects items whose macOS date-added value is strictly more than seven days old and skips unknown ages. Fixture tests do not access protected Trash paths. Activation must occur from the user's normal Terminal.

## Verification

- Tally root package tests pass, including abandonment, cross-turn recovery, stdout-only results, stale receipts, new local edits, and shipping/deployment identity.
- All ship-it Go packages pass, including legacy-hook compatibility and deployment failure propagation.
- File-guard runtime tests pass for removal guidance, allowed commands, hook protocols, and protected path handling.
- `scripts/test-native-delivery.py` runs compiled binaries against isolated local Git remotes and a simulated deployment command. It verifies failure, one clean continuation retry, recovery, immutable commit identity, no duplicate deployment, independent repository retries, and visible reporter failures.
- The extended integration test passed against the installed binaries. Native edit events selected an external repository, a read-only event did not select another dirty repository, clean failed deployment remained pending, an unchanged third Stop did no work, and a later turn recorded recovery and cleared the obligation.
- deploy-it's full Go suite passed with `DEPLOY_IT_KEEP_TEST_DIRS=1`, including required and optional environment handling. SMSBridge's contract verification passed all 30 tests. CISL2's focused recovery suite passed all nine tests.
- A full native tally test run passed the modified root package but failed existing file-guard and recovery tests on temporary-directory cleanup and directory moves. The modified root package and ship-it application tests passed again after the final registry fixes.

Tests on this host use `GOFLAGS=-work` and `SHIP_IT_KEEP_TEST_DIRS=1`. Assertions run normally; temporary fixture directories are retained. Native delivery receipts are a cooperating local executable protocol, not a security boundary against another process running as the same OS user.
