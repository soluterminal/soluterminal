# Pane-runtime provenance

`go run ./cmd/agent-fingerprint -root .` (from `daemon`) emits the Linux/amd64
pane-runtime v2 source digest used by `scripts/release-ldflags.sh`. `-detail` lists
covered declarations on stderr. It follows compiler-resolved dependencies from
the wrapper, native Muse driver, startup prefix, init functions and nonconstant
global initializers. Imported packages include source, assembly and embed data.
Every main-package import is covered, including named imports used only by daemon
handlers, because their package initializers run before pane startup too.
The startup prefix runs through the last required pane dispatch, including
any code between the dispatches;
missing or unrecognized dispatches stop the build for review. The Muse driver
(a root) and the Codex terminal bridge (a required dispatch, covered through the
prefix) count while something in the package refers to `runMuseDriver` or
`runCodexTerminal`; then main must dispatch each as above (a terminating
`return` or `os.Exit`). Once nothing refers to the Muse driver
(the Focus removal drops main's `muse-driver` dispatch and then the driver), no
pane can start it and the prefix ends at the last remaining pane dispatch.
`runAgent` stays mandatory. Nonconstant
initializers are covered even when their result variables are unused, including
builtin calls and non-call expressions that can panic or block before main.
The web provenance reader is covered, but its UI bundle/version-label input is
excluded. Unreferenced daemon handlers stay outside the closure; nonconstant
global callback declarations are conservatively included.
The audited main dispatch shape is a top-level `if` whose body calls the pane
function and ends in an unconditional return; startup gotos are refused.
The v2 hash namespace prevents old v1 stamps or receipt digests from matching
this stronger analysis. Legacy artifacts need new analysis with this tool.
One nonconstant exception is a whole-expression `regexp.MustCompile` call with
a compiler-proven valid constant pattern. This pure allocation is included only
when reachable from pane code. Dynamic or invalid patterns, surrounding
expressions that can panic or block, and other initializers stay conservative.
This avoids daemon-only parser edits requesting a restart of every pane while
retaining the v2 startup coverage.

The same rule applies to an imported package whose own initialization is
provably pure data: package-level initializers built only from literals,
identifiers, selector constants, address-of and composite literals, with no
`init` function and no cgo/assembly input. Such a package is hashed only when
pane code names one of its declarations; otherwise its initializers cannot
change pane behavior and it is omitted, so a daemon-only static catalog no
longer invalidates every pane agent. An impure package is always hashed,
whichever root reached it. Anything uncertain — a call, an index, a slice,
division, a receive, a type assertion, a dereference, a function literal, or a
parse failure — is treated as impure and the package keeps its full coverage.

Note that `soluterminal-shared/providers` is **not** omittable under this rule.
The Muse pane resolves its own binary through `resolveMuseBin` →
`providers.ByRuntimeID`, and the daemon startup prefix reaches the catalog
through `agentTemplateRegistry`, so a provider-catalog edit is genuinely pane
code and must keep requesting a pane roll.

The fleet inspector also compares Go version and semantic build settings (debug
symbol/DWARF stripping alone is not an executable behavior change). Only matching
nonempty fingerprints exempt pane agents from inode-based restart warnings.
Daemon replacement, target-change guards and post-restart verification still
require the exact installed executable. Other architectures, cgo builds and
unstamped ad-hoc binaries keep the conservative inode check.

Legacy migration is opt-in local installation metadata. The adjacent
`solud.pane-runtime-legacy.json` has shape
`{"schema":1,"artifacts":{"<binary SHA-256>":"<source digest>"}}`.
Before adding an entry, verify the **exact artifact's** original build receipt
and source commit, calculate that source tree's digest with this tool, and
compare it to the repaired build. Never derive a receipt from a version label
or UI source marker alone. Back up existing receipts; restart the daemon after
installing one. These receipts do not affect signature/update authentication or
authorize workflow actions. A changed source/toolchain still requires restart.
