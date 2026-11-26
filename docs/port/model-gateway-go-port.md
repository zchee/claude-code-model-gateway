# Go port of the model-gateway Claude Code plugin: work plan (v7)

- **Status: phase 0 landed; phase 1 is in progress (see the phase table).** Approval covers phase 5 and the phase 6 rehearsal (T6.0); stop before T6.1.
- Mode: `/plan --direct --deliberate` (direct plan with deliberate-mode content: options, pre-mortem, expanded test plan, ADR).
- Written: v6 on 2026-11-25; v7 on 2026-11-26, corrected after the test inventory on 2026-11-26 (from `date`). v1 and v2 were each reviewed by an architect lane and a
  critic lane (`claude-gpt-6.2-astra-fast[1m]`); verdicts were "sound with changes" and "revise" both times. v3, v4 and v5
  each had a targeted critic check of phases 0 and 1; all three said "revise", with a shorter list each time, and the
  last one states that T0.0 and T0.1 can start after approval. v6 applied that list. The sixth check said "revise" for
  T0.4, T0.7 and T0.7b, all three "can start after the listed changes"; the user chose to apply those changes in v7
  and proceed without another review round. Appendix C lists the changes and Appendix D what the reviews leave open.
- Source: `zchee/eigenwise-toolshed`, branch `dev`, **pinned commit `a8501f251755`** (the head of `dev` on 2026-11-25
  22:51), `plugins/model-gateway` v0.56.3, tree `aaf4f26b527c2ff08dbc47707a5f69e6ec2cf40a`: 13,060 lines of JavaScript in
  `lib/`, `bin/`, `hooks/` (33 files); 57 test files with 21,680 lines (65 files and 23,542 lines in `test/` in total).
  The 57 test files have 693 textual matches of `test(` at line start after optional indentation (662 unindented, 31 indented); 692 register tests.
  `test/gateway-process-isolation.test.js:1150` is inside the template literal at lines 1146–1157 that the case registered at line 1143 writes out and runs as a child suite.
  Five nested `t.test(` sites (`test/lmstudio.test.js:552,589`, `test/local-llama.test.js:276`, `test/proxy-providers.test.js:605,664`) bring the total to 697 registration sites; 35 expand to multiple cases, for 832 cases in all.
  The pin was moved once, on the user's decision, before any implementation: v1 to v4 and the inventories were written
  against `4ffb2f672f0c`. In the plugin the two commits differ by `6ffbf58d3` alone: `lib/runtime.js` gains a second
  evidence group (`models.dev google-vertex 2026-11-25`) with four policy rows, `gemini-4-hydrogen` and `gemini-4-neon`
  under the bare and the `vertex/` spelling, window 1,048,576, placed between the Llama 4 Scout row and the
  `vertex/openai/*` rows (Vertex rows 45 to 49, bare Gemini ids 9 to 11); `test/context-window.test.js` gains two table
  cases, the new counts and a per-row measurement date (no new `test()` site); `README.md` and
  `skills/model-gateway/SKILL.md` say 49 and name the second catalog date. The inventories and this plan were moved to
  the new pin (INV-07: citations into the two changed files shifted and checked line by line, the policy table
  extended); the reviews of v1 through v4 still cite the old pin. Every lane reads the source from an export of the pinned
  commit (T0.0), never from the moving checkout.
- Target: module `github.com/zchee/claude-code-model-gateway` (`go.mod:1`), `go 1.27` (toolchain go1.27.4), Apache-2.0,
  remote `git@github.com:zchee/claude-code-model-gateway.git`. The first commit of `port/phase-0` tracks the user's
  `main.go` (`package main`, no `main` function yet) and `README.md` unchanged; the plan builds on `main.go`.
- Porting spec: `docs/port/` is authoritative for the committed inventories, review records and this plan; copies under
  `.omc/` are not maintained. Every claim in the inventories cites the source as `path:line` at the pinned commit.

| id | file | lines | covers |
|---|---|---:|---|
| INV-01 | `01-cli.md` | 563 | every CLI command, flows, hook contract, release acquisition |
| INV-02 | `02-worker.md` | 1035 | HTTP surface, routing, transforms, streaming, sentry, catalog, Grok backend |
| INV-03 | `03-supervision.md` | 686 | process topology, state files, relay, lifecycle protocols, ownership proof, RC compatibility |
| INV-04 | `04-policy-state.md` | 1006 | env vars, paths, model window policy table, persisted JSON, pins, settings wiring |
| INV-05 | `05-tests-skills.md` | 648 | test harness, per-file test classes, behavior catalogue, skills contract, changelog digest |
| INV-06 | `06-claude-code-plugin-facts.md` | 153 | Claude Code plugin, hook and gateway-discovery facts from the official docs |
| INV-07 | `07-repin.md` | 109 | how the inventories and this plan were moved from `4ffb2f672f0c` to `a8501f251755`: line mapping, rewritten citations, the check |
| REV-A | `review-architect.md` | 192 | architect review of v1 |
| REV-C | `review-critic.md` | 146 | critic review of v1 |
| REV-A2, REV-C2 | `review-architect-2.md`, `review-critic-2.md` | 76, 51 | second-round reviews of v2 (condensed records) |
| REV-C3 | `review-critic-3.md` | 41 | targeted critic check of v3 (phases 0 and 1) |
| REV-C4 | `review-critic-4.md` | 49 | delta check of v4 (phases 0 and 1) |
| REV-C5 | `review-critic-5.md` | 35 | last delta check, of v5 (phases 0 and 1) |
| REV-C6 | `review-critic-6.md` | 166 | sixth critic check of v6 (venue tooling and test inventory), with the lead's spot check |
| VER-R | `verify-repin.md` | 35 | verification of the move to the new pin by a separate lane, with the corrections |

Task ids (`T2.3`), decision ids (`D4`, `OD-2`) and inventory ids (`INV-02`) are plan markers. They must never appear in
source files, comments, test names or commit messages; code states the reason in plain words.

---

## 1. Requirements summary

### 1.1 What is built

One Go binary that replaces the Node.js runtime of the plugin and keeps the contracts in scope. `go install
github.com/zchee/claude-code-model-gateway@<version>` builds it as `claude-code-model-gateway`; `setup` copies it to the
stable path `~/.claude/model-gateway/bin/model-gateway`, which is what hooks, skills and launchers call.

| # | Deliverable | Source of truth |
|---|---|---|
| R1 | CLI with 16 source commands (`setup login start stop ensure recover readiness-gate status models catalog pin context-window env doctor serve-shim serve-worker`): same flags, exit codes, stdout/stderr split, except as listed in §3.10 | INV-01 §0-§2; `lib/commands.js:2935-3041` |
| R2 | Request path: `/healthz`, `/v1/models`, inference routing to Anthropic / claude-code-proxy / Grok, body transforms, SSE relay, context sentry, compaction guard, dispatch model, usage and route telemetry | INV-02 §1-§12 |
| R3 | Supervision: detached supervisor, replaceable worker, supervised `claude-code-proxy`, relay, `/restart`, `/drain`, lifecycle lock, intentional stop, ownership-checked stop | INV-03 §1-§7 |
| R4 | State and settings: same paths and JSON shapes under `$HOME/.claude/model-gateway`, same Claude Code settings keys, discovery cache, sidequest catalog | INV-04 §2, §4, §6; INV-01 §1.1 |
| R5 | Plugin packaging for a compiled binary: marketplace entry, `plugin.json`, hooks, the `model-gateway` skill, cold install | INV-06 §1-§5, §7 |
| R6 | A reversible migration from a running Node gateway, and a shadow mode for a soak before cutover | section 3.6, 3.7 |

`claude-code-proxy` (Rust, `raine/claude-code-proxy`; `gh api` on 2026-11-25 shows release v0.1.49 of 2026-11-21 and the
fork `zchee/claude-code-proxy` with a `dev` branch) stays an external binary that the gateway downloads, verifies and
supervises (`lib/commands.js:439-527`).

Extensions that are not parity, each with its own tests: `version`, `completions`, `migrate`, `hook session-start`,
shadow mode, `MODEL_GATEWAY_STATE_DIR`.

### 1.2 Contracts that must keep working

| Consumer | Contract | Evidence |
|---|---|---|
| Claude Code 2.1.332 | settings `env` block keys; readiness hooks on SessionStart and UserPromptSubmit; `GET /v1/models?limit=1000` (3 s) reading `data[].id`, `display_name`, `description`; cache `$CLAUDE_CONFIG_DIR/cache/gateway-models.json`; `[1m]` suffix | INV-06 §3, §6; INV-04 §3.6, §3.8 |
| claude-code-proxy | spawned as `claude-code-proxy serve --no-monitor` with `PORT=<proxy port>`, cwd inherited; listing read from HTTP `GET /v1/models` (`data` array) | INV-03 §1.1; INV-04 §7.1 |
| `gateway-switch` mod (user's own, separate repository) | runs `node ~/.claude/model-gateway/model-gateway.js ensure --quiet`, parses the JSON block that `env` prints, probes `/healthz` | `claude-code-mods/mods/gateway-switch/hooks/register.ts:142,250-293` |
| updater | `node ~/.claude/model-gateway/update.js` takes no arguments and runs `setup --preserve-wiring` | `hooks/registry-writer.js:114-122` |
| sidequest plugin | `~/.claude/model-gateway/catalog.json` schema 4; breadcrumb `~/.claude/toolshed/registry/model-gateway.json` schema 1 | INV-01 §3.9, §4.2 |
| the `model-gateway` skill | its commands, paths and output strings in INV-05 §5.2-§5.5 | INV-05 §5 |
| the user's sessions | ports 18764 (gateway), 18765 (proxy); model ids `claude-<backend id>[1m]` | INV-04 §2.3; INV-02 §2.2 |

### 1.3 Out of scope

| Item | Reason |
|---|---|
| Reimplementing claude-code-proxy | Rust provider code that the plugin never contained |
| Remote-control compatibility | The user decided not to port it (OD-21). Keep only read-only hosts-block detection and migration-only recognition and cleanup of the legacy URL (§3.6). |
| Windows runtime parity in the first milestone | the user's machines are macOS arm64 and Linux amd64; Windows needs WMI launch, taskkill trees, named pipes, PowerShell probes and an alternative to `ExtraFiles` (INV-03 §8; REV-A item 6). The tree must still compile for `GOOS=windows` (section 3.9). Open decision OD-2 |
| `hooks/ensure.js` | dormant since commit `101a64fd2` removed the SessionStart `ensure` hook (INV-01 §3.2, §8 item 2) |
| Dead code in `lib/commands.js` | never called (INV-01 §8 item 4) |
| Source tests of Node artifacts (harness self-tests, `native-budget-probe`, `windows-hide`, `start-gateway-timeout`, `support`) | `test/conformance/classification.tsv` decides per case, not per file: only a case with no assertion of observable behavior can be `js-only` (T0.7). No file is exempt as a whole |
| Changes to the `gateway-switch` repository, uninstalling the old plugin | separate repositories and user actions; listed as follow-ups that need their own approval (T6.3) |

### 1.4 Hard constraints

1. **The live gateway is out of reach by construction, not by convention.** Ports 18764/18765 and `~/.claude/model-gateway`
   serve the user's running sessions, including the sessions that execute this plan. Tests are split into two tiers
   (section 3.8); anything that starts a gateway process, signals a process or runs the Node suite runs only on machines
   where no live gateway exists. Cutover is run by the user from a terminal.
2. **Tests never write the user's real Claude settings.** This happened to the source once (`test/isolate-home.js:4-7`).
3. **Go rules** of `~/.claude/instructions/Go.md`: Go 1.27 idioms, `encoding/json/v2` + `jsontext`, `omitzero`, table tests
   as `map[string]struct`, `go-cmp`, `t.Context()`, single-line signatures, godoc ending with a period, `gofumpt -extra`,
   `modernize`, `goimports-rereviser`. Third-party packages are used actively (user instruction 2026-11-25), section 3.3.
4. **Worker routing**: use the simple or complex lanes in section 9, including for implementation.
5. **Credentials stay out of the lead context.** Steps that read Codex or Grok tokens or call authenticated endpoints are
   run by the user; security review goes to the grok lane.

---

## 2. RALPLAN-DR summary

### 2.1 Principles

1. **Port contracts, not structure.** CLI, HTTP, file and settings contracts are preserved; the inside is idiomatic Go.
   Every intentional difference is named in advance with the source tests it changes (section 3.10).
2. **Fail closed on ownership, fail open on the prompt gate.** A process or file that cannot be proven ours is never
   signaled or overwritten; a gate that the host cannot run never blocks a prompt (INV-05 §4.8, §4.9).
3. **Two JSON policies.** Passthrough forwards the original bytes. Bodies the source re-serializes are normalized the way
   `JSON.parse` + `JSON.stringify` normalize them. User-owned files keep what the user wrote (section 3.5).
4. **Evidence, not assertion.** A behavior counts as ported when a test that ran against Go code passed, and the test's
   expectation did not come from the lane that wrote the code under test.
5. **Use a library when it removes risk or code; use the standard library when it already does the job.**

### 2.2 Decision drivers (top three)

1. **Regression risk in the 13 recurring bug classes** of the changelog: stream left open, sentry latch, killing a foreign
   install or a reused PID, tests touching the real home (INV-05 §6.2).
2. **Provability of parity**: which tests really exercised Go, with expectations written by someone else.
3. **Operational continuity**: the gateway carries the user's daily sessions; migration must be reversible and must not
   change whether the gateway was running or intentionally stopped.

### 2.3 Options for the process model

What the relay actually does (`lib/commands.js:2345-2391`, pinned by `test/shim-request.test.js:84,107,194`): delivery is
latched when the TCP connection to the worker is established. Before delivery, failures are retried (81 attempts, 50 ms
apart). After delivery, the request is **sent again only if the worker that took it has been replaced**; while that same
worker is still current the supervisor waits and finally answers 503 with `x-should-retry: false`. Once the worker's
response headers have been relayed, a worker death resets the client connection. This is not at-most-once delivery.

| | A. Same topology, Go-native IPC (**chosen**) | B. Single daemon | C. Supervisor passes the listening socket to the worker |
|---|---|---|---|
| Shape | supervisor (public listener, relay, proxy child) + worker (router) + proxy | one process; router generations swapped in process; upgrade by process handoff | supervisor binds, worker accepts on the inherited descriptor |
| Public listener stays up while the router is replaced or has crashed | yes | only for in-process swaps; a daemon crash closes it until the next gate runs | yes (kernel backlog holds connections) |
| Can answer for a router that died before sending headers | yes (resend to the replacement, or 503) | in-process failures only; it keeps the client's response writer and can track "possibly submitted" precisely | no: an accepted connection dies with the worker |
| Extra loopback hop and one buffered body per request | yes (as today, cap 64 MiB) | no | no |
| Existing wire contracts (`/restart`, `/drain`, lifecycle events, pid files, `/healthz` supervisor fields) | unchanged | new handoff protocol; several contracts change meaning | control endpoints and health merging move into the worker |
| Source tests usable as a judge | request path, lifecycle, CLI, state | request path, CLI and state tests still apply; lifecycle tests do not | same as B, plus most drain tests |
| New protocol to design and prove | port report + liveness descriptors (small) | binary handoff, proxy adoption | descriptor passing, control channel, health merge |
| Windows later | pipes work | simplest | `net.FileListener` is not available |

Why A: it keeps the public listener, the drain/control wire contract and worker-crash isolation as they are, and it is the
only option whose supervisor/worker behavior is already specified by tests we can port one to one. B's strongest argument
is real: it removes a hop and a buffer and can decide "resend or not" from exact knowledge instead of a TCP-connect latch.
It loses when the whole daemon dies, and it needs a binary-handoff protocol the source never had. The retry rule itself is
ported as it is and stays a separate decision (follow-up F1). B and C remain recorded with a re-evaluation trigger.

### 2.4 Options for proving parity

| | T1. Go tests only | T2. Source suite as a judge where it can run, Go tests elsewhere (**chosen**) | T3. Recorded transcripts |
|---|---|---|---|
| Method | every expectation re-derived from the inventories | the source's own tests drive the Go binary through an adapted launch path; unit and integration tests are ported to Go | record CLI, state and lifecycle transcripts from the Node gateway, replay against Go |
| Independent expectations | only if a separate lane writes the tests first | yes for every test that runs unchanged | yes for recorded paths |
| Cost | a test-authoring lane per phase | a launch adapter, a per-test classification, Node in that job | recorder tooling, timing fragility |
| Known limits | none structural | tests that load `lib/`, patch modules, or assert Node process identity cannot judge Go (INV-05 §3.3; REV-A item 7) | only what was recorded |

T2 is **not yet proven**: the launch adapter must keep the harness's PID, stdio, IPC shutdown and teardown assumptions
(`test/support.js:105-120,790-829`). T2.0b proves it with launch-only probes on the walking skeleton; behavioral source
tests run when their features exist. Where a seam cannot be adapted, only the tests that need it become Go ports. In every
case a Go port's expectations are written before the implementation by a lane that does not implement it (T0.7, section 9).

### 2.5 Options for state

| | S1. Same state root and file shapes (**chosen**) | S2. New state root with an import step |
|---|---|---|
| Existing wiring, pins, context windows, control token | keep working | imported once; projects need not be re-wired if the import rewrites hooks |
| Source tests that assert on-disk shapes | apply | apply at any root |
| Risk | a legacy Node CLI and a Go CLI may act on one directory during the transition | two directories to keep apart; consumers (`gateway-switch`, sidequest) read the old paths |

S1 is chosen because three consumers read fixed paths under `~/.claude/model-gateway`. Its risk is handled by making the
transition single-writer (section 3.6), not by making the two runtimes understand each other's records.

---

## 3. Architecture

### 3.1 Process topology and the supervisor/worker protocol

```
 model-gateway <cmd>  ── detached (setsid, logs/guardian.log) ──▶  SUPERVISOR   model-gateway serve-shim      guardian.pid
 (setup/start/ensure/recover/stop;                                 │ listens 127.0.0.1:18764
  lifecycle.lock; pid records)                                     │ Host/Origin gate · /restart · 64 MiB cap · relay
                                                                   ├─ exec ─▶ WORKER   model-gateway serve-worker           shim.pid
                                                                   │          127.0.0.1:<ephemeral> + gateway.sock · routing · /drain
                                                                   └─ exec ─▶ claude-code-proxy serve --no-monitor (PORT=18765)   proxy.pid
```

**Worker modes.** A worker started with a preconfigured non-zero port is supervised without a port report, as in the
source. There is no compatibility listener in either mode.
- Standalone (a user or a test started it; no `MODEL_GATEWAY_CONTROL_FDS`): no descriptor is read, stdin is ignored, the
  worker prints `model-gateway shim listening on 127.0.0.1:<port>`. This is what the source harness expects
  (`test/support.js:790-829`: stdin `ignore`).
- Supervised: `MODEL_GATEWAY_CONTROL_FDS=3,4` and `MODEL_GATEWAY_WORKER_GENERATION=<id>`. Descriptor 3 is the report pipe:
  the worker writes one line `{"type":"listening","port":N,"generation":"…"}` (at most 256 bytes) and closes it; the
  supervisor accepts it within 5 s (`lib/commands.js:2488,2606-2617`), checks port range and generation, ignores a report
  from an older generation, and otherwise exits 1 as today. Descriptor 4 is the liveness pipe: the worker reads it, and EOF
  means the supervisor is gone (the source's `disconnect` rule, `lib/request-worker.js:1210`). A missing or malformed
  descriptor makes the worker exit 1 with a message; it never falls back to stdin. The supervisor holds the only write end
  (close-on-exec), closes the child ends after `Start`, and removes `NODE_CHANNEL_FD` from the worker's environment.
- Test control channel (the source harness only). The source stops a supervisor that has an IPC channel on the message
  `{"type":"fixture-shutdown"}` (`lib/commands.js:2900-2904`; `test/support.js:105-122`), and a worker exits when its
  channel disconnects (`lib/request-worker.js:1210`; `test/support.js:146-150`). When `NODE_CHANNEL_FD` is set and the
  test guard of section 3.8 is active, the Go supervisor reads newline-delimited JSON from that descriptor and treats
  `fixture-shutdown` as it treats SIGTERM, exiting 0; a standalone worker exits 0 when the descriptor reaches EOF.
  Outside the test guard the descriptor is closed unread. T2.0 builds this, T2.0b proves it.
- EOF needs a scheduled worker and no leaked write end, so it does not replace orphan cleanup: birth records and orphan
  reaping are ported with the source's version rule (INV-03 §4.8), see D6.

**Relay state machine** (port of `lib/commands.js:2345-2441`; tests ported from `test/shim-request.test.js`):

| State | Event | Action |
|---|---|---|
| undelivered | no worker port yet, or connect error | wait 50 ms, up to 81 attempts; then 503 `could not deliver … retry it shortly` (retryable) |
| undelivered | TCP connection to worker generation *g* established | latch `delivered(g)`, send the buffered body |
| delivered(g) | error before response headers, current generation is still *g* | do not resend; wait; when attempts run out: 503 with `x-should-retry: false` and lifecycle event `worker-request-lost` |
| delivered(g) | error before response headers, generation changed | send the body to the new worker (the source's rule; a second upstream call is possible) |
| committed | worker dies or aborts after its headers were relayed | reset the client connection; no status rewrite |

Transport rules that make Go behave like Node's `http.request` here (REV-A item 4): one connection per relayed request
(`DisableKeepAlives`), requests built without `GetBody` so `net/http` cannot replay them on its own, redirects never
followed (`Transport.RoundTrip`, not `http.Client`). Worker to upstream: pooled connections (64 per host), `Proxy: nil`,
HTTP/1.1 only, no automatic decompression, no redirect following, no automatic replay of a request that carries a body, a
1 h socket-inactivity deadline as in the source (`lib/request-worker.js:2221`), bounded additionally by service shutdown.
What happens to the provider request when the client disconnects is measured first and then matched (section 3.10).

Other replacements for Node-only mechanisms (INV-03 §12, INV-02 §13):

| Node mechanism | Go replacement |
|---|---|
| `spawn({detached:true})` + `unref()` (`lib/process-supervision.js:981-991`) | `exec.Cmd` with `SysProcAttr{Setsid: true}`, stdio to `logs/guardian.log`, `Process.Release()`; a start error is written to the log and returned |
| `ps` / PowerShell process probes (`lib/process-supervision.js:307-348`) | `procinfo` reads `/proc` on Linux and uses `sysctl` for argv, birth time, parent and the process table on macOS. On macOS, `lsof` supplies mapped-file evidence, not authoritative image identity (§3.4). `Probe.Birth` does not inspect argv or the image: it reads the kernel process record on macOS and the proc clock and stat on Linux. |
| `lsof` / `/proc` / `netstat` port-owner probes (`lib/process-supervision.js:242-289`) | `procinfo` reads the full listener table in `/proc` on Linux and uses `lsof` alone on macOS, without `netstat`. See §3.4 for what each result proves. |
| `AsyncLocalStorage` re-entrant lock (`lib/lifecycle-lock.js:88-94`) | the held lock travels in `context.Context` |
| module-load side effects (`lib/runtime.js:319`) | one explicit `config.Load()` at process start; an invalid `compactAt` fails there with the source's message, except for null (§3.10; `lib/runtime.js:291-297`), a file that is not JSON falls back to defaults as today (`lib/runtime.js:281-283`) |
| `process.exit` inside the lock (`lib/commands.js:241`) | commands return an exit code; `main` releases the lock, then exits |
| Patterns with no potential Unicode property escape are skipped. For affected patterns, the compatibility scan runs first, then `new RegExp(pattern, 'u')` checks the original pattern before the rewrite is applied (`lib/tool-schema-compat.js:195-246,263-269,286-289`). | Preserve that order and its reason codes. A syntax checker written in this repository must reproduce the verdict of `new RegExp(pattern, 'u')` on the original pattern (valid or a syntax error), including the ECMA-262 property names and values the source's patterns can use, except for the engine quirks in §3.10. The committed Node-generated corpus is the oracle. |

### 3.2 Package map and owners

`main.go` (repository root, the user's file) gains `func main` and calls `internal/cli`. Each package has one owning task at a time;
other tasks use it only through its exported API, which the owning task publishes first as a doc-commented stub.

Inject environment, home and working-directory lookups into `internal/` packages instead of reading ambient values with
`os.Getenv`, `os.LookupEnv`, `os.UserHomeDir` or `os.Getwd`.

A path check and the operation it guards must use the same resolved object. A symbolic link followed by `..` can make
lexical cleaning name a different object from the one the kernel resolves. Reject paths that are not clean and absolute
and use the same path for both, or open the directory once and do both relative to its descriptor.

| Source (lines at the pin) | Go package (`internal/…`) | Owner |
|---|---|---|
| `lib/runtime.js` (801): paths, env, policy, install | `paths`, `config`, `policy`, `install` | T0.2, T1.1, T1.3 |
| `lib/context-window.js` (133) | `policy` | T1.1 |
| `lib/proxy-models.js` (176) | `proxymodels` (leaf: listing parse, route table, advertised ids) | T1.2 |
| `lib/lmstudio.js` (175), `lib/local-llama.js` (76), `lib/proxy-config.js` (24) | `localprov`, `proxyconfig` | T1.3 (parsers), T2.1 (live probes) |
| `lib/pins.js` (482), `lib/anthropic-catalog.js` (125), `lib/previous-picker-rows.js` (46) | `pins`, `anthropiccatalog` | T1.4 (logic), T1.14 (legacy URL), T4.3 (probe) |
| `lib/tool-schema-compat.js` (388) | `toolschema` | T1.5a, T1.5b |
| `lib/request-worker.js` (2734) | `worker` (server), `worker/route`, `worker/sentry`, `worker/dispatch`, `worker/upstream`, `worker/reply`, `worker/sse`, `worker/compactguard`; the frame scanner `sse` | T2.0, T1.7, T1.6, T1.8, T2.2, T2.3, T2.4, T2.6; T1.13 |
| `lib/request-worker.js:1117-1171,1272-1295,2430-2676` (route and terminal response facts) | `worker/outcome` (values only) | T2.0 |
| `lib/request-worker.js:2444-2592` (proxy request branch) | `worker/proxyreq` | T2.5 |
| `lib/request-worker.js:1211-1233,1477-1526,2409-2414` (model cache and `/v1/models`) | `worker/modelcache` | T2.1 |
| `lib/request-worker.js:1080-1171,1272-1295` (route telemetry and request-route log) | `telemetry` | T2.8 |
| `lib/commands.js:3039-3040` (`serve-shim` and `serve-worker`) | `cli/serveshim`, `cli/serveworker` | T2.0 |
| `lib/grok-backend.js` (477) | `grok` | T1.9 (translation), T2.7 (branch) |
| `lib/codex-upstream-state.js` (101), `lib/control-auth.js` (56) | `upstreamstate`, `controlauth` | T1.12a |
| `lib/usage-observability.js` (1063) | `usage` | T1.12d (records), T2.8 (emitter) |
| `lib/commands.js:2262-2905` (supervisor) | `supervisor`, `supervisor/relay`, `supervisor/workerproc` | T2.0, T3.3, T3.4, T3.5 |
| `lib/process-supervision.js` (1216) | `procinfo`, `ownership`, `supervisor/proxyrecovery`, `launch` | T0.5, T3.1, T3.2, T3.6 |
| `lib/lifecycle-lock.js` (110), `lib/lifecycle-diagnostics.js` (61) | `lifecycle` | T1.11 |
| `lib/shim-state.js` (78) | `shimstate` | T3.6 |
| `lib/settings-wiring.js` (486), `lib/readiness-hooks.js` (53), `lib/recovery-policy.js` (38), `lib/readiness-gate.js` (65) | `wiring`, `readiness` | T1.10, T1.14, T4.6 |
| `lib/picker-guard.js` (152) | `pickerguard` | T4.4 |
| `lib/remote-control.js` (302) | not ported; only read-only detection of the Node-managed hosts block | T4.4, T5.3 |
| `lib/github-release.js` (53), `lib/release-verification.js` (30) | `release` | T4.1 |
| `lib/atomic-file.js` (65) | `atomicfile` | T0.3 |
| `hooks/registry-writer.js` (204) | `selfinstall` | T4.0, T5.1 |
| rest of `lib/commands.js` (3130 in total) | `cli` (root registration), `cli/<command>` (one package per command), `catalog`, `doctor`, `status` | T0.1 (registration), T0.8 (argument scan), T3.6, T4.1-T4.4, T4.6 |
| `lib/windows-detached.js` (84) | `launch` (`_windows.go` stub) | section 3.9 |
| new | `jsonx`, `venue`, `gatewaytest`, `migrate`, `shadow` | T0.3, T0.4, T0.4b, T5.3, T5.4 |
| new, not Go | `plugin/` (plugin files), `.claude-plugin/marketplace.json`, `test/conformance/` | T5.2, T2.0b |

### 3.3 Third-party dependencies

The direct and indirect requirements below match `go.mod`. Lanes do not change the pins with `go get`.

Direct requirements:

| Module | Version | Used for | Why not hand-written or stdlib |
|---|---|---|---|
| `github.com/spf13/cobra` | v1.10.2 | command tree, help, shell completions | 16 source commands plus extensions. For the source commands cobra's flag parsing is switched off and the source's argument scan is ported (`lib/commands.js:199-201`), because pflag's unknown-flag mode consumes a following positional argument |
| `github.com/shirou/gopsutil/v4` | v4.26.10 | macOS executable path lookup; Linux `/proc` root override keys | `procinfo` uses these helpers, not gopsutil's process or listener census |
| `github.com/dlclark/regexp2` | v1.12.0 | Retained only by the `retain_dependencies` blank import; no normal build uses it | Not used by the in-repository syntax checker (§3.1). |
| `github.com/andybalholm/brotli` | v1.3.0 | side-branch decoding of `br` bodies for usage observation; `gzip` and `deflate` use `compress/gzip` and `compress/zlib` | no brotli in the standard library |
| `golang.org/x/sync` | v0.25.0 | `singleflight` (readiness check, catalog refresh, Grok token refresh, proxy recovery), `errgroup` | |
| `golang.org/x/sys` | v0.50.0 | process control, macOS `sysctl` and rooted file operations | `atomicfile` stages and publishes files using hard links or rename relative to the open parent directory. The calls also allow no-follow opens and mode and ownership checks before publication. |
| `golang.org/x/text` | v0.44.0 | root collation for the two `localeCompare` tie-breaks (INV-04 §12) | byte order sorts `_` after digits, ICU does not |
| `github.com/google/go-cmp` | v0.7.0 | test diffs | required by the Go rules |
| `github.com/rogpeppe/go-internal` | v1.16.0 | `testscript`: txtar-driven CLI scenarios | replaces spawn-and-assert test code such as `test/wiring-mode.test.js` (1,096 lines) |
| `go.uber.org/goleak` | v1.3.0 | goroutine-leak check in relay and stream tests | the "stream left open" bug class (INV-05 §6.2) |

Indirect requirements:

| Module | Version |
|---|---|
| `github.com/ebitengine/purego` | v0.11.2 |
| `github.com/go-ole/go-ole` | v1.2.6 |
| `github.com/inconshreveable/mousetrap` | v1.1.0 |
| `github.com/lufia/plan9stats` | v0.0.0-20211012122336-39d0f177ccd0 |
| `github.com/power-devops/perfstat` | v0.0.0-20260805114148-88456608a4f6 |
| `github.com/spf13/pflag` | v1.0.9 |
| `github.com/tklauser/go-sysconf` | v0.4.0 |
| `github.com/tklauser/numcpus` | v0.12.0 |
| `github.com/yusufpapurcu/wmi` | v1.2.4 |
| `golang.org/x/net` | v0.61.0 |
| `golang.org/x/tools` | v0.52.0 |

Considered and not adopted:

| Module | Reason |
|---|---|
| `github.com/google/uuid` | go1.27.4 provides the standard package `uuid` (`New`, `NewV4`, `NewV7`, `Parse`); use it instead |
| `tmaxmax/go-sse` (v0.12.0) | default event limit 64 KiB, joins `data:` lines, drops comments and an unterminated final frame (`event.go:23-109`); the relay must forward an untouched frame byte for byte, rewrite only the first data line of a frame it changes (joining that frame's lines with LF and re-emitting the frame's own delimiter as matched) and emit the unterminated tail (`lib/request-worker.js:1668-1761`). One in-house frame scanner serves relay, peek, observation and Grok translation |
| `tidwall/gjson`, `tidwall/sjson` | first duplicate wins and invalid JSON is tolerated; the source's routing depends on `JSON.parse` (last wins, invalid rejected) |
| `santhosh-tekuri/jsonschema`, any Go schema validator as oracle | not V8; the oracle is the pinned source plus Ajv 8.20.0 run under Node in the venue (T1.5a) |
| `klauspost/compress` | standard `gzip`/`zlib` suffice until a measurement says otherwise |
| `gofrs/flock`, `go-internal/lockedfile` | the lifecycle lock stays the source's directory-rename protocol (INV-03 §6) so a legacy CLI and a Go CLI exclude each other during migration |
| OpenTelemetry SDK and OTLP exporters | they send protobuf; the source and its tests use OTLP/HTTP JSON with fixed attribute sets (INV-02 §11; INV-04 §10) |
| `cenkalti/backoff`, `sethvargo/go-retry` | two fixed, jitter-free schedules |
| `Masterminds/semver`, `x/mod/semver` | version compare is a lenient numeric-tuple compare (`hooks/registry-writer.js:19-28`); pins have their own rules (INV-04 §5.2) |
| `google/go-github` | one `GET releases/latest` |
| `cloudflare/tableflip`, `kardianos/service` | belong to options B/C or to an OS-service model |
| `creativeprojects/go-selfupdate` | only if OD-5 chooses release archives; not pinned otherwise |

### 3.4 Identity and ownership of Go processes

- **Instance** = state root (absolute, symlinks resolved) + startup generation. A pid and wall-clock birth time alone
  cannot establish process identity: the pid may be reused, and both values survive `exec`.
- A pid record `<name>.pid.json`, written atomically in the instance's state root:
  `{pid, command, startedAt, role, stateRoot, generation, port, version, image: {path, device, inode}}` (`command` and
  `startedAt` keep the source's field names; `<name>.pid` stays a decimal mirror that is rewritten from the JSON when it
  differs). The supervisor publishes its own record before it reports ready and re-publishes it on every recovery tick when
  it is missing or different; it writes the worker's and the proxy's records. A supervisor that cannot publish its record
  exits instead of serving untracked.
- Before phase 3, add a **clock-independent process-start token** to distinguish reuse of a PID. The test-first handoff,
  scheduled before phase 3 starts, specifies and verifies its per-platform representation and unreadable case. A start
  token is not proof that the process has not exec’d.
- **Authority to signal** still requires a record in this state root naming the PID, matching live birth and start-token
  evidence, matching argv role and, for a listener, a matching port. A listener on our port without such a record is not
  ours, even when it is the same binary. Resolve any change to the birth check and any need for independent image evidence
  before the signaling gate.
- **Recovery without a record.** If the record is missing or unusable but the listener accepts this state root's control
  token, the CLI asks it to stop itself with an authenticated `POST /internal/stop` (same token, Host, Origin and
  content-type rules as `/restart`). No signal is sent on port or pid evidence alone; otherwise `stop` refuses and prints
  the pid `/healthz` reports and what to check.
- Port-owner classes: `ours`, `foreign`, `legacy-node` (argv and install root satisfy `lib/process-supervision.js:510-542`),
  `unowned`, `port-owned` (bound, pid not resolvable), `unknown` (unreadable, timed out, several listeners). Automatic
  paths act only on `ours`; the others produce the source's refusal texts (`lib/process-supervision.js:598-614`).
  On Linux, `procinfo` reads the full TCP listener table from `/proc` and checks its socket holders. On macOS, `lsof`
  alone identifies one distinct visible PID, even if it holds several listening sockets. Several visible holders are
  ambiguous. A clean exit status 1 with no output is `absent`: no listener is visible to this user. Warnings or partial
  output yield `unknown`. A caller that needs the port must bind it.
- **Durable launches** start the stable binary; when none is installed (tests, a first run) the running executable is used.
  The stored launch identifier comes from the executable the gateway itself opened at launch. A worker respawn uses the
  recorded image and refuses when the file at that path is no longer the recorded image, unless `/restart` supplied an
  accepted candidate.
- **Live image evidence** is direct only on Linux when read through the kernel's own executable reference
  (`ImageEvidence.DirectFromExecutable`). Every macOS observation correlates a name with a vnode. For deletion of a
  replaced executable, relaunch or signaling, correlated evidence is `unknown` whether it matches or not. It never
  creates or updates the stored launch identifier.
- `/restart` accepts an optional candidate (field `script`, as on the wire today; `binary` is an alias) only if it is the
  supervisor's recorded image or the stable installed binary (symlinks resolved, regular file, same owner uid, not group-
  or world-writable) whose `--version` is not older; anything else gets the source's 400 body
  (`lib/commands.js:2831-2857`).
- **Replacing the stable binary** is one algorithm for `setup`, `migrate` and `self update`: hard-link the current binary
  to `model-gateway.old-<version>-<n>`, write the new file beside the target, fsync, then one `rename` over the target.
  The path hooks call is never absent, and the previous binary stays recoverable under its `.old-*` name wherever the
  process dies.
- `model-gateway.old-*` files are deleted only when every live recorded process has a known image identity and none of
  them is that file; with incomplete evidence nothing is deleted. On macOS, a replaced executable is not cleaned up
  until every recorded process that could be using it has exited.

### 3.5 JSON policies

| Where | Policy | Detail |
|---|---|---|
| Classification of a request body | parse like `JSON.parse` | the body must be exactly one JSON document (checked to EOF, also after `model` was found); duplicate names allowed, last wins. Return a refusal for a decoder depth error or an escaped lone surrogate in the final top-level `model` string; the worker returns HTTP 400 locally. Where an object member name is required, a next token that is not a string is ordinary malformed JSON at any depth and goes to the default upstream unchanged, as does other malformed JSON that does not hit the depth limit first (§3.10). |
| Anthropic passthrough | original bytes | no parse result is ever written back |
| Proxy and Grok request bodies, rewritten JSON responses and SSE event data | **wire policy**: exactly what `JSON.stringify(JSON.parse(buf.toString()))` emits | an own decoder and encoder over a tree: bytes to text as Node's `toString()` does (ill-formed UTF-8 becomes U+FFFD); strings and names held as UTF-16 code units so an escaped lone surrogate stays distinct; duplicate names collapsed on the decoded name (last value, first position); canonical array-index names below 2^32−1 first, ascending; numbers through float64 with ECMAScript number-to-string, non-finite as `null`. `jsontext` is only the tokenizer. Codex forwards an unparseable JSON response or untouched SSE frame without rewriting it (§3.10). Grok skips unparseable SSE data and translates an unparseable non-stream response from null (`lib/request-worker.js:363-377,2330-2342`). |
| Claude settings files (the user's) | **file policy**: preserve authored bytes in existing files | In an existing file, keep ordered members and raw scalars outside the gateway-owned entries being edited; use two-space indentation only for what is inserted. Before any edit, one file-wide check inspects raw duplicates of owned root and env keys, owned hook events, group `hooks` arrays and hook ownership fields (`type`, `command`, `args`). Name the owned key and leave the file untouched; unrelated duplicates are accepted. Accept one leading UTF-8 byte-order mark and retain it in edits; this does not change the wire decoder. When creating a file, write the resulting settings byte for byte as `JSON.stringify(settings, null, 2) + '\n'`, including the final newline (`lib/settings-wiring.js:107-112`). |
| Gateway state files (`pins.json`, `context-window.json`, `wired-projects.json`, pin and discovery caches, catalog) | typed values written as `JSON.stringify(v, null, 2) + "\n"` | Exceptions: the dispatch cache and both `codex-upstream-blocked.json` and `codex-upstream-unavailable.json` use compact `JSON.stringify(v) + "\n"` (`lib/request-worker.js:792`, `lib/codex-upstream-state.js:22-24,56,77`). The usage request-body high-water file uses compact `JSON.stringify(v)` without a trailing newline (`lib/usage-observability.js:202`). Match each format byte for byte. The lifecycle lock owner, intentional-stop marker and startup-failure record also use compact JSON without a final newline (`lib/lifecycle-lock.js:57-63,96-108`, `lib/commands.js:614-625`). Replace the stop marker atomically. |

Goldens: the wire corpus and the state-file corpus are generated by Node and must match byte for byte with no exception
class (inputs: duplicate keys at several depths, `9007199254740993`, `1e400`, escaped lone surrogates, ill-formed UTF-8,
`4294967294` and `4294967295` as keys). The settings corpus consists of authored before/after pairs with non-canonical
spellings (`1.00`, `a`, unusual indentation, a byte-order mark). Node rewrites the non-canonical spellings and its settings
reader rejects a leading byte-order mark.

Preliminary wiring reads, including readiness scans, follow symlinks to regular files and refuse other file types
without blocking on Unix. The settings and registry reader returns an error naming the file and the 16 MiB bound for
oversized files; do not treat them as absent or prune or rewrite the registry on that basis. `ReplaceSettingsInfo`
enforces the same bound when reading and verifying the settings target. See the bounded-snapshot tests in §3.10.

Settings writes resolve a symlinked file and replace its target; keep the existing mode. Re-evaluate ownership and edit
eligibility inside every replacement attempt against the exact snapshot being replaced, and leave hooks unchanged if the
edit is no longer eligible (`internal/wiring.TestReplaceInterleavingsReevaluateOwnership`). `atomicfile.ReplaceSettingsInfo`
supplies the opened file's identity and existence with its bytes: an existing empty object is not a new file. Tests:
`internal/wiring.TestCreatedSettingsBytes`, `internal/wiring.TestExistingSettingsKeepTrailingBytes`,
`internal/wiring.TestProductionAdapterExistenceFollowsOpenedSnapshot`, `internal/atomicfile.TestReplaceSettingsInfoExistenceAndIdentity`
and `internal/atomicfile.TestReplaceSettingsInfoInterleavings`.

Settings edit errors do not echo authored content: report the file, byte offset and fixed reason or known owned key,
not an input JSON pointer or offending token. Skip diagnostics may include a numeric window value but never a
nonnumeric one. Structured diagnostic values preserve wire member order (`internal/wiring.TestErrorTextNeverContainsAuthoredContent`,
`internal/wiring.TestDiagnosticValuesKeepWireMemberOrderAndAvoidRescan`).

Re-read just before the rename and, if the file changed since it was read, merge again (three attempts, then fail with
the file name); refuse a target that is not a regular file. An editor that writes between the last re-read and the
rename can still lose its edit. The window is smaller than the source's in-place write but not zero, and migration
therefore requires a maintenance window (section 3.6). The control token keeps its create-exclusive write
(`lib/control-auth.js:23-33`).

### 3.6 Migration from the Node gateway (`migrate`)

Migration is an **offline, operator-run** operation. The two runtimes are never expected to coordinate: the source's lock
does not cover every legacy writer (a disabled `ensure` returns before the lock, `lib/commands.js:577-585`; `env`, `pin`
and the SessionStart registry writer never take it, `hooks/registry-writer.js:145-156`), so exclusivity comes from a
maintenance window, not from a fence. `migrate` never signals or starts a Node process and never touches the proxy binary.
On a home with a legacy install and no committed migration, `setup` and the SessionStart publisher refuse to act and
say "run migrate". Lead ruling, 2026-11-26: the Go `env --write-project`, `env --write-user`, `env --remove` and
`env --reconcile` operations likewise refuse before any write and say "run migrate". A Go settings write can install
exec-form hooks for a binary that migration has not yet installed at the stable path.

The operator's part, in the runbook with complete commands (T6.2): close every Claude Code session and perform both
legacy checks below. Apply the remedies before the final Node stop. Disable the old plugin
`model-gateway@eigenwise-toolshed` (disabled, not uninstalled: with a wired endpoint a disabled plugin's Node gate exits 0,
`lib/recovery-policy.js:27-32`, while a missing install exits 2, `hooks/registry-writer.js:85-105`); stop the Node gateway
with its own CLI. Run `claude-code-model-gateway migrate --start` or `migrate --keep-stopped`. The end state is chosen by
that flag, never inferred from the stop marker that the Node `stop` just wrote.

**Legacy compatibility checks.** Check the Node-managed hosts block and each live or shadowed definition of
`ANTHROPIC_BASE_URL` independently. Inspect the process environment, user settings, the current project's local and
shared settings, and registered project settings. Report which of the four cases holds: hosts block only, legacy URL
only, both, or neither. A finding must identify the affected file or process environment and give its own remedy:

- Hosts block: run the Node version's `remote-control disable --confirm` or remove the block by hand. The Node
  command can start the gateway (`lib/remote-control.js:238-245`), so run it before the final Node stop. If the block
  is already absent, Node can return without repairing settings (`lib/remote-control.js:188-190`). The Go port never
  writes the hosts file.
- Legacy URL `http://api.anthropic.com`: use the existing `env --write-project` or `env --write-user` operation for
  the appropriate scope to replace the legacy value with the configured default loopback URL, or `env --remove` to
  clear owned wiring. Before migration, apply that remedy with the Node CLI once the hosts block is gone; the Go CLI
  can apply it after `migrate` has committed. Report shadowed definitions separately; changing the effective file alone
  does not clear them. For a process definition, tell the operator to unset or correct it in their shell before
  relaunching. The port never edits the shell environment.

The legacy URL is an owned value for migration and removal only. Recognizing it is not proof of a serving gateway.
Only explicit writes switch wiring to the default endpoint. They remove a retired `CLAUDE_CODE_MAX_CONTEXT_TOKENS`
value of `920000` (number or string) but leave personal values (`internal/wiring.TestExplicitWriteRemovesRetiredMaxContextTokens`).
Explicit project removal clears owned entries in both local and shared project settings, since automatic migration leaves
legacy URL entries in the shared file. If the direct project target aliases user settings through a symbolic or hard link,
refuse and name the target; if only the secondary shared target aliases user settings, skip it and succeed. Clean
hard-linked local and shared project files independently. Follow a project symlink to any other regular file as in the
source (`internal/wiring.TestProjectRemovalProtectsUserSettings`, `internal/wiring.TestProjectRemovalHardAndSymlinkedProjectFiles`,
`internal/wiring.TestProjectRemovalClearsBothProjectFilesOnly`). An explicit removal deletes a present empty `env`
in the targeted file without creating one (`internal/wiring.TestExplicitRemovalDeletesPresentEmptyEnvObject`). The socket
setting is removed by key on explicit writes, by owned value on explicit removal, and never touched by automatic paths
(`unix-socket-removal-by-value`, §3.10).

Reconcile accepts only the configured default URL as its target. `env --write-user` lists conflicting project entries
without changing their files; `env --write-user --reconcile` confirms removal of those owned entries
(`internal/wiring.TestReconcileRegisteredProjects`, `internal/wiring.TestReconcileRejectsRetiredCompatTarget`). Automatic
wiring paths never change the effective base URL. A move of owned entries from shared to local settings proceeds only if
both process-effective and persisted file-backed URLs stay unchanged; otherwise leave the settings alone
(`internal/wiring.TestAutomaticMigrationPreservesEffectiveBaseURL`). Re-read the source during both replacements.
Before copying, require the same opened source file and identical bytes. Before deleting from the source, require the
same opened file and delete only copied entries whose original values still match. A declined migration stops
`CleanLegacyEnv` before any further edits. A copy already committed is not rolled back if source deletion declines
(`internal/wiring.TestMigrationStaleSourceInterleavings`, `internal/wiring.TestProductionAdapterMigrationStaleSourceAndGrowthBound`).
No automatic caller, including hooks, `setup`, `ensure` or `readiness-gate`, switches endpoints. `migrate` itself
rewrites only hook entries in settings; its compatibility checks are read-only, with endpoint edits done separately by
the operator.

| Step | Action | On failure |
|---|---|---|
| 0 Preconditions | Lead ruling, 2026-11-26: `migrate` runs both legacy compatibility checks read-only and prints each finding with its remedy. A hosts-block finding is a refusal that modifies nothing: after start, the gateway would resolve `api.anthropic.com` to loopback for its own upstream requests. A legacy-URL finding is reported and does not refuse. Nothing listens on the gateway port and no recorded legacy guardian or worker pid is alive; the old plugin is `false` in `enabledPlugins` of the user settings and of the current project's two settings files; no `claude` process of this user is visible (`--sessions-closed` attests what cannot be seen); no journal is in progress. A rerun with the same end-state flag resumes its own journal, and the journal's stage then decides what is checked: once a start intent is journaled (step 4), a listener on the gateway port is accepted when its pid record in the state root and its `/healthz` carry the generation of that intent, whether or not the result was journaled, and refused otherwise | refuse and name the unmet condition; nothing was modified |
| 1 Journal | create `migration/<timestamp>/` (mode 0700) with protected pre-images of everything later steps touch: both launchers, the breadcrumb, each settings file (bytes, mode, symlink target, or "absent"), the stop marker (bytes or "absent"), and the stable binary when one exists (a hard link in the journal directory). Every later step appends its intent, then its result with pre- and post-digests | abort; nothing was modified |
| 2 Install | the stable binary, by the replacement algorithm of section 3.4 (one rename; the path hooks call is never absent) | abort; the previous binary is still in place |
| 3 Switch publishers | write the two trampolines; write the breadcrumb unless an existing one has a newer schema (as the source, `hooks/registry-writer.js:158-197`); rewrite readiness hooks to the exec form in the user settings and every registered project, owned entries only | rollback |
| 4 End state | `--start`: remove the stop marker; choose a generation id and journal it as the start intent; start the Go supervisor with that id; require `/healthz` reporting it; journal the result. `--keep-stopped`: keep or write the marker, require that nothing listens | rollback |
| 5 Commit | mark the journal `committed`. The Go plugin's SessionStart publisher rewrites launchers only when a committed journal exists, so it is inert before commit and after rollback | — |

`migrate --rollback` stops the Go supervisor it started (authenticated stop) and then, per journaled file: if the current
bytes equal the post-image, restore the pre-image exactly (or delete a file that was absent); else if only gateway-owned
entries differ from the post-image, restore those entries and keep the other edits; else leave the file and report the
conflict. The stop marker and the stable binary are restored the same way. Rollback therefore returns the home to the
state in which `migrate` found it, which is "stopped by the operator": the Node `stop` of the runbook wrote the marker
before `migrate` ran (`lib/commands.js:2951-2953`), so the marker cannot say whether the gateway was serving before the
maintenance window, and rollback does not guess. It starts nothing, marks the journal `rolled-back`, and prints the
complete commands that remain for the operator: re-enable the old plugin, and start the Node gateway with its CLI if it
is to serve again. Forward and rollback are idempotent: `migrate` with the same end-state flag, or `--rollback`, resumes
from the journal after a crash at any step; the other end-state flag is refused while a journal is in progress.

`doctor` reports: a launcher that is not the trampoline, a `legacy-node` listener, legacy hook strings in the user
settings, the current project and registered projects (it says that unregistered projects cannot be checked), and a
journal that is neither `committed` nor `rolled-back`. It performs both legacy compatibility checks without writes.
It exits 1 if either finding is present, or if any of the source's six failure conditions holds, including a mismatch
between the effective and shadowed gateway definitions (INV-01 §3.6). With neither legacy finding, that check adds no
failure; the other diagnostics still apply.

### 3.7 Shadow mode

`MODEL_GATEWAY_SHADOW=1` with `MODEL_GATEWAY_STATE_DIR` and non-default gateway ports starts a second instance for the
soak. The only resource of the default instance it may touch is a **dial** to `127.0.0.1:<configured proxy port>`, which
may be the live proxy. In shadow mode the gateway never binds a default port; never spawns, signals, adopts or upgrades a
proxy and never runs the proxy binary for an auth check (readiness comes from HTTP only, auth is reported `unknown`);
never refreshes Grok credentials; writes nothing outside its state directory; refuses
every command except `serve-shim`, `serve-worker`, `status`, `doctor`, `stop`, `shadow preflight` and `shadow report`. The
preflight runs at every start: it resolves symlinks and lists each path the instance will write and each address it will
bind or dial, and refuses when any of them, other than the proxy dial, belongs to the default instance.

`logs/soak.jsonl` (metadata only, append-only, not truncated during a run) holds two kinds of record, each with run id,
supervisor generation, sequence number and time. **Request records**: session and agent keys (HMAC with a per-run key),
backend, model, phase (`ordinary` or `compaction`), status, outcome (`ok`, `upstream-error-relayed`, `client-abort`,
`sentry-413`, `gateway-fault`), the response's stop reason, observed input and output tokens, the sentry's inputs and
result for the request (its session and model key, each usage observation `u` with the completed-turn flag as the
sentry recorded them, whether a genuine upstream overflow was seen, the effective trigger, the state before and after:
`absent`, `armed-low`, `armed-high`, `fired`, and whether the request was refused; INV-02 §5.3), compaction outcome, the
ordered worker generations the request was delivered to, and each upstream submission with its cause (`first`, or a
retry cause from the closed list that T5.4 takes from INV-02 §4 and §8 and INV-03 §4: at least `upgrade-retry`,
`compaction-retry`, `relay-resend`). **Lifecycle records**: `worker-start` and `worker-exit` with the worker generation
and the exit cause, `stop-request`, `restart-request`, `supervisor-exit`. `shadow report` evaluates the checks of AC-10
from this file alone, because the bounded `lifecycle.jsonl` loses history (INV-03 §7), and is tested with files that
satisfy and violate each check.

### 3.8 Test isolation

The boundary is the venue: no test of this module runs on a machine that has a gateway. Guards and digests are further
layers; they detect, they do not contain.

**Venue.** Provision only the approved current account on `debian-13-trixie.gaudiy-platform` or an identified hosted CI
runner (`ubuntu-26.04` or `xcode-27`). `hack/remote provision` rejects the developer machine and local destinations before
provisioning. On the destination, check the account's home as given by the password database, not `$HOME`, for a
`.claude/model-gateway` directory, and both ports (18764 and 18765) for listeners without contacting a gateway. If state or
any listener is present, or a check is unknown, stop and report without writing anything or running tests. Tests require
a regular `.model-gateway-test-venue` file in that home on every runner; an environment variable alone is insufficient.
The hosted CI job runs `hack/venue-attest-ci` before its test steps to check the runner and create the attestation.
The same venue checks then apply to CI as to the Linux venue.

The rule is enforced in two places, so that neither a make target nor a direct `go test` runs tests elsewhere:

| Where | Enforcement |
|---|---|
| the make targets `test` and `test-json` (Tier 1: every package, without the `process` tag), `test-process` and `test-process-json` (Tier 2: tests that start a process, signal, bind a fixed port or exercise migration, `//go:build process`), `conformance`, `baseline-node`, `bench`, and every Node run | each target runs `hack/venue-check` first and exits non-zero with "not a test venue"; in a venue they run through `hack/remote` (copies the tree to `/tmp` on the Linux host, installs Go and Node there, runs the target, returns its log) or through the CI workflow. The Linux venue also needs `lsof` for the pinned suite's port-owner checks; `hack/baseline-node` refuses to run if a tool the suite spawns is missing |
| every test binary | each package's `TestMain` calls `venue.Require()` as its first statement; it applies the same venue predicate and exits non-zero before any test function runs. `hack/lint-venue` checks this for the default and for the `process` build tag and fails when a package with test files lacks it. Package initialization runs before `TestMain`, so `gochecknoinits` is enabled and package-level variables are initialized from literals or pure functions (a review rule) |

`hack/remote negative-control` uses a gateway-free, non-CI Linux account reserved exclusively for the control. While
attested, build a fresh fixture for each tag set (default and `process`). Invoke each binary directly through the remote
control harness, not the make preflight; require exit zero and creation of its per-run sentinel. Remove the sentinel,
withdraw attestation, and require the same binary to exit with the guard's expected status and exact diagnostic
("not a test venue") and leave no sentinel. Restore the prior attestation on exit, recheck the venue before normal tests,
and require reprovisioning after interrupted cleanup. Exercise each protected make target's refusal separately on this
disposable host and verify that no build, test or Node command ran. On the developer Mac run only the standalone shell
preflight; do not dispatch a test target, even to prove it is safe.

There is no local test target. Local test execution may be added later only behind an operating-system sandbox
(`sandbox-exec`) that has passed a proof with positive cases (allowed write, bind, dial and child signal succeed) and
negative cases (write outside the work directory; bind and dial of 18764, 18765 and 80 over IPv4 and IPv6; signal to an
outside process of the same user and to its process group). No gate depends on it, and it is not part of phases 0 and 1.

Further layers: `gatewaytest.NewEnv` reproduces the source's forced environment block (INV-05 §1.4) and clears inherited
`MODEL_GATEWAY_*` variables. With `MODEL_GATEWAY_TEST_SANDBOX` or `NODE_TEST_CONTEXT` present, `gatewaytest` refuses
to write settings outside the sandbox or bind any of the three protected ports. A `claude` launch requires an explicit
absolute executable path inside the sandbox; the source only checks for a nonempty override under its test runner
(`lib/pins.js:212-214`). A probe endpoint must be HTTP with a literal loopback address and an explicit nonzero port
other than the protected ports. That check covers only the first authority passed to the child, not its later traffic.
`RunFixture` accepts a sandbox executable or a named helper of the test binary, validates the launch, and applies a
five-second default deadline. The consuming package must define exactly one process-tagged `TestGatewayFixtureHelper`
that calls `DispatchHelper`; this is a caller contract, not a launcher check. Assert helper-produced output or a file,
not exit status alone. `RunFixtureWithin` allows a positive limit of at most 30 seconds; an earlier caller deadline or
cancellation still applies. A prepared named helper cannot be started directly. The launcher passes exactly the
supplied environment on Linux and macOS; Windows is compile-only.

Usage reporting uses the first nonempty value of `CODEX_GATEWAY_USAGE_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`
and `OTEL_EXPORTER_OTLP_ENDPOINT` (with `/v1/logs` appended to the last), or defaults to `http://127.0.0.1:4318/v1/logs`.
`CODEX_GATEWAY_USAGE_ENDPOINT=0` disables reporting (`lib/usage-observability.js:10,620-630,1013`; `docs/env.md:75,93-94`).
`NewEnv` sets that disabled value by default and removes `OTEL_EXPORTER_OTLP*` variables, including overrides.
A test that needs reporting starts its own loopback collector, sets its URL in the environment, and launches through
that collector, which records requests without forwarding and closes at test cleanup. Ordinary launches require
reporting to be disabled; only the registered zero-entry legacy helper may omit the setting. These checks do not
restrict later outbound traffic; the venue is still the boundary.

Before the first task of each phase the lead captures a read-only digest manifest of the approved
settings files and pid records on the developer machine (path, absent/present, SHA-256, size; no contents, token or
credential files). Compare the same paths by the same rules at the gate; use the committed `hack/tripwire` from phase 1.
For phase 0 the lead captured `.omc/artifacts/tripwire/phase-0-baseline.tsv` by hand on 2026-11-26 00:22:52 JST, after
T0.0 and T0.1 had started. Gate reports must mark the interval before capture unverified, never backdate a baseline.

`go build`, `go vet`, `golangci-lint` and the formatters execute no product code and run anywhere. The built binary is
not executed on the developer machine. Lanes never call `go test` or Node directly; they use the make targets.

A design rule keeps most tests free of processes: packages take process identity, clock and network access through
interfaces that `procinfo` and the commands implement (the lifecycle lock decides staleness with a pure function over an
injected identity source), so their tests need no `process` tag.

The macOS process-identity tests (T0.5) ran on the hosted `xcode-27` runner in phase 0 (OD-8). A build on the developer
machine is not evidence for a Darwin process test; the phase-3 gate requires the macOS venue results.

**macOS Node baseline.** The pinned Node baseline runs on hosted `xcode-27` through a pushed phase branch by the phase-3
gate (T3.9, OD-19). The runner is discarded after the job. Before upload the benchmark sweeps for its own marked
processes and records the result as “containment by runner teardown and marker sweep; absence of unmarked detached
descendants not proven”.

Linux `hack/bench` publishes evidence only after its child subreaper proves no detached descendant survives. It reads
`/proc/thread-self/children` to enumerate children and requires `waitpid` to return `ECHILD` (`hack/bench:46-63,87-151,725-729`).
The pinned identity probe starts detached children (`lib/process-supervision.js:456`), and the interruption control uses
an unmarked descendant in another process group (`docs/port/baseline-node.md:229-248`). macOS reparents orphans to
`launchd` and has no caller-selected subreaper; a process-group kill or `ps` sweep cannot provide the Linux proof. The
Linux run records every observation required by T0.6; the performance criterion compares both runtimes in one job at the
phase-4 gate. The macOS baseline waits for the phase-3 process layer to design and test detached-descendant cleanup.

### 3.9 Windows

Out of the first milestone (OD-2). OS differences live in build-tagged files; the Windows files return a "not supported on
Windows yet" error for detached launch, tree kill, descriptor passing, the named pipe and atomic writes. CI builds
`GOOS=windows GOARCH=amd64 go build ./...`. The later Windows phase needs: launch outside the caller's Job Object
(`lib/windows-detached.js`), `taskkill /T /F`, `github.com/Microsoft/go-winio`, an alternative to `ExtraFiles`, `.zip`
extraction, PowerShell hook shell (INV-03 §8).

### 3.10 Differences from the source

Everything not listed here is ported as it is, including behavior the inventories flag as questionable. Declare the
source tests each difference changes before any run. OD-18 records the user's approval of the additional rows present
when that decision was made and the launcher-guidance coverage. Later decisions and lead rulings are attributed in
this table. T5.6 creates `docs/differences-from-node.md` after the differences are tested; until then, this plan holds
the table.

The checker reads the last column: each backquoted `package.Test` or `package.Test/subtest` is followed by `(phase-N)`,
the gate that requires it to run and pass. Separate entries with `<br>`. The preceding column names the same tests and
says what they cover. A sentence starting with **Test pending:** names a required test that must be written and pass
before the listed gate can pass; tests named before it in the same cell exist.

| # | slug | Source behavior | Port | Reason | Evidence at the pin (test paths relative to the plugin) | Required Go replacement test | Replacement tests (phase) |
|---|---|---|---|---|---|---|---|
| D1 | settings-atomic-replacement | User settings, pin overrides and the wiring registry are written in place. | Replace atomically; preserve the settings target's mode and symlink and re-read per §3.5. | A crash can truncate an in-place write. | No test requires in-place writes. Implementation: `lib/settings-wiring.js:111`, `lib/settings-wiring.js:203`, `lib/pins.js:358`; `test/atomic-file.test.js:41` tests the existing atomic helper, not the settings writer. | `internal/atomicfile.TestSettingsReplacement` (settings crash point, symlink target, mode and concurrent edits); `internal/pins.TestAtomicPinOverrides` and `internal/wiring.TestAtomicProjectRegistry` (interrupted replacement of `pins.json` and `wired-projects.json` leaves a complete old or new file). | `internal/atomicfile.TestSettingsReplacement` (phase-0)<br>`internal/pins.TestAtomicPinOverrides` (phase-4)<br>`internal/wiring.TestAtomicProjectRegistry` (phase-1) |
| D2 | readiness-hook-exec-form | The command string invokes the Node launcher. | Emit the absolute binary and separate `args` for `readiness-gate`, timeout 35, SessionStart matcher `startup\|resume`, no `onFailure`; recognize both forms as owned, preserve other arguments/hooks/groups, honor `disableAllHooks`. | Removes Node from the per-prompt path. | `test/wiring-mode.test.js:886` (assertion at `test/wiring-mode.test.js:898`), `test/readiness-gate.test.js:210`; `test/readiness-gate.test.js:32` and `test/readiness-gate.test.js:45` pin unchanged timeout, ownership and host-failure policy, not the literal command. | `internal/wiring.TestReadinessHookForms` | `internal/wiring.TestReadinessHookForms` (phase-1) |
| D3 | rc-control-requires-token | The RC endpoint uses a fixed header as its control credential. | Withdrawn on the user's decision of 2026-11-26; there is no RC endpoint. | See `remote-control-not-ported`. | `test/rc-compat-mode.test.js:876`, `test/shim-upload-abort.test.js:181` (RC expansion only). | `internal/cli.TestRemoteControlIsUnknownCommand` pins the replacement CLI behavior; `/internal/rc-compatibility` is no longer a control endpoint. | `internal/cli.TestRemoteControlIsUnknownCommand` (phase-1) |
| D4 | recovery-recorded-proxy-adoption | An unhealthy shared proxy is replaced only if descended from this supervisor. | Also allow adoption with a live listener and a record in this state root satisfying §3.4, including a matching start token; never in shadow mode or on health evidence alone. | Recovery otherwise waits for manual intervention. | No source test supplies an adoption record for a non-descendant. The foreign shared-executable case remains refused (`test/gateway-process-isolation.test.js:1720`). The original recovery predicate requires descent (`lib/process-supervision.js:1040`). | `internal/supervisor/proxyrecovery.TestRecordedProxyAdoption`: give a non-descendant listener a record satisfying §3.4 in this state root and confirm unsuccessful probes cause recovery. Also refuse an otherwise matching record whose start token is missing or different. Without that record, with a stale or ambiguous record, or in shadow mode, assert that no replacement occurs. | `internal/supervisor/proxyrecovery.TestRecordedProxyAdoption` (phase-3) |
| D5 | worker-crash-backoff | A crashed worker is respawned after 50 ms without backoff. | Quick exits within 10 s double the delay from 50 ms to 5 s; 60 s of uptime resets it. Exclude drain exits. Missing the 5 s port-report deadline remains fatal (exit 1); a backoff beyond the relay window (one try plus 80 waits of 50 ms) gives retryable 503. | Bounds the restart rate during repeated failures. | `test/gateway-drain.test.js:329` requires recovery, not its delay. No timing test; fixed interval in `lib/commands.js:2653`. | `internal/supervisor/workerproc.TestCrashBackoff` (fake clock, drain, reset, port deadline, relay window) | `internal/supervisor/workerproc.TestCrashBackoff` (phase-3) |
| D6 | worker-exit-retires-birth-record | A current-version worker's observed exit does not remove its birth record. | Delete that worker's birth record on observed exit; preserve orphan rules and newer or unknown births. | Otherwise each worker leaves a record. | No test requires the leak; `lib/commands.js:2652` removes the pid file, not the birth record. Retain `test/owned-worker-cleanup.test.js:135`, `test/owned-worker-cleanup.test.js:149`, `test/owned-worker-cleanup.test.js:159`. | `internal/supervisor/workerproc.TestBirthRecordOnExit` (observed exit, replacement birth, lost parent) | `internal/supervisor/workerproc.TestBirthRecordOnExit` (phase-3) |
| D7 | readiness-event-deadlines | Launcher stdin has no deadline; the launcher kills its child after 25 s and exits 2 on launcher errors regardless of event. | Bound stdin at 2 s (then treat as empty); start one 25 s gate budget after reading. On exhaustion name the phase and exit 2 for UserPromptSubmit or 1 for SessionStart. | The Go gate has no launcher child to kill. | `test/readiness-gate.test.js:169`; stdin and launcher error branches in `hooks/registry-writer.js:75`, `hooks/registry-writer.js:106`. | `internal/readiness.TestGateDeadlines` (blocked stdin, phase exhaustion, both events) | `internal/readiness.TestGateDeadlines` (phase-4) |
| D8 | settings-preserve-authored-json | Settings serialization orders integer-like keys first and normalizes escapes and numbers. The settings reader passes input directly to `JSON.parse`, which rejects a leading UTF-8 byte-order mark. | Apply §3.5 file policy only to Claude settings: edit only named members of an existing document, preserve its leading byte-order mark, and perform the file-wide raw duplicate check before edits. | Preserve authored bytes outside edited entries. | `lib/settings-wiring.js:76-83,107-112`; `test/wiring-mode.test.js:886` checks preservation on a no-op, not the spelling of rewritten scalars. | `internal/jsonx.TestAuthoredSettingsCorpus`; `internal/wiring.TestBOMPrefixedSettingsEndToEnd` (resolution and explicit edits retain the mark); `internal/wiring.TestDuplicateOwnedKeysRefusedByteIdentical` (raw duplicates fail without changing the file); `internal/wiring.TestFileWideDuplicateOwnedKeysAllOperations` (owned root, env and hook paths checked across operations; unrelated duplicates allowed). | `internal/jsonx.TestAuthoredSettingsCorpus` (phase-0)<br>`internal/wiring.TestBOMPrefixedSettingsEndToEnd` (phase-1)<br>`internal/wiring.TestDuplicateOwnedKeysRefusedByteIdentical` (phase-1)<br>`internal/wiring.TestFileWideDuplicateOwnedKeysAllOperations` (phase-1) |
| D9 | stop-order-supervisor-first | Stop recorded worker, supervisor, then proxy; the supervisor can respawn the worker during stop. | Stop the supervisor first, then any remaining recorded worker, then the proxy. | Prevents respawn during deliberate stop. | No test requires this order: `lib/process-supervision.js:851`; `test/owned-worker-cleanup.test.js:322` pins the guardian/shim/proxy order of the separate `reapGatewayOrphans` sweep (`test/owned-worker-cleanup.test.js:328`), not the order of `stopAll`. | `internal/launch.TestStopPreventsWorkerRespawn`: observe the controller’s termination requests from entry to `stop`, not the later supervisor log event (`lib/commands.js:2561`); require supervisor first, then remaining worker, then proxy. Include a worker exiting during stop; no replacement worker may start from controller entry onward. | `internal/launch.TestStopPreventsWorkerRespawn` (phase-3) |
| D10 | ownership-requires-instance-record | An install-root argv can establish ownership without a record; a matching birth record can establish it with hidden argv. | Require a matching record in this state root per §3.4; supervisor publishes its own record. Recover a lost record by token-authenticated `/internal/stop`, not by signaling on pid evidence. | Distinguish instances of the same executable. | `test/gateway-process-isolation.test.js:367`, `test/gateway-process-isolation.test.js:392`, `test/gateway-process-isolation.test.js:422`, `test/gateway-process-isolation.test.js:1693` (the shared proxy is a descendant but has no instance record); `test/gateway-ps-locale.test.js:106` and the expansions of `test/gateway-ps-width.test.js:183` also rely on argv ownership; locale/width-independent probing remains required. | `internal/ownership.TestInstanceRecordAuthority` (two instances, missing/stale/partial record, authenticated stop) | `internal/ownership.TestInstanceRecordAuthority` (phase-3) |
| D11 | restart-recorded-or-stable-image | Restart reads only `script` (`lib/commands.js:2845`) and accepts a non-older sibling in the same plugin cache. | Accept the recorded image or validated stable binary through `script` or its `binary` alias (§3.4). | There is no versioned Go binary plugin cache. | `test/gateway-drain.test.js:116`, `test/gateway-drain.test.js:508`, `test/gateway-drain.test.js:541`, `test/gateway-drain.test.js:706`, `test/control-auth.test.js:49`, `test/gateway-process-isolation.test.js:1562`; older/foreign candidates stay refused (`test/gateway-drain.test.js:460`, `test/gateway-drain.test.js:562`). | `internal/supervisor/workerproc.TestRestartCandidate`: run both acceptance and refusal through each field spelling (`script`, `binary`), covering recorded image, validated newer stable image, older image, foreign path and symlink. | `internal/supervisor/workerproc.TestRestartCandidate` (phase-3) |
| D12 | linux-listener-proc-fallback | The async POSIX owner probe has no `/proc` fallback. | Read the Linux TCP listener tables and resolve their holders from `/proc`, without `lsof`. | Start/stop otherwise refuse with unknown ownership. | No test requires the missing fallback: `lib/process-supervision.js:476`; `test/shim-lifecycle.test.js:430` tests the **synchronous** lookup. | `internal/procinfo.TestLinuxListenerWithoutLsof` (identifiable, inaccessible, ambiguous) | `internal/procinfo.TestLinuxListenerWithoutLsof` (phase-0) |
| D13 | launcher-stable-binary | Launcher selects the newest registered install on each call; recovery checks `model-gateway@eigenwise-toolshed`. Codex credential-block guidance (no `expiresAt`), local-engine restart hints, and readiness setup, login and ensure advice invoke a Node launcher. Expiring 429 guidance contains no setup command and is unchanged. | Invoke one stable binary and quote its path without `node` in these messages; recovery checks `model-gateway@claude-code-model-gateway` (§3.11). | Compiled binaries are not installed in the plugin cache. Lead ruling, 2026-11-26: the restart and readiness advice must name the same executable. | The literal Node invocation changes in `test/codex-upstream-cases.test.js:88` (assertion 116), `test/context-window.test.js:882` (907) and `test/gateway-drain.test.js:733` (775), not their upstream-status or version-diagnostic behavior. `test/readiness-gate.test.js:121`, `test/readiness-gate.test.js:145`, `test/readiness-gate.test.js:155`; `test/stable-command-path.test.js:44`, `test/stable-command-path.test.js:50`, `test/stable-command-path.test.js:55`, `test/stable-command-path.test.js:63`, `test/stable-command-path.test.js:74`; active launcher selection in `test/proxy-upgrade.test.js:116`, `test/proxy-upgrade.test.js:186`, `test/proxy-upgrade.test.js:223`; `test/plugin-registry.test.js:92` pins the unchanged registry reader. `hooks/ensure.js` is dormant (§1.3) and its tests are not affected launcher cases. Credential-block and rate-limit branches: `lib/runtime.js:553-565`, `test/codex-upstream-cases.test.js:113-116`. Additional hints: `lib/commands.js:1717-1728,1754-1769,1793-1797`, `lib/runtime.js:569-573`; `test/lmstudio.test.js:380` (assertion `408:5`, fixture `:390`), `test/local-llama.test.js:230` (assertion `249:5`, fixture `:236`), and `test/proxy-providers.test.js:401-421` (assertion `415:5`). | `internal/upstreamstate.TestReadinessGuidance/QuotedStableLauncher` (401 credential-block guidance, not expiring 429 guidance); `internal/selfinstall.TestStableCommandPath` (with the stable binary absent the resolver reports it missing and falls back to no other file; with it present the resolver returns that path; the printed advice names the stable binary and no interpreter); `internal/selfinstall.TestStableLauncher` (change the recorded plugin installation; prove the launcher still calls the same stable executable, forwards arguments and propagates status; check missing binary); `internal/readiness.TestPluginEnablement` (disabled id, absent id). Test pending: `internal/localprov.TestStudioReadiness/non_ready` and `internal/localprov.TestLlamaReadiness/non_ready` check the local-engine restart hints; `internal/readiness.TestProxyProviderReadiness/shared_checks` and `internal/readiness.TestHealthzCodexReadiness/readiness_messages` check setup, login and ensure advice. | `internal/upstreamstate.TestReadinessGuidance/QuotedStableLauncher` (phase-1)<br>`internal/localprov.TestStudioReadiness/non_ready` (phase-2)<br>`internal/localprov.TestLlamaReadiness/non_ready` (phase-2)<br>`internal/readiness.TestProxyProviderReadiness/shared_checks` (phase-2)<br>`internal/readiness.TestHealthzCodexReadiness/readiness_messages` (phase-2)<br>`internal/selfinstall.TestStableCommandPath` (phase-4)<br>`internal/readiness.TestPluginEnablement` (phase-4)<br>`internal/selfinstall.TestStableLauncher` (phase-5) |
| D14 | unix-socket-removal-by-value | Explicit `writeEnv` deletes `env.ANTHROPIC_UNIX_SOCKET` unconditionally by key on both write and remove. | Explicit writes still delete the entry by key so it cannot override the written gateway URL. Explicit removal deletes it only when its value matches a gateway-written socket path (`Paths.SocketPath()`, `<gatewayHome>/gateway.sock`, `<stateDir>/gateway.sock`, `CODEX_GATEWAY_SOCKET_PATH`, or `\\.\pipe\model-gateway`). Preserve any other value silently. Automatic paths never touch the entry. | Lead ruling, 2026-11-26: the port never writes this settings key; removing gateway wiring must not discard a user's socket setting. | `lib/commands.js:1191` (unconditional deletion), `lib/runtime.js:16,47-49` (`STATE` and `SOCKET_PATH`), `lib/request-worker.js:2698-2700` (listener), `test/rc-compat-mode.test.js:681-697` (explicit write deletes `retired-socket-value` at `:690`; removal asserts the key is still absent at `:697`). | `internal/wiring.TestWriteEnvironment` (explicit writes delete by key; removal deletes a gateway-owned path) and `internal/wiring.TestUnixSocketRemovalByValueAndWriteByKey` (personal values preserved, owned paths removed, explicit writes delete by key, automatic paths unchanged). | `internal/wiring.TestWriteEnvironment` (phase-1)<br>`internal/wiring.TestUnixSocketRemovalByValueAndWriteByKey` (phase-1) |
| — | setup-legacy-home-requires-migration | Setup updates an existing installation without a separate migration. | On a home with a legacy install, refuse setup and direct the operator to `migrate` (§3.6). | Changes to legacy settings and launchers require an offline migration. | `lib/commands.js:439`; `test/readiness-gate.test.js:226` exercises maintenance setup in an existing home, not Go/Node coexistence. No source case for two runtimes. | `internal/cli/setup.TestSetupRefusesLegacyHome` | `internal/cli/setup.TestSetupRefusesLegacyHome` (phase-4) |
| — | publisher-legacy-home-requires-migration | SessionStart publishes the stable launchers without checking a migration journal. | Publish on a fresh home or after committed migration. On a legacy home before commit or after rollback, refuse without writes and direct the user to run `migrate` (§§3.6, 3.11). | Prevent concurrent publishers from overwriting the legacy entry points. | `hooks/registry-writer.js:145`, `hooks/registry-writer.js:151`; `test/wiring-mode.test.js:994` exercises the published updater. No source test for a migration journal. | `internal/selfinstall.TestPublisherMigrationState` (refusal and `run migrate` direction; unchanged launchers, settings and breadcrumb; publication permitted after commit). | `internal/selfinstall.TestPublisherMigrationState` (phase-5) |
| — | lifecycle-release-before-cli-exit | Fatal CLI errors can exit from within the lifecycle operation, before its `finally` runs. | Release the held lock before returning the exit code from main (§3.1). | A normal command refusal must not leave a stale lock for the next caller to reclaim. | `lib/commands.js:241`, `lib/lifecycle-lock.js:88`; `test/lifecycle-lock.test.js:61` pins release on a thrown error, not on process exit. | `internal/cli.TestFatalCommandReleasesLock` | `internal/cli.TestFatalCommandReleasesLock` (phase-3) |
| — | worker-generation-report-pipes | Node IPC carries the worker listening report and parent disconnect. | In supervised Go mode use separate report and liveness pipes with a bounded, generation-checked report (§3.1); preconfigured ports do not need a report. | Reject stale reports and preserve parent-loss signaling without depending on Node IPC. | IPC fork and receive: `lib/commands.js:2619-2622`, `lib/commands.js:2632-2638`; sender: `lib/request-worker.js:2692`; parent disconnect: `lib/request-worker.js:1210`. `test/gateway-drain.test.js:329` pins recovery, not the private report format. No source test expects a generation in a pipe report. | `internal/supervisor/workerproc.TestSupervisedReportPipes`: one valid report for the current generation; liveness EOF exits the worker; ignore stale generations; reject oversized or malformed reports; fail on the wrong descriptor; preconfigured nonzero port needs no report. | `internal/supervisor/workerproc.TestSupervisedReportPipes` (phase-3) |
| — | fixture-control-channel-test-only | A supervisor with a connected Node IPC channel accepts fixture shutdown without checking a test guard. | Outside the test guard close the inherited fixture-control descriptor unread (§3.1). | A fixture-only shutdown channel must not authorize production control. | `lib/commands.js:2900`, `test/support.js:105`; no source test requires processing fixture shutdown outside a test. | `internal/supervisor.TestFixtureControlRequiresGuard` | `internal/supervisor.TestFixtureControlRequiresGuard` (phase-2) |
| — | inherited-object-names-are-unknown | A command named after a property that every JavaScript object inherits is not reported as unknown: `toString`, `constructor`, `hasOwnProperty` and `valueOf` exit 0 with no output, and `__proto__` fails with `commands[command] is not a function` and status 1. | Such a name is an ordinary unknown command, with the usual message and status. | The source's result is an accident of looking a name up in a plain object; nothing can rely on it. | `bin/model-gateway.js:4-16`, `lib/commands.js:2935-3041`; no source test runs such a name. | `internal/cli.TestInheritedObjectNamesAreUnknownCommands` (each inherited name gives the unknown-command result) | `internal/cli.TestInheritedObjectNamesAreUnknownCommands` (phase-0) |
| — | empty-home-is-an-error | An explicitly empty `HOME` is kept, so every path below the home becomes relative to the working directory. | Report an error that names `HOME`; do not fall back to the account database. | State and settings written relative to wherever the command happens to run are never what the user meant. | `lib/runtime.js:15-16` (`os.homedir`), `lib/settings-wiring.js:104`; Node v24.11.1 `deps/uv/src/unix/core.c:1195-1209`; no source test sets an empty `HOME`. | `internal/paths.TestEmptyHOMEFailsWithoutAccountLookup` (every resolver that needs the home fails and the account lookup is not called) | `internal/paths.TestEmptyHOMEFailsWithoutAccountLookup` (phase-0) |
| — | stale-temporary-sweep-generated-names | After an atomic write, every sibling named `<target>.*.tmp` that is at least one hour old is removed. | Remove only regular files whose name has the shape a writer generates, `<target>.<pid>.<12 hex digits>.tmp`, and that are at least one hour old; symbolic links and names of any other shape are kept. | A sibling such as `<target>.backup.tmp` may be the user's file; the writer deletes only what a writer could have left behind. | `lib/atomic-file.js:9`, `lib/atomic-file.js:39`; `test/atomic-file.test.js:60` uses the name `state.json.4242.abandoned.tmp`, which the port keeps. | `internal/atomicfile.TestSweepTemporarySiblings` (old and live generated names; an old `.backup.tmp` and an old name of another shape survive) | `internal/atomicfile.TestSweepTemporarySiblings` (phase-0) |
| — | wire-json-nesting-depth | Wire JSON is parsed and serialized by the runtime, whose limits follow its stack: at the pinned Node version a document nested 65,536 levels deep still parses, while serializing one nested deeper than 4,457 levels failed (both observed in the test venue; neither is a guaranteed limit). | Accept and produce at most 10,000 levels; deeper input or output is a named error. | A fixed limit in both directions bounds memory and does not depend on the runtime's stack; no legitimate request approaches it. The port therefore refuses some input the source parses, and can serialize some output the source could not. | `lib/request-worker.js:197` and the other `JSON.parse` and `JSON.stringify` call sites; the observation is produced by `hack/json-goldens.mjs`; no source test nests more than a few levels. | `internal/jsonx.TestWireDepth` (10,000 levels round-trip; 10,001 is the named error when decoding and when encoding) | `internal/jsonx.TestWireDepth` (phase-0) |
| — | dispatch-cache-ticket-type | The cache's regexp coerces a non-null ticket to a string; a boolean or a single-element array can pass and be kept in its original type. | Drop cache rows whose ticket is neither a string nor null. | Keep the cached ticket type consistent with the route marker. | `lib/request-worker.js:768-775,797-801` | `internal/worker/dispatch.TestCacheLoadRejectsMalformedRows/non-string_tickets` (drop boolean and array tickets, keep the valid string) | `internal/worker/dispatch.TestCacheLoadRejectsMalformedRows/non-string_tickets` (phase-1) |
| — | drop-ill-formed-model-identifiers | An escaped lone surrogate survives in a JavaScript string and can be retained in a listing row. | Drop listing or catalog entries whose identifying string contains an escaped lone surrogate. Keep literal U+FFFD. Request classification is covered separately by `unclassifiable-model-request-refused`. | Lead ruling, 2026-11-26: drop malformed listing identifiers without inventing a replacement ID. | `lib/request-worker.js:1480-1481`, `lib/proxy-models.js:40-50`, `lib/lmstudio.js:88-106,109-119,133-139` | `internal/proxymodels.TestParseIgnoresIllFormedIDs` and `internal/localprov.TestCatalogIgnoresIllFormedKeys` (drop escaped lone surrogates; keep literal U+FFFD). | `internal/proxymodels.TestParseIgnoresIllFormedIDs` (phase-1)<br>`internal/localprov.TestCatalogIgnoresIllFormedKeys` (phase-1) |
| — | node-utf8-replacement-before-classification | Decoding bytes to text replaces malformed UTF-8 with U+FFFD before parsing. A replaced key and a literal U+FFFD key can become equal. | Preserve this decoding in the request classifier and LM Studio model-list parser. The LM Studio parser can coalesce keys after replacement. Proxy listings and raw Anthropic picker-row merges instead discard identifiers with malformed raw bytes. | Keep the byte-decoding rule separate from the escaped-surrogate ruling; the listed strict readers reject malformed identifiers before normalizing them. | `lib/request-worker.js:1480-1481,2435-2444`, `lib/lmstudio.js:88-119`, `lib/previous-picker-rows.js:13-45`. | `internal/worker/route.TestClassifyBodyEdges` (Node replacement before routing), `internal/localprov.TestCatalogIgnoresIllFormedKeys` (replacement key coalesces), `internal/proxymodels.TestParseIgnoresIllFormedIDs` and `internal/anthropiccatalog.TestRawIdentifierBytes` (strict readers). | `internal/worker/route.TestClassifyBodyEdges/success:_ill-formed_byte_decodes_as_Node_does` (phase-1)<br>`internal/localprov.TestCatalogIgnoresIllFormedKeys/success:_replaced_raw_bytes_match_a_literal_replacement_key` (phase-1)<br>`internal/proxymodels.TestParseIgnoresIllFormedIDs` (phase-1)<br>`internal/anthropiccatalog.TestRawIdentifierBytes` (phase-1) |
| — | inherited-provider-labels | The label lookup returns inherited properties of the label object, such as the `constructor` function. | Capitalize an unrecognized provider name, so `constructor` becomes `Constructor`. | An inherited property is not a provider label. | `lib/proxy-models.js:14-28,155-157` | `internal/proxymodels.TestProviderLabel/success:_capitalize_constructor_provider` | `internal/proxymodels.TestProviderLabel/success:_capitalize_constructor_provider` (phase-1) |
| — | install-root-collation | Equal numeric versions are ordered by descending `lastUpdated` text using the host's default `localeCompare` collation. | Use root collation for that tie-break. | Select the same install regardless of host locale. | `lib/runtime.js:718-727`, `hooks/registry-writer.js:61-65` | `internal/install.TestRegistryRootCollation` (root rather than Swedish or byte order) | `internal/install.TestRegistryRootCollation` (phase-1) |
| — | pid-record-order-and-mode | Writes compact JSON with `pid`, `command` and `startedAt` in that order, with no explicit file mode. | Write the fields in the order specified in §3.4 and request mode `0600`; the creation mask may remove owner permissions. Never grant group or other permissions. | Make the extended record deterministic and keep it private. | `lib/process-supervision.js:80-102` | `internal/procinfo.TestRecordFormatAndPermissions` (exact JSON and mirror bytes and mode under permissive and ordinary creation masks) | `internal/procinfo.TestRecordFormatAndPermissions` (phase-0) |
| — | catalog-writer-is-string | `writtenBy` is the plugin manifest version, or null if unreadable or not a string. | Always write `writtenBy` as a string. If no version is supplied, write `0.0.0-dev`. | Lead ruling, 2026-11-26: keep the catalog field's type stable, including for builds without a version. | `lib/commands.js:85,242-247,1900-1908` | `internal/catalog.TestMissingVersionWritesString` asserts that the serialized field is `0.0.0-dev` when no version is supplied. | `internal/catalog.TestMissingVersionWritesString` (phase-1) |
| — | sse-preserve-malformed-utf8 | The Codex SSE transformer decodes UTF-8 before framing, so an untouched frame can carry replacement characters instead of its original bytes. | Keep the original bytes of a frame forwarded without a rewrite, even if it contains ill-formed UTF-8. Rewritten events still follow the JSON wire policy. | Forwarding an unmodified frame should not change its bytes. | `lib/request-worker.js:1668-1682,1705-1720` | Test pending: `internal/worker/sse.TestForwardMalformedUTF8Unchanged` must assert the client receives the original ill-formed bytes end to end through the worker relay. `internal/sse.FuzzFrameRoundTrip` has a seed for the frame scanner only. | `internal/worker/sse.TestForwardMalformedUTF8Unchanged` (phase-2) |
| — | macos-listener-visible-holders | On macOS the probe returns the first nonzero PID from `lsof`, or null for no match or a nonzero exit; the asynchronous probe returns `undefined` on timeout. | Use `lsof` alone. One visible PID is identified even if it holds several sockets; several visible PIDs are ambiguous; clean status 1 with no output is absent; diagnostic stderr or partial output is unknown. | Identified and absent are scoped to listeners visible to this user; a caller that needs the port must bind it (§3.4). Another user's listener may not be visible; that limit is documented, not tested. | `lib/process-supervision.js:242-244,272-284,476-481` | `internal/procinfo.TestListenerFiles` (decoder outcomes, including partial output); `internal/procinfo.TestCommandWarningRefusal` (stderr refuses a successful or partial listing); `internal/procinfo.TestDarwinListenerEvidence` (one live visible holder). | `internal/procinfo.TestListenerFiles` (phase-0)<br>`internal/procinfo.TestCommandWarningRefusal` (phase-0)<br>`internal/procinfo.TestDarwinListenerEvidence` (phase-0) |
| — | reverse-model-spelling-by-snapshot | The policy reverse-spelling fallback lazily builds a process-wide map and keeps it when routes change. | Resolve reverse spelling from the published route snapshot. | Keep aliases, transport and windows consistent with the routes used for the request. | `lib/runtime.js:379-390` | `internal/policy.TestReverseSpellingFollowsRouteSnapshot` (two route snapshots with different alias collisions) | `internal/policy.TestReverseSpellingFollowsRouteSnapshot` (phase-1) |
| — | grok-ids-by-snapshot | A Grok request reads the model IDs from the cache file again and falls back to the default list if the cache is empty. | Resolve against the Grok ID list in the published route snapshot. | Requests see changes at publication and use one coherent set of routes. | `lib/request-worker.js:2596-2604`, `lib/grok-backend.js:427-441,454-456` | `internal/worker/route.TestGrokIDsFollowPublishedSnapshot` (new IDs visible only after publication) | `internal/worker/route.TestGrokIDsFollowPublishedSnapshot` (phase-1) |
| — | empty-pin-family-description | A Fable override such as `claude--5` reaches the display formatter with an empty family and throws on `family[0].toUpperCase()`. | Use the raw model ID in the description when the family is empty. | A malformed override must not prevent building the settings entries. | `lib/pins.js:37-44,400-409` | `internal/pins.TestEmptyFamilyDescription` (description and owned value use the raw ID) | `internal/pins.TestEmptyFamilyDescription` (phase-1) |
| — | anthropic-catalog-id-type | The catalog regexp coerces an ID to a string for filtering; an array with one matching ID can pass and is kept as an array. | Drop any row with a non-string ID on read. | Downstream consumers expect an ID string. | `lib/anthropic-catalog.js:20-30` | `internal/anthropiccatalog.TestRead/success:_drop_nonstring_id` (drop array ID, keep valid row) | `internal/anthropiccatalog.TestRead/success:_drop_nonstring_id` (phase-1) |
| — | endpoint-rejects-idna-hosts | LM Studio and OTLP usage endpoints use WHATWG `new URL`, which can normalize host and path spellings before their endpoint-specific checks. LM Studio rejects nonempty user info but allows empty user info; the usage endpoint parser does not check user info. | Use a stricter parser for LM Studio and OTLP. In addition to IDNA, reject empty user info, escaped hosts, backslashes, embedded controls and paths containing literal spaces or incomplete percent escapes. OTLP also rejects credentials. See `docs/env.md` for the setting-specific rules. | Refuse endpoint spellings the port does not normalize. | `lib/lmstudio.js:39-53`, `lib/usage-observability.js:605-615` | `internal/config.TestEndpointDecisions` (conservative refusals for credentials, hosts, backslashes, controls and paths) | `internal/config.TestEndpointDecisions/error:_conservative_reject_credentials` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_empty_userinfo` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_escaped_host` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_non-ASCII_hostname` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_punycode_hostname` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_backslash` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_embedded_tab` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_embedded_newline` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_incomplete_percent_escape` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_literal_spaces_in_path` (phase-0) |
| — | lifecycle-journal-regular-file-append | Appends one serialized line with `fs.appendFileSync` without checking the leaf. | Append each record through an `O_APPEND` primitive that accepts only regular files and does not follow a final symlink. Use one data-bearing write; retry `EINTR` only if no bytes were written. Report a short write without retrying the remainder. The primitive does not enforce a size limit; on macOS, repeated appends through fresh descriptors can exceed a file-size limit. | Do not write through a symlink or to a special file; do not split one record across writes that can interleave. | `lib/lifecycle-diagnostics.js:36-42` | `internal/lifecycle.TestSingleRecordAppend` (one complete record per call); `internal/atomicfile.TestAppendFileRefusals` (non-regular leaves); `internal/atomicfile.TestAppendFileWriteCalls` (one write, zero-byte `EINTR` retry, short write); `internal/atomicfile.TestAppendFileConcurrentRecords` (records stay intact). | `internal/atomicfile.TestAppendFileRefusals` (phase-0)<br>`internal/atomicfile.TestAppendFileWriteCalls` (phase-0)<br>`internal/atomicfile.TestAppendFileConcurrentRecords` (phase-0)<br>`internal/lifecycle.TestSingleRecordAppend` (phase-1) |
| — | remote-control-not-ported | The CLI manages a hosts block; the supervisor or a standalone worker can listen on the mapped address. The supervisor handles `/internal/rc-compatibility` and upstream DNS bypasses the block. | `remote-control` has the ordinary unknown-command result. Omit its help and completion entries and the `env` advice to enable it. No compatibility listener, control endpoint, DNS bypass or skill ships. Omit the `compat` runtime diagnostics. `doctor` and the migration check that `migrate` runs before its first step (§3.6) detect the Node-managed hosts block without writing it and tell the operator to run the Node version's `remote-control disable --confirm` or remove the block by hand. | User decision, 2026-11-26 (OD-21): the retained block can point the API host at a listener the port does not provide. | `lib/remote-control.js:137-269`, `lib/commands.js:2715-2799`, `lib/request-worker.js:881-931,2710-2714`; `test/rc-compat-mode.test.js`. | `internal/cli.TestRemoteControlIsUnknownCommand` (normal unknown-command output and exit). The phase-3 gate checks that the listener and control handling are absent. `doctor` and the migration check verify the block without writing the hosts file (AC-7, AC-12). | `internal/cli.TestRemoteControlIsUnknownCommand` (phase-1) |
| — | legacy-compat-url-cleanup-only | The source recognizes both URLs as owned, can write the compatibility URL and can switch between endpoints during automatic repair. | Recognize `http://api.anthropic.com` only as a legacy owned value for diagnosis, migration checks and explicit cleanup. Never write it. `env --write-project` and `env --write-user` use the configured default URL; removal deletes value-matched owned entries from both project files, since automatic paths leave legacy URL entries in the shared file. Reconcile accepts only the configured default URL as its target and removes conflicts only on confirmation. The pre-migration guard applies to Go `env` writes on a home with a legacy install (§3.6). Hooks, `setup`, `ensure` and `readiness-gate` never switch endpoints. `doctor` and migration checks report the legacy URL and hosts block separately (§3.6). | User decision, 2026-11-26: retain cleanup without exposing a transport the port does not provide. | `lib/pins.js:472-476`, `lib/commands.js:1134-1171,1220-1235,1486-1510,3015-3025`, `lib/remote-control.js:188-190`; `test/rc-compat-mode.test.js:629` (`:633–634`), `test/rc-compat-mode.test.js:651` (`:689`), `test/wiring-mode.test.js:677` (`:688`, `:691`), `test/wiring-mode.test.js:695` (`:707–708`, `:710`, `:712`), `test/wiring-mode.test.js:715` (`:724`), `test/wiring-mode.test.js:753` (`:765`). | `internal/wiring.TestAutomaticPathsKeepLegacyURL` (owned legacy URL, supplied-health passthrough and no automatic endpoint write), `internal/wiring.TestExplicitAndAutomaticSettingsChanges` (explicit default write), `internal/wiring.TestVerifiedMode/success:_explicit_write_replaces_a_migrated_legacy_URL` (explicit project write migrates shared legacy URL to default while preserving local settings and removing shared env), `internal/wiring.TestCompatModeWriteRefused` (refused write and explicit removal), and `internal/wiring.TestReconcileRegisteredProjects` (preview of the legacy value without edits and confirmed removal), `internal/wiring.TestProjectRemovalClearsBothProjectFilesOnly` (explicit removal even if shared wiring has not moved), `internal/wiring.TestReconcileRejectsRetiredCompatTarget` (refuses before scanning or writing), and `internal/wiring.TestAutomaticMigrationPreservesEffectiveBaseURL` (unsafe moves leave both settings files untouched). Test pending: `internal/doctor.TestLegacyCompatibilityFindings` (four states, shadowed and process values, exit status and separate read-only remedies) is part of `doctor`. The migration check reports each finding before any migration write (AC-7). | `internal/wiring.TestAutomaticPathsKeepLegacyURL` (phase-1)<br>`internal/wiring.TestExplicitAndAutomaticSettingsChanges` (phase-1)<br>`internal/wiring.TestVerifiedMode/success:_explicit_write_replaces_a_migrated_legacy_URL` (phase-1)<br>`internal/wiring.TestCompatModeWriteRefused` (phase-1)<br>`internal/wiring.TestReconcileRegisteredProjects` (phase-1)<br>`internal/wiring.TestProjectRemovalClearsBothProjectFilesOnly` (phase-1)<br>`internal/wiring.TestReconcileRejectsRetiredCompatTarget` (phase-1)<br>`internal/wiring.TestAutomaticMigrationPreservesEffectiveBaseURL` (phase-1)<br>`internal/doctor.TestLegacyCompatibilityFindings` (phase-4) |
| — | tool-schema-engine-quirk-refusals | V8 can narrow non-ASCII property names and accept some duplicate captures that can participate together. Both checkers accept duplicate names in genuinely disjoint alternatives. The adapter refuses captures before checking the original pattern. | Reject property-name narrowing and the participating duplicates that V8 accepts in the pinned cases. For the adapter's property-name refusal, report `invalid-regex`; the worker returns HTTP 400 for the whole request and forwards nothing. Capture refusal retains its source precedence. | Lead ruling, 2026-11-26: do not reproduce these V8 extensions. | `lib/tool-schema-compat.js:195-246,263-269,286-289`, `lib/request-worker.js:2517-2540`; Node supplement to the tool-schema corpus. | `internal/toolschema.TestEngineQuirkRefusals` (checker and adapter verdicts). Test pending: `internal/worker/proxyreq.TestToolSchemaEngineQuirkHTTPRefusal` (400 and no upstream request). | `internal/toolschema.TestEngineQuirkRefusals` (phase-1)<br>`internal/worker/proxyreq.TestToolSchemaEngineQuirkHTTPRefusal` (phase-2) |
| — | compact-at-null-message | `Object.entries(null)` throws a V8 TypeError. | Return `invalid compactAt: must not be null`. | Lead ruling, 2026-11-26: keep a validation message rather than the engine's exception text. | `lib/runtime.js:281-297`. | `internal/policy.TestCompactAtContainers/error:_null` | `internal/policy.TestCompactAtContainers/error:_null` (phase-1) |
| — | explicit-date-grammar | The upstream hold and usage timestamps call `Date.parse`, which also accepts legacy or normalized calendar spellings. | `internal/jsdate` accepts ISO `YYYY-MM-DD` in UTC, ISO time with an explicit zone (`Z` or `±HH:MM`, seconds optional, 1–9 fractional digits only after seconds), IMF-fixdate (`GMT`, `UTC` or `±HHMM`, year at least 100) and RFC 850 with `GMT` and a 50-year pivot. Reject zone-less times, `1970-01-01T24:00:00Z` and `1900-02-29`. | Lead ruling, 2026-11-26: date parsing has an explicit grammar and does not depend on local time. | `lib/codex-upstream-state.js:34-49`, `lib/usage-observability.js:557-567`. | `internal/jsdate.TestParse` (accepted forms and refusals), `internal/upstreamstate.TestRateLimitExpiryHeaderForms` and `internal/usage.TestTimestampFamilies` (callers). | `internal/jsdate.TestParse` (phase-1)<br>`internal/upstreamstate.TestRateLimitExpiryHeaderForms` (phase-1)<br>`internal/usage.TestTimestampFamilies` (phase-1) |
| — | usage-high-water-safe-integers | A finite request-body size can be recorded at or above 2^53. The store coerces the stored value to a finite non-negative number, including fractions and numeric strings. | Ignore measurements at or above 2^53. Treat a stored high-water value as absent unless it is a non-negative safe integer. | Lead ruling, 2026-11-26: a byte count must be exact and fit the safe integer range. | `lib/usage-observability.js:181-206`. | `internal/usage.TestUnsafeHighWaterObservation` and `internal/usage.TestSafeHighWater`. | `internal/usage.TestUnsafeHighWaterObservation` (phase-1)<br>`internal/usage.TestSafeHighWater` (phase-1) |
| — | usage-transcript-root | The session transcript can be opened through a symlink that leaves the projects directory. | Ignore a transcript symlink that leaves the projects directory. If no readable transcript yields a project name, leave it unset. | Lead ruling, 2026-11-26: transcript reads are restricted to the configured projects root. | `lib/usage-observability.js:115-170`. | `internal/usage.TestTranscriptLinkOutsideRoot` (reject escaping links and accept an in-root link). | `internal/usage.TestTranscriptLinkOutsideRoot` (phase-1) |
| — | usage-git-pointer-size | `.git` and `commondir` pointer files are read without a size bound. | Read at most 64 KiB of pointer data, plus one byte to detect and refuse an oversized file. Pointer symlinks still resolve as in the source. | Lead ruling, 2026-11-26: bound metadata reads without treating git pointers like session transcripts. | `lib/usage-observability.js:65-113`. | `internal/usage.TestProjectPointerSize` (limit and one byte over, for both pointers). | `internal/usage.TestProjectPointerSize` (phase-1) |
| — | usage-ranking-root-collation | The top 20 MCP servers are ranked by aggregate serialized tool-definition bytes; equal byte totals use host-locale `localeCompare` on the server name. | Use root collation for the server-name tie-break. | Lead ruling, 2026-11-26: ranking does not depend on the host locale. | `lib/usage-observability.js:471-497`. | `internal/usage.TestMCPServerRootCollation` (servers with equal aggregate tool-definition bytes and different host-locale or byte order). | `internal/usage.TestMCPServerRootCollation` (phase-1) |
| — | install-version-failed-coercion | A registry version whose object has an own non-callable `toString` can throw during sorting. | Compare the object as `[object Object]`, which supplies no numeric version parts. Apply the normal ordering and tie-break to the result. | Lead ruling, 2026-11-26: keep ordering deterministic without reproducing the JavaScript exception. | `lib/runtime.js:718-727,741-745`. | `internal/install.TestStableOrderingOfRegistryEntries` (a non-callable `toString` does not disrupt ordering). | `internal/install.TestStableOrderingOfRegistryEntries/success:_non-callable_toString_keeps_comparator_deterministic` (phase-1) |
| — | unicode-final-sigma-casing | The install identity and pin version family use JavaScript's whole-string `toLowerCase`. | Use `x/text` lowercasing. Final sigma can differ when lookahead crosses its bound or when the sigma changes position relative to a transform chunk. | Lead ruling, 2026-11-26: accept the casing library's bounded context rather than reproduce JavaScript's full context. | `lib/process-supervision.js:508`, `lib/pins.js:37-44`. | `internal/install.TestComparisonKeySigmaLookahead` (bounded lookahead and chunk boundary). `internal/pins.TestCompareSigmaLookahead` (the port's pin family spelling). | `internal/install.TestComparisonKeySigmaLookahead` (phase-1)<br>`internal/pins.TestCompareSigmaLookahead` (phase-1) |
| — | lifecycle-journal-trim-existing-file | Trimming writes to the pathname; the write can recreate a disappeared journal or follow a replacement symlink. | Trim in place on the existing regular file. Do not create a missing file or follow a final symlink. | Lead ruling, 2026-11-26: preserve the journal inode and mode so existing append descriptors stay valid. | `lib/lifecycle-diagnostics.js:24-34`. | `internal/lifecycle.TestTrimVanishedJournal`, `internal/lifecycle.TestTrimPreservesJournalDescriptor` and `internal/atomicfile.TestDirRewriteFileRefusals`. | `internal/lifecycle.TestTrimVanishedJournal` (phase-1)<br>`internal/lifecycle.TestTrimPreservesJournalDescriptor` (phase-1)<br>`internal/atomicfile.TestDirRewriteFileRefusals` (phase-1) |
| — | lifecycle-stop-marker-atomic | The stop marker is written directly to its destination. | Atomically replace the stop marker. Preserve the compact encodings of the lock owner, stop marker and startup-failure records (§3.5). | Lead ruling, 2026-11-26: keep the stop marker complete across an interrupted replacement. | `lib/lifecycle-lock.js:57-63,96-108`, `lib/commands.js:614-625`. | `internal/lifecycle.TestStopMarker` and `internal/lifecycle.TestStartupFailure` pin marker and failure bytes. `internal/lifecycle.TestAtomicStopReplacement` (interruption leaves the old or complete new marker). | `internal/lifecycle.TestStopMarker` (phase-1)<br>`internal/lifecycle.TestStartupFailure` (phase-1)<br>`internal/lifecycle.TestAtomicStopReplacement` (phase-1) |
| — | proxy-config-ill-formed-token | The proxy config reader can return a string token containing an unpaired surrogate. | Discard the LM Studio config section if the token contains an unpaired surrogate. Do not substitute U+FFFD for it. | Lead ruling, 2026-11-26: fail configuration validation instead of changing a credential. | `lib/proxy-config.js:17-24`, `lib/lmstudio.js:22-35`. | `internal/proxyconfig.TestStudioFileUnicodeAndDuplicateMembers` (unpaired surrogate rejects the section, a well-formed token is retained). | `internal/proxyconfig.TestStudioFileUnicodeAndDuplicateMembers` (phase-1) |
| — | dispatch-cache-directory-lifetime | Each persistence attempt uses the cache pathname, creating its directory again when needed. | Hold the first directory successfully opened for persistence. A name later pointing elsewhere does not redirect writes. If that directory is removed, persistence does not resume before restart. | Lead ruling, 2026-11-26: a cache cannot write to a directory it did not open and retain. | `lib/request-worker.js:724-729,789-795`. | `internal/worker/dispatch.TestCachePersistenceDirectory` (original directory after a rename; retry before the first successful open). `internal/worker/dispatch.TestCacheDeletedDirectory` (remove it and recreate its old pathname; no write before restart). | `internal/worker/dispatch.TestCachePersistenceDirectory` (phase-1)<br>`internal/worker/dispatch.TestCacheDeletedDirectory` (phase-1) |
| — | upstream-hold-failed-coercion | The expiry comparison throws for a stored object with an own non-callable `toString`. | Keep the upstream hold active when the expiry cannot be coerced for the comparison. | Lead ruling, 2026-11-26: unreadable expiry must not silently clear the hold. | `lib/codex-upstream-state.js:27-32`. | `internal/upstreamstate.TestStateDecisionMembers` (own null `toString`, directly or in an array). | `internal/upstreamstate.TestStateDecisionMembers` (phase-1) |
| — | catalog-provider-names | Readiness lookup by provider name can select the shared `checks` object or the inherited `constructor` function. With only shared checks supplied, the collision can report `unavailable` instead of the shared check's failure. | Treat `checks` and `constructor` as ordinary provider names. Keep shared checks separate and use them to derive fallback diagnostics for those providers as for any other unknown provider. | Lead ruling, 2026-11-26: a provider name must not select readiness metadata or an inherited property. | `lib/commands.js:1859-1876,1897-1908`. | `internal/catalog.TestProviderPropertyNames` checks that both provider names receive the shared failure (`binary-missing`). | `internal/catalog.TestProviderPropertyNames` (phase-1) |
| — | catalog-label-error | An empty word in the Codex label suffix can produce a V8 TypeError in `labelFor`. | The checked catalog builder reports `cannot label Codex model %q: empty suffix word`, with the quoted base model ID. | Lead ruling, 2026-11-26: use a catalog validation error rather than the engine exception. | `lib/commands.js:1611-1620,1885-1895`. | `internal/catalog.TestPrepare` pins the exact label diagnostic. | `internal/catalog.TestPrepare` (phase-1) |
| — | catalog-cache-errors-omit-input | Catalog fetch rejects malformed JSON with a fixed message. Saved-cache parsing failures are caught, and the source writer has no Go decoder errors. | Failure to decode raw catalog data or a saved snapshot for writing reports `invalid catalog data` or `invalid catalog snapshot`, respectively, without input text. The separate catalog label error retains the base ID. | Lead ruling, 2026-11-26: do not expose raw parser diagnostics. | `lib/anthropic-catalog.js:20-34,50,72-74`. | `internal/anthropiccatalog.TestCatalogErrorsExcludeInput` (a sentinel is absent in both write errors). | `internal/anthropiccatalog.TestCatalogErrorsExcludeInput` (phase-1) |
| — | pin-version-field-limit | Parse fields with `Number.parseInt(..., 10)` and require the canonical string round-trip, without a field-length check. | Reject a numeric field longer than 21 bytes before conversion. | Lead ruling, 2026-11-26: bound work for oversized version fields. | `lib/pins.js:37-44`. | `internal/pins.TestVersionFieldLimits` rejects a 100,000-digit field; `internal/pins.TestParseVersion` covers the 21- and 22-digit boundary. | `internal/pins.TestVersionFieldLimits` (phase-1)<br>`internal/pins.TestParseVersion` (phase-1) |
| — | grok-picker-empty-id-list | With an empty cache and the picker `[object Object]`, the fallback matches the first default model object and uses the object as the upstream model. | With an empty ID list, use the string `grok-[object Object]` for that picker, by the ordinary no-match rule. | Lead ruling, 2026-11-26: the snapshot carries model IDs, not default model objects. | `lib/request-worker.js:2596-2604`, `lib/grok-backend.js:413-423`. | `internal/worker/route.TestGrokObjectPickerWithEmptyList` classifies the object picker with zero cached IDs. `internal/worker/route.TestClassifyBodyEdges` uses a populated list for this picker. | `internal/worker/route.TestGrokObjectPickerWithEmptyList` (phase-1) |
| — | unclassifiable-model-request-refused | Node can parse a body deeper than the Go decoder limit and can retain an escaped lone surrogate in a model ID. An over-deep top-level array or a truncated JSON body that cannot be parsed takes the default route. | For any non-`HEAD` method whose path starts with `/v1/messages`, refuse with a reason if the body would exceed the decoder's depth limit, including in a top-level array or before detecting truncation. Also refuse an escaped lone surrogate in the final top-level `model` string of an otherwise valid body. The worker returns HTTP 400 without contacting any upstream. Where an object member name is required, a next token that is not a string is ordinary malformed JSON at any depth and goes unchanged to the default upstream. Decode ill-formed raw UTF-8 as Node does; other malformed bodies go unchanged to the default upstream unless the decoder reports the depth error first. | User decision, 2026-11-26: the port's classifier limits must not change the recipient of a request. Lead ruling, 2026-11-26: invalid member-name syntax takes precedence over the depth of that token. | `lib/request-worker.js:2435-2444,2594-2604,2648-2676`; `wire-json-nesting-depth`. | `internal/worker/route.TestClassifyRefusals` checks both reasons, including an over-deep array and a non-`HEAD` method other than `POST`. Add a classifier case for malformed member-name syntax at the depth boundary. Test pending: `internal/worker.TestClassificationRefusalHTTP` must verify the HTTP and logging contract of AC-3 for both reasons, including a body whose depth error precedes truncation, plus byte-exact ordinary non-JSON passthrough. | `internal/worker/route.TestClassifyRefusals` (phase-1)<br>`internal/worker.TestClassificationRefusalHTTP` (phase-2) |
| — | grok-stream-coercion-is-atomic | A coercion can throw after the translator has already emitted a start frame or changed stream state. | When a coercion fails, emit no frame and leave stream state unchanged for that input event. | Lead ruling, 2026-11-26: a failed input event must not emit a partial translation. | `lib/grok-backend.js:293-389`. | `internal/grok.TestCoercionBoundaries` (no output or state change for events that fail coercion, checked with later events). | `internal/grok.TestCoercionBoundaries` (phase-1) |
| — | grok-register-stream-block-after-write | Block creation registers the block before writing its `content_block_start`. | Register a block only after its start frame is written successfully; do not emit deltas or stops for a block that never started. | Lead ruling, 2026-11-26: only a block visible to the client can have later frames. | `lib/grok-backend.js:304-315`. | `internal/grok.TestStreamDepthFailureDoesNotRegisterTool` (no orphan delta or stop when the start cannot be encoded). | `internal/grok.TestStreamDepthFailureDoesNotRegisterTool` (phase-1) |
| — | wiring-state-directory | The wired-projects registry, `wiring.json` and `context-window.json` resolve only under `~/.claude/model-gateway`. | Resolve all three under the gateway state directory, honoring `MODEL_GATEWAY_STATE_DIR` without moving Claude settings or the stable launcher. `wiring.json` is only retired; it is not written. | Lead ruling, 2026-11-26: keep wiring state with the selected gateway instance. | `lib/runtime.js:15-20,244`, `lib/settings-wiring.js:23-28,201-215`. | Test pending: `internal/wiring.TestStateDirectoryOverride` verifies that registry writes, obsolete mode-file removal and context-window lookup use the override and leave the default directory untouched. Add it alongside the `env` and `context-window` command work. | `internal/wiring.TestStateDirectoryOverride` (phase-4) |
| — | readiness-legacy-outcome | The recovery policy returns `recover` for the effective legacy URL unless the plugin is explicitly disabled. | Return `legacy` instead: do not recover or claim readiness from that URL. `disabled` still takes precedence for the legacy URL. | Lead ruling, 2026-11-26: a healthy loopback gateway cannot serve the legacy endpoint. | `lib/recovery-policy.js:17-32`, `lib/readiness-gate.js:52-57`. | `internal/readiness.TestRecoveryPolicy` pins `legacy` with the plugin enabled or an unrelated plugin disabled. Test pending: `internal/readiness.TestLegacyReadinessGate` verifies no recovery or readiness claim for `legacy` and that explicit disable takes precedence. Add it with the `readiness-gate` command. | `internal/readiness.TestRecoveryPolicy` (phase-1)<br>`internal/readiness.TestLegacyReadinessGate` (phase-4) |
| — | wiring-https-override-without-idna | The HTTPS process-override check uses WHATWG `new URL` with IDNA host processing. | Match the scheme and percent-decoded hostname with ASCII-only case folding. Do not use IDNA or Unicode case folding. | Lead ruling, 2026-11-26: do not reproduce WHATWG IDNA normalization in this check. | `lib/settings-wiring.js:61-74`. | `internal/wiring.TestHTTPSOverrideASCIIOnlyCaseFolding` (ASCII case and percent escapes match; non-ASCII spellings do not). | `internal/wiring.TestHTTPSOverrideASCIIOnlyCaseFolding` (phase-1) |
| — | wiring-skip-aliased-migration-files | The source guards the shared project path against the user settings path by resolved name, but not against the local project file. Copying and then deleting through aliased shared and local paths can remove the wiring. | When the opened shared file is also the local or user settings file, skip the move. Use device and inode identity, including symbolic and hard links; recheck in the replacement callbacks. | Lead ruling, 2026-11-26: leave the file alone when the source and destination are the same object. | `lib/settings-wiring.js:365-398`. | `internal/wiring.TestMigrationSkipsSharedLocalAliases` (symlink and hardlink; contents unchanged). | `internal/wiring.TestMigrationSkipsSharedLocalAliases` (phase-1) |
| — | wiring-bounded-regular-snapshots | Preliminary settings, registry, and model-cache reads use unbounded `fs.readFileSync` without checking the opened file type. | Require regular files after opening, cap reads at 16 MiB, and use a nonblocking open on Unix. Follow symlinks to regular files. An oversized settings or registry read errors with file and bound instead of treating the file as absent or pruning or rewriting the registry. Apply the same bound to both reads inside the atomic settings replacement. | Lead ruling, 2026-11-26: a wiring or readiness scan must not wait on special files or read without a bound. | `lib/settings-wiring.js:76-83,206-215,424-426`. | `internal/wiring.TestSnapshotReaderRefusesNonRegularWithoutBlocking` (special files, symlinks and oversized-file error); `internal/wiring.TestSnapshotReaderRefusesUnixSocketWithoutBlocking` (real socket, process tier only); `internal/wiring.TestOversizedSettingsAndRegistryReturnBoundError` (valid oversized JSON; no prune or rewrite); `internal/atomicfile.TestReplaceSettingsInfoSizeBound` and `internal/wiring.TestProductionAdapterMigrationStaleSourceAndGrowthBound` (bounded replacement and growth after preliminary read). | `internal/wiring.TestSnapshotReaderRefusesNonRegularWithoutBlocking` (phase-1)<br>`internal/wiring.TestSnapshotReaderRefusesUnixSocketWithoutBlocking` (phase-1)<br>`internal/wiring.TestOversizedSettingsAndRegistryReturnBoundError` (phase-1)<br>`internal/atomicfile.TestReplaceSettingsInfoSizeBound` (phase-1)<br>`internal/wiring.TestProductionAdapterMigrationStaleSourceAndGrowthBound` (phase-1) |
| — | grok-precommit-translation-error | The buffered Grok response callback translates and serializes before writing headers, but has no local response for a translation or serialization exception. | Before committing headers, answer either failure with a fixed, sanitized HTTP 502 `api_error`; use route status override `upstream_error`. Never expose the exception or provider input. | Lead ruling, 2026-11-26: return a bounded failure rather than an uncaught callback error. | `lib/request-worker.js:2330-2345` (translation and encode), `:2350-2356` (separate request transport error). | Test pending: `internal/grok.TestBranchForwardJSON` checks the pre-commit 502 and must pin the fixed message and cover both translation and encoding failures. | `internal/grok.TestBranchForwardJSON` (phase-2) |
| — | grok-shadow-auth-no-refresh | The source refreshes when expiry falls within the five-minute skew and persists the new auth state; the ordinary Grok path has no shadow exception. | In shadow mode, use the existing credential until its actual expiry, including inside the refresh window. Refuse expired credentials or unknown expiry without refreshing or writing. | Lead ruling, 2026-11-26: shadow mode may read valid auth but must not change it. | `lib/grok-backend.js:52-67,83-119`, `lib/request-worker.js:2244-2253`. | Test pending: `internal/grok.TestAccessToken` must pin valid inside-skew use and expiry refusal, without network or disk writes. | `internal/grok.TestAccessToken` (phase-2) |
| — | discovery-legacy-url-skip-reason | Discovery-cache sync returns `rc-compatibility` for the compatibility URL before checking freshness or force. | If the legacy URL is effective, skip as `gateway-not-wired`, including when forced. Do not write or delete the cache or dial a provider as part of the skipped sync. | Lead ruling, 2026-11-26: the retired URL does not wire a gateway capable of serving discovery. | `lib/runtime.js:614-629`, `lib/request-worker.js:1524-1534`. | Test pending: `internal/worker/modelcache.TestDiscoveryCacheSync` (reason, absent and preexisting cache, ordinary and forced sync). | `internal/worker/modelcache.TestDiscoveryCacheSync` (phase-2) |
| — | local-engine-state-copy | Local-engine getters expose the stored object; LM Studio refresh returns the same object policy reads. | Hand out deep copies of local-engine state. Preserve windows and rows by value without exposing shared mutable state. | Lead ruling, 2026-11-26: a consumer cannot mutate published engine state. | `lib/proxy-models.js:131-145`, `lib/lmstudio.js:151-161`, `lib/local-llama.js:55-66`; `test/lmstudio.test.js:313` (identity assertion `338:3`). | Test pending: `internal/localprov.TestRefreshStudioState` replaces the identity assertion with equality and mutation isolation. | `internal/localprov.TestRefreshStudioState` (phase-2) |

Kept unchanged although questionable, recorded so nobody fixes them unasked (follow-ups F1-F7): the relay's resend to a
replacement worker; the CLI's argument scans (below); the gate recovers when the serving supervisor is newer than the
installed version, which prints the reload notice (`test/readiness-fast-path.test.js:76`);
`count_tokens` on a `claude-grok-*` model becomes a billed generation (INV-02 §14 item 1); `stop` writes the stop marker
before the stop is known to have worked (INV-03 §13 item 2); `sentrySessions` is never evicted (INV-02 §12); pin sync skips
a whole project when one owned key holds a user value (INV-04 §13 item 14).

**CLI argument scans.** `setup`, `login`, `ensure`, `catalog` and `env` check for known flag tokens anywhere, even after
`--` or in a value position. A presence check ignores unknown flags without consuming the following token
(`lib/commands.js:199-201,1134-1146,2078-2080,2935-2939,2961-2962`). `pin` and `context-window` read consecutive raw
option/value pairs and validate them, so an unknown option is not discarded (`lib/commands.js:1001-1012,1075-1098`).
The other ported commands do not scan command-specific arguments;
`readiness-gate` reads stdin (`lib/readiness-gate.js:52-63`). The shared `--json` check routes `log` output to stderr,
including the `models` document, without moving direct stdout output (`lib/commands.js:217,3028-3032`).

**Client disconnects.** What the source does to the provider request when the client goes away (before the upload ends,
before the worker answers, mid-stream, for a direct and for a relayed client) cannot be settled by reading: Node's `pipe`
back-pressure decides whether the provider stream is drained or stalls until the 1 h idle timer (INV-02 §14 item 2). T0.6
measures it with the pinned Node gateway and a counting fake upstream in a Tier 2 venue; the Go behavior is then written to
match and pinned by tests (T2.4, T3.3). Until measured, nothing depends on it. Fixed in any case: no retry (compaction,
rejected upgrade) is issued once the consumer is gone (`lib/request-worker.js:2081-2096`), and a truncated committed
response is aborted (`http.ErrAbortHandler`), never ended cleanly.

### 3.11 Hooks, launchers and distribution

- **Version.** `0.57.0` is the first Go version (above the plugin's `0.56.3`). Print the `-ldflags -X` value verbatim
  when set; otherwise print the released `debug.ReadBuildInfo().Main.Version` without its leading `v`. A `+dirty` release
  counts as a development build. For development builds print `0.0.0-dev+<full revision>`, or `0.0.0-dev` without a
  revision; `setup` refuses to self-install a development build over a released version.
- **Stable binary.** `~/.claude/model-gateway/bin/model-gateway`, installed by `setup` from the running executable (T4.0).
  Skills, settings hooks and launchers use this absolute path; nothing relies on `PATH`.
- **Compatibility launchers.** `~/.claude/model-gateway/model-gateway.js` becomes a trampoline that runs the stable binary
  with the caller's arguments, stdin, stdio and exit status. `update.js` becomes a trampoline that takes no arguments and
  runs `setup --preserve-wiring`, as today. They keep `gateway-switch`, the updater and settings files that still hold the
  Node hook string working. The Go plugin's SessionStart hook rewrites them when their content differs, but only on a home without a legacy install or
  with a committed migration journal (section 3.6).
- **Plugin.** `.claude-plugin/marketplace.json` at the repository root names the marketplace `claude-code-model-gateway` and
  one plugin `model-gateway` with source `./plugin`. The plugin id is `model-gateway@claude-code-model-gateway`; the
  recovery policy reads `enabledPlugins` for that id and treats an absent entry as enabled, as the source does for its id.
  `plugin/hooks/hooks.json` runs `sh "${CLAUDE_PLUGIN_ROOT}/hooks/session-start.sh"` (timeout 10): with the stable binary
  present it runs `model-gateway hook session-start`; without it, it prints one SessionStart JSON object whose
  `additionalContext` says the binary is missing and gives the two install commands, and exits 0.
- **Install commands** in the skill and README: `go install github.com/zchee/claude-code-model-gateway@v0.57.0`, then
  `claude-code-model-gateway setup`. Release archives and `self update` exist only if OD-5 says so.

---

## 4. Implementation steps

Size: S (under ~300 lines of Go with tests), M (~300-1,000), L (~1,000-2,500). No task is larger than L. Lane: **sol** =
simple; **astra** = complex (agent types in section 9). "Needs" lists every prerequisite; a
task starts only when all of them are merged. A task that implements source behavior needs its **test handoff**, which
is a task of its own (T0.9 for phase 0, T1.0 for phase 1; the same pattern opens each later phase): the `go-port` tests
that T0.7 assigned to it, written by a test lane and merged first (section 9). T0.0, T0.1, T0.4, T0.7 and T0.7b are
inputs and tooling and have none. Each task's inputs are the cited inventory sections and the cited ranges of the pinned
source as `hack/pinned-source` materializes it (`.cache/model-gateway-js/plugins/model-gateway/`, T0.0). Source cases follow `test/conformance/classification.tsv`: `conformance` cases run unchanged against the Go binary;
`go-port` cases are re-expressed as Go tests; a whole-case difference is replaced by the test named in section 3.10.
`js-only` and `unsupported` cases are not ported; an exemption holds only while `test/conformance/exemptions-approved.tsv` has a matching approval row.

### Phase table

| Phase | Content | Exit gate | Status |
|---|---|---|---|
| 0 | Foundations, isolation, identity seam, test inventory | AC-1, AC-2; test inventory (T0.7) reviewed and landed | ✅ done: gate passed at `ccdc0b6` on 2026-11-26; landed on `main` |
| 1 | Pure logic packages | AC-3 (tables), AC-8 for phase 1 rows | 🔶 in progress: implementation complete; phase-gate evidence recorded separately by the lead |
| 2 | Walking skeleton, launch adapter, worker | AC-3 (HTTP), AC-4, AC-5 (worker paths), AC-11 for tests assigned to phase 2 | 🔜 not started |
| 3 | Supervisor and process layer | AC-5 (supervisor paths), AC-6 (start/stop/recover), AC-14, macOS Node baseline (T3.9), security review 1 (AC-13) | 🔜 not started |
| 4 | CLI and settings | AC-6 (ensure/setup), AC-9, AC-12, AC-11 for the CLI set | 🔜 not started |
| 5 | Packaging, migration, shadow mode | AC-7, AC-15a, AC-16, security review 2 (AC-13) | 🔜 not started |
| 6 | Rehearsal, shadow soak and cutover | AC-10, AC-15b; phase 6 after the rehearsal is run by the user | 🔜 not started |

Phase verification runs the cases marked `gate=phase` at that phase's exit gate, alongside the acceptance criteria above.
Use the reviewed inventory after the remote-control reclassification: replace wholly removed cases with the approved
classification, exempt only the changed assertions in mixed cases, and retain their in-scope Unix-socket, relay and
other checks. Do not run a source case unchanged when it requires the removed behavior. Before requiring the gates,
review the revised classification, assertion mappings, parity matrix and test ownership together. The phase-1 gate
requires the companion command-removal commit and a passing `internal/cli.TestRemoteControlIsUnknownCommand`.
The inventory assigns 151 cases to phase exits: 1, 2, 130, 3, 15 and 0 for phases 0 through 5, respectively. Phases 2–4
have numbered gate tasks; phases 0, 1 and 5 use the exit gate in the table directly.

Current stop point: phase 0 passed its gate and landed on `main` at `ccdc0b6`. Phase 1 implementation is complete;
the lead records phase-gate evidence and lands the branch when it passes. The lead reports
this table at every gate and starts the next phase when its predecessor lands, through T6.0. Stop before T6.1: the shadow
soak, cutover and removal of the old plugin belong to the user.

### Phase 0: foundations

Phase 0 works on branch `port/phase-0` in the main checkout until T0.0 is committed; lanes use worktrees from then on.
The procinfo test fixture now waits for the child's stdin echo before sampling its identity, as checked by
`internal/procinfo.TestChildWithoutReadyEcho`.

| Task | Work | Size / lane | Needs | Done when |
|---|---|---|---|---|
| T0.0 | Inputs every worktree and venue needs. Commit the user's `main.go` and `README.md` unchanged as the first commit of the branch. Commit the seven inventory documents, the review records and this plan under `docs/port/`. Do not commit the `gpt62-*.md` agent definitions; lanes use `~/.claude/agents/` from any checkout. Add `hack/pinned-source`: it materializes the whole repository tree of commit `a8501f251755` (1,005 files, 21.5 MiB) into the ignored directory `.cache/model-gateway-js/`, from a local checkout or from `https://github.com/zchee/eigenwise-toolshed` (public, MIT), and fails unless the root tree hash is `22be5dae8859c7307906f938fa364b7ffadf006c` and the tree hash of `plugins/model-gateway` is `aaf4f26b527c2ff08dbc47707a5f69e6ec2cf40a`. The whole tree is exported because the source tests read three files outside the plugin: `plugins/sidequest/lib/suite-resolver.js` (`test/tool-schema-compat.test.js:8`, which also resolves the repository root at `:96`), `plugins/test-support/windows-hide.js` (`test/windows-hide.test.js:6`) and `docs/src/content/docs/getting-started/model-gateway.md` (`test/gateway-readiness.test.js:219`, `test/rc-compat-mode.test.js:280`). Every source citation of this plan and of the inventories resolves under `.cache/model-gateway-js/plugins/model-gateway/`. Add the source's root `LICENSE` text to `THIRD_PARTY_NOTICES.md`. Start `docs/port/upstream-sync.md`, the ledger of source commits after the pin (empty on 2026-11-25); they are applied only in T5.8 | S / sol | approval | `hack/pinned-source` succeeds in a fresh worktree; the materialized plugin has 33 production JavaScript files with 13,060 lines; `sed -n '2951,2953p'` of its `lib/commands.js`, run from that worktree, prints the stop command's marker write; the three outside files exist in the export |
| T0.1 | Bootstrap on `main.go`; no source behavior. `func main` calling `internal/cli`; cobra root with the 16 source commands registered as stubs that exit 1 with "not implemented", each with cobra's flag parsing switched off; real `version` (section 3.11: linker value verbatim, else released module version without `v`, else `0.0.0-dev+<full revision>` or bare `0.0.0-dev` without a revision; `+dirty` is development) and `completions`; dependencies of section 3.3 pinned; license header from `main.go:1-13` on every Go file; `Makefile` pins `gofumpt` v0.12.0, `modernize` from `golang.org/x/tools` v0.53.0, `goimports-rereviser` v4.2.0 and `golangci-lint` v2.14.0; targets `fmt` (path-scoped), `fmt-check`, `lint`, `test`, `test-process`, `conformance`, `bench`, `tripwire`; `.golangci.yaml` with `gochecknoinits`; `.github/workflows/test.yaml` on `ubuntu-26.04` and `xcode-27` with `actions/checkout@v7`, `actions/setup-go@v7`, `golangci/golangci-lint-action@v9`, a Windows compile-only step and no test step | M / sol | T0.0 | `go build ./... && go vet ./... && make fmt-check lint` exit 0; `GOOS=windows go build ./...` exit 0; the `version` tests are written and compile (they run for the first time in T0.4) |
| T0.4 | Venue tooling (section 3.8): `hack/remote` with `provision` and `negative-control`, `hack/venue-check`, the package `internal/venue` with `Require` (it imports nothing of the product), `hack/lint-venue` with its reject fixture, build tag `process`, `hack/tripwire`, and the make targets wired to them; add the test steps to `.github/workflows/test.yaml` with `hack/venue-check` | M / astra | T0.1 | the user approved the venue; provisioning and the checks of section 3.8 succeeded on it. The Linux version and `internal/venue` tests pass, both negative-control fixtures pass, and `hack/lint-venue` rejects missing and non-first guards for both tag sets. The standalone developer-host shell preflight returns the expected refusal. No local build or process execution is used as evidence for a test |
| T0.7 | Test inventory (data). `test/conformance/classification.tsv` has one row per expanded case: `path`, `line`, `column`, `name`, `class`, `phase`, `package`, `gate`, `note`. The first four fields identify the case. Files under `test/` contain no task, decision or inventory ids. For `gate=task`, `phase` and `package` identify the implementing task in `docs/port/test-ownership.md`: the case can pass when that task is done, but a black-box case is due no earlier than the launch-adapter proof. `gate=phase` means the case needs other work in the same phase and is due at its exit gate. Exempt cases have no phase, package or gate. Classes: `conformance`, `go-port`, `difference:<slug>`, `js-only:<reason>` (reasons: `node-harness-self-test`, `js-source-lint`, `out-of-root-dependency`, `developer-probe`), `unsupported:<reason>` (reasons: `windows-only`, `dormant-code`). Use the section 3.10 slug only as a whole-case difference when every assertion changes; a mixed case is `go-port`, with the changed assertion lines and slug in `note`. Use `js-only` only when every assertion is a JavaScript implementation artifact. User-visible message text is behavior even when the test scans a source file; an in-process `require` does not make a behavioral check `js-only`. Run `hack/baseline-node` through the venue-gated `make baseline-node` via `hack/remote`. Materialize and verify T0.0's whole pinned tree in that venue; run locked `npm ci` in the exported plugin (`ajv@8.20.0`), then its unmodified package test script with the isolation preload (Node ≥ 22.5). Use an allowlisted environment and a PATH on which no real `claude` resolves (`test/picker-guard.test.js:110-130` uses `which`). Record the pin, tool versions, suite exit and terminal runner events keyed by source path, registration line/column and full parent/child name, with status. `hack/baseline-extract` checks the runner output per registration site and stores the census independently of the classification. `test/conformance/launch-sites.tsv` records direct gateway launch sites and their owning cases for T2.0b. For a nested case's launch, a second row with the same coordinates and arguments attributes it to the parent (`via` = `subtest: <child name>`), not to siblings or to another process. `docs/port/parity-matrix.md`: the invariant rows of INV-05 §4 split into single assertions, each naming its tests, with a ledger of the rows in each phase's scope; `docs/port/parity-matrix.lock`: its row ids as approved in review, changed afterwards only with the lead's recorded approval. The difference table of section 3.10 completed. A separate lane reviews the result. Record one row per exempt case in `test/conformance/exemptions-approved.tsv`, with its case key, class and approving review. A `js-only` or `unsupported` exemption is valid only with a matching row; a new or changed exemption needs a new independent review | L / astra | T0.0, T0.4 | all 57 files finished discovery. Of 693 line-start `test(` text matches (662 unindented, 31 indented), 692 register tests; the remaining match is child-suite source in the template literal identified under Source. With five nested `t.test(` sites, this gives 697 registration sites and 832 cases (35 sites expand, including the three `relay-stream-abort` sites that yield six cases, INV-05 §2). Each site reports at least one case; each reported case maps to a site. Classify every expanded case, including skips; T0.7b's `structure` check compares expanded identities against the classification. Record reasons for skipped or individually failed baseline cases; an abort, incomplete registration, missing input, unknown identity or unexplained discrepancy blocks completion. The independent classification and exception review is recorded |
| T0.7b | Test inventory (tools). In the venue `hack/assertion-sites` uses a pinned JavaScript parser and the preserved expanded-case census to produce the expected relation independently of the matrix, keyed by case and assertion source path/line/column. Include nested tests, helper and callback edges, and JavaScript passed to child interpreters or preloads. Under `test/conformance/`, `assertion-pairs.tsv` is generated in the venue from the pin and never edited: each row records the case key, assertion site, call and how the case reaches it. `assertion-overrides.tsv` records independently reviewed manual additions and removals of pairs. `hack/assertions/call-dispositions.tsv` records scoped dispositions and reasons for calls the extractor could not follow, with assertion references for attributed child launches; extraction rejects missing, invalid or stale dispositions. Review generated or unresolved code without evaluating it. `assertion-sites.tsv` is the editable handoff mapping: one row per pair with due phase, target and review. The assertion defaults to the case's phase; it may be due later, never earlier. Assertions on shipped text or configuration are due with packaging in phase 5; for wording in unshipped source documentation use the individually reviewed `source-doc-wording` exemption. In `test/tool-schema-compat.test.js`, calls at `:199`, `:311`, `:321` and `:344` reach the five `refusal` assertions at `:46-50` from three registrations (`:184`, `:285`, `:331`): 15 distinct case/site pairs. For a `go-port` case use a reviewed package-qualified full Go subtest name or `not-ported:<reason>` from the closed list (`js-runtime-artifact`, `harness-internal`, `source-doc-wording`, `changed-by-difference:<slug>`) with the reviewer's approval for that pair. `hack/parity-check` has three modes, each with negative fixtures. `structure`: compare the generated pair set, after reviewed overrides, with the editable mapping's pair set in both directions; check exempt cases against `exemptions-approved.tsv`; require unique keys, classification of every static site and expanded case, matrix coverage of every `conformance` and `go-port` case, exact matrix row ids against the lock, closed reasons and valid difference references. Do not require targets. `mapping <phase> [package]`: select by `phase`, `package` and `gate` using `docs/port/test-ownership.md`; require a nonempty phase ledger; check all `go-port` pairs due in the selected scope by their assertion due phase for the reviewed target or individually approved exemption. For a shared subtest record the reviewed unconditional check for each obligation in the review column; a conditional check needs a distinct observable subtest or recorded assertion event. A negative assertion maps to an observable check that the forbidden effect did not occur. `results <phase>`: require fresh, uncached, non-skipped passing `go test -json` evidence for each due Go target, a passing replacement test for each due difference, and a passing result with an attributed Go launch for each due `conformance` case. Require approval for each due `unsupported` row | M / sol | T0.1, T0.7 | `structure` passes on T0.7's data; each mode rejects its fixtures: a missing expansion, an omitted matrix row, a matrix row deleted while its case stays named by another row (the five `assert.match` calls of `test/gateway-readiness.test.js:221-225` are the model), one pair of a shared helper deleted while another case still covers its site, a missing embedded assertion, a reason outside its list (`structure`); an unmapped pair or unapproved exemption (`mapping`); an absent, skipped, wrong-package or unobserved target (`results`) |
| T0.9 | Test handoff for phase 0: the `go-port` tests that T0.7 assigned to T0.8, T0.2, T0.3, T0.4b (the guard) and T0.5, written from the pinned tests and the inventories by test lanes that implement none of those tasks. The tests fix the package API they call and fail until the behavior exists; each lane fills the target column of `assertion-sites.tsv` for its pairs, and a reviewer other than the author confirms them | M / astra | T0.7, T0.7b | merged on the phase branch; `hack/parity-check mapping phase-0` passes |
| T0.8 | `cli/argscan`: the source's argument scan, usage text, unknown-command behavior and the `--json` log routing (INV-01 §0-§2), used by every command | S / sol | T0.1, T0.9 | the in-scope source argument and usage cases pass as table tests; the removed command's scan, help and completion entries are excluded |
| T0.2 | `paths`, `config`: one table of every environment variable with name, default, parse rule and reader, derived from the pinned source including `bin/` and `hooks/` (INV-04 §1 is a starting point). Exclude `ANTHROPIC_UNIX_SOCKET`: it is a legacy settings key removed during wiring (`lib/commands.js:1187-1196`), not a gateway environment input. The gateway reads `CODEX_GATEWAY_SOCKET_PATH` for its listener (`lib/runtime.js:47-49`). Implement `jsNumber` (INV-04 §12) and home, config and state resolution incl. `MODEL_GATEWAY_STATE_DIR`; generate `docs/env.md` from the table | M / sol | T0.1, T0.9 | a table test covers every variable; `docs/env.md` regenerates without diff |
| T0.3 | `jsonx` (the wire encoder and decoder, the settings file policy, the state-file writer of section 3.5) and `atomicfile` (D1). The Node-generated corpora are produced in the venue by a committed generator script that runs plain Node on JSON inputs (no plugin code), and are committed | L / astra | T0.1, T0.4, T0.9 | wire and state-file goldens match byte for byte; the authored settings pairs pass; a 60 s fuzz run of "settings parse, no edit, write" changes no byte |
| T0.4b | `gatewaytest` environment and fakes as real HTTP servers and helper processes; the product guard of section 3.8 | L / astra | T0.2, T0.4, T0.9 | in the venue: with the guard set, binding 18764, 18765 or 80, running a real `claude` and writing a settings file outside the sandbox each fail with a named error; a helper process started with an empty environment finds no gateway home and no listener; one sample process test runs |
| T0.5 | `procinfo` (argv, birth time, parent, table, liveness, port owner with the probe budget `CODEX_GATEWAY_PROBE_TIMEOUT_MS`, default 2 s) and the pid-record schema of section 3.4 with its atomic writer and reader. Venue tests: renamed, replaced and unlinked running binary; record crash points (after spawn, between the JSON and the decimal file); a non-UTC time zone; an injected start date with a single-digit day | M / astra | T0.3, T0.4b | tests pass on Linux; the Darwin run on `xcode-27` is recorded |
| T0.6 | Measurements of the pinned Node gateway in the venue, with machine, commands and raw samples in `docs/port/baseline-node.md`; `hack/bench`. (a) the three baselines of AC-9; (b) behavior probes: what happens to the provider request on a client disconnect at each stage, direct and relayed (section 3.10); how a client `Upgrade:` request is answered (INV-02 §14 item 21). What the real `claude -p` returns during the pin probe (INV-04 §13 item 4) is not measured: a fake CLI returns only the status it was programmed with, so `pin` is ported from the source's branches, both statuses are unit-tested, and the question stays open (appendix B) | M / sol | T0.4b | the Linux venue run satisfies phase 0: every probe has a recorded observation that later tasks cite; the macOS Node baseline is due at the phase-3 gate (T3.9) |

### Phase 1: pure logic (packages are disjoint; two venue-only jobs are named)

The user approved only the early start of test-only handoffs independent of `procinfo` (§12). The lead separately stacked
those handoffs on the unpublished `port/phase-1` branch; this was not part of the user’s approval. As a further recorded
deviation, the lead started phase-1 implementation there while the phase-0 gate was open. No phase-1 work merges into the
gated phase branch or is pushed before the phase-0 gate passes.

As a further recorded deviation on 2026-11-26, the user instructed the lead to run as many lanes in parallel as possible.
The lead started the worker phase's test-first lanes and the launch-adapter tooling lane before the phase-1 gate, each
in its own worktree on the unpushed phase-1 stack. None of that work is merged into or pushed to a phase branch before
the phase-1 gate passes.

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T1.0 | Test handoffs for phase 1, one per package and one test lane each: the `go-port` tests that T0.7 assigned to T1.1 through T1.14 (T1.12b is withdrawn), with the target column of `assertion-sites.tsv` filled and reviewed; a package's handoff passes `hack/parity-check mapping phase-1 <package>` for its assigned pairs; after all package handoffs merge, `hack/parity-check mapping phase-1` must pass for the entire phase | at most M per package / astra | T0.7, T0.7b, T0.9 |
| T1.1 | `policy`: the model window table as ordered data (every row of INV-04 §3.2, exactly as pinned), policy functions (§3.3), context-window settings and CLI flag parsing (§3.4), sentry policy and trigger arithmetic; tests from INV-04 §3.5 | L / astra | T0.2, T1.2, T1.0 |
| T1.2 | `proxymodels`: listing parse, route table, advertised ids and the Fable respelling (INV-04 §7) | M / sol | T0.2, T1.0 |
| T1.3 | `localprov` response parsers, `proxyconfig`, `install` (installed-plugin registry read, version compare with the collation tie-break) (INV-04 §8, §11) | M / sol | T0.2, T1.0 |
| T1.4 | `pins` (version parse and compare, precedence, catalog-following rule, default env block, ownership of both current and legacy values; writable compat mode removed in T1.14) and `anthropiccatalog` (cache schema, merge, "(previous)" relabel) (INV-04 §5, §9); no process probe | L / astra | T0.3, T1.1, T1.0 |
| T1.5a | Tool-schema corpus job (venue): the pinned `lib/tool-schema-compat.js` and `ajv@8.20.0` under Node produce the expected outputs for the corpus (the source tests' schemas, the counterexamples of `test/fixtures/prior-adapters/`, plus generated cases); the outputs and their generator are committed | S / astra | T0.4 |
| T1.5b | `toolschema`: implement the in-repository syntax check and apply it in the order specified in §3.1, preserving the source's reason codes and JSON Pointers (INV-02 §9.1). Tests consume the committed corpus. | L / astra | T0.3, T1.5a, T1.0 |
| T1.6 | `worker/sentry`: the state table of INV-02 §5 (keys, latch, rearm, stop-reason rule, compaction exemption), overflow parsing, usage merge; timers under `testing/synctest` | L / astra | T1.1, T1.0 |
| T1.7 | `worker/route`: the id-prefix rules of `lib/pins.js:17-28` and the classifier (the 15 rows of INV-02 §2.2, the precedence for a body that is not JSON, the fallback to the default upstream for requests outside the classified routes). Return a refusal with a reason for a decoder depth error or an escaped lone surrogate in a top-level `model` string (§3.10). Publish the model snapshot type (one immutable value swapped atomically; update order as INV-02 §12) with a race test | M / astra | T1.1, T1.2, T1.0 |
| T1.8 | `worker/dispatch`: marker regex, route cache with TTL, LRU and write-on-read persistence (INV-02 §7) | M / sol | T0.3, T1.0 |
| T1.9 | `grok` translation: request, JSON response and stream translation as pure functions over the frame scanner's output (INV-02 §8) | L / astra | T0.3, T1.13, T1.0 |
| T1.10 | `wiring`, `readiness` (pure parts): effective base URL and precedence, env block, ownership of entries, readiness-hook sync for both forms (D2), wired-projects registry (unlocked read/modify/write, last writer wins as in `lib/settings-wiring.js:201-253,317-331`; `internal/wiring.TestAtomicProjectRegistry` pins atomic replacement, not update serialization), automatic repair without endpoint switches, legacy URL replacement and removal by the existing explicit operations under T1.14, recovery policy with the configurable plugin id (INV-04 §6; INV-01 §4.3, §4.4). Use the bounded reader, file-wide duplicate check and exact-existence settings replacement of §3.5. `internal/wiring.TestReportsPreserveLoneSurrogatesLosslessly` pins lossless report values; `internal/wiring.TestCleanModelCacheReturnsFalseOnWriteFailure` pins advisory cache failures | L / astra | T0.3, T1.3, T1.4, T1.0 |
| T1.11 | `lifecycle`: directory lock in the source's protocol, with stale detection decided by a pure function over an injected process-identity source (pid and birth time); stop marker; bounded `lifecycle.jsonl` (INV-03 §6, §7). A `process`-tagged test in the venue runs two real processes that contend for the lock, one of them killed while holding it | M / astra | T0.5, T1.0 |
| T1.12a | `controlauth` (token file, Host/Origin predicates), `upstreamstate` (INV-03 §3.2; `lib/codex-upstream-state.js`) | S / sol | T0.3, T1.0 |
| T1.12b | Withdrawn: implemented, then removed on the user's decision of 2026-11-26. | — | — |
| T1.12c | `catalog` builder and slug rules, with the readiness inputs of `lib/commands.js:1885-1908` as an explicit interface (INV-01 §3.9) | M / sol | T0.3, T1.1, T1.0 |
| T1.12d | `usage` record builders, bounded snapshots, request-body high-water files (INV-04 §10) | M / astra | T0.3, T1.0 |
| T1.13 | `sse`: the raw frame scanner (CRLF and LF delimiters, comments, several data lines, unterminated tail). An untouched frame is re-emitted byte for byte; a rewritten frame has its lines joined with LF and its own delimiter re-emitted as matched; the tail is emitted without a delimiter (`lib/request-worker.js:1675-1721`). Byte-exact cases for a rewritten CRLF frame and for an unterminated tail; a fuzz target | S / astra | T0.1, T1.0 |
| T1.14 | Remove writable compat mode from `internal/pins` and `internal/wiring` (§3.6). Keep ownership recognition of the legacy URL, but never emit it or automatically switch endpoints. Revise `internal/pins/pins_test.go` (`TestEnvBlock`), `internal/wiring/settings_test.go:45-88`, `internal/wiring/ownership_test.go:94-100,144-158`, `internal/wiring/preservation_test.go:40` and `internal/wiring/migration_test.go:88-169`. Preserve read-only ownership and precedence cases in `internal/wiring/migration_test.go:72-79`, `internal/wiring/resolution_test.go:245` and `internal/wiring/registry_test.go:107-149`. Review `internal/readiness/policy_test.go:45-46`: recognition is not proof of service (test line numbers at `7f59bdf`). Independently test explicit rewrite and removal, reconcile preview and confirmation, and automatic repair leaving the effective base URL and unrelated user values unchanged. Apply the shared/local move and removal rules of §3.6 and the alias and socket-ownership guards of §3.10 (D14) | M / astra | T1.4, T1.10, T1.0 |

### Phase 2: walking skeleton, launch adapter, worker

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T2.0 | Walking skeleton. Worker: both modes of section 3.1, TCP and Unix-socket listeners (stale socket reclaimed only on `ECONNREFUSED` and a socket file), `/healthz` shape of INV-02 §1.1 without the RC compatibility fields, `/drain`, in-flight tracking, `HEAD`, `/v1/models` with the fallback list. For a classifier refusal, take a terminal branch before selecting or contacting any upstream, including request-triggered catalog probes, fallbacks or retries. Return HTTP 400, `Content-Type: application/json`, and `{"type":"error","error":{"type":"invalid_request_error","message":...}}` (`lib/request-worker.js:2487-2494,2505-2506`), using a fixed message chosen from the refusal reason, never a parser diagnostic. Log only the fixed reason and status: no request bytes, model text, credentials or headers. Test both reasons with zero requests to every fake upstream, plus byte-exact ordinary non-JSON passthrough. Preserve Node-compatible UTF-8 decoding for routing. Use the worker branch and admission contract below. Supervisor: listener, Host/Origin gate, relay happy path, `/healthz` augmentation, worker spawn with the report and liveness pipes, pid records, SIGTERM/SIGINT orderly stop with lifecycle events, and the test control channel of section 3.1. The supervisor prints one TCP readiness line on its stdout (`lib/commands.js:2879`); a supervised worker sends both stdout and stderr to `logs/shim.log` (`lib/commands.js:2618-2624`). A standalone worker prints its own TCP readiness line on its stdout (`lib/request-worker.js:2693`). The harness waits for one readiness line from whichever command it started (`test/support.js:790-825`). For control checks use canonical header keys, all Origin occurrences, the exact media type and the actual local port. Lead ruling, 2026-11-26: check the Host header as sent on the wire, as the source does (`lib/commands.js:2801-2813`, `lib/control-auth.js:41-43`). For absolute-form targets, Go's `net/http` sets `Request.Host` from the target authority and deletes the Host header before any handler runs, so a handler-only check is not enough. Pin the wire Host behavior with a raw-TCP test against the real listener. Match worker-local and supervisor-control paths on the raw request target cut at `?`, not `URL.Path`; an absolute-form target is not a local or control path and follows ordinary routing subject to the wire Host/Origin gate (`lib/request-worker.js:2361-2368,2383-2416,2435`, `lib/commands.js:2808-2819,2858`). Supply `crypto/rand` at startup and do not serve with an empty token. `serve-shim` and `serve-worker` replace their stubs | L / astra | T0.5, T1.7, T1.11, T1.12a |
| T2.0b | Launch adapter and its proof (Tier 2). The source's unmodified tests must be able to start, observe and stop the Go binary. Preferred adapter: keep `process.execPath` real and redirect only gateway launches, at the harness seams (`test/support.js:766-829`) and at the direct spawn sites that T0.7 listed; second choice: a preload that points `process.execPath` at a wrapper which `exec`s the Go binary for the CLI path; a spawning trampoline only for tests classified as independent of the child's pid. The harness's IPC shutdown reaches the test control channel of section 3.1, built in T2.0; that descriptor is never passed to the worker or the proxy. **Launch-only probes**, written for the skeleton and independent of later features: argv and environment as the harness sends them; the stdout line followed by `/healthz` 200; `health.supervisorPid` equal to the harness's child pid; fixture shutdown over IPC; worker `/drain` with the fixture token answering 202; orderly SIGTERM with lifecycle records (the complete test `gateway-drain.test.js:390-424`); teardown and removal of the fixture home; a copied fake proxy and a fixture that replaces `NODE_OPTIONS` still work. The adapter logs test name, Go pid and build id for every launch; `hack/conformance-launches` exports the metadata and TSV that `parity-check` reads (`docs/port/parity-check.md`). Bound the whole job to a wall-clock limit of 200 s by default and at most 200 s; measured full identity and stand-in runs took 101–102 s. On timeout name the active test files, stop the job's process group and fail. Run the job in its own session, without a child subreaper: the process-isolation tests depend on normal orphan parentage (`test/gateway-process-isolation.test.js:1272-1288,1356-1369`) | L / astra | T2.0, T0.7 |
| | **Decision (lead; recorded before T2.1 and T2.2 start):** every launch probe passes → the `conformance` class stands. Each such case runs unchanged at the gate specified by `test/conformance/classification.tsv`: `gate=task` means the gate of the task that builds the case's package, but no earlier than the launch-adapter proof; `gate=phase` means the case needs more of the phase and runs at its exit gate. Of 254 conformance cases, 133 are due at a phase gate. Of the 177 in phase 2, 6 are due at the adapter's gate, 44 at later task gates and 127 at the phase gate. A probe that cannot pass reclassifies only the tests that depend on that seam to `go-port`. A failing behavioral test is a product defect, never a reason to change the adapter or the class | | |
| T2.1 | Model cache and `/v1/models`: serialize publication at startup and on refresh, using one increasing generation and immutable resolver inputs. Publish sentry policy first, then the matching route snapshot, then expose the cache snapshot and generation together. Never publish a new route with stale sentry policy or hold the commit lock over network work. The request keeps its routing snapshot, but sentry callbacks use the latest published policy. Give `proxymodels.ParseListing` the raw `.data` array, not the envelope or normalized JSON. Pass raw models and cached-row arrays to `anthropiccatalog.WithCached` and raw models and server-option arrays to `WithPrevious`. Pass only classified, payload-free messages to `anthropiccatalog.WriteFailure`. Proxy listing refresh (60 s; retry 1 s doubling to 30 s), fallback catalog, live probes of llama-server `/props` and LM Studio `/api/v1/models`, Anthropic catalog refresh with header credentials kept in memory, picker aliases, discovery-cache sync (off in shadow mode; legacy URL handling in §3.10), shared readiness check (15 s, single flight) (INV-02 §6; INV-04 §8). Bind background work to worker lifetime: `NewSharedCheck(workerCtx, run, clock)` with `Latest()`; `Cache.Start(workerCtx)` and `TriggerAnthropicRefresh(incoming http.Header)` without a request context. The worker adapts its context-bearing hook with a closure; `ModelsResponse(ctx, headers)` still uses the request context to wait for the first fill. Apply `modelcache.MaxAnthropicPageUTF16Units` to decoded UTF-16 code units (`lib/anthropic-catalog.js:42-46`). Test pending: `internal/readiness.TestSharedCheckWorkerLifetime`, `internal/worker/modelcache.TestRefreshPublicationOrderAndOverlappingRefreshes`, and `internal/worker/modelcache.TestAnthropicCatalogAndModelsEndpoint` | L / astra | T2.0b, T1.3, T1.4 |
| T2.2 | `worker/upstream`: header sets of INV-02 §3.2 and the transport rules of section 3.1. Keep `Client.Do(ctx, req)` and bind the prepared request once as `upstream.Send`, a `func(context.Context) (*upstream.Response, error)`. `NewTransport` shares safe transport configuration between inference and a separate nonnil Grok auth transport; auth uses its own client, no environment proxy, redirects or automatic decompression, and never uses inference or incoming request headers. Live bodies have one reader and one owner responsible for closing them. Keep parsed capture headers separate from a cloned, normalized relay set. Pass the header values to `upstreamstate` without pre-trimming: it uses the first Retry-After value, does not trim before the date parse, and joins duplicate reset headers as Node does. Test pending: `internal/worker/upstream.TestNewTransport` and `internal/worker/upstream.TestTransportShutdownBoundsRequest` | M / astra | T2.0b |
| T2.3 | `worker/reply`: the buffered response branches R1-R6 and the JSON half of R7 of INV-02 §4.1, 413 normalization (genuine and legacy overflow), Codex auth-failure and empty-completion mapping, the rejected-WebSocket-upgrade retry (2 retries, 250 ms), 502 bodies. `reply.Handle` consumes a bound `upstream.Send` and either returns terminal facts or transfers one open stream to the relay. Reply alone encodes and writes request-side 413s through `WriteSentry`; sentry owns policy state, not response encoding. Apply LM Studio cache correction before observing successful proxy JSON. Normalize genuine 413s only on the proxy path with sentry enabled. Keep the earlier proxy 403 handling, including the rejected-upgrade retry, ahead of legacy overflow. Apply legacy normalization only when the response reaches that branch (status ≥400 other than 413), after the earlier response handlers and the empty-completion check. With sentry disabled, normalize the recognized errors but do not latch or learn. A usable limit from an engine-normalized error suppresses learning; it does not suppress learning from a genuine 413. Do not expose the original text of normalized legacy or engine errors | L / astra | T2.2, T1.6 |
| T2.4 | `worker/sse` on the frame scanner of T1.13 owns the per-attempt tracker, transformer and shared `PeekFirstEvent`. A successful or timed-out peek transfers one open replay body with the original bytes; a terminal failure closes it. The peeker does not write a status or count bytes. Relay with a flush per chunk, 20 s heartbeat on proxy streams only, model rewrite and plan-tool filtering per event, side-branch decoding for observation with the 4 MiB budget, first-event peek that replays the original bytes. Decode the whole frame as Node does on a rewrite, including surviving comments and metadata; preserve raw bytes on passthrough only. Apply LM Studio cache accounting before passing proxy usage to sentry; merge observations per attempt and report a completed turn only at a deliverable normal end. For an eligible successful LM Studio stream, peek the first data event before headers and normalize engine overflow only if that event decodes to the matching error. Do not scan for later errors. Otherwise replay the peeked bytes. Do not observe Anthropic passthrough in sentry. This task covers the streaming half of R7 and the untouched passthrough R8, which completes Anthropic forwarding (INV-02 §4.2-§4.5); client-disconnect behavior as measured in T0.6. On native Anthropic passthrough, preserve back-pressure without eager provider close, bound by service shutdown, with no unmeasured expiry deadline. Test pending: `internal/worker/sse.TestLMStudioFirstEventPeek` and `internal/worker/sse.TestProxyRelaySentryAccounting` | L / astra | T2.2, T1.13, T0.6 |
| T2.5 | Proxy branch request side: dispatch resolution, deferred-tool hoisting (INV-02 §3.3), schema compatibility, plan tools, thread refusal, sentry check; `count_tokens` and the path rules of INV-02 §2.3. Pass dispatch an absolute state path resolved against the injected startup working directory; keep one cache for the worker lifetime. Pass the header-derived session and optional agent through Unicode validation before lossy conversion. Apply the sentry switch on the exact proxy `/v1/messages` path. `proxyreq.Handler.Prepare` returns route/audit facts and prepared bytes separately from the original body; it writes only branch-local 400s. The worker admits the request, checks sentry on the original bytes, and calls reply to write any synthetic 413. A schema compatibility refusal returns HTTP 400 before admission or upstream contact. Test pending: `internal/worker/proxyreq.TestSentryOriginalBytesOverHTTP` and `internal/worker/proxyreq.TestToolSchemaEngineQuirkHTTPRefusal` | L / astra | T2.3, T1.5b, T1.8 |
| T2.6 | `worker/compactguard`: buffering, retries only while the client is writable, outcomes (INV-02 §4, §11). Observe per-event usage (low observations can rearm); reuse the relay's attempt tracker and transformer, and call the shared peeker only before headers commit. Pass each attempt through buffered reply handling, the first with headers uncommitted and retries with headers committed. Never mark a guarded attempt ordinary-completed | M / astra | T2.4 |
| T2.7 | Grok branch: credential file, single-flight refresh, forwarding and translation; shadow token policy in §3.10. Translate and encode a non-stream response before committing response headers; use the fixed sanitized 502 on translation or encoding failure (§3.10). Admit after thread validation, then apply sentry to the Grok messages prefix on original bytes before auth or upstream contact. Reply alone writes a request-side 413. For JSON, report usage as ordinary-completed only when the upstream status field is `completed` and the request is not compaction. For SSE, observe each frame's usage without completion; report final usage only for a deliverable stream that saw `response.completed` and no error, and mark it ordinary-completed only for a non-compaction request. As in the source, observe translated usage and finish the route span but emit no usage record: `forwardGrok` never finishes the capture (`lib/request-worker.js:1822-1825,2244-2359,2635-2644`; `lib/usage-observability.js:878-894,1023-1042`). Test pending: `internal/grok.TestBranchStream` | M / astra | T2.2, T2.4, T1.6, T1.9 |
| T2.8 | Usage observability, route telemetry (OTLP/HTTP JSON to loopback only), request-route log, request-body high-water files (INV-02 §11; INV-04 §10). Alias route, overflow, compaction and audit values to `worker/outcome`; the worker owns terminal publication after observation, not the branch or relay. Finish one route span per traced client request, once at terminal completion; internal retries do not add spans (`test/gateway-telemetry.test.js:293-335`: three client requests, four upstream attempts, three spans) | L / astra | T2.4, T1.12d |
| T2.9 | Phase gate: every in-scope case assigned to phase 2 by the revised test inventory, in its reviewed class, plus the Tier 1 integration suites. Keep in-scope assertions of mixed cases; no test requires the removed listener, control endpoint or DNS bypass | M / astra | T2.1-T2.8 |

**Worker branch contract (T2.0).** `worker/outcome` is a leaf of values only. The branch supplies resolved route and
optional audit facts. Reply sets overflow facts; the compaction guard sets compaction facts. The component that settles
the response returns status and any span-status override; the branch attaches those facts without losing resolved route
metadata. `Contacted` means an inference send was invoked, including a failed dial, not admission. `Cancelled` means
client cancellation won; it suppresses terminal route publication. An unbound dispatch can return a log-only audit
without admission; accepted audits are delivered at admission and not repeated at completion.

```go
type Forward func(http.ResponseWriter, *http.Request, route.Snapshot, route.Decision, []byte, func(outcome.Route, *outcome.Audit)) (outcome.Result, error)
```

Each branch calls its request-bound admission callback at most once: after local validation, before request sentry or
an accepted Anthropic request's catalog refresh. The handler increments the provider's `/healthz` `served` counter
at admission, not completion or upstream contact. A synthetic sentry 413 counts; thread, schema and unbound-dispatch
refusals do not (`lib/request-worker.js:2551-2568,2620-2634,2648-2650`).

After bypassing local endpoints and HEAD, invoke `BeforeBody func(*http.Request) func(outcome.Result)` before the
first body read. The returned finalizer receives one terminal outcome, including cancellation during the body read;
it does not run per attempt. The hook starts observation, not upstream or catalog work (`lib/request-worker.js:2420-2429`).
Publish terminal facts before propagating `http.ErrAbortHandler` at the outer HTTP boundary. Do not report a client
cancellation as a status or emit a span after it. The request context tracks client liveness; the worker's service context
owns upstream dialing and reads.

Test pending: `internal/worker/outcome.TestOutcomePresence`, `internal/worker.TestClassificationRefusalHTTP`,
`internal/worker.TestRequestStartBeforeBody`, `internal/worker.TestDrainWhileForwarding`, and
`internal/worker.TestAbsoluteRequestTarget`. Test pending: `internal/supervisor.TestListenerHostAsSent` and
`internal/cli/serveshim.TestForegroundSupervisor` cover the real listener and command wiring.

### Phase 3: supervisor and process layer

Before phase 3 starts, complete the test-first handoff that fixes the pid record's start-token representation (§3.4).
A freshly started child can be alive before its executable identity is complete. Supervisor callers must re-sample
within their own deadline and verify that the sample belongs to the same process, not just the same numeric PID.
Treat an unknown identity as "do not act," never as absent. `internal/procinfo.TestIdentityDuringExec` pins unknown for
missing argv and recovery of the identity when argv appears; `internal/procinfo.TestChildWithoutReadyEcho` pins the
fixture's readiness ordering, not production retries.

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T3.1 | `ownership`: record publication and reconciliation by the supervisor, the classes and the authority rule of section 3.4, the authenticated `/internal/stop`, kill primitives (SIGTERM, 3 s, re-check, SIGKILL; success judged by the process being gone), two-instance and missing-record tests | L / astra | T0.5, T2.0 |
| T3.2 | `supervisor/proxyrecovery`: the decision sequence of INV-03 §4.6, backoff `min(30 s, 1 s·2^(n-1))`, 30 s grace, fresh-connection confirmation, single flight, D4, and the unmanaged mode used by shadow | L / astra | T3.1 |
| T3.3 | `supervisor/relay`: the full state machine of section 3.1, client-disconnect behavior as measured in T0.6, the 64 MiB cap with the 250 ms grace on 413, upload-abort paths, 503 bodies; `test/shim-request.test.js` ported case by case; a fake worker and a fake upstream that count requests | L / astra | T2.0 |
| T3.4 | `supervisor/workerproc`: respawn with D5, restart with drain and kill at the drain timeout, `/restart` with the candidate rule (D11), birth records and orphan reaping (D6) | L / astra | T3.1, T3.3 |
| T3.5 | Startup-failure file: clear it when the supervisor begins listening. On main-listener bind failure, record the pid, generation and remedy; on a fatal supervisor error, record the pid, generation and reason (INV-03 §2, §4.1, §11). The readiness wait only accepts a failure for its launch (§4.2 of INV-03). In an isolated fixture without a hosts block, require that nothing answers on the former compatibility port. With a fixture holding that port, require HTTP 200 from primary `GET /v1/models`. Add the named Go targets for these deferred assertions to the `remote-control-not-ported` replacement-test list when the tests are written | M / astra | T3.3 |
| T3.6 | `launch`, `shimstate`, and the commands `start`, `stop` (D9), `recover`: the plan table of INV-01 §3.3, readiness wait (300 ms poll; 12 s quiet, 42 s otherwise) | L / astra | T3.2, T3.4, T3.5 |
| T3.7 | Phase gate (Tier 2): conformance set or fallback tests for the lifecycle files; ownership matrix of AC-6 for `start`, `stop`, `recover`; relay counts of AC-14; assert that a fixture with the Node-managed hosts block starts no compatibility listener and the old control path has no special handling; macOS Node baseline (T3.9) | M / astra | T3.6, T3.9 |
| T3.8 | Security review 1 (grok lane): control token, Host/Origin gate, relay, pid-record trust | review | T3.7 |
| T3.9 | Node baseline on macOS. Port the Linux-only gate and ephemeral-port checks to Darwin (`sysctl net.inet.ip.portrange.*`; `hack/bench:8-13,250,316-318,347`); adapt invocation metadata, machine inventory (`lscpu`), timestamps and GNU `timeout` (`hack/bench:203-216,226,625-628,686-700`). Replace `ss` and `/proc` listener, liveness, parent and `VmRSS` reads with macOS probes (`lsof` field mode with a canary, `ps -o ppid=`, and `ps` RSS; `hack/bench:390,413,440-446,500-502`); fit paths to Darwin's 104-byte Unix socket limit (`hack/bench:352,723`), and publish the evidence frame only after the required cleanup checks (`hack/bench:663-665,685-705`). Acceptance: hosted `xcode-27` run through the guarded `bench` target with exactly Node v24.11.1 and verified pinned tree hashes. Run all T0.6 measurements and behavior probes. Require the readiness and TTFB samples, three 60 s idle supervisor/worker RSS observations from `ps` (label the Darwin metric, not Linux `VmRSS`), all 16 disconnect cases and both Upgrade captures. A missing required observation fails acceptance. Always retain attempt-specific diagnostics and any partial samples and machine metadata actually collected in an artifact; mark failed attempts incomplete. Publish and commit a baseline only after successful cleanup and a complete run. Commit macOS samples under `docs/port/baseline-node-samples/darwin/` with their checksum in `docs/port/baseline-node.md`, and state the §3.8 containment contract in the record. | M / sol | T3.4 |

### Phase 4: CLI and settings

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T4.0 | `selfinstall` core: install the running executable at the stable path (the replacement algorithm of section 3.4; verified `--version`; the sweep rule of section 3.4), refusal for unversioned builds | M / astra | T3.1 |
| T4.1 | `release`, `setup` without migration: latest-release fetch with the token rule and cross-origin `Authorization` drop, checksum fail-closed, extraction with `archive/tar` + gzip, binary swap with rollback, the steps of INV-01 §3.1; on a home with a legacy install `setup` refuses and names `migrate`; skip the automatic env write when the project is wired in legacy mode. `setup` owns launcher publication before readiness-hook migration and emits its wiring diagnostics, not `internal/wiring` | L / astra | T3.6, T4.0, T1.14 |
| T4.2 | `ensure` (the `--quiet` contract of INV-01 §3.2: one JSON object, or empty stdout with the notice on stderr), `status`, `models`, `login`. `ensure` may repair other owned entries, but skips the automatic env write in legacy wired mode and never treats the legacy URL as proof of service. `ensure` owns launcher publication before readiness-hook migration and routes repair notices to its normal output channels | L / astra | T3.6, T1.14 |
| T4.3 | `env` (print, `--write-project`, `--write-user`, `--remove`, `--reconcile`, retired flags exit 2). On a home with a legacy install and no committed migration, refuse each of the four mutating operations before any write and say "run migrate" (§3.6). The existing explicit writes replace an owned legacy URL with the default loopback URL. Project removal clears owned entries in both project files subject to the user-file alias guards; socket ownership follows D14. Reconcile previews conflicts against the configured default URL and removes them only after confirmation (§3.6). Never edit the shell environment. Omit the source's advice to enable remote-control. `env` owns launcher publication before the settings-write path (not the absent-file removal) and emits the write, removal and reconcile messages, including stderr routing under `--json`; `internal/wiring` does neither. Also port `pin` with the isolated `claude` probe (INV-04 §5.6), `context-window` with `autoCompactWindow` sync, and tests for wiring state under the override (§3.10). Pin validation reports the reason without echoing the rejected model ID | L / astra | T1.14 |
| T4.4 | `catalog`, discovery-cache writer, `doctor` (the sections of INV-01 §3.6 except RC runtime diagnostics, plus build info, state-dir override, legacy hooks and the installed hook form), `pickerguard`. Check the hosts block and all legacy URL definitions without changing them. Report separate remedies, retain mixed-mode detection, and apply the exit rules in §3.6. For the deferred detector assertions, use only the fixture hosts-file override. Check a valid marked block, an absent block and a missing override path. Do not fall back to the system hosts file when the configured path is missing. An unmarked mapping is not proof of a managed block. Assert that the detector never writes the hosts file. Add the named Go targets for these assertions to the `remote-control-not-ported` replacement-test list when the tests are written | L / astra | T2.1, T4.3 |
| T4.5 | Dropped on the user's decision of 2026-11-26: `remote-control enable/disable/doctor` is not ported. | — | — |
| T4.6 | `readiness-gate`: stdin payload, policy, healthy fast path as in the source (`lib/readiness-gate.js:10-32`), budget D7, exit 2 only for UserPromptSubmit. A readiness-gate repair never switches endpoints. For the `legacy` policy outcome, do not recover or claim readiness; an explicit disable still wins. The command emits the source's applicable stderr diagnostics, including repair notices, not `internal/wiring`. Test the legacy policy outcomes at the command boundary (§3.10) | M / astra | T3.6, T1.14 |
| T4.7 | Phase gate (Tier 2): `testscript` suites for every ported command (AC-12), conformance set or fallback tests for the CLI files, ownership matrix of AC-6 for `ensure` and `setup`, including that both skip the automatic env write in legacy wired mode | L / astra | T4.1-T4.4, T4.6 |

### Phase 5: packaging, migration, shadow mode

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T5.1 | Trampolines, breadcrumb, `hook session-start` with the publisher rule of section 3.6 (section 3.11); no hook switches endpoints, including when the legacy URL is effective or shadowed; tests for both launchers: arguments, stdin, exit status, signals, missing binary | M / astra | T4.1 |
| T5.2 | `plugin/` and `.claude-plugin/marketplace.json`; the `model-gateway` skill rewritten for the Go CLI with the applicable statements of appendix A corrected; cold-install test (AC-15a) | M / sol | T5.1 |
| T5.3 | `migrate` forward and rollback with the journal (section 3.6). Run the legacy compatibility checks read-only before the first step and report both findings as in §3.6; only the hosts-block finding refuses. Migration writes only readiness-hook entries in settings and never changes endpoints, the hosts file or shell environment. Tier 2 tests in an isolated home holding the pinned Node plugin, with its supervisor started and stopped by the Node CLI (AC-7) | L / astra | T5.1, T5.2, T4.4, T4.6, T2.0b |
| T5.4 | Shadow mode, `shadow preflight`, `soak.jsonl` (section 3.7); test that removing the proxy under a shadow instance never makes it spawn or signal anything (AC-16); `shadow report` with fixtures that satisfy and violate each check of AC-10 | L / astra | T3.2, T2.1, T2.7, T2.8 |
| T5.5 | Only if OD-5 chooses release archives: goreleaser v2 config, `.github/workflows/release.yaml` with `goreleaser/goreleaser-action@v7`, `self update` | S / sol | T4.0 |
| T5.6 | Docs after the differences are tested: `README.md` (extends the user's stub), `docs/differences-from-node.md`, `docs/port/parity-matrix.md`, the final parity matrix | S / sol | T4.7, T5.3, T5.4 |
| T5.7 | Security review 2 (grok lane): release download and verification, self-install, launchers, hook installation, plugin wrapper, migration, shadow mode | review | T5.2, T5.3, T5.4 (and T5.5 if it exists) |
| T5.8 | Upstream sync, after parity is shown against the pin: triage and apply the source commits that `docs/port/upstream-sync.md` lists by then (none on 2026-11-25), each with its tests | S / sol | T5.6 |

### Phase 6: rehearsal, soak, cutover

| Task | Work | Who | Needs |
|---|---|---|---|
| T6.0 | Rehearsal (Tier 2): in one isolated home with the pinned Node plugin installed, follow the runbook steps with fresh CLI processes and the hook commands of both plugins invoked as the host invokes them: rehearse hosts-block-only, legacy-URL-only, both and neither, including shadowed settings and a process definition. Check that diagnosis is read-only and each finding names its remedy. Apply the hosts-block remedy first and use the Node CLI for the legacy settings URL remedy (correct any process definition in the shell), then stop Node, migrate, roll back, start Node, migrate again | astra lane | phase 5 |
| T6.1 | Shadow soak with real sessions (AC-10); the runbook with complete commands is written at that time | the user | T6.0 |
| T6.2 | Cutover in a maintenance window, from the runbook written at that time with complete commands: close all Claude Code sessions; check for the Node-managed hosts block and effective or shadowed legacy URL definitions; apply their separate remedies as in §3.6; disable the old plugin; stop the Node gateway with its own CLI; `claude-code-model-gateway migrate --start`; install and enable the new plugin; `doctor`; start Claude Code and fill in the AC-15b checklist. Rollback: `model-gateway migrate --rollback`, then the steps it prints | the user | T6.1 |
| T6.3 | Needs separate approval each: change `gateway-switch` to call the binary; uninstall the old plugin; remove the trampolines | the user decides | T6.2 |

---

## 5. Acceptance criteria

Evidence for every criterion is command output stored by the verifier lane under `.omc/artifacts/verify/<phase>/`
(`go test -json` output, the conformance run's TAP and per-test launch log, benchmark samples). A model's statement that
something passed is not evidence. A test counts only when it was executed, passed and was not skipped.

| # | Criterion | Check |
|---|---|---|
| AC-1 | `go build ./...`, `go vet ./...`, `golangci-lint run`, `make fmt-check` (formatters change no file of a clean checkout) and `GOOS=windows GOARCH=amd64 go build ./...` exit 0 | on the developer machine (darwin/arm64; these commands execute no product code) and in the Linux venue at every gate; in the CI workflow on `ubuntu-26.04` and `xcode-27` at every gate, from the pushed `port/phase-N` branch |
| AC-2 | Isolation. (a) `hack/venue-check` applies the predicate in section 3.8. Hosted CI runs `hack/venue-attest-ci` first and then applies the same predicate, with no exemption. Record the end-to-end shell refusal for missing attestation and separately for each protected make target: non-zero with "not a test venue", no build, test or Node command dispatched. For the other prerequisites (home lookup, unclean home, invalid marker, existing state, busy ports and reader errors), record the passing `internal/venue` `TestCheck` subtests in the venue and on both hosted runners. These Go fixtures do not exercise the shell script's own OS adapters. On the developer Mac exercise only the standalone shell check. (b) Every package with tests has a `TestMain` whose first statement is `venue.Require()`, under the default and the `process` tag (`hack/lint-venue` rejects missing and non-first guards). Record the positive and negative binary controls for both tag sets in section 3.8. No test binary starts on the developer machine, and there is no local test target. (c) In the venue, with the guard variable set, binding 18764, 18765 or 80, running a real `claude`, and writing a settings file outside the sandbox each fail with a named error; a helper process started with an empty environment finds no gateway home and no listener. (d) Compare the same developer-machine paths by the same digest rules as the baseline in section 3.8; the manifests must match. Phase 0 uses the manual manifest and reports the interval before capture as unverified. Later phases use the committed tripwire before their first task and at their gate; report any interval without a baseline as unverified. This detects, it does not contain | T0.4 and T0.4b tests; `make tripwire` |
| AC-3 | The 15 classification rows of INV-02 §2.2 and the 10 policy examples of INV-04 §3.5 hold as table tests (phase 1). One HTTP test per branch shows which fake (Anthropic, proxy, Grok) received the request (phase 2). Exercise both classifier refusals over HTTP. Each refusal terminates before any upstream selection or contact, including request-triggered catalog probes, fallbacks or retries. Require zero requests to every fake upstream, status 400, `Content-Type: application/json`, and `{"type":"error","error":{"type":"invalid_request_error","message":...}}` with the fixed message for the reason, never a parser diagnostic. The refusal log contains only the fixed reason and status, not request bytes, model text, credentials or headers. An ordinary non-JSON body reaches the default upstream byte for byte; malformed raw UTF-8 in a model string is decoded and routed as in Node | `worker/route`, `policy`; `worker` |
| AC-4 | Passthrough fidelity for 12 named bodies: 4 MiB valid JSON; two top-level `model` members whose last value selects Anthropic; nested duplicate keys; ill-formed UTF-8 in a string; not JSON; empty; an integer above 2^53; `1e400`; an escaped lone surrogate outside the top-level `model` with no model selecting a non-default route; CRLF-heavy formatting; a leading byte-order mark; JSON sent to a path outside `/v1/messages`. The SHA-256 of what the fake Anthropic upstream receives equals that of what was sent. Sentinel values placed in `authorization`, `proxy-authorization`, `x-api-key`, `cookie` and the control-token header appear in no header or body received by the fake proxy or the fake Grok endpoint; the control-token sentinel reaches no fake at all | `worker/upstream` integration tests |
| AC-5 | Streams. For the worker path with Anthropic and proxy upstreams and for the supervisor path: when the fake upstream closes mid-stream, the client has status 200 and the earlier data, and its HTTP response is incomplete (transport error, not a clean end) within 1 s (`test/relay-stream-abort.test.js:84-87,119-122,172-175`). For Grok the translated stream ends as the source ends it (`lib/grok-backend.js:384-389`). An uninterrupted stream delivers its terminal event. `goleak` reports nothing. 20 concurrent compaction streams each deliver a complete terminal event (`test/compact-stream.test.js:670`) | `worker/sse`, `supervisor/relay`; source tests at their assigned gates |
| AC-6 | Ownership matrix below. Each decoy is checked for survival and unchanged birth time after the command (a count of delivered signals is kept in addition) | Tier 2: T3.7 (start, stop, recover), T4.7 (ensure, setup), Linux and macOS |
| AC-7 | Migration in an isolated home that holds the pinned Node plugin (Tier 2). The rehearsal distinguishes hosts block only, legacy URL only, both and neither, including effective and shadowed file definitions and a process definition. It reports the correct independent remedies and leaves the hosts file, all settings and the shell environment unchanged during diagnosis. Before its first step, `migrate` checks both legacy findings read-only and prints each with its remedy. Under the lead ruling of 2026-11-26, a hosts-block finding refuses: the gateway would resolve `api.anthropic.com` to loopback for its own upstream requests after start. A legacy-URL finding alone is reported and does not stop an otherwise eligible migration. The operator applies any endpoint remedies separately. Migration itself changes only readiness-hook entries in settings. Refusals that change no file: Node-managed hosts block present; Node supervisor still listening; old plugin not disabled; a journal in progress for the other end state. After the Node CLI's `stop`: `migrate --start` ends with the Go supervisor answering `/healthz`, no stop marker, exec-form hooks, both launchers as trampolines; `migrate --keep-stopped` ends with the marker present and nothing listening. In both, every settings file differs from its pre-image only in readiness-hook entries. A legacy `node <launcher> readiness-gate` then exits 0 on a healthy gateway. `migrate --rollback` restores every untouched file, the stop marker and the previous stable binary byte for byte, keeps an edit made after migration and reports it, starts nothing, and prints the commands that re-enable the old plugin and start the Node gateway. A process kill injected before and after each journaled effect leaves a home from which rerunning the same command completes. The hook commands of both plugins, invoked as the host invokes them, behave as documented before migration, after it and after rollback | T5.3, T6.0 |
| AC-8 | `hack/parity-check structure` passes globally at every gate: `docs/port/parity-matrix.md` (T0.7) holds every in-scope row of INV-05 §4 split into single assertions and records the approved dispositions for removed assertions, its row ids equal `parity-matrix.lock`, and every `conformance` and `go-port` case of `classification.tsv` is named by at least one row. Run `mapping <phase> [package]` (select by `phase`, `package` and `gate`) and `results <phase>` over the nonempty due ledger only. Each due row is `passed` (every named test ran, passed and was not skipped), `difference:<slug>` (the replacement test passed), or `unsupported:<reason>` with a listed reason and reviewer approval. Independently of the matrix, each `go-port` pair in `assertion-sites.tsv` needs passing evidence for its reviewed target or an individually approved `not-ported` reason when its assertion is due (T0.7b), not necessarily when the case is due. Do not assign Go-launch conformance evidence before the adapter decision at T2.0b. Later rows may remain open until their assigned gate. At phase 5 also check every earlier ledger and require no open row | every phase gate |
| AC-9 | Not slower than Node, both runtimes measured in the same job on the same runner with `hack/bench` (T0.6), after one warm-up run: (a) healthy `readiness-gate` p95 wall time over 50 runs; (b) p99 time to first response byte over 200 runs at the client, through supervisor and worker, for a 1 MiB request answered by a 2,000-event SSE stream; (c) summed resident memory of supervisor and worker after 60 s idle. Each Go value ≤ the Node value; raw samples are kept | phase 4 gate |
| AC-10 | Shadow soak, at least 20 sessions or 3 days. `shadow report` exits 0, which requires, from `soak.jsonl` alone: request records for at least one Anthropic model, one Codex model and one compaction; no `gateway-fault`; every upstream submission after a request's first carries a cause from the closed list of section 3.7, and a `relay-resend` only when the request's delivery list names two worker generations with a `worker-exit` record of the first between them; replaying the state table of INV-02 §5.3 over the request records of each session and model key reproduces every recorded sentry state and refusal: a request is refused exactly when it is a non-compaction request on a path the sentry covers and the state before it was armed-high (the last usage observation exceeded the recorded trigger and nothing had fired since), and a compaction request is forwarded in every state (`lib/request-worker.js:1360-1364`); every `worker-exit` and `supervisor-exit` record is preceded, within the same supervisor generation, by a `stop-request` or `restart-request` record | the user, T6.1 |
| AC-11 | Every source test case, identified by file, static site and full expanded name, has exactly one class in the reviewed `classification.tsv`. No gate requires a removed remote-control assertion. A mixed case retains the assertions about behavior still in scope, under its reviewed class. `conformance`: it passed at the task or phase exit specified by the `gate` column and `docs/port/test-ownership.md`, no earlier than launch-adapter proof, and the launch log attributes at least one Go launch to that test name. `go-port`: its named Go test passed and was authored before the implementing task by a lane other than the implementer, and every assertion site of the case is mapped or individually exempted by its due phase (AC-8). `difference:<slug>`: the replacement test passed. `js-only` and `unsupported`: the reason is in T0.7's lists and the T0.7 reviewer approved the row. A test leaves `conformance` only through the decision of T2.0b or with the lead's recorded reason that names the missing seam | `make conformance`; `hack/parity-check` |
| AC-12 | CLI contract in `testscript`, one scenario per in-scope row of the INV-01 §3.2 outcome table and of INV-01 §1: `ensure --quiet` exits 0 for the wrapper notices, a missing binary, a cut-short wait, a failed start and completion, and 1 for a lock timeout, a thrown error or a fatal error; its stdout is one JSON object when a notice or log line was buffered and empty for the wrapper notices, with the expected text asserted per scenario. `catalog --json` writes exactly one JSON document to stdout. Retired `env --mode` and `--show-mode` exit 2. An unknown command prints the usage and exits 1; no command exits 0. Assert that an unknown flag is ignored even when a positional argument follows it, only for the commands that scan for flag presence (`setup`, `login`, `ensure`, `catalog`, `env`). `pin` and `context-window` retain the source's option/value validation and reject unknown options. `remote-control` has the ordinary unknown-command result and is absent from help and completions; `env` does not advise enabling it. On a legacy home without committed migration, the Go `env --write-project`, `--write-user`, `--remove` and `--reconcile` operations each refuse without writes and say "run migrate" (§3.6). Assert that the existing explicit write commands replace legacy URLs with the default in their applicable scopes; removal clears legacy owned entries; reconcile previews and removes only on confirmation; and automatic repair leaves endpoints unchanged. `doctor` covers the four legacy states and shadowed file and process definitions, reports separate remedies without changing them, exits 1 for either legacy finding and retains all six source failure checks, including mixed mode (INV-01 §3.6) | T4.7 |
| AC-13 | Both security reviews report no open finding of high or critical severity at the reviewed commit; fixes are re-reviewed | grok lane reports |
| AC-14 | Relay, with a fake worker and a fake upstream that count separately. Transient failure before delivery, then a worker appears: one worker delivery, one upstream submission. Permanent failure before delivery: no delivery, a retryable 503 (`test/shim-request.test.js:194-206`). Delivered to a worker that stays current and loses the connection: no second delivery, 503 with `x-should-retry: false` (`:107`). Delivered to a worker that is then replaced: delivered again to the new worker (`:84`). Worker death after its headers: client connection reset, no second delivery (`:210`) | `supervisor/relay` |
| AC-15a | Install path, simulated. In a fresh isolated home with no Node on `PATH` and no stable binary: the plugin's SessionStart hook command exits 0 and prints one JSON object with the install commands. After build and `setup` against a fake release server and fake proxy: the stable binary exists, `env --write-project` writes exec-form hooks, `readiness-gate` exits 0, `doctor` exits 0. A kill at each point of the stable-binary replacement (before the hard link, between link and rename, after the rename) leaves a working binary at the stable path. Installing a higher version replaces the binary and the supervisor. Setting the plugin id to `false` makes the gate exit 0 without starting anything | T5.2 (phase 5 gate) |
| AC-15b | Install path, real host. The user's checklist at cutover records that Claude Code lists the marketplace, loads the plugin and the `model-gateway` skill, and runs the SessionStart hook | T6.2 checklist (phase 6) |
| AC-16 | Shadow isolation, observed with a process-start log and a file-system snapshot of everything outside the shadow state directory: while the proxy it dials answers, requests succeed; after that proxy is killed, the instance reports `proxy-down`, starts no process, sends no signal and changes no outside file; every command outside the allowed list is refused; `shadow preflight` lists only paths under the shadow state directory, the gateway ports and the one proxy dial | T5.4 |

Ownership matrix for AC-6. Precondition of every row: the project is wired, the plugin id is enabled, there is no stop
marker, and the fake proxy binary is installed. (With the plugin disabled, `recover` and `ensure` succeed without acting,
`lib/commands.js:571-585,633-637`; that is a separate scenario of AC-12.)

| State | `start`, `recover` | `ensure` | `setup` | `stop` |
|---|---|---|---|---|
| identified foreign listener on the gateway port | refuse, exit 1, owner named | refuse; `--quiet` exits 0 with the notice | refuse, exit 1 | refuse, exit 1; the stop marker is written (source behavior) |
| second instance of the same binary on the gateway port (other state root) | as above | as above | as above | as above |
| listener on the gateway port whose pid cannot be resolved, or a probe that times out | refuse with the unknown-owner text | as above | as above | as above |
| healthy foreign listener on the proxy port | gateway ready; proxy used | same | same | exit 0; proxy alive |
| unhealthy identified foreign listener on the proxy port | gateway ready; recovery `foreign-port-owner` | same | same | exit 0; proxy alive |
| unhealthy listener on the proxy port, owner lookup completes without a pid | gateway ready; recovery `port-owned` (INV-03 §4.6 rule 9) | same | same | exit 0; proxy alive |
| unhealthy listener on the proxy port, owner probe times out | gateway ready; recovery `owner-unknown` (INV-03 §4.6 rule 8) | same | same | exit 0; proxy alive |
| pid record whose pid now belongs to an unrelated process (birth time differs), gateway port free | record retired; normal start | same | same | record retired; exit 0 |

---

## 6. Verification steps

1. **Per task.** The author lane runs its package-scoped gates in its own worktree and hands over. A separate review lane
   (section 9) reads the diff against the cited inventory sections and source ranges. The author never approves its own work.
2. **Per phase.** A complex verifier lane (section 9) runs the phase gate from a clean checkout of the phase
   branch: every tier in the Linux venue and on the `xcode-27` CI runner (OD-8). It stores each command, exit code
   and raw output under `.omc/artifacts/verify/<phase>/` and runs `hack/parity-check`.
3. **Lead check per phase.** For each in-scope difference in §3.10 and for the relay, sentry and ownership code of that phase,
   the lead compares the Go code with the pinned source lines directly, not with the inventory.
4. **Before phase 6.** `fd -H -I -t d '^\.omc$'` shows no stray state directory; every spawned lane has acknowledged
   shutdown; the tripwire is clean.

---

## 7. Expanded test plan

| Level | What | Tools | Tier |
|---|---|---|---|
| Unit | policy and classification tables; `jsNumber`; `jsonx` wire and file corpora against Node goldens; read-only detection of the Node-managed hosts block; pins compare; tool-schema reason codes against the Node/Ajv corpus; sentry state table; dispatch cache; Grok translation; lifecycle lock stale rules; frame scanner | table tests (`map[string]struct`), `go-cmp`, `testing/synctest` for every timer, fuzz targets for `toolschema`, the frame scanner and `jsonx` | 1 |
| Integration | worker handler against real HTTP fakes: every response branch R1-R8, overflow normalization, catalog refresh, usage records, header sinks; relay against a scripted worker with request counters | `httptest`, `gatewaytest`, `goleak` in `TestMain` | 1 |
| End to end | the built binary in an isolated home: `testscript` per command; real process trees for start, drain, restart, crash loop and recovery; two processes contending for the lifecycle lock; the ownership matrix; migration and rollback against a real Node supervisor; cold install; the source suite through the launch adapter | `testscript`, `make test-process`, `make conformance` (Node ≥ 22.5 in that job) | 2 |
| Live (opt-in) | one request per backend through a real claude-code-proxy in shadow mode, enabled by `MODEL_GATEWAY_LIVE=1`; run by the user only | `go test -run Live` | user |
| Observability | `lifecycle.jsonl` event sequences for start, worker crash, drain, proxy recovery, stop; `request-routes.jsonl` schema and the absence of bodies and credentials (sentinel strings in headers, query, body and tokens must appear in no log, diagnostic or collector payload, also when the collector is slow or failing); `/healthz` schema golden; OTLP log and span payload goldens; `doctor` section order and the `PASS`/`FAIL` model-window line; `soak.jsonl` schema | golden files, `testscript` | 1 and 2 |
| Performance | the three AC-9 measurements; `b.Loop()` benchmarks for body classification, the wire-policy rewrite of a 4 MiB body and frame scanning; never run in parallel | `go test -bench`, `hack/bench` | 2 |

The tier column names the make target: 1 is `make test`, 2 is `make test-process` or `make conformance`. Both run only
in a venue.

No mocking framework and no mocked interfaces: fakes are real servers and real processes.

---

## 8. Risks and mitigations

| # | Risk | Likelihood / impact | Mitigation (owner task) | Detected by |
|---|---|---|---|---|
| K1 | A test or a lane reaches the live gateway or the real settings | medium / high | no test runs outside a venue, a machine with no gateway of its own: the make targets and every test binary check it; guard and tripwire as further layers (T0.4, T0.4b) | AC-2 |
| K2 | The data path differs subtly: missing flush, wrong usage sum, sentry at the wrong turn, a relay resend where the source would not resend | medium / high | state-table and count tests (T1.6, T3.3); tests with independent expectations (T2.0b); shadow soak with `soak.jsonl` (T5.4, T6.1) | AC-5, AC-10, AC-11, AC-14 |
| K3 | Migration changes the user's run/stop intent, leaves two writers, or cannot be undone | medium / high | the offline, journaled `migrate` and its rollback (T5.3); rehearsal of the runbook (T6.0) | AC-7 |
| K4 | The source suite cannot judge the Go binary (PID identity, IPC shutdown, module patches) | medium / medium | launch-only probes and a decision before the worker tasks start; a seam that cannot be adapted moves only its tests to Go ports, which are written first by a separate lane in any case (T2.0b, T0.7) | the decision point |
| K5 | The source keeps moving: `dev` gained a plugin commit during planning, and the pin was moved to it once | high / medium | pinned export (T0.0); `docs/port/upstream-sync.md`: for each new source commit, the diff of `lib/`, `bin/`, `hooks/`, `test/`, `skills/` and `.claude-plugin/` is triaged into ported, not applicable or deferred; the policy table is data | upstream-sync review per phase |
| K6 | `procinfo` evidence gaps: another user's argv or listener may not be visible on macOS, and birth-time precision is platform-dependent | medium / medium | two-OS tests first (T0.5); unreadable or ambiguous evidence does not authorize action. A clean macOS listener result is scoped to the probing user (§3.4). | AC-6 |
| K7 | A GPT lane ports intricate logic plausibly but wrongly | medium / high | expectations from the source's tests or from a separate test lane; review lane; the lead's direct source comparison (section 6 step 3) | AC-11, review |
| K8 | Claude Code changes hook or discovery behavior | low / medium | facts verified against the unpacked 2.1.332 CLI; `doctor` prints the installed hook form and warns when the running Claude Code version is older than the first version known to accept `args` (recorded in T4.4 from the reverse tree) | `doctor` |
| K9 | License: MIT source ported into an Apache-2.0 repository | low / low | `THIRD_PARTY_NOTICES.md` with the source's copyright and MIT text (T0.0) | review |
| K10 | Secrets in logs or test output | low / high | sentinel tests of section 7; lanes never read token files | AC-4, AC-13 |
| K11 | The tool-schema validator may accept or reject an original ECMAScript Unicode-mode pattern differently from V8 | medium / low | compare the in-repository checker's verdicts with the committed Node-generated corpus (§3.1); add cases for later mismatches | T1.5b |
| K12 | An inventory statement is imprecise although its citation resolves: the verification of the pin move read about 240 citations and found six such statements (corrected, VER-R) | medium / medium | the pinned source is the authority and the inventories are leads: every brief says so; test lanes write expectations from the pinned tests and source (T0.9, T1.0); the reviewer reads the diff against the cited source lines (section 6); a wrong statement is corrected in the inventory with a dated note | review; AC-8, AC-11 |

---

## 9. Execution routing

User instruction, 2026-11-26: run simple tasks on `claude-gemini-4-carbon[1m]` or `claude-gpt-6.2-sol-fast[1m]`,
and complex tasks on `claude-gemini-4-neon[1m]` or `claude-gpt-6.2-astra-fast[1m]`. This includes implementation.

| Role | Agent type (model) | Used for |
|---|---|---|
| Implementation, simple | `gemini4-carbon-executor` or `gpt62-sol-fast-executor` | tasks marked **sol** |
| Implementation, complex | `gemini4-neon-executor` or `gpt62-astra-fast-executor` | tasks marked **astra**; test-first lanes of the fallback |
| Code review | `gemini4-carbon-code-reviewer` or `gpt62-sol-fast-code-reviewer` (S, M); `gemini4-neon-code-reviewer` or `gpt62-astra-fast-code-reviewer` (L) | every task; never the author lane |
| Verification | `gemini4-carbon-verifier` or `gpt62-sol-fast-verifier` per task; `gemini4-neon-verifier` or `gpt62-astra-fast-verifier` per phase gate | section 6 |
| Architecture and plan review | `gemini4-neon-architect` or `gpt62-astra-fast-architect`; `gemini4-neon-critic` or `gpt62-astra-fast-critic` | read-only |
| Security review | `grok-security-reviewer` (`claude-grok-4.7-fast[1m]`). A two-line probe lane runs first; if its model line is not the grok id, the review runs on `oh-my-claudecode:security-reviewer` (Opus) and the substitution is recorded | T3.8, T5.7; never the lead |
| Steps with live credentials, and all of phase 6 after T6.0 | the user | live tests, soak, cutover |

Three phase-1 implementation tasks were declined by GPT lanes and completed by `opus-gw-executor`
(`claude-opus-5-5[1m]`): Grok translation, route classification and settings wiring.

**Tests first.** For every implementing task, the `go-port` tests that T0.7 assigned to it are written beforehand by a
separate complex executor lane from the source tests and the inventories; the implementing lane may add
tests but may not change those expectations without the lead's recorded approval. The handoffs are tasks of their own (T0.9, T1.0, and the first task of each later phase), merged before the tasks they serve. Each CLI command lives in its own package
under `internal/cli/`, and the lead owns the root registration file, so parallel command tasks do not share a package.

**Lane brief rules** (briefs are in English): the task, its inventory sections and pinned source ranges, the package it
owns, its gate commands; read the source only from the pinned export, which is the authority where an inventory and the source disagree (the lane reports the disagreement); run from the repository root with absolute paths and
`go -C` / `git -C`, never `cd`; never call `go test` or Node directly, the make targets run them in a venue; never read `control-token` or auth files; any
time comes from `date`; line 1 of the report is the model the lane ran as, and the lead checks it against the pin; the
gateway must be on (`/gateway on`) or the pin is silently ignored. A lane is shut down in the turn its report is accepted;
a run is complete only when every lane has acknowledged shutdown. Data files under `test/` and `hack/` name phases,
packages and difference slugs, never task, decision or inventory ids. After creating an agent definition, start a
two-line probe lane and check its model line before starting real lanes with that definition.

On 2026-11-26, the plan re-review, settings-newline and first worker-upstream test lanes were spawned from Gemini
agent definitions created in the same session before the session had loaded them. Their pins were ignored and all
three ran on the lead's model (`claude-fable-5-2`). The first line of the re-review report and the lane transcripts
exposed the mismatch. The upstream test lane was stopped and restarted on its pinned model. The other two lanes
had finished or nearly finished, so their work was kept; the re-review was still independent of the author.

**Branches and worktrees.** The lead creates one integration branch per phase (`port/phase-N`) from `main` and one
branch and worktree per lane (`git worktree add`). A lane formats and tests only its own package paths. The lead
rebases a finished lane branch onto the phase branch with `git r port/phase-N` **before** review. After approval,
fast-forward the phase branch (`git merge --ff-only`), push it to `origin`, and delete the merged lane branch and worktree.

User instruction, 2026-11-26: land each finished phase branch by fast-forwarding `main` and pushing it when the phase
passes its gate. Base the phase branch on `main` before gating it (`git r main` if a rebase is needed); never rebase a
reviewed or gated tip and then rely on the old evidence. Opening a pull request still needs the user's word.
While reviews are open, push the stacked phase-1 candidate to `origin/port/phase-1-candidate` for hosted runs on
`ubuntu-26.04` and `xcode-27`. Advance `port/phase-1` only to approved commits.

**Commits.** Commit each small task separately on its phase branch and push it to `origin/port/phase-N`. Signed, message
from a file (`git commit --gpg-sign -F <file>`), subject and body lines at most 72 columns,
the subject says why the change exists; no task, decision or inventory id anywhere. Add the applicable co-author trailer
beside the Claude trailer when a lane writes code:

- GPT: `Co-Authored-By: Codex <noreply@openai.com>`.
- Gemini (user instruction, 2026-11-26): `Co-Authored-By: gemini-code-assist[bot] <176961590+gemini-code-assist[bot]@users.noreply.github.com>`.
- Grok: `Co-Authored-By: Grok <noreply@x.ai>`.

A review-only lane adds no trailer.

**Parallelism.** Phase 0: T0.0; T0.1; T0.4; T0.7; T0.7b; T0.9; then T0.2, T0.3 and T0.8 together; then T0.4b; then T0.5 and T0.6. Phase 1: the handoffs T1.0 first; then T1.2, T1.3, T1.8,
T1.13 and the corpus job T1.5a; then T1.5b and T1.9; T1.1 after T1.2; then T1.4, T1.6, T1.7, T1.11, T1.12a, T1.12c, T1.12d; then T1.10 and its follow-up T1.14. Phase 2: T2.0, then T2.0b and its decision; then
T2.1 and T2.2; then T2.3 and T2.4; then T2.5, T2.6, T2.7, T2.8. Phase 3: T3.1 and T3.3; then T3.2, T3.4, T3.5; then T3.6
and T3.9 (T3.9 can start as soon as T3.4 lands); T3.7 waits for both.
Phase 4: T4.0 and T4.3; then T4.1, T4.2, T4.4, T4.6. Two lanes never own the same package at the same time.

---

## 10. Pre-mortem

Assume the project failed six weeks after cutover. The three most damaging plausible stories:

### Scenario 1: a test run or a lane damaged the live setup

A process test ran on the user's Mac outside the sandbox, or a helper process started without the guard variable. It
connected to port 18764, read the real pid records and stopped the live supervisor, or `env --write-user` rewrote
`~/.claude/settings.json`.
- Why plausible: it happened to the source once (`test/isolate-home.js:4-7`); an environment variable is the only thing
  between a helper and the real home.
- Prevention: no test runs outside a venue, a machine with no gateway of its own: the make targets and every test
  binary apply the venue predicate and accept no environment variable alone as proof; there is no local test target (T0.4, AC-2);
  ownership needs a record in the instance's own state root (D10), so a test binary can never own the live supervisor.
- Early signal: tripwire digest mismatch; a record in the real `lifecycle.jsonl` with a test pid.
- Response: stop all lanes; restore the settings from the user's own backup and restart the gateway with its CLI; the missing boundary becomes a failing
  test before work resumes.

### Scenario 2: sessions were billed twice or compacted at the wrong time

Either the sentry's usage arithmetic or rearm rule was ported with a small difference, or the relay resent a request in a
case where the source would not. Sessions hit a synthetic 413 on every turn, or ran past 272,000 input tokens (the
source's billing rule, `lib/runtime.js:248-260`), or one prompt produced two upstream generations.
- Why plausible: the sentry was fixed three times in the source (INV-05 §6.2); the relay's rule depends on a latch at TCP
  connect and on worker generations; Go's HTTP client can replay or follow redirects where Node does not.
- Prevention: the sentry as a pure state machine tested against the inventory's table (T1.6) and the source's
  `context-sentry-lifecycle` and `compact-stream` tests; the transport rules of section 3.1; AC-14 counts deliveries;
  the soak must contain a compaction and checks each synthetic 413 against the preceding usage (AC-10).
- Early signal: `soak.jsonl` or the usage records showing a `sentry-413` without a preceding over-trigger turn, or two
  records for one request id.
- Response: `model-gateway migrate --rollback`; reproduce from the recorded usage sequence as a new table row.

### Scenario 3: after cutover prompts were blocked, or the gateway was found stopped

The old plugin was still enabled, or an old session was still open: its SessionStart hook rewrote both launchers
(`hooks/registry-writer.js:145-156`), so `gateway-switch` and legacy hooks ran the Node CLI again, which refused the Go
supervisor as foreign and exited 2 on every prompt. Or migration stopped the Node supervisor with the Node CLI's `stop`,
which wrote the stop marker, and `setup --preserve-wiring` then left the gateway stopped.
- Why plausible: both writers exist in the source; hook ownership is exact-string equality; `stop` records intent before
  it knows the outcome (`lib/commands.js:2951-2960`).
- Prevention: migration is offline (section 3.6): it refuses while a Node supervisor listens, while the old plugin is
  enabled or while a session is visible; it never calls or signals Node; the end state is given by a flag instead of
  being read from the marker; the new publisher is inert until the journal is committed and after a rollback; AC-7 covers
  the refusals, both end states, a kill around every journaled effect and rollback with a later user edit; T6.0
  rehearses the runbook with fresh processes and both plugins' hook commands.
- Early signal: `doctor` reporting a launcher that is not the trampoline, a `legacy-node` owner, or a stop marker newer
  than the migration journal.
- Response: `model-gateway migrate --rollback`, or `env --remove` in the affected project.

Further failure cases, each with a fixture and an owner:

| Failure | Fixture | Owner |
|---|---|---|
| the live proxy disappears while a shadow instance uses it | AC-16 | T5.4 |
| an interrupted self-install leaves no working binary | kill before the hard link, between link and rename, and after the rename; a working binary is at the stable path each time (AC-15a) | T4.0 |
| an interrupted migration leaves mixed hooks | AC-7 failure injection | T5.3 |
| a worker crash loop floods logs and records | D5 test with a worker that exits at boot | T3.4 |
| the source's upstream changes a contract the port copied | upstream-sync triage finds a diff in a ported range | lead, each phase |

---

## 11. ADR

- **Decision.** Port the plugin to one Go binary that keeps the source's three-process topology (detached supervisor with
  the public listener and relay, replaceable worker, supervised claude-code-proxy), its on-disk state and settings formats
  and its CLI and HTTP contracts. Node-only mechanisms are replaced by two inherited pipes, native process inspection and
  two explicit JSON policies. Ownership of a process is proven by a record in the instance's own state root. Migration
  from the Node gateway is an offline, journaled, reversible operation run by the operator; a shadow mode allows a soak without touching the
  live proxy. Parity is judged by the source's own tests where they can drive the Go binary, by tests written
  independently of the implementation elsewhere, and by a soak before cutover. Section 3.10 lists the deliberate differences.
- **Drivers.** Regression risk in invariants that were each learned from a failure; the need to show which tests exercised
  Go; reversibility of the transition for a gateway that is in daily use.
- **Alternatives considered.** A single daemon (no hop, exact resend decisions, but nothing answers when the daemon dies and
  a binary handoff has to be invented); a supervisor that passes the listening socket (no hop, but an accepted connection
  dies with the worker; no Windows path); Go-only tests re-derived from the inventories (cheaper, shared misreadings);
  recorded transcripts; a new state root with an import step; calling the Node CLI to stop the legacy gateway (deadlocks on
  the shared lock and changes the stop intent); `go-sse`, `gjson`/`sjson`, a Go schema validator as oracle (each differs
  from the source's observable behavior).
- **Why chosen.** The topology keeps the public listener, the drain and control contract and worker-crash isolation, and
  its behavior is already specified by tests that can be ported one to one. It is not chosen for delivery guarantees: the
  relay resends to a replacement worker, and that rule is ported unchanged.
- **Consequences.** Two long-lived Go processes plus the proxy; a small supervisor/worker protocol to maintain; one loopback
  hop and one buffered body per request, as today; Node remains needed for the conformance job and for the two trampolines
  until their consumers change; process tests cannot run on the user's Mac outside a sandbox; the source must be re-diffed
  when it changes; Windows is not served by the first milestone.
- **Follow-ups.** Section 12.

---

## 12. Open decisions and follow-ups

The defaults are what the plan assumes unless a row records the user's decision.

| # | Decision | Default | If decided otherwise |
|---|---|---|---|
| OD-1 | Process model | A, same topology | B or C needs a handoff or descriptor-passing design task before phase 3 and removes T3.3 |
| OD-2 | Windows | compile-only now | adds a phase and a `windows-2025` CI job |
| OD-3 | Differences D1-D14 | D1-D13 accepted except D3, withdrawn by OD-21; D14 is the lead's wiring ruling (§3.10) | remaining differences can be dropped separately; dropping D2 keeps Node on the per-prompt hook path; dropping D10 needs another rule for two instances of one binary |
| OD-4 | Source of truth after cutover | Go; the fork's `plugins/model-gateway` is frozen and later upstream changes are ported by hand | keeping both alive needs a sync cadence |
| OD-5 | Release archives and `self update` | no; `go install` plus `setup` | adds T5.5 and `go-selfupdate` |
| OD-6 | Plugin identity | marketplace `claude-code-model-gateway` in this repository, plugin id `model-gateway@claude-code-model-gateway` | another marketplace changes one constant and the skill text |
| OD-7 | First version | `0.57.0` | any value above `0.56.3` |
| OD-8 | Where macOS process tests run | decided: `xcode-27` hosted CI runner through pushed phase branches | — |
| OD-9 | Agent definitions `gpt62-*.md` | decided: not committed; `~/.claude/agents/` makes the definitions available from any checkout | — |
| OD-10 | Inventories, reviews and this plan under `docs/port/` | done: committed there as the authoritative porting specification; copies under `.omc/` are not maintained | — |
| OD-11 | Tests written first by a separate lane (section 9) | yes, for every `go-port` test | implementers port their own tests; AC-11 then loses its independence clause |
| OD-13 | Tool-schema pattern validator | decided 2026-11-26: use the in-repository ECMAScript Unicode-mode syntax checker (§3.1) | — |
| OD-14 | Hosted-CI attestation wording in AC-2 | decided 2026-11-26: no CI exemption; accept the per-prerequisite evidence specified in AC-2(a) | — |
| OD-15 | Unknown-flag wording in AC-12 | decided 2026-11-26: limit the ignore rule to the flag-presence scans named in AC-12 | — |
| OD-16 | Clock-independent process start token in the pid record | decided 2026-11-26: add the token against pid reuse, in addition to the existing birth-time check; a test-first handoff before phase 3 fixes its representation (§3.4) | Before the signaling gate, settle any change to the birth-time check and whether signaling requires separate image evidence. |
| OD-17 | Early phase-1 test-first handoffs | decided 2026-11-26: the user approved the early start of test-only handoffs independent of `procinfo`; the lead separately stacked them and started implementation on the unpublished phase-1 branch (§4) | No phase-1 work merges into the gated phase branch or is pushed before the phase-0 gate passes. |
| OD-18 | Additional deliberate differences | decided 2026-11-26: accept the 22 unnumbered §3.10 rows recorded in `99e78ad` and the launcher-guidance coverage, separately from D1–D13. Later additions retain their own attribution in §3.10. | — |
| OD-19 | macOS half of the Node baseline | decided by the lead 2026-11-26; reported to the user: require the macOS Node baseline at the phase-3 gate, not phase 0 (§3.8, T3.9) | To require it at phase 0, first agree on a macOS containment contract, then port `hack/bench` under that contract before closing the phase-0 gate. |
| OD-20 | Escaped lone surrogates in listing and catalog identifiers | decided by the lead 2026-11-26; reported to the user: drop the affected entries without substituting U+FFFD and keep literal U+FFFD. Raw invalid UTF-8 has a separate decoding rule (§3.10). | — |
| OD-21 | Remote-control compatibility | decided by the user 2026-11-26: do not port the command, listener, control endpoint, DNS bypass or skill. Detect the Node-managed hosts block read-only in `doctor` and in the migration check that `migrate` runs before its first step (§3.6, §3.10) | — |
| OD-22 | Requests the classifier cannot classify | decided by the user 2026-11-26: refuse locally with HTTP 400 for a decoder depth error or an escaped lone surrogate in a top-level `model` string; never forward either case. Preserve Node-compatible raw UTF-8 decoding and ordinary non-JSON passthrough (§3.10). | — |
| OD-23 | Legacy compatibility URL | decided by the user 2026-11-26: recognize the URL as owned for diagnosis and explicit cleanup only. Never write it or switch endpoints automatically. The existing explicit write commands use the default URL and removal clears owned legacy values; `doctor` and migration checks report the legacy value (§3.6, §3.10). | — |

Follow-ups, not part of this plan: F1 decide whether the relay should never resend after delivery (availability cost: a
worker replaced mid-request yields a 503 instead of a transparent resend); F2 cancel the upstream request when the client
disconnects; F3 make `count_tokens` on Grok models not start a generation; F4 write the stop marker only after a successful
stop; F5 evict idle sentry sessions; F6 let pin sync skip one key instead of a whole project; F7 a `restart` command; F8
relay over a private Unix socket instead of an ephemeral TCP port (INV-03 §13 item 11); F9 re-evaluate options B and C
once AC-9 and the soak have numbers — trigger: time to first byte through the relay more than 5 ms above the direct worker
path at p99, or two defects in the supervisor/worker protocol within three months of cutover.

---

## 13. Confidence

Forecasts, not measurements.

| Dimension | Score | Basis |
|---|---|---|
| Performance | 0.80 | `Probe.Birth` and process-table queries do not spawn children; full macOS identity and listener evidence use `lsof`. No Node start on the per-prompt hook; the relay keeps its shape. Checked only by T0.6 and AC-9 |
| Scalability | 0.75 | one user's sessions on loopback; goroutine per request and 64 upstream connections per host as in the source; the unbounded sentry map is ported as it is |
| Reliability | 0.65 | topology and guarantees are kept and tested with independent expectations, but supervision and migration are where the source failed most and where the source's tests can judge least |
| Cost effectiveness | 0.60 | about 13,000 lines to port and 21,700 lines of tests to re-express or adapt; whether the source suite can drive the Go binary is unknown until T2.0b |

---

## Appendix A: documentation and code disagreements in the source

The port follows the code except for the differences in §3.10. Correct the applicable statements in the rewritten
`model-gateway` skill and README (T5.2, T5.6).

| # | Documentation says | Code does | Evidence |
|---|---|---|---|
| A1 | `hooks/hooks.json:2`: SessionStart "waits up to 12 seconds for the local Codex gateway" | the hook runs only `registry-writer.js`, which writes launchers and the breadcrumb | `hooks/hooks.json:9`; `hooks/registry-writer.js:192-202` |
| A2 | `README.md:110`, `skills/model-gateway/SKILL.md:60,127`: SessionStart runs `ensure --quiet` | removed by commit `101a64fd2`; readiness comes from settings-installed `readiness-gate` hooks | INV-01 §3.2, §4.3 |
| A3 | `README.md:74`: "Anthropic requests remain byte-identical" | true for the body; `traceparent`, `tracestate`, `baggage` and hop-by-hop headers are removed and `content-length` recomputed | `lib/request-worker.js:1827-1839` |
| A4 | `lib/request-worker.js:2703-2704`: the hosts entry is "never written by this plugin" | The Node CLI's `remote-control enable --confirm` writes it. The Go port does not write the hosts file (OD-21). | `lib/remote-control.js:219` |
| A5 | `README.md:3`: "every model `claude-code-proxy models` lists" | the listing is read from the proxy's HTTP `GET /v1/models` | `lib/request-worker.js:1480`; `lib/commands.js:2025` |
| A6 | the usage text lists `start \| stop` and `serve-shim` only | `readiness-gate` and `serve-worker` exist; there is no `restart` command although the README describes restart flows | `lib/commands.js:153-197,2935-3041` |
| A7 | the skills describe Muse, Vertex AI, local GGUF and LM Studio rows | `CHANGELOG.md` (0.56.3) does not list them | INV-05 §6 |

## Appendix B: what was verified while planning, and what was not

| Verified | How |
|---|---|
| remote `dev` head equalled the local checkout at 21:26 (`4ffb2f672f0c`); the checkout moved to `a8501f251` at 21:52 | `gh api repos/zchee/eigenwise-toolshed/branches/dev`; `git log` |
| `a8501f251755` is the head of the local `dev` and of the remote-tracking ref `zchee/dev` at 22:39; its plugin diff against the previous pin `4ffb2f672f0c` is `6ffbf58d3` alone (4 files, 21 insertions, 14 deletions) | `git log`, `git diff --stat` |
| the new citations of v4: the stop command's marker write, the fixture-shutdown handler, the worker's disconnect exit, the SSE frame rewrite, the two imports from outside the plugin root | the pinned `lib/commands.js:2951-2953`, `:2900-2904`, `lib/request-worker.js:1210`, `:1675-1721`, `git ls-tree` |
| the move to `a8501f251755`: 221 citations into `lib/runtime.js` and `test/context-window.test.js` rewritten, 82 left as they were; for every citation the script handled that touches no changed line, the old lines equal the new lines (0 differences); 4 ranges decided by hand; a separate lane then read 216 `lib/runtime.js` citations and all 18 context-window citations at the new pin, found seven the script had missed and six imprecise statements, and all were corrected | `07-repin.md`, `verify-repin.md` |
| the files the source tests read outside the plugin root; repository size and tree hashes at the pin | `git show`, `git ls-tree`, `git rev-parse` on `a8501f251755` |
| the sentry state table used by AC-10 | INV-02 §5.3 |
| line and file counts of the pinned source | `git show a8501f251755:<path> \| wc -l` |
| module path, remote and files of the target repository | `go.mod`, `git remote -v`, `git status` at 22:06 |
| `encoding/json/v2` and `jsontext` without an experiment flag | `go doc` on go1.27.4 |
| Module versions and direct/indirect requirements in §3.3 | `go.mod` |
| Linux `/proc`, macOS `sysctl` and `lsof`, and the birth-only lookup (§3.4) | `internal/procinfo/procinfo.go`, `internal/procinfo/native_linux.go`, `internal/procinfo/native_darwin.go`; `aef0f58` is under review |
| hook exec form (`args`) and `GET /v1/models?limit=1000` in Claude Code 2.1.332 | `claude-code-reverse-new/cli.unpack.js` |
| relay semantics, the `RegExp` validity check, the gate's behavior for a newer supervisor, the context-window load rules, the recovery policy | the pinned `lib/commands.js:2332-2454`, `lib/tool-schema-compat.js:228-238`, `test/readiness-fast-path.test.js:70-88`, `lib/runtime.js:281-316`, `lib/recovery-policy.js` |
| claude-code-proxy latest release v0.1.49 (2026-11-21); the fork has a `dev` branch | `gh api` |
| the gateway serves `claude-gpt-6.2-sol-fast[1m]` and `claude-gpt-6.2-astra-fast[1m]` | `~/.claude/cache/gateway-models.json` |

After planning, the unmodified pinned suite ran in the Linux venue with Node v24.11.1: 57 files, 832 cases, 825 passed,
7 skipped, exit 0. Two skips require a real Claude CLI; five are Windows-only by a coded predicate. Without `lsof`, nine
cases failed because the source uses it to identify the owner of a listening port. See `test/conformance/baseline/run.json`,
`census.tsv` and the discarded-run comparison in that directory's `README.md`.

Not verified, because it needs running code that planning must not run: that the source suite can drive a Go binary (K4);
Node and Go performance numbers and the client-disconnect behavior of the Node gateway (T0.6); whether the real
`claude -p` exits 0 during the pin probe (INV-04 §13 item 4; this needs a real `claude` in an isolated home and the
user's separate authorization); the unlinked-executable probe on hosted macOS (T0.5; §3.4 gives its evidentiary
limit); whether the in-repository syntax checker matches the Node-generated corpus (§3.1, T1.5b).

## Appendix C: changes from v1

| Change | Review item |
|---|---|
| Process-model rationale rewritten around the relay's real rule; relay state machine, transport rules and AC-14 added; the resend rule recorded as follow-up F1 | REV-A 4, antithesis; REV-C risk summary |
| Takeover replaced by the fenced, manifest-based `migrate` with rollback, preconditions for the old plugin and old sessions, and tests against a real Node supervisor (section 3.6, T5.3, T6.0, AC-7) | REV-A 1, 3; REV-C 1 |
| Shadow mode defined as unmanaged-proxy, no outside writes, with a preflight and a soak log; AC-10 rewritten without a counterfactual; AC-16 added | REV-A 2; REV-C 3, 4 |
| Test isolation by venue and kernel sandbox instead of an environment variable; tiers; digests instead of size and mtime | REV-C 3; REV-A 10 |
| Ownership by a record in the instance's state root; executable path no longer confers ownership; `/restart` candidate rule; safe sweep of old binaries | REV-A 5; REV-C 6 |
| Worker modes: descriptors only in supervised mode; birth records and orphan reaping kept | REV-A 6; REV-C 2, 5 |
| Walking skeleton and the parity-method decision moved before the worker tasks; five named proof cases; per-test classification; fallback with test-first lanes; exec log as proof that Go ran | REV-A 7; REV-C 2 |
| Two JSON policies with Node-generated goldens; buffer-then-emit rewriting; settings writes with symlink, mode and re-read rules | REV-A 8; REV-C 10 |
| Differences reduced and re-argued: dropped strict flags, upstream cancellation, the relaxed gate fast path, deletion of birth records and the context-window fallback; added stop order, record-based ownership, `/restart` rule, Linux port owner; each difference names the source tests it changes | REV-A principle review; REV-C 5 |
| `regexp2` moved to a run-time dependency judged by a Node/Ajv corpus (subsequently superseded; see §3.1 and §12); `go-sse`, `klauspost/compress`, `jsonschema` dropped | REV-A 9, dependencies; REV-C 10 |
| Task graph: no task above L; owners for the classifier, model snapshot, command registration, live probes, standalone listener; `selfinstall` before `setup`; identity seam in phase 0; ownership tests for `ensure`/`setup` moved to phase 4; worktree and integration rules | REV-A 10; REV-C 7, 8 |
| Module path, existing files, license header source, line counts and the pinned export corrected; the source's new commit recorded | REV-C 7, citation audit |
| Distribution made concrete: marketplace and plugin id, install commands, version source, cold-install criterion AC-15; `update.js` trampoline takes no arguments | REV-C 6, 9; REV-A 3 |
| Acceptance criteria rewritten with state-dependent outcomes, executed-test evidence and named inputs; AC-14, AC-15, AC-16 added | REV-C 4 |

Changes from v2 (second review round: `review-architect-2.md`, `review-critic-2.md`):

| Change | Review item |
|---|---|
| Migration made offline and operator-run: no fence, no signal to Node, end state by flag, byte-level journal, inert publisher, conflict-reporting rollback | architect 1-3; critic 1, 6 |
| Launch-only probes separated from behavioral tests; adapter preference reversed; per-test attribution; classification and parity matrix moved to phase 0 (T0.7); tests written first by a separate lane | architect 4, 5; critic 2, 4 |
| Isolation by venue only: allowlisted pure packages locally, build tag for process tests, no local Tier 2, digests without copies, the sandbox wrapper optional | architect 10; critic 3 |
| Ownership: supervisor-published records with image identity, token-authenticated stop as recovery, durable launches from the stable binary | architect 6; critic 9 |
| Client-disconnect behavior measured on the pinned Node gateway before it is specified; HTTP/1.1 only; no replay of requests with a body | architect 7 |
| Wire JSON as an own `JSON.stringify`-equivalent encoder; authored settings goldens; state files written as Node writes them; duplicate owned keys refused | architect 8; critic 10 |
| Shadow mode: one allowed dial to the proxy, no auth subprocess, preflight at every start, `shadow report` and a soak record with correlation keys | architect 9; critic 7 |
| Worker descriptors fixed at 3 and 4; the preconfigured-port rule kept; missing port report still ends the supervisor. The RC rule retained in v2 was later withdrawn (OD-21) | architect 11 |
| Differences: affected source tests corrected and extended; D7 states its two changes; D13 added for the stable binary and the plugin id | critic 8 |
| Inputs for worktrees and CI (T0.0); real `version`; scanner as a phase 1 leaf; T1.12 split; R7/R8 owners; post-pin commits deferred to T5.8; AC-6, AC-12, AC-14 corrected | critic 4, 5, 9, 10; architect task graph |

Changes from v3 (targeted critic check `review-critic-3.md`; the user's question about `a8501f251755`):

| Change | Review item |
|---|---|
| Every test binary runs in a venue by default; a venue is attested and checked, not declared by a variable; local runs only for packages whose import closure and calls the checker accepts, with reject fixtures | critic-3 critical; second-round critic 3 |
| Phase 0 reordered: bootstrap, venue tooling and the test inventory first; argument scan split out (T0.8); `gatewaytest` and the guard split out (T0.4b); T0.5 needs T0.3; test handoffs are prerequisites | critic-3 major 2 |
| Test identity by file, site and expanded name from one venue run of the pinned suite; closed reason lists; a matrix that must cover every case; negative checker fixtures | critic-3 major 3; second-round critic 4 |
| One export path for the pinned source, with the two out-of-root test imports; a read-back check in T0.0 | critic-3 major 4 |
| The pin probe's real-CLI question is left open instead of being answered by a fake | critic-3 major 5 |
| Phase 1: corpus job (T1.5a) and two-process lock test named as venue-only; lock decisions over an injected identity source | critic-3 major 6 |
| AC-1 states what runs before a macOS venue and CI exist | critic-3 major 7 |
| Phase branches stack on the verified tip of the previous phase | critic-3 major 8 |
| Frame rewrite wording corrected: lines joined with LF, the frame's own delimiter kept | critic-3 minor 9 |
| Migration: rollback returns to the state `migrate` found and starts nothing; a rerun resumes its own journal; the stable binary is journaled; one replacement algorithm (hard link, then one rename) | second-round critic 1, 6 |
| Test control channel for the harness's IPC shutdown owned by T2.0 | second-round critic 2 |
| `soak.jsonl` carries stop reason, completed flag, worker generations, submission causes and lifecycle records; AC-10 no longer flags the source's own retries | second-round critic 7 |
| AC-15 split into a simulated gate (phase 5) and the real-host checklist (phase 6); AC-6 separates `port-owned` from `owner-unknown` | second-round critic 9, 10 |
| The post-pin commit described in full (rows, position, test and document changes); OD-12 added for moving the pin | the user's question |

Changes from v4 (delta check `review-critic-4.md`; the user's decision to move the pin):

| Change | Review item |
|---|---|
| Pin moved to `a8501f251755`: counts, tree hashes, T0.0, T5.8; the inventories' citations and policy table moved with a line-by-line check (INV-07); OD-12 closed | the user's decision |
| No local test target: every make target and every test binary checks the venue; the import-closure allowlist is dropped | critic-4 A |
| The pinned source is exported as the whole repository tree, so no test input can be missing; the baseline run fails on a missing input | critic-4 B |
| T0.1 is complete on build and static checks; its tests first run in T0.4 | critic-4 C |
| Test handoffs are tasks (T0.9, T1.0) between the inventory and the implementing tasks | critic-4 D |
| Assertion sites are listed from the source by a script, mapped per `go-port` case, and the matrix has a lock; the checker rejects a row deleted while its case stays covered (T0.7b) | critic-4 E |
| `migrate` resume accepts the Go supervisor that its own journal started | critic-4, earlier disposition 1 |
| AC-10 replays the sentry state table instead of requiring a completed turn before a refusal; the soak record carries the sentry's inputs and states | critic-4, earlier disposition 7 |

Changes from v5 (delta check `review-critic-5.md`):

| Change | Review item |
|---|---|
| No test binary on the developer machine at all: the guard's negative control runs on the Linux host with the attestation removed; the guard must be the first statement of `TestMain` under both build tags | critic-5 major 2 |
| `hack/parity-check` has three modes (structure, mapping per phase, results per phase), each gating a different task | critic-5 major 1 |
| Assertion sites are recorded as pairs of case and site, so shared helpers are covered; the target is a Go subtest confirmed per pair; `not-ported` has a closed reason list and per-pair approval | critic-5 major 3, minor 5 |
| Every implementing task of phase 1 lists its handoff T1.0 in its Needs | critic-5 major 4 |
| `migrate --start` journals the chosen generation as its intent before starting the supervisor, and a resume recognizes that supervisor | critic-5, earlier 1 |
| AC-10 limits the refusal rule to non-compaction requests on covered paths | critic-5, earlier 7 |
| The move to the new pin was verified by a separate lane; seven missed citations and six imprecise inventory statements corrected; the pinned source named as the authority over the inventories (K12) | VER-R |

Changes from v6 (sixth check `review-critic-6.md` and execution decisions):

| Change | Review item or decision |
|---|---|
| T0.4 and AC-2 require fail-closed venue checks with a verified hosted-CI exception, separate make refusals, direct positive and negative fixture controls for both tag sets on disposable Linux, and only the standalone shell preflight on the Mac | critic-6 major 1 |
| Phase gates compare digest manifests; phase 0 uses the manual manifest captured after T0.0 and T0.1 started and reports the earlier interval unverified | critic-6 major 2; lead's baseline |
| T0.7 runs the pinned suite through `baseline-node` with no real `claude` on PATH, retains independent terminal events, and reconciles all 57 files, including the five nested sites, expansions and skips; incomplete evidence blocks completion | critic-6 major 3; lead's spot check |
| T0.7 records direct gateway launch sites and their owning cases for the adapter decision | critic-6 minor 7 |
| T0.7b derives the expected case/site pairs independently, including embedded JavaScript and reviewed attribution overrides; exact set comparison and negative fixtures catch a missing embedded assertion or shared pair. Five helper sites serve three cases, giving 15 pairs | critic-6 major 4, minor 6 |
| Mapping checks reviewed per-obligation targets or approved exemptions; T1.0 gates packages before the phase. Results require fresh evidence for due targets, differences, Go launches and unsupported approvals. AC-8 checks structure globally, results when due after the appropriate handoff, and all earlier ledgers at phase 5 | critic-6 major 5 |
| The user adopted the sixth check without another round and authorized execution through T6.0, with a phase table at each gate and a stop before the user's soak, cutover and plugin removal | user's decision |
| Push each completed small task to its phase branch; land and push `main` at each passing phase gate (§9). Opening a pull request still needs permission. CI runs on pushed branches from the first gate; macOS uses `xcode-27`, Linux the approved current account with the no-write stop rule | user's decisions |
| The user's `main.go` and `README.md` landed unchanged first. Agent definitions stay at user level; `docs/port/` is authoritative for the inventories, reviews (including the sixth check) and plan, not the `.omc/` copies | user's decisions |
| Use standard `uuid`; pin Brotli and `x/text`. T0.1 specifies version precedence, development fallbacks and Makefile tool versions; T0.4 adds the first CI test steps. Do not execute the built binary on the developer machine | lead's rulings |

After the test inventory (2026-11-26): corrected the site and case counts, recorded the Linux baseline and its `lsof`
prerequisite, and separated per-site extraction from expanded-case checking. The plan now uses the classification and
ownership columns, whole- versus mixed-case rules, difference slugs, nested launch attribution, assertion due phases
and phase exit gates. The CLI map now matches the command packages; data files use no plan ids.

## Appendix D: what the reviews leave open

- It is not known which source test connects to the session bus during the Linux suite.
- The baseline extractor checks registrations per site; the independent expanded-case comparison belongs to T0.7b's `structure` check.
- Two low-severity collector issues remain: the interrupt handler stays armed after the evidence frame is printed, and
  a retained process group id could be reused on a busy host.
- v7 itself is unreviewed; the user chose to proceed without another plan review.
- The inventories are a porting aid, not a verified specification: a sample check found imprecise statements (K12),
  and the corrections made after that check were not verified by another lane.
- Whether the source's tests can drive the Go binary is decided by T2.0b, not by this plan.
- Linux venue evidence covers provisioning, both test tiers, the negative control and five baseline runs, each with a clean
  post-run status. The hosted `ubuntu-26.04` and `xcode-27` runners passed attestation and both tiers in CI. Every run must
  still recheck the venue under section 3.8. At the planning baseline, only the Darwin guard tests had run; see the
  run ledger for subsequent process-tier results.
- What the real `claude -p` returns during the pin probe is not measured (appendix B).
- The detailed design of `migrate`, shadow mode and the relay's disconnect handling is fixed here at the level of rules and
  criteria; each gets a design note that an architect lane and a critic lane review before its implementing task starts
  (T5.3, T5.4, T3.3).
- The shadow soak (T6.1), cutover and removal of the old plugin remain the user's. Approval covers execution through
  T6.0; report each phase gate and proceed when it passes, without further approval.
