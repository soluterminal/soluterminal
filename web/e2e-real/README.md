# Playwright against a real side daemon

This suite is intended for the fixed `side-daemon-e2e` Blacksmith profile.
The profile builds the daemon and web bundle remotely, starts an isolated
loopback daemon with a throwaway HOME, and retains browser and daemon evidence.
It never uses the production port or personal credentials.

From the worktree being tested:

```sh
SOLU_TEST_EXECUTION=remote bash scripts/solu-test side-daemon-e2e --stop
SOLU_TEST_EXECUTION=remote bash scripts/solu-test side-daemon-e2e --spec smoke.spec.ts --stop
SOLU_TEST_EXECUTION=remote bash scripts/solu-test side-daemon-e2e --spec approve-device.spec.ts --stop
# Deliberately returns nonzero; checks failure evidence and detached-child cleanup:
SOLU_TEST_EXECUTION=remote bash scripts/solu-test side-daemon-e2e --spec failure-artifacts.spec.ts --stop
```

`--spec` accepts one existing regular file from this directory, named with
lowercase letters, digits and dashes followed by `.spec.ts`. Paths, symlinks,
options and shell expressions are refused before allocating a box and again
on the box. No arbitrary command or environment forwarding is supported.
Without selection, all specs run except the intentional failure probe and
the specs that need a startup scenario.
The runner records selection in `result.json` alongside the source digest.

The box installs browser dependencies, runs the runner/lifecycle contracts,
builds `web/dist` and solud, then allocates an ephemeral loopback port. Health
must report the exact launched PID before Playwright starts. The launcher also
requires version `dev` (update polling disabled) and
an unconfigured, unpaired cloud status, recorded in `runtime.json`. There is no fixture
server, request interception or local build. Runtime HOME, XDG directories,
data and temporary files are disposable; the daemon and browser receive a small
explicit environment with both MCP and hook registration disabled. The runtime
does not inherit host credentials, cloud claims, agent authority or preview flags.
Only the box's installed browser binaries are shared with the runtime.

The smoke test checks daemon PID identity, observes the UI's real pane list,
and checks the daemon's expected refusal when an automated browser tries to
open a terminal. It does not bypass caller classification. A Linux child subreaper
adopts orphaned descendants, including detached sessions and PTY shells, and
terminates/reaps them after success, failure or TERM/INT. Cleanup fails the run
if descendants remain or the side port still accepts connections. Forced host
termination is additionally covered by the runner's VM teardown and expiry.

`browser-artifacts.tar.gz` in the run evidence directory contains the HTML report,
failure trace/screenshot, `web/test-results/side-daemon/solud.log`, `launch.json`
and `cleanup.json`. `cleanup.json` must say `passed: true`, `remainingPids: []`
and `listeningAfterCleanup: false`; VM cleanup is separately recorded in the
runner's `result.json`. The failure probe also records a detached child PID,
which should occur in `terminatedPids`. Open a trace with Playwright's trace viewer.

The profile runs runner/lifecycle contracts on Blacksmith. They are not added
to the existing always-on PR contract job in this change.

Device approval / ST-9: this profile preserves the daemon's default human-proof
policy. A same-host Linux daemon can trace headless Playwright and intentionally
reject its device bootstrap and person-only REST writes (including pane creation).
The Windows-to-WSL `human-proof-e2e/flow.mjs` recipe
therefore cannot be copied as equivalent human proof. @approve-screen should add
real UI specs here for the allowed/refused states and code display; successful
human device enrollment still needs the documented Windows browser/Preview
check or a separately reviewed remote topology that preserves real caller checks.
Do not disable automation detection or add a test-only approval bypass. The
API-level `device.mjs` signer is not a substitute for proving a real browser's
approval eligibility, so it is not ported into this profile.

Startup scenarios are fixed in `scripts/side-daemon-e2e.py` (`SCENARIOS`) and
chosen only by the selected spec; no environment or command is taken from the
caller. `approve-device.spec.ts` runs in `seeded-approver`: before solud
starts, `scripts/side-daemon-e2e-seed.mjs` makes a fresh P-256 key in the
throwaway HOME and writes one approved browser into the throwaway data dir in
the daemon's normal record format (as a first-device approval leaves it, with
no test marker). The launcher checks that the started daemon reports that
device approved and records it in `runtime.json`. The spec gives that key to
one browser context, so its approvals carry its own local signature (the
daemon logs `approved … via device:<seeded id>`), and checks the same daemon
refuses a context without it (`canManageDevices: false`, forced POST 403). The
asking devices enrol through the real V3 routes and compute their own codes.
That spec runs with tracing off, since the key is handed to the page. The
first-device path and a person's own enrolment still need the Windows/Preview
check.
