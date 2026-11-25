# Go port of the model-gateway Claude Code plugin: work plan (v7)

- **Status: phase 0 is in progress; the plan reflects the pinned test inventory.** Approval covers phase 5 and the phase 6 rehearsal (T6.0); stop before T6.1.
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
| INV-01 | `01-cli.md` | 556 | every CLI command, flows, hook contract, release acquisition |
| INV-02 | `02-worker.md` | 1028 | HTTP surface, routing, transforms, streaming, sentry, catalog, Grok backend |
| INV-03 | `03-supervision.md` | 679 | process topology, state files, relay, lifecycle protocols, ownership proof, RC compatibility |
| INV-04 | `04-policy-state.md` | 1001 | env vars, paths, model window policy table, persisted JSON, pins, settings wiring |
| INV-05 | `05-tests-skills.md` | 643 | test harness, per-file test classes, behavior catalogue, skills contract, changelog digest |
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

One Go binary that replaces the Node.js runtime of the plugin and keeps every contract its consumers use. `go install
github.com/zchee/claude-code-model-gateway@<version>` builds it as `claude-code-model-gateway`; `setup` copies it to the
stable path `~/.claude/model-gateway/bin/model-gateway`, which is what hooks, skills and launchers call.

| # | Deliverable | Source of truth |
|---|---|---|
| R1 | CLI with the 17 existing commands (`setup login start stop ensure recover readiness-gate status models catalog pin context-window env doctor remote-control serve-shim serve-worker`): same flags, exit codes, stdout/stderr split | INV-01 §0-§2; `lib/commands.js:2935-3041` |
| R2 | Request path: `/healthz`, `/v1/models`, inference routing to Anthropic / claude-code-proxy / Grok, body transforms, SSE relay, context sentry, compaction guard, dispatch model, usage and route telemetry | INV-02 §1-§12 |
| R3 | Supervision: detached supervisor, replaceable worker, supervised `claude-code-proxy`, relay, `/restart`, `/drain`, lifecycle lock, intentional stop, ownership-checked stop | INV-03 §1-§7 |
| R4 | State and settings: same paths and JSON shapes under `$HOME/.claude/model-gateway`, same Claude Code settings keys, discovery cache, sidequest catalog | INV-04 §2, §4, §6; INV-01 §1.1 |
| R5 | Plugin packaging for a compiled binary: marketplace entry, `plugin.json`, hooks, two skills, cold install | INV-06 §1-§5, §7 |
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
| the two skills | commands, paths and output strings of INV-05 §5.2-§5.5 | INV-05 §5 |
| the user's sessions | ports 18764 (gateway), 18765 (proxy), 80 (RC compatibility); model ids `claude-<backend id>[1m]` | INV-04 §2.3; INV-02 §2.2 |

### 1.3 Out of scope

| Item | Reason |
|---|---|
| Reimplementing claude-code-proxy | Rust provider code that the plugin never contained |
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
4. **Worker routing** (user instruction 2026-11-25): simple tasks on `claude-gpt-6.2-sol-fast[1m]`, complex tasks on
   `claude-gpt-6.2-astra-fast[1m]` (section 9).
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
 (setup/start/ensure/recover/stop;                                 │ listens 127.0.0.1:18764, plus <hosts ip>:80 in RC compatibility
  lifecycle.lock; pid records)                                     │ Host/Origin gate · /restart · /internal/rc-compatibility · 64 MiB cap · relay
                                                                   ├─ exec ─▶ WORKER   model-gateway serve-worker           shim.pid
                                                                   │          127.0.0.1:<ephemeral> + gateway.sock · routing · /drain
                                                                   └─ exec ─▶ claude-code-proxy serve --no-monitor (PORT=18765)   proxy.pid
```

**Worker modes.** The source's rules stay: the worker binds the RC-compatibility listener only when the hosts file maps
`api.anthropic.com` to loopback and `CODEX_GATEWAY_WORKER_PORT` is unset (`lib/request-worker.js:2710-2714`), and the
supervisor always sets that variable for its worker (`lib/commands.js:2622`). A worker started with a preconfigured
non-zero port is supervised without a port report, as today.
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
| `ps` / PowerShell process probes (`lib/process-supervision.js:307-348`) | `procinfo` reads `/proc` on Linux and uses `sysctl` for argv, birth time, parent and the process table on macOS. Full macOS identity still uses `lsof` for the executing image's device and inode. `Probe.Birth` does not inspect argv or the image: it reads the kernel process record on macOS and the proc clock and stat on Linux. |
| `lsof` / `/proc` / `netstat` port-owner probes (`lib/process-supervision.js:242-289`) | `procinfo` reads the full listener table in `/proc` on Linux and uses `lsof` alone on macOS, without `netstat`. See §3.4 for what each result proves. |
| `AsyncLocalStorage` re-entrant lock (`lib/lifecycle-lock.js:88-94`) | the held lock travels in `context.Context` |
| module-load side effects (`lib/runtime.js:319`) | one explicit `config.Load()` at process start; an invalid `compactAt` fails there with the source's message (`lib/runtime.js:291-297`), a file that is not JSON falls back to defaults as today (`lib/runtime.js:281-283`) |
| `process.exit` inside the lock (`lib/commands.js:241`) | commands return an exit code; `main` releases the lock, then exits |
| Patterns with no potential Unicode property escape are skipped. For affected patterns, the compatibility scan runs first, then `new RegExp(pattern, 'u')` checks the original pattern before the rewrite is applied (`lib/tool-schema-compat.js:195-246,263-269,286-289`). | Preserve that order and its reason codes. Translating the pattern and compiling it with `regexp2` does not reproduce the original syntax verdict. Check the validator against the pinned Node corpus; the implementation choice is open (§12). |

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
| `lib/pins.js` (482), `lib/anthropic-catalog.js` (125), `lib/previous-picker-rows.js` (46) | `pins`, `anthropiccatalog` | T1.4 (logic), T4.3 (probe) |
| `lib/tool-schema-compat.js` (388) | `toolschema` | T1.5a, T1.5b |
| `lib/request-worker.js` (2734) | `worker` (server), `worker/route`, `worker/sentry`, `worker/dispatch`, `worker/upstream`, `worker/reply`, `worker/sse`, `worker/compactguard`, `telemetry`; the frame scanner `sse` | T2.0, T1.7, T1.6, T1.8, T2.2, T2.3, T2.4, T2.6, T2.8; T1.13 |
| `lib/grok-backend.js` (477) | `grok` | T1.9 (translation), T2.7 (branch) |
| `lib/codex-upstream-state.js` (101), `lib/control-auth.js` (56) | `upstreamstate`, `controlauth` | T1.12a |
| `lib/usage-observability.js` (1063) | `usage` | T1.12d (records), T2.8 (emitter) |
| `lib/commands.js:2262-2905` (supervisor) | `supervisor`, `supervisor/relay`, `supervisor/workerproc`, `supervisor/rclistener` | T2.0, T3.3, T3.4, T3.5 |
| `lib/process-supervision.js` (1216) | `procinfo`, `ownership`, `supervisor/proxyrecovery`, `launch` | T0.5, T3.1, T3.2, T3.6 |
| `lib/lifecycle-lock.js` (110), `lib/lifecycle-diagnostics.js` (61) | `lifecycle` | T1.11 |
| `lib/shim-state.js` (78) | `shimstate` | T3.6 |
| `lib/settings-wiring.js` (486), `lib/readiness-hooks.js` (53), `lib/recovery-policy.js` (38), `lib/readiness-gate.js` (65) | `wiring`, `readiness` | T1.10, T4.6 |
| `lib/picker-guard.js` (152) | `pickerguard` | T4.4 |
| `lib/remote-control.js` (302) | `rccompat` | T1.12b (hosts block), T4.5 (command) |
| `lib/github-release.js` (53), `lib/release-verification.js` (30) | `release` | T4.1 |
| `lib/atomic-file.js` (65) | `atomicfile` | T0.3 |
| `hooks/registry-writer.js` (204) | `selfinstall` | T4.0, T5.1 |
| rest of `lib/commands.js` (3130 in total) | `cli` (root registration), `cli/<command>` (one package per command), `catalog`, `doctor`, `status` | T0.1 (registration), T0.8 (argument scan), T3.6, T4.1-T4.6 |
| `lib/windows-detached.js` (84) | `launch` (`_windows.go` stub) | section 3.9 |
| new | `jsonx`, `venue`, `gatewaytest`, `migrate`, `shadow` | T0.3, T0.4, T0.4b, T5.3, T5.4 |
| new, not Go | `plugin/` (plugin files), `.claude-plugin/marketplace.json`, `test/conformance/` | T5.2, T2.0b |

### 3.3 Third-party dependencies

The direct and indirect requirements below match `go.mod`. Lanes do not change the pins with `go get`.

Direct requirements:

| Module | Version | Used for | Why not hand-written or stdlib |
|---|---|---|---|
| `github.com/spf13/cobra` | v1.10.2 | command tree, help, shell completions | 17 commands plus extensions. For the 17 source commands cobra's flag parsing is switched off and the source's argument scan is ported (`lib/commands.js:199-201`), because pflag's unknown-flag mode consumes a following positional argument |
| `github.com/shirou/gopsutil/v4` | v4.26.10 | macOS executable path lookup; Linux `/proc` root override keys | `procinfo` uses these helpers, not gopsutil's process or listener census |
| `github.com/dlclark/regexp2` | v1.12.0 | pinned, but not sufficient for the tool-schema validity check (§3.1) | production validator undecided (§12) |
| `github.com/miekg/dns` | v1.1.73 | A/AAAA queries to the system name servers for the RC-compatibility bypass (errors closed, no fallback) | `net.Resolver` may consult `/etc/hosts`, which would loop back into the gateway (INV-02 §10.2) |
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
| `goodhosts/hostsfile`, `txn2/txeh` | managed block, adoption of an unmarked line, CRLF preservation and byte-exact rollback are specific (INV-03 §9) |
| `cloudflare/tableflip`, `kardianos/service` | belong to options B/C or to an OS-service model |
| `creativeprojects/go-selfupdate` | only if OD-5 chooses release archives; not pinned otherwise |

### 3.4 Identity and ownership of Go processes

- **Instance** = state root (absolute, symlinks resolved) + startup generation. **Process** = pid + birth time.
- A pid record `<name>.pid.json`, written atomically in the instance's state root:
  `{pid, command, startedAt, role, stateRoot, generation, port, version, image: {path, device, inode}}` (`command` and
  `startedAt` keep the source's field names; `<name>.pid` stays a decimal mirror that is rewritten from the JSON when it
  differs). The supervisor publishes its own record before it reports ready and re-publishes it on every recovery tick when
  it is missing or different; it writes the worker's and the proxy's records. A supervisor that cannot publish its record
  exits instead of serving untracked.
- **Authority to signal** exists only when a record in *this* state root names the pid, the live birth time equals the
  recorded one, the argv role matches and, for a listener, the port matches. A listener on our port without such a record
  is not ours, even when it is the same binary. The phase-3 signaling gate also requires a stable process discriminator;
  its representation awaits the user (§12).
- **Recovery without a record.** If the record is missing or unusable but the listener accepts this state root's control
  token, the CLI asks it to stop itself with an authenticated `POST /internal/stop` (same token, Host, Origin and
  content-type rules as `/restart`). No signal is sent on port or pid evidence alone; otherwise `stop` refuses and prints
  the pid `/healthz` reports and what to check.
- Port-owner classes: `ours`, `foreign`, `legacy-node` (argv and install root satisfy `lib/process-supervision.js:510-542`),
  `unowned`, `port-owned` (bound, pid not resolvable), `unknown` (unreadable, timed out, several listeners). Automatic
  paths act only on `ours`; the others produce the source's refusal texts (`lib/process-supervision.js:598-614`).
  On Linux, `procinfo` reads the full TCP listener table from `/proc` and checks its socket holders. On macOS, `lsof`
  alone identifies one distinct visible PID, even if it holds several listening sockets. Several visible holders are
  ambiguous. A clean exit status 1 with no output means none is visible; warnings or partial output yield `unknown`.
  Another user's listener may not be visible. A caller that needs to know the port is free must bind it.
- **Durable launches** start the stable binary; when none is installed (tests, a first run) the running executable is used
  and its image identity recorded. A worker respawn uses the recorded image and refuses when the file at that path is no
  longer the recorded image, unless `/restart` supplied an accepted candidate.
- `/restart` accepts an optional candidate (field `script`, as on the wire today; `binary` is an alias) only if it is the
  supervisor's recorded image or the stable installed binary (symlinks resolved, regular file, same owner uid, not group-
  or world-writable) whose `--version` is not older; anything else gets the source's 400 body
  (`lib/commands.js:2831-2857`).
- **Replacing the stable binary** is one algorithm for `setup`, `migrate` and `self update`: hard-link the current binary
  to `model-gateway.old-<version>-<n>`, write the new file beside the target, fsync, then one `rename` over the target.
  The path hooks call is never absent, and the previous binary stays recoverable under its `.old-*` name wherever the
  process dies.
- `model-gateway.old-*` files are deleted only when every live recorded process has a known image identity and none of
  them is that file; with incomplete evidence nothing is deleted.

### 3.5 JSON policies

| Where | Policy | Detail |
|---|---|---|
| Classification of a request body | parse like `JSON.parse` | the body must be exactly one JSON document (checked to EOF, also after `model` was found); duplicate names allowed, last wins; anything else goes to Anthropic unchanged (INV-02 §2) |
| Anthropic passthrough | original bytes | no parse result is ever written back |
| Proxy and Grok request bodies, rewritten JSON responses and SSE event data | **wire policy**: exactly what `JSON.stringify(JSON.parse(buf.toString()))` emits | an own decoder and encoder over a tree: bytes to text as Node's `toString()` does (ill-formed UTF-8 becomes U+FFFD); strings and names held as UTF-16 code units so an escaped lone surrogate stays distinct; duplicate names collapsed on the decoded name (last value, first position); canonical array-index names below 2^32−1 first, ascending; numbers through float64 with ECMAScript number-to-string, non-finite as `null`. `jsontext` is only the tokenizer. Codex forwards an unparseable JSON response or untouched SSE frame without rewriting it (§3.10). Grok skips unparseable SSE data and translates an unparseable non-stream response from null (`lib/request-worker.js:363-377,2330-2342`). |
| Claude settings files (the user's) | **file policy**: keep what the user wrote | ordered members and raw scalars are kept; only gateway-owned entries are edited; two-space indentation is used only for what is inserted. A file in which an owned key occurs twice is not edited: the command fails and names the key |
| Gateway state files (`pins.json`, `context-window.json`, `wired-projects.json`, pin and discovery caches, catalog) | typed values written as `JSON.stringify(v, null, 2) + "\n"` | Exceptions: the dispatch cache and both `codex-upstream-blocked.json` and `codex-upstream-unavailable.json` use compact `JSON.stringify(v) + "\n"` (`lib/request-worker.js:792`, `lib/codex-upstream-state.js:22-24,56,77`). The usage request-body high-water file uses compact `JSON.stringify(v)` without a trailing newline (`lib/usage-observability.js:202`). Match each format byte for byte. |

Goldens: the wire corpus and the state-file corpus are generated by Node and must match byte for byte with no exception
class (inputs: duplicate keys at several depths, `9007199254740993`, `1e400`, escaped lone surrogates, ill-formed UTF-8,
`4294967294` and `4294967295` as keys). The settings corpus consists of authored before/after pairs with non-canonical
spellings (`1.00`, `a`, unusual indentation, a byte-order mark), because Node's own output would rewrite them.

Settings writes: resolve a symlinked settings file and replace its target; keep the existing mode; re-read just before the
rename and, if the file changed since it was read, merge again (three attempts, then fail with the file name); refuse a
target that is not a regular file. An editor that writes between the last re-read and the rename can still lose its edit;
the window is smaller than the source's in-place write but not zero, and migration therefore requires a maintenance window
(section 3.6). The control token keeps its create-exclusive write (`lib/control-auth.js:23-33`).

### 3.6 Migration from the Node gateway (`migrate`)

Migration is an **offline, operator-run** operation. The two runtimes are never expected to coordinate: the source's lock
does not cover every legacy writer (a disabled `ensure` returns before the lock, `lib/commands.js:577-585`; `env`, `pin`
and the SessionStart registry writer never take it, `hooks/registry-writer.js:145-156`), so exclusivity comes from a
maintenance window, not from a fence. `migrate` never signals or starts a Node process and never touches the proxy binary.
From T4.1 on, `setup` and the SessionStart publisher refuse to act on a home with a legacy install and say "run migrate".

The operator's part, in the runbook with complete commands (T6.2): close every Claude Code session; disable the old plugin
`model-gateway@eigenwise-toolshed` (disabled, not uninstalled: with a wired endpoint a disabled plugin's Node gate exits 0,
`lib/recovery-policy.js:27-32`, while a missing install exits 2, `hooks/registry-writer.js:85-105`); stop the Node gateway
with its own CLI; then run `claude-code-model-gateway migrate --start` or `migrate --keep-stopped`. The end state is chosen
by that flag, never inferred from the stop marker that the Node `stop` just wrote.

| Step | Action | On failure |
|---|---|---|
| 0 Preconditions | nothing listens on the gateway port and no recorded legacy guardian or worker pid is alive; the old plugin is `false` in `enabledPlugins` of the user settings and of the current project's two settings files; no `claude` process of this user is visible (`--sessions-closed` attests what cannot be seen); no journal is in progress. A rerun with the same end-state flag resumes its own journal, and the journal's stage then decides what is checked: once a start intent is journaled (step 4), a listener on the gateway port is accepted when its pid record in the state root and its `/healthz` carry the generation of that intent, whether or not the result was journaled, and refused otherwise | refuse and name the unmet condition; nothing was modified |
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
journal that is neither `committed` nor `rolled-back`.

### 3.7 Shadow mode

`MODEL_GATEWAY_SHADOW=1` with `MODEL_GATEWAY_STATE_DIR` and non-default gateway ports starts a second instance for the
soak. The only resource of the default instance it may touch is a **dial** to `127.0.0.1:<configured proxy port>`, which
may be the live proxy. In shadow mode the gateway never binds a default port; never spawns, signals, adopts or upgrades a
proxy and never runs the proxy binary for an auth check (readiness comes from HTTP only, auth is reported `unknown`);
never refreshes Grok credentials; binds no RC-compatibility listener; writes nothing outside its state directory; refuses
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
| the make targets `test` (Tier 1: every package, without the `process` tag), `test-process` (Tier 2: tests that start a process, signal, bind a fixed port or exercise migration, `//go:build process`), `conformance`, `baseline-node`, `bench`, and every Node run | each target runs `hack/venue-check` first and exits non-zero with "not a test venue"; in a venue they run through `hack/remote` (copies the tree to `/tmp` on the Linux host, installs Go and Node there, runs the target, returns its log) or through the CI workflow. The Linux venue also needs `lsof` for the pinned suite's port-owner checks; `hack/baseline-node` refuses to run if a tool the suite spawns is missing |
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

The Darwin halves of T0.5 and T0.6 run on the `xcode-27` CI runner through pushed phase branches (OD-8). A build on the
developer machine is not evidence for a Darwin process test; the phase 3 gate requires the macOS venue results.

### 3.9 Windows

Out of the first milestone (OD-2). OS differences live in build-tagged files; the Windows files return a "not supported on
Windows yet" error for detached launch, tree kill, descriptor passing, the named pipe and atomic writes. CI builds
`GOOS=windows GOARCH=amd64 go build ./...`. The later Windows phase needs: launch outside the caller's Job Object
(`lib/windows-detached.js`), `taskkill /T /F`, `github.com/Microsoft/go-winio`, an alternative to `ExtraFiles`, `.zip`
extraction, PowerShell hook shell (INV-03 §8).

### 3.10 Differences from the source

Everything not listed here is ported as it is, including behavior the inventories flag as questionable. Declare the
source tests each difference changes before any run. The lists below come from the two review rounds and the test
inventory; report additions to the user before phase 2 starts. The newly proposed differences await user confirmation
(§12). T5.6 creates `docs/differences-from-node.md` after the differences are tested; until then, this plan holds the table.

The checker reads the last column: each backquoted `package.Test` or `package.Test/subtest` is followed by `(phase-N)`,
the gate that requires it to run and pass. Separate entries with `<br>`. The preceding column names the same tests and
says what they cover. A description starting with **Test pending:** names a required test that must be written and pass
before the listed gate can pass; related tests are supporting evidence only.

| # | slug | Source behavior | Port | Reason | Evidence at the pin (test paths relative to the plugin) | Required Go replacement test | Replacement tests (phase) |
|---|---|---|---|---|---|---|---|
| D1 | settings-atomic-replacement | User settings, pin overrides and the wiring registry are written in place. | Replace atomically; preserve the settings target's mode and symlink and re-read per §3.5. | A crash can truncate an in-place write. | No test requires in-place writes. Implementation: `lib/settings-wiring.js:111`, `lib/settings-wiring.js:203`, `lib/pins.js:358`; `test/atomic-file.test.js:41` tests the existing atomic helper, not the settings writer. | `internal/atomicfile.TestSettingsReplacement` (settings crash point, symlink target, mode and concurrent edits); `internal/pins.TestAtomicPinOverrides` and `internal/wiring.TestAtomicProjectRegistry` (interrupted replacement of `pins.json` and `wired-projects.json` leaves a complete old or new file). | `internal/atomicfile.TestSettingsReplacement` (phase-0)<br>`internal/pins.TestAtomicPinOverrides` (phase-4)<br>`internal/wiring.TestAtomicProjectRegistry` (phase-1) |
| D2 | readiness-hook-exec-form | The command string invokes the Node launcher. | Emit the absolute binary and separate `args` for `readiness-gate`, timeout 35, SessionStart matcher `startup\|resume`, no `onFailure`; recognize both forms as owned, preserve other arguments/hooks/groups, honor `disableAllHooks`. | Removes Node from the per-prompt path. | `test/wiring-mode.test.js:886` (assertion at `test/wiring-mode.test.js:898`), `test/readiness-gate.test.js:210`; `test/readiness-gate.test.js:32` and `test/readiness-gate.test.js:45` pin unchanged timeout, ownership and host-failure policy, not the literal command. | `internal/wiring.TestReadinessHookForms` | `internal/wiring.TestReadinessHookForms` (phase-1) |
| D3 | rc-control-requires-token | The RC endpoint uses a fixed header as its control credential. | Also require the control token; the CLI sends it. Keep Host, Origin, content-type and expected-pid checks. | Binding or releasing the compatibility listener requires instance authority. | `test/rc-compat-mode.test.js:876`, `test/shim-upload-abort.test.js:181` (the RC expansion); aborted uploads must still leave the sibling stream alive. | `internal/supervisor/rclistener.TestRCControlAuthorization` (valid token, missing token, each independent refusal); `internal/supervisor/rclistener.TestAuthenticatedUploadAbort` (abort an authenticated control upload during sibling SSE; require the same supervisor pid, the complete original sibling bytes and unchanged `unknown` reply for a malformed action). | `internal/supervisor/rclistener.TestRCControlAuthorization` (phase-3)<br>`internal/supervisor/rclistener.TestAuthenticatedUploadAbort` (phase-3) |
| D4 | recovery-recorded-proxy-adoption | An unhealthy shared proxy is replaced only if descended from this supervisor. | Also allow adoption with matching pid, birth time, role, port and a live listener in this state root; never in shadow mode or on health evidence alone. | Recovery otherwise waits for manual intervention. | No source test supplies an adoption record for a non-descendant. The foreign shared-executable case remains refused (`test/gateway-process-isolation.test.js:1720`). The original recovery predicate requires descent (`lib/process-supervision.js:1040`). | `internal/supervisor/proxyrecovery.TestRecordedProxyAdoption`: give a non-descendant listener a real matching pid/birth/role/port record in this state root; confirm unsuccessful probes cause recovery. Without that record, with a stale or ambiguous record, or in shadow mode, assert that no replacement occurs. | `internal/supervisor/proxyrecovery.TestRecordedProxyAdoption` (phase-3) |
| D5 | worker-crash-backoff | A crashed worker is respawned after 50 ms without backoff. | Quick exits within 10 s double the delay from 50 ms to 5 s; 60 s of uptime resets it. Exclude drain exits. Missing the 5 s port-report deadline remains fatal (exit 1); a backoff beyond the relay window (one try plus 80 waits of 50 ms) gives retryable 503. | Bounds the restart rate during repeated failures. | `test/gateway-drain.test.js:329` requires recovery, not its delay. No timing test; fixed interval in `lib/commands.js:2653`. | `internal/supervisor/workerproc.TestCrashBackoff` (fake clock, drain, reset, port deadline, relay window) | `internal/supervisor/workerproc.TestCrashBackoff` (phase-3) |
| D6 | worker-exit-retires-birth-record | A current-version worker's observed exit does not remove its birth record. | Delete that worker's birth record on observed exit; preserve orphan rules and newer or unknown births. | Otherwise each worker leaves a record. | No test requires the leak; `lib/commands.js:2652` removes the pid file, not the birth record. Retain `test/owned-worker-cleanup.test.js:135`, `test/owned-worker-cleanup.test.js:149`, `test/owned-worker-cleanup.test.js:159`. | `internal/supervisor/workerproc.TestBirthRecordOnExit` (observed exit, replacement birth, lost parent) | `internal/supervisor/workerproc.TestBirthRecordOnExit` (phase-3) |
| D7 | readiness-event-deadlines | Launcher stdin has no deadline; the launcher kills its child after 25 s and exits 2 on launcher errors regardless of event. | Bound stdin at 2 s (then treat as empty); start one 25 s gate budget after reading. On exhaustion name the phase and exit 2 for UserPromptSubmit or 1 for SessionStart. | The Go gate has no launcher child to kill. | `test/readiness-gate.test.js:169`; stdin and launcher error branches in `hooks/registry-writer.js:75`, `hooks/registry-writer.js:106`. | `internal/readiness.TestGateDeadlines` (blocked stdin, phase exhaustion, both events) | `internal/readiness.TestGateDeadlines` (phase-4) |
| D8 | settings-preserve-authored-json | Settings serialization orders integer-like keys first and normalizes escapes and numbers. | Apply §3.5 file policy only to Claude settings. A settings write is a set of member edits on the existing document, never an overwrite with a whole object, so members an edit does not name survive; the source's `writeSettings` (`lib/settings-wiring.js:107-112`) serializes the object it is given. | Preserve authored bytes outside edited entries. | No test requires the normalization: `lib/settings-wiring.js:111` serializes parsed JSON; `test/wiring-mode.test.js:886` covers preservation on a no-op, not the spelling of rewritten scalars. | `internal/jsonx.TestAuthoredSettingsCorpus` | `internal/jsonx.TestAuthoredSettingsCorpus` (phase-0) |
| D9 | stop-order-supervisor-first | Stop recorded worker, supervisor, then proxy; the supervisor can respawn the worker during stop. | Stop the supervisor first, then any remaining recorded worker, then the proxy. | Prevents respawn during deliberate stop. | No test requires this order: `lib/process-supervision.js:851`; `test/owned-worker-cleanup.test.js:322` pins the guardian/shim/proxy order of the separate `reapGatewayOrphans` sweep (`test/owned-worker-cleanup.test.js:328`), not the order of `stopAll`. | `internal/launch.TestStopPreventsWorkerRespawn`: observe the controller’s termination requests from entry to `stop`, not the later supervisor log event (`lib/commands.js:2561`); require supervisor first, then remaining worker, then proxy. Include a worker exiting during stop; no replacement worker may start from controller entry onward. | `internal/launch.TestStopPreventsWorkerRespawn` (phase-3) |
| D10 | ownership-requires-instance-record | An install-root argv can establish ownership without a record; a matching birth record can establish it with hidden argv. | Require a matching record in this state root per §3.4; supervisor publishes its own record. Recover a lost record by token-authenticated `/internal/stop`, not by signaling on pid evidence. | Distinguish instances of the same executable. | `test/gateway-process-isolation.test.js:367`, `test/gateway-process-isolation.test.js:392`, `test/gateway-process-isolation.test.js:422`, `test/gateway-process-isolation.test.js:1693` (the shared proxy is a descendant but has no instance record); `test/gateway-ps-locale.test.js:106` and the expansions of `test/gateway-ps-width.test.js:183` also rely on argv ownership; locale/width-independent probing remains required. | `internal/ownership.TestInstanceRecordAuthority` (two instances, missing/stale/partial record, authenticated stop) | `internal/ownership.TestInstanceRecordAuthority` (phase-3) |
| D11 | restart-recorded-or-stable-image | Restart reads only `script` (`lib/commands.js:2845`) and accepts a non-older sibling in the same plugin cache. | Accept the recorded image or validated stable binary through `script` or its `binary` alias (§3.4). | There is no versioned Go binary plugin cache. | `test/gateway-drain.test.js:116`, `test/gateway-drain.test.js:508`, `test/gateway-drain.test.js:541`, `test/gateway-drain.test.js:706`, `test/control-auth.test.js:49`, `test/gateway-process-isolation.test.js:1562`; older/foreign candidates stay refused (`test/gateway-drain.test.js:460`, `test/gateway-drain.test.js:562`). | `internal/supervisor/workerproc.TestRestartCandidate`: run both acceptance and refusal through each field spelling (`script`, `binary`), covering recorded image, validated newer stable image, older image, foreign path and symlink. | `internal/supervisor/workerproc.TestRestartCandidate` (phase-3) |
| D12 | linux-listener-proc-fallback | The async POSIX owner probe has no `/proc` fallback. | Read the Linux TCP listener tables and resolve their holders from `/proc`, without `lsof`. | Start/stop otherwise refuse with unknown ownership. | No test requires the missing fallback: `lib/process-supervision.js:476`; `test/shim-lifecycle.test.js:430` tests the **synchronous** lookup. | `internal/procinfo.TestLinuxListenerWithoutLsof` (identifiable, inaccessible, ambiguous) | `internal/procinfo.TestLinuxListenerWithoutLsof` (phase-0) |
| D13 | launcher-stable-binary | Launcher selects the newest registered install on each call; recovery checks `model-gateway@eigenwise-toolshed`. Codex credential-block guidance (no `expiresAt`) names `node "<command>" setup`; expiring 429 guidance contains no setup command and is unchanged. | Invoke one stable binary and quote its path without `node` in the credential-block guidance; recovery checks `model-gateway@claude-code-model-gateway` (§3.11). | Compiled binaries are not installed in the plugin cache. | The literal Node invocation changes in `test/codex-upstream-cases.test.js:88` (assertion 116), `test/context-window.test.js:882` (907) and `test/gateway-drain.test.js:733` (775), not their upstream-status or version-diagnostic behavior. `test/readiness-gate.test.js:121`, `test/readiness-gate.test.js:145`, `test/readiness-gate.test.js:155`; `test/stable-command-path.test.js:44`, `test/stable-command-path.test.js:50`, `test/stable-command-path.test.js:55`, `test/stable-command-path.test.js:63`, `test/stable-command-path.test.js:74`; active launcher selection in `test/proxy-upgrade.test.js:116`, `test/proxy-upgrade.test.js:186`, `test/proxy-upgrade.test.js:223`; `test/plugin-registry.test.js:92` pins the unchanged registry reader. `hooks/ensure.js` is dormant (§1.3) and its tests are not affected launcher cases. Credential-block and rate-limit branches: `lib/runtime.js:553-565`, `test/codex-upstream-cases.test.js:113-116`. | `internal/upstreamstate.TestReadinessGuidance/QuotedStableLauncher` (401 credential-block guidance, not expiring 429 guidance); `internal/selfinstall.TestStableCommandPath` (with the stable binary absent the resolver reports it missing and falls back to no other file; with it present the resolver returns that path; the printed advice names the stable binary and no interpreter); `internal/selfinstall.TestStableLauncher` (change the recorded plugin installation; prove the launcher still calls the same stable executable, forwards arguments and propagates status; check missing binary); `internal/readiness.TestPluginEnablement` (disabled id, absent id). | `internal/upstreamstate.TestReadinessGuidance/QuotedStableLauncher` (phase-1)<br>`internal/selfinstall.TestStableCommandPath` (phase-4)<br>`internal/readiness.TestPluginEnablement` (phase-4)<br>`internal/selfinstall.TestStableLauncher` (phase-5) |
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
| — | inherited-provider-labels | The label lookup returns inherited properties of the label object, such as the `constructor` function. | Capitalize an unrecognized provider name, so `constructor` becomes `Constructor`. | An inherited property is not a provider label. | `lib/proxy-models.js:14-28,155-157` | `internal/proxymodels.TestProviderLabel/success:_capitalize_constructor_provider` | `internal/proxymodels.TestProviderLabel/success:_capitalize_constructor_provider` (phase-1) |
| — | install-root-collation | Equal numeric versions are ordered by descending `lastUpdated` text using the host's default `localeCompare` collation. | Use root collation for that tie-break. | Select the same install regardless of host locale. | `lib/runtime.js:718-727`, `hooks/registry-writer.js:61-65` | `internal/install.TestRegistryRootCollation` (root rather than Swedish or byte order) | `internal/install.TestRegistryRootCollation` (phase-1) |
| — | pid-record-order-and-mode | Writes compact JSON with `pid`, `command` and `startedAt` in that order, with no explicit file mode. | Write the fields in the order specified in §3.4 and request mode `0600`; the creation mask may remove owner permissions. Never grant group or other permissions. | Make the extended record deterministic and keep it private. | `lib/process-supervision.js:80-102` | `internal/procinfo.TestRecordFormatAndPermissions` (exact JSON and mirror bytes and mode under permissive and ordinary creation masks) | `internal/procinfo.TestRecordFormatAndPermissions` (phase-0) |
| — | catalog-writer-is-string | `writtenBy` is the plugin manifest version, or null if unreadable or not a string. | Always write `writtenBy` as a string. | Keep the catalog field's type stable, including for builds without a version. | `lib/commands.js:85,242-247,1900-1908` | Test pending: `internal/catalog.TestMissingVersionWritesString` must assert the serialized field is a string when no version is supplied. `internal/catalog.TestEmptyCatalog` only covers an explicitly supplied `custom-build` version. | `internal/catalog.TestMissingVersionWritesString` (phase-1) |
| — | sse-preserve-malformed-utf8 | The Codex SSE transformer decodes UTF-8 before framing, so an untouched frame can carry replacement characters instead of its original bytes. | Keep the original bytes of a frame forwarded without a rewrite, even if it contains ill-formed UTF-8. Rewritten events still follow the JSON wire policy. | Forwarding an unmodified frame should not change its bytes. | `lib/request-worker.js:1668-1682,1705-1720` | Test pending: `internal/worker/sse.TestForwardMalformedUTF8Unchanged` must assert the client receives the original ill-formed bytes end to end through the worker relay. `internal/sse.FuzzFrameRoundTrip` has a seed for the frame scanner only. | `internal/worker/sse.TestForwardMalformedUTF8Unchanged` (phase-2) |
| — | macos-listener-visible-holders | On macOS the probe returns the first nonzero PID from `lsof`, or null for no match or a nonzero exit; the asynchronous probe returns `undefined` on timeout. | Use `lsof` alone. One visible PID is identified even if it holds several sockets; several visible PIDs are ambiguous; clean status 1 with no output is absent; diagnostic stderr or partial output is unknown. | An identified holder is the only one visible to the caller. Another user's listener may not be visible; this is a documented limit, not a tested property. A caller that needs the port must bind it (§3.4). | `lib/process-supervision.js:242-244,272-284,476-481` | `internal/procinfo.TestListenerFiles` (decoder outcomes, including partial output); `internal/procinfo.TestCommandWarningRefusal` (stderr refuses a successful or partial listing); `internal/procinfo.TestDarwinListenerEvidence` (one live visible holder). | `internal/procinfo.TestListenerFiles` (phase-0)<br>`internal/procinfo.TestCommandWarningRefusal` (phase-0)<br>`internal/procinfo.TestDarwinListenerEvidence` (phase-0) |
| — | reverse-model-spelling-by-snapshot | The policy reverse-spelling fallback lazily builds a process-wide map and keeps it when routes change. | Resolve reverse spelling from the published route snapshot. | Keep aliases, transport and windows consistent with the routes used for the request. | `lib/runtime.js:379-390` | `internal/policy.TestReverseSpellingFollowsRouteSnapshot` (two route snapshots with different alias collisions) | `internal/policy.TestReverseSpellingFollowsRouteSnapshot` (phase-1) |
| — | grok-ids-by-snapshot | A Grok request reads the model IDs from the cache file again and falls back to the default list if the cache is empty. | Resolve against the Grok ID list in the published route snapshot. | Requests see changes at publication and use one coherent set of routes. | `lib/request-worker.js:2596-2604`, `lib/grok-backend.js:427-441,454-456` | `internal/worker/route.TestGrokIDsFollowPublishedSnapshot` (new IDs visible only after publication) | `internal/worker/route.TestGrokIDsFollowPublishedSnapshot` (phase-1) |
| — | empty-pin-family-description | A Fable override such as `claude--5` reaches the display formatter with an empty family and throws on `family[0].toUpperCase()`. | Use the raw model ID in the description when the family is empty. | A malformed override must not prevent building the settings entries. | `lib/pins.js:37-44,400-409` | `internal/pins.TestEmptyFamilyDescription` (description and owned value use the raw ID) | `internal/pins.TestEmptyFamilyDescription` (phase-1) |
| — | anthropic-catalog-id-type | The catalog regexp coerces an ID to a string for filtering; an array with one matching ID can pass and is kept as an array. | Drop any row with a non-string ID on read. | Downstream consumers expect an ID string. | `lib/anthropic-catalog.js:20-30` | `internal/anthropiccatalog.TestRead/success:_drop_nonstring_id` (drop array ID, keep valid row) | `internal/anthropiccatalog.TestRead/success:_drop_nonstring_id` (phase-1) |
| — | endpoint-rejects-idna-hosts | LM Studio and OTLP usage endpoints use WHATWG `new URL`, which can normalize host and path spellings before their endpoint-specific checks. LM Studio rejects nonempty user info but allows empty user info; the usage endpoint parser does not check user info. | Use a stricter parser for LM Studio and OTLP. In addition to IDNA, reject empty user info, escaped hosts, backslashes, embedded controls and paths containing literal spaces or incomplete percent escapes. OTLP also rejects credentials. See `docs/env.md` for the setting-specific rules. | Refuse endpoint spellings the port does not normalize. | `lib/lmstudio.js:39-53`, `lib/usage-observability.js:605-615` | `internal/config.TestEndpointDecisions` (conservative refusals for credentials, hosts, backslashes, controls and paths) | `internal/config.TestEndpointDecisions/error:_conservative_reject_credentials` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_empty_userinfo` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_escaped_host` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_non-ASCII_hostname` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_punycode_hostname` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_backslash` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_embedded_tab` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_embedded_newline` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_incomplete_percent_escape` (phase-0)<br>`internal/config.TestEndpointDecisions/error:_conservative_reject_literal_spaces_in_path` (phase-0) |
| — | lifecycle-journal-regular-file-append | Appends one serialized line with `fs.appendFileSync` without checking the leaf. | Append each record through an `O_APPEND` primitive that accepts only regular files and does not follow a final symlink. Use one data-bearing write; retry `EINTR` only if no bytes were written. Report a short write without retrying the remainder. The primitive does not enforce a size limit; on macOS, repeated appends through fresh descriptors can exceed a file-size limit. | Do not write through a symlink or to a special file; do not split one record across writes that can interleave. | `lib/lifecycle-diagnostics.js:36-42` | `internal/lifecycle.TestSingleRecordAppend` (one complete record per call); `internal/atomicfile.TestAppendFileRefusals` (non-regular leaves); `internal/atomicfile.TestAppendFileWriteCalls` (one write, zero-byte `EINTR` retry, short write); `internal/atomicfile.TestAppendFileConcurrentRecords` (records stay intact). | `internal/atomicfile.TestAppendFileRefusals` (phase-0)<br>`internal/atomicfile.TestAppendFileWriteCalls` (phase-0)<br>`internal/atomicfile.TestAppendFileConcurrentRecords` (phase-0)<br>`internal/lifecycle.TestSingleRecordAppend` (phase-1) |

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
`remote-control` takes the first raw token as its action, even if it looks like a flag, and checks for `--confirm` by
presence (`lib/remote-control.js:137-145,194-198`). The other commands do not scan command-specific arguments;
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
simple, `gpt62-sol-fast-executor`; **astra** = complex, `gpt62-astra-fast-executor`. "Needs" lists every prerequisite; a
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
| 0 | Foundations, isolation, identity seam, test inventory | AC-1, AC-2; test inventory (T0.7) reviewed and landed | 🔶 in progress |
| 1 | Pure logic packages | AC-3 (tables), AC-8 for phase 1 rows | 🔜 not started |
| 2 | Walking skeleton, launch adapter, worker | AC-3 (HTTP), AC-4, AC-5 (worker paths), AC-11 for tests assigned to phase 2 | 🔜 not started |
| 3 | Supervisor and process layer | AC-5 (supervisor paths), AC-6 (start/stop/recover), AC-14, security review 1 (AC-13) | 🔜 not started |
| 4 | CLI and settings | AC-6 (ensure/setup), AC-9, AC-12, AC-11 for the CLI set | 🔜 not started |
| 5 | Packaging, migration, shadow mode | AC-7, AC-15a, AC-16, security review 2 (AC-13) | 🔜 not started |
| 6 | Rehearsal, shadow soak and cutover | AC-10, AC-15b; phase 6 after the rehearsal is run by the user | 🔜 not started |

Phase verification runs the cases marked `gate=phase` at that phase's exit gate, alongside the acceptance criteria above.
The inventory assigns 151 cases to phase exits: 1, 2, 130, 3, 15 and 0 for phases 0 through 5, respectively. Phases 2–4
have numbered gate tasks; phases 0, 1 and 5 use the exit gate in the table directly.

Current stop point: phase 0 is in progress. The lead reports this table at every gate and starts the next phase when
its predecessor's gate passes, through T6.0. Stop before T6.1: the shadow soak, cutover and removal of the old plugin
belong to the user.

### Phase 0: foundations

Phase 0 works on branch `port/phase-0` in the main checkout until T0.0 is committed; lanes use worktrees from then on.

| Task | Work | Size / lane | Needs | Done when |
|---|---|---|---|---|
| T0.0 | Inputs every worktree and venue needs. Commit the user's `main.go` and `README.md` unchanged as the first commit of the branch. Commit the seven inventory documents, the review records and this plan under `docs/port/`. Do not commit the `gpt62-*.md` agent definitions; lanes use `~/.claude/agents/` from any checkout. Add `hack/pinned-source`: it materializes the whole repository tree of commit `a8501f251755` (1,005 files, 21.5 MiB) into the ignored directory `.cache/model-gateway-js/`, from a local checkout or from `https://github.com/zchee/eigenwise-toolshed` (public, MIT), and fails unless the root tree hash is `22be5dae8859c7307906f938fa364b7ffadf006c` and the tree hash of `plugins/model-gateway` is `aaf4f26b527c2ff08dbc47707a5f69e6ec2cf40a`. The whole tree is exported because the source tests read three files outside the plugin: `plugins/sidequest/lib/suite-resolver.js` (`test/tool-schema-compat.test.js:8`, which also resolves the repository root at `:96`), `plugins/test-support/windows-hide.js` (`test/windows-hide.test.js:6`) and `docs/src/content/docs/getting-started/model-gateway.md` (`test/gateway-readiness.test.js:219`, `test/rc-compat-mode.test.js:280`). Every source citation of this plan and of the inventories resolves under `.cache/model-gateway-js/plugins/model-gateway/`. Add the source's root `LICENSE` text to `THIRD_PARTY_NOTICES.md`. Start `docs/port/upstream-sync.md`, the ledger of source commits after the pin (empty on 2026-11-25); they are applied only in T5.8 | S / sol | approval | `hack/pinned-source` succeeds in a fresh worktree; the materialized plugin has 33 production JavaScript files with 13,060 lines; `sed -n '2951,2953p'` of its `lib/commands.js`, run from that worktree, prints the stop command's marker write; the three outside files exist in the export |
| T0.1 | Bootstrap on `main.go`; no source behavior. `func main` calling `internal/cli`; cobra root with the 17 source commands registered as stubs that exit 1 with "not implemented", each with cobra's flag parsing switched off; real `version` (section 3.11: linker value verbatim, else released module version without `v`, else `0.0.0-dev+<full revision>` or bare `0.0.0-dev` without a revision; `+dirty` is development) and `completions`; dependencies of section 3.3 pinned; license header from `main.go:1-13` on every Go file; `Makefile` pins `gofumpt` v0.12.0, `modernize` from `golang.org/x/tools` v0.53.0, `goimports-rereviser` v4.2.0 and `golangci-lint` v2.14.0; targets `fmt` (path-scoped), `fmt-check`, `lint`, `test`, `test-process`, `conformance`, `bench`, `tripwire`; `.golangci.yaml` with `gochecknoinits`; `.github/workflows/test.yaml` on `ubuntu-26.04` and `xcode-27` with `actions/checkout@v7`, `actions/setup-go@v7`, `golangci/golangci-lint-action@v9`, a Windows compile-only step and no test step | M / sol | T0.0 | `go build ./... && go vet ./... && make fmt-check lint` exit 0; `GOOS=windows go build ./...` exit 0; the `version` tests are written and compile (they run for the first time in T0.4) |
| T0.4 | Venue tooling (section 3.8): `hack/remote` with `provision` and `negative-control`, `hack/venue-check`, the package `internal/venue` with `Require` (it imports nothing of the product), `hack/lint-venue` with its reject fixture, build tag `process`, `hack/tripwire`, and the make targets wired to them; add the test steps to `.github/workflows/test.yaml` with `hack/venue-check` | M / astra | T0.1 | the user approved the venue; provisioning and the checks of section 3.8 succeeded on it. The Linux version and `internal/venue` tests pass, both negative-control fixtures pass, and `hack/lint-venue` rejects missing and non-first guards for both tag sets. The standalone developer-host shell preflight returns the expected refusal. No local build or process execution is used as evidence for a test |
| T0.7 | Test inventory (data). `test/conformance/classification.tsv` has one row per expanded case: `path`, `line`, `column`, `name`, `class`, `phase`, `package`, `gate`, `note`. The first four fields identify the case. Files under `test/` contain no task, decision or inventory ids. For `gate=task`, `phase` and `package` identify the implementing task in `docs/port/test-ownership.md`: the case can pass when that task is done, but a black-box case is due no earlier than the launch-adapter proof. `gate=phase` means the case needs other work in the same phase and is due at its exit gate. Exempt cases have no phase, package or gate. Classes: `conformance`, `go-port`, `difference:<slug>`, `js-only:<reason>` (reasons: `node-harness-self-test`, `js-source-lint`, `out-of-root-dependency`, `developer-probe`), `unsupported:<reason>` (reasons: `windows-only`, `dormant-code`). Use the section 3.10 slug only as a whole-case difference when every assertion changes; a mixed case is `go-port`, with the changed assertion lines and slug in `note`. Use `js-only` only when every assertion is a JavaScript implementation artifact. User-visible message text is behavior even when the test scans a source file; an in-process `require` does not make a behavioral check `js-only`. Run `hack/baseline-node` through the venue-gated `make baseline-node` via `hack/remote`. Materialize and verify T0.0's whole pinned tree in that venue; run locked `npm ci` in the exported plugin (`ajv@8.20.0`), then its unmodified package test script with the isolation preload (Node ≥ 22.5). Use an allowlisted environment and a PATH on which no real `claude` resolves (`test/picker-guard.test.js:110-130` uses `which`). Record the pin, tool versions, suite exit and terminal runner events keyed by source path, registration line/column and full parent/child name, with status. `hack/baseline-extract` checks the runner output per registration site and stores the census independently of the classification. `test/conformance/launch-sites.tsv` records direct gateway launch sites and their owning cases for T2.0b. For a nested case's launch, a second row with the same coordinates and arguments attributes it to the parent (`via` = `subtest: <child name>`), not to siblings or to another process. `docs/port/parity-matrix.md`: the invariant rows of INV-05 §4 split into single assertions, each naming its tests, with a ledger of the rows in each phase's scope; `docs/port/parity-matrix.lock`: its row ids as approved in review, changed afterwards only with the lead's recorded approval. The difference table of section 3.10 completed. A separate lane reviews the result. Record one row per exempt case in `test/conformance/exemptions-approved.tsv`, with its case key, class and approving review. A `js-only` or `unsupported` exemption is valid only with a matching row; a new or changed exemption needs a new independent review | L / astra | T0.0, T0.4 | all 57 files finished discovery. Of 693 line-start `test(` text matches (662 unindented, 31 indented), 692 register tests; the remaining match is child-suite source in the template literal identified under Source. With five nested `t.test(` sites, this gives 697 registration sites and 832 cases (35 sites expand, including the three `relay-stream-abort` sites that yield six cases, INV-05 §2). Each site reports at least one case; each reported case maps to a site. Classify every expanded case, including skips; T0.7b's `structure` check compares expanded identities against the classification. Record reasons for skipped or individually failed baseline cases; an abort, incomplete registration, missing input, unknown identity or unexplained discrepancy blocks completion. The independent classification and exception review is recorded |
| T0.7b | Test inventory (tools). In the venue `hack/assertion-sites` uses a pinned JavaScript parser and the preserved expanded-case census to produce the expected relation independently of the matrix, keyed by case and assertion source path/line/column. Include nested tests, helper and callback edges, and JavaScript passed to child interpreters or preloads. Under `test/conformance/`, `assertion-pairs.tsv` is generated in the venue from the pin and never edited: each row records the case key, assertion site, call and how the case reaches it. `assertion-overrides.tsv` records independently reviewed manual additions and removals of pairs. `hack/assertions/call-dispositions.tsv` records scoped dispositions and reasons for calls the extractor could not follow, with assertion references for attributed child launches; extraction rejects missing, invalid or stale dispositions. Review generated or unresolved code without evaluating it. `assertion-sites.tsv` is the editable handoff mapping: one row per pair with due phase, target and review. The assertion defaults to the case's phase; it may be due later, never earlier. Assertions on shipped text or configuration are due with packaging in phase 5; for wording in unshipped source documentation use the individually reviewed `source-doc-wording` exemption. In `test/tool-schema-compat.test.js`, calls at `:199`, `:311`, `:321` and `:344` reach the five `refusal` assertions at `:46-50` from three registrations (`:184`, `:285`, `:331`): 15 distinct case/site pairs. For a `go-port` case use a reviewed package-qualified full Go subtest name or `not-ported:<reason>` from the closed list (`js-runtime-artifact`, `harness-internal`, `source-doc-wording`, `changed-by-difference:<slug>`) with the reviewer's approval for that pair. `hack/parity-check` has three modes, each with negative fixtures. `structure`: compare the generated pair set, after reviewed overrides, with the editable mapping's pair set in both directions; check exempt cases against `exemptions-approved.tsv`; require unique keys, classification of every static site and expanded case, matrix coverage of every `conformance` and `go-port` case, exact matrix row ids against the lock, closed reasons and valid difference references. Do not require targets. `mapping <phase> [package]`: select by `phase`, `package` and `gate` using `docs/port/test-ownership.md`; require a nonempty phase ledger; check all `go-port` pairs due in the selected scope by their assertion due phase for the reviewed target or individually approved exemption. For a shared subtest record the reviewed unconditional check for each obligation in the review column; a conditional check needs a distinct observable subtest or recorded assertion event. A negative assertion maps to an observable check that the forbidden effect did not occur. `results <phase>`: require fresh, uncached, non-skipped passing `go test -json` evidence for each due Go target, a passing replacement test for each due difference, and a passing result with an attributed Go launch for each due `conformance` case. Require approval for each due `unsupported` row | M / sol | T0.1, T0.7 | `structure` passes on T0.7's data; each mode rejects its fixtures: a missing expansion, an omitted matrix row, a matrix row deleted while its case stays named by another row (the five `assert.match` calls of `test/gateway-readiness.test.js:221-225` are the model), one pair of a shared helper deleted while another case still covers its site, a missing embedded assertion, a reason outside its list (`structure`); an unmapped pair or unapproved exemption (`mapping`); an absent, skipped, wrong-package or unobserved target (`results`) |
| T0.9 | Test handoff for phase 0: the `go-port` tests that T0.7 assigned to T0.8, T0.2, T0.3, T0.4b (the guard) and T0.5, written from the pinned tests and the inventories by test lanes that implement none of those tasks. The tests fix the package API they call and fail until the behavior exists; each lane fills the target column of `assertion-sites.tsv` for its pairs, and a reviewer other than the author confirms them | M / astra | T0.7, T0.7b | merged on the phase branch; `hack/parity-check mapping phase-0` passes |
| T0.8 | `cli/argscan`: the source's argument scan, usage text, unknown-command behavior and the `--json` log routing (INV-01 §0-§2), used by every command | S / sol | T0.1, T0.9 | the source's argument and usage cases pass as table tests |
| T0.2 | `paths`, `config`: one table of every environment variable with name, default, parse rule and reader, derived from the pinned source including `bin/` and `hooks/` (INV-04 §1 is a starting point). Exclude `ANTHROPIC_UNIX_SOCKET`: it is a legacy settings key removed during wiring (`lib/commands.js:1187-1196`), not a gateway environment input. The gateway reads `CODEX_GATEWAY_SOCKET_PATH` for its listener (`lib/runtime.js:47-49`). Implement `jsNumber` (INV-04 §12) and home, config and state resolution incl. `MODEL_GATEWAY_STATE_DIR`; generate `docs/env.md` from the table | M / sol | T0.1, T0.9 | a table test covers every variable; `docs/env.md` regenerates without diff |
| T0.3 | `jsonx` (the wire encoder and decoder, the settings file policy, the state-file writer of section 3.5) and `atomicfile` (D1). The Node-generated corpora are produced in the venue by a committed generator script that runs plain Node on JSON inputs (no plugin code), and are committed | L / astra | T0.1, T0.4, T0.9 | wire and state-file goldens match byte for byte; the authored settings pairs pass; a 60 s fuzz run of "settings parse, no edit, write" changes no byte |
| T0.4b | `gatewaytest` environment and fakes as real HTTP servers and helper processes; the product guard of section 3.8 | L / astra | T0.2, T0.4, T0.9 | in the venue: with the guard set, binding 18764, 18765 or 80, running a real `claude` and writing a settings file outside the sandbox each fail with a named error; a helper process started with an empty environment finds no gateway home and no listener; one sample process test runs |
| T0.5 | `procinfo` (argv, birth time, parent, table, liveness, port owner with the probe budget `CODEX_GATEWAY_PROBE_TIMEOUT_MS`, default 2 s) and the pid-record schema of section 3.4 with its atomic writer and reader. Venue tests: renamed, replaced and unlinked running binary; record crash points (after spawn, between the JSON and the decimal file); a non-UTC time zone; an injected start date with a single-digit day | M / astra | T0.3, T0.4b | tests pass on Linux; the Darwin run on `xcode-27` is recorded |
| T0.6 | Measurements of the pinned Node gateway in the venue, with machine, commands and raw samples in `docs/port/baseline-node.md`; `hack/bench`. (a) the three baselines of AC-9; (b) behavior probes: what happens to the provider request on a client disconnect at each stage, direct and relayed (section 3.10); how a client `Upgrade:` request is answered (INV-02 §14 item 21). What the real `claude -p` returns during the pin probe (INV-04 §13 item 4) is not measured: a fake CLI returns only the status it was programmed with, so `pin` is ported from the source's branches, both statuses are unit-tested, and the question stays open (appendix B) | M / sol | T0.4b | every probe has a recorded observation that later tasks cite |

### Phase 1: pure logic (packages are disjoint; two venue-only jobs are named)

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T1.0 | Test handoffs for phase 1, one per package and one test lane each: the `go-port` tests that T0.7 assigned to T1.1 through T1.13, with the target column of `assertion-sites.tsv` filled and reviewed; a package's handoff passes `hack/parity-check mapping phase-1 <package>` for its assigned pairs; after all package handoffs merge, `hack/parity-check mapping phase-1` must pass for the entire phase | at most M per package / astra | T0.7, T0.7b, T0.9 |
| T1.1 | `policy`: the model window table as ordered data (every row of INV-04 §3.2, exactly as pinned), policy functions (§3.3), context-window settings and CLI flag parsing (§3.4), sentry policy and trigger arithmetic; tests from INV-04 §3.5 | L / astra | T0.2, T1.2, T1.0 |
| T1.2 | `proxymodels`: listing parse, route table, advertised ids and the Fable respelling (INV-04 §7) | M / sol | T0.2, T1.0 |
| T1.3 | `localprov` response parsers, `proxyconfig`, `install` (installed-plugin registry read, version compare with the collation tie-break) (INV-04 §8, §11) | M / sol | T0.2, T1.0 |
| T1.4 | `pins` (version parse and compare, precedence, catalog-following rule, env blocks, ownership of values) and `anthropiccatalog` (cache schema, merge, "(previous)" relabel) (INV-04 §5, §9); no process probe | L / astra | T0.3, T1.1, T1.0 |
| T1.5a | Tool-schema corpus job (venue): the pinned `lib/tool-schema-compat.js` and `ajv@8.20.0` under Node produce the expected outputs for the corpus (the source tests' schemas, the counterexamples of `test/fixtures/prior-adapters/`, plus generated cases); the outputs and their generator are committed | S / astra | T0.4 |
| T1.5b | `toolschema`: skip unaffected patterns. For affected patterns, run the compatibility scan with the source's reason codes and JSON Pointers (INV-02 §9.1). If it succeeds, validate the original pattern in ECMAScript Unicode mode before applying the rewrite. Tests consume the committed corpus. The validator choice is open (§12). | L / astra | T0.3, T1.5a, T1.0 |
| T1.6 | `worker/sentry`: the state table of INV-02 §5 (keys, latch, rearm, stop-reason rule, compaction exemption), overflow parsing, usage merge; timers under `testing/synctest` | L / astra | T1.1, T1.0 |
| T1.7 | `worker/route`: the id-prefix rules of `lib/pins.js:17-28` and the classifier (the 15 rows of INV-02 §2.2, the precedence for a body that is not JSON, the catch-all to Anthropic for any method and path) and the model snapshot type (one immutable value swapped atomically; update order as INV-02 §12) with a race test | M / astra | T1.1, T1.2, T1.0 |
| T1.8 | `worker/dispatch`: marker regex, route cache with TTL, LRU and write-on-read persistence (INV-02 §7) | M / sol | T0.3, T1.0 |
| T1.9 | `grok` translation: request, JSON response and stream translation as pure functions over the frame scanner's output (INV-02 §8) | L / astra | T0.3, T1.13, T1.0 |
| T1.10 | `wiring`, `readiness` (pure parts): effective base URL and precedence, env block, ownership of entries, readiness-hook sync for both forms (D2), wired-projects registry, repair, legacy migration, recovery policy with the configurable plugin id (INV-04 §6; INV-01 §4.3, §4.4) | L / astra | T0.3, T1.3, T1.4, T1.0 |
| T1.11 | `lifecycle`: directory lock in the source's protocol, with stale detection decided by a pure function over an injected process-identity source (pid and birth time); stop marker; bounded `lifecycle.jsonl` (INV-03 §6, §7). A `process`-tagged test in the venue runs two real processes that contend for the lock, one of them killed while holding it | M / astra | T0.5, T1.0 |
| T1.12a | `controlauth` (token file, Host/Origin predicates), `upstreamstate` (INV-03 §3.2; `lib/codex-upstream-state.js`) | S / sol | T0.3, T1.0 |
| T1.12b | `rccompat` hosts-block functions (INV-03 §9) | S / sol | T0.1, T1.0 |
| T1.12c | `catalog` builder and slug rules, with the readiness inputs of `lib/commands.js:1885-1908` as an explicit interface (INV-01 §3.9) | M / sol | T0.3, T1.1, T1.0 |
| T1.12d | `usage` record builders, bounded snapshots, request-body high-water files (INV-04 §10) | M / astra | T0.3, T1.0 |
| T1.13 | `sse`: the raw frame scanner (CRLF and LF delimiters, comments, several data lines, unterminated tail). An untouched frame is re-emitted byte for byte; a rewritten frame has its lines joined with LF and its own delimiter re-emitted as matched; the tail is emitted without a delimiter (`lib/request-worker.js:1675-1721`). Byte-exact cases for a rewritten CRLF frame and for an unterminated tail; a fuzz target | S / astra | T0.1, T1.0 |

### Phase 2: walking skeleton, launch adapter, worker

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T2.0 | Walking skeleton. Worker: both modes of section 3.1, TCP and Unix-socket listeners (stale socket reclaimed only on `ECONNREFUSED` and a socket file), `/healthz` shape of INV-02 §1.1, `/drain`, in-flight tracking, `HEAD`, `/v1/models` with the fallback list. Supervisor: listener, Host/Origin gate, relay happy path, `/healthz` augmentation, worker spawn with the report and liveness pipes, pid records, SIGTERM/SIGINT orderly stop with lifecycle events, the two stdout lines the harness waits for, the test control channel of section 3.1. `serve-shim` and `serve-worker` replace their stubs | L / astra | T0.5, T1.7, T1.11, T1.12a |
| T2.0b | Launch adapter and its proof (Tier 2). The source's unmodified tests must be able to start, observe and stop the Go binary. Preferred adapter: keep `process.execPath` real and redirect only gateway launches, at the harness seams (`test/support.js:766-829`) and at the direct spawn sites that T0.7 listed; second choice: a preload that points `process.execPath` at a wrapper which `exec`s the Go binary for the CLI path; a spawning trampoline only for tests classified as independent of the child's pid. The harness's IPC shutdown reaches the test control channel of section 3.1, built in T2.0; that descriptor is never passed to the worker or the proxy. **Launch-only probes**, written for the skeleton and independent of later features: argv and environment as the harness sends them; the stdout line followed by `/healthz` 200; `health.supervisorPid` equal to the harness's child pid; fixture shutdown over IPC; worker `/drain` with the fixture token answering 202; orderly SIGTERM with lifecycle records (the complete test `gateway-drain.test.js:390-424`); teardown and removal of the fixture home; a copied fake proxy and a fixture that replaces `NODE_OPTIONS` still work. The adapter logs test name, Go pid and build id for every launch | L / astra | T2.0, T0.7 |
| | **Decision (lead; recorded before T2.1 and T2.2 start):** every launch probe passes → the `conformance` class stands. Each such case runs unchanged at the gate specified by `test/conformance/classification.tsv`: `gate=task` means the gate of the task that builds the case's package, but no earlier than the launch-adapter proof; `gate=phase` means the case needs more of the phase and runs at its exit gate. Of 258 conformance cases, 133 are due at a phase gate. Of the 177 in phase 2, 6 are due at the adapter's gate, 44 at later task gates and 127 at the phase gate. A probe that cannot pass reclassifies only the tests that depend on that seam to `go-port`. A failing behavioral test is a product defect, never a reason to change the adapter or the class | | |
| T2.1 | Model cache and `/v1/models`: proxy listing refresh (60 s; retry 1 s doubling to 30 s), fallback catalog, live probes of llama-server `/props` and LM Studio `/api/v1/models`, Anthropic catalog refresh with header credentials kept in memory, picker aliases, discovery-cache sync (off in shadow mode), shared readiness check (15 s, single flight) (INV-02 §6; INV-04 §8) | L / astra | T2.0b, T1.3, T1.4 |
| T2.2 | `worker/upstream`: header sets of INV-02 §3.2, the transport rules of section 3.1, hosts-bypass dialer with `miekg/dns` (one cache slot, 5 min; when both lookups fail the stale slot is served, otherwise an error; never the OS resolver, `lib/request-worker.js:894-909`) | M / astra | T2.0b |
| T2.3 | `worker/reply`: the buffered response branches R1-R6 and the JSON half of R7 of INV-02 §4.1, 413 normalization (genuine and legacy overflow), Codex auth-failure and empty-completion mapping, the rejected-WebSocket-upgrade retry (2 retries, 250 ms), 502 bodies | L / astra | T2.2, T1.6 |
| T2.4 | `worker/sse` on the frame scanner of T1.13: relay with a flush per chunk, 20 s heartbeat on proxy streams only, model rewrite and plan-tool filtering per event, side-branch decoding for observation with the 4 MiB budget, first-event peek that replays the original bytes; the streaming half of R7 and the untouched passthrough R8, which completes Anthropic forwarding (INV-02 §4.2-§4.5); client-disconnect behavior as measured in T0.6 | L / astra | T2.2, T1.13, T0.6 |
| T2.5 | Proxy branch request side: dispatch resolution, deferred-tool hoisting (INV-02 §3.3), schema compatibility, plan tools, thread refusal, sentry check; `count_tokens` and the path rules of INV-02 §2.3 | L / astra | T2.3, T1.5b, T1.8 |
| T2.6 | `worker/compactguard`: buffering, retries only while the client is writable, outcomes (INV-02 §4, §11) | M / astra | T2.4 |
| T2.7 | Grok branch: credential file, single-flight token refresh (none in shadow mode), forward, stream translation, sentry on the Grok path | M / astra | T2.2, T2.4, T1.6, T1.9 |
| T2.8 | Usage observability, route telemetry (OTLP/HTTP JSON to loopback only), request-route log, request-body high-water files (INV-02 §11; INV-04 §10) | L / astra | T2.4, T1.12d |
| T2.9 | Phase gate: every source test that T0.7 assigned to phase 2, in its class, plus the Tier 1 integration suites | M / astra | T2.1-T2.8 |

### Phase 3: supervisor and process layer

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T3.1 | `ownership`: record publication and reconciliation by the supervisor, the classes and the authority rule of section 3.4, the authenticated `/internal/stop`, kill primitives (SIGTERM, 3 s, re-check, SIGKILL; success judged by the process being gone), two-instance and missing-record tests | L / astra | T0.5, T2.0 |
| T3.2 | `supervisor/proxyrecovery`: the decision sequence of INV-03 §4.6, backoff `min(30 s, 1 s·2^(n-1))`, 30 s grace, fresh-connection confirmation, single flight, D4, and the unmanaged mode used by shadow | L / astra | T3.1 |
| T3.3 | `supervisor/relay`: the full state machine of section 3.1, client-disconnect behavior as measured in T0.6, the 64 MiB cap with the 250 ms grace on 413, upload-abort paths, 503 bodies; `test/shim-request.test.js` ported case by case; a fake worker and a fake upstream that count requests | L / astra | T2.0 |
| T3.4 | `supervisor/workerproc`: respawn with D5, restart with drain and kill at the drain timeout, `/restart` with the candidate rule (D11), birth records and orphan reaping (D6) | L / astra | T3.1, T3.3 |
| T3.5 | `supervisor/rclistener` and control endpoints: `/internal/rc-compatibility` probe and reconcile (D3), compatibility listener, failure file | M / astra | T3.3 |
| T3.6 | `launch`, `shimstate`, and the commands `start`, `stop` (D9), `recover`: the plan table of INV-01 §3.3, readiness wait (300 ms poll; 12 s quiet, 42 s otherwise) | L / astra | T3.2, T3.4, T3.5 |
| T3.7 | Phase gate (Tier 2): conformance set or fallback tests for the lifecycle files; ownership matrix of AC-6 for `start`, `stop`, `recover`; relay counts of AC-14 | M / astra | T3.6 |
| T3.8 | Security review 1 (grok lane): control token, Host/Origin gate, relay, RC endpoint, pid-record trust | review | T3.7 |

### Phase 4: CLI and settings

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T4.0 | `selfinstall` core: install the running executable at the stable path (the replacement algorithm of section 3.4; verified `--version`; the sweep rule of section 3.4), refusal for unversioned builds | M / astra | T3.1 |
| T4.1 | `release`, `setup` without migration: latest-release fetch with the token rule and cross-origin `Authorization` drop, checksum fail-closed, extraction with `archive/tar` + gzip, binary swap with rollback, the steps of INV-01 §3.1; on a home with a legacy install `setup` refuses and names `migrate` | L / astra | T3.6, T4.0, T1.10 |
| T4.2 | `ensure` (the `--quiet` contract of INV-01 §3.2: one JSON object, or empty stdout with the notice on stderr), `status`, `models`, `login` | L / astra | T3.6, T1.10 |
| T4.3 | `env` (print, `--write-project`, `--write-user`, `--remove`, `--reconcile`, retired flags exit 2), `pin` with the isolated `claude` probe (INV-04 §5.6), `context-window` with `autoCompactWindow` sync | L / astra | T1.10 |
| T4.4 | `catalog`, discovery-cache writer, `doctor` (the 20 sections of INV-01 §3.6 plus lines for build info, state-dir override, legacy hooks and the installed hook form), `pickerguard` | L / astra | T2.1, T4.3 |
| T4.5 | `remote-control enable/disable/doctor` with backup, race guard and byte-exact rollback (INV-03 §9) | M / astra | T3.5, T1.12b |
| T4.6 | `readiness-gate`: stdin payload, policy, healthy fast path as in the source (`lib/readiness-gate.js:10-32`), budget D7, exit 2 only for UserPromptSubmit | M / astra | T3.6, T1.10 |
| T4.7 | Phase gate (Tier 2): `testscript` suites for every command (AC-12), conformance set or fallback tests for the CLI files, ownership matrix of AC-6 for `ensure` and `setup` | L / astra | T4.1-T4.6 |

### Phase 5: packaging, migration, shadow mode

| Task | Work | Size / lane | Needs |
|---|---|---|---|
| T5.1 | Trampolines, breadcrumb, `hook session-start` with the publisher rule of section 3.6 (section 3.11); tests for both launchers: arguments, stdin, exit status, signals, missing binary | M / astra | T4.1 |
| T5.2 | `plugin/` and `.claude-plugin/marketplace.json`; both skills rewritten for the Go CLI with the statements of appendix A corrected; cold-install test (AC-15a) | M / sol | T5.1 |
| T5.3 | `migrate` forward and rollback with the journal (section 3.6); Tier 2 tests in an isolated home holding the pinned Node plugin, with its supervisor started and stopped by the Node CLI (AC-7) | L / astra | T5.1, T5.2, T4.4, T4.6, T2.0b |
| T5.4 | Shadow mode, `shadow preflight`, `soak.jsonl` (section 3.7); test that removing the proxy under a shadow instance never makes it spawn or signal anything (AC-16); `shadow report` with fixtures that satisfy and violate each check of AC-10 | L / astra | T3.2, T2.1, T2.7, T2.8 |
| T5.5 | Only if OD-5 chooses release archives: goreleaser v2 config, `.github/workflows/release.yaml` with `goreleaser/goreleaser-action@v7`, `self update` | S / sol | T4.0 |
| T5.6 | Docs after the differences are tested: `README.md` (extends the user's stub), `docs/differences-from-node.md`, `docs/port/parity-matrix.md`, the final parity matrix | S / sol | T4.7, T5.3, T5.4 |
| T5.7 | Security review 2 (grok lane): release download and verification, self-install, launchers, hook installation, plugin wrapper, migration, shadow mode | review | T5.2, T5.3, T5.4 (and T5.5 if it exists) |
| T5.8 | Upstream sync, after parity is shown against the pin: triage and apply the source commits that `docs/port/upstream-sync.md` lists by then (none on 2026-11-25), each with its tests | S / sol | T5.6 |

### Phase 6: rehearsal, soak, cutover

| Task | Work | Who | Needs |
|---|---|---|---|
| T6.0 | Rehearsal (Tier 2): in one isolated home with the pinned Node plugin installed, follow the runbook steps with fresh CLI processes and the hook commands of both plugins invoked as the host invokes them: stop Node, migrate, roll back, start Node, migrate again | astra lane | phase 5 |
| T6.1 | Shadow soak with real sessions (AC-10); the runbook with complete commands is written at that time | the user | T6.0 |
| T6.2 | Cutover in a maintenance window, from the runbook written at that time with complete commands: close all Claude Code sessions; disable the old plugin; stop the Node gateway with its own CLI; `claude-code-model-gateway migrate --start`; install and enable the new plugin; `doctor`; start Claude Code and fill in the AC-15b checklist. Rollback: `model-gateway migrate --rollback`, then the steps it prints | the user | T6.1 |
| T6.3 | Needs separate approval each: change `gateway-switch` to call the binary; uninstall the old plugin; remove the trampolines | the user decides | T6.2 |

---

## 5. Acceptance criteria

Evidence for every criterion is command output stored by the verifier lane under `.omc/artifacts/verify/<phase>/`
(`go test -json` output, the conformance run's TAP and per-test launch log, benchmark samples). A model's statement that
something passed is not evidence. A test counts only when it was executed, passed and was not skipped.

| # | Criterion | Check |
|---|---|---|
| AC-1 | `go build ./...`, `go vet ./...`, `golangci-lint run`, `make fmt-check` (formatters change no file of a clean checkout) and `GOOS=windows GOARCH=amd64 go build ./...` exit 0 | on the developer machine (darwin/arm64; these commands execute no product code) and in the Linux venue at every gate; in the CI workflow on `ubuntu-26.04` and `xcode-27` at every gate, from the pushed `port/phase-N` branch |
| AC-2 | Isolation. (a) `hack/venue-check` applies the predicate in section 3.8; only verified hosted CI is exempt from the attestation. Record isolated refusals for each missing prerequisite, then separately for each protected make target: non-zero with "not a test venue", no build, test or Node command dispatched. On the developer Mac exercise only the standalone shell check. (b) Every package with tests has a `TestMain` whose first statement is `venue.Require()`, under the default and the `process` tag (`hack/lint-venue` rejects missing and non-first guards). Record the positive and negative binary controls for both tag sets in section 3.8. No test binary starts on the developer machine, and there is no local test target. (c) In the venue, with the guard variable set, binding 18764, 18765 or 80, running a real `claude`, and writing a settings file outside the sandbox each fail with a named error; a helper process started with an empty environment finds no gateway home and no listener. (d) Compare the same developer-machine paths by the same digest rules as the baseline in section 3.8; the manifests must match. Phase 0 uses the manual manifest and reports the interval before capture as unverified. Later phases use the committed tripwire before their first task and at their gate; report any interval without a baseline as unverified. This detects, it does not contain | T0.4 and T0.4b tests; `make tripwire` |
| AC-3 | The 15 classification rows of INV-02 §2.2 and the 10 policy examples of INV-04 §3.5 hold as table tests (phase 1). One HTTP test per branch shows which fake (Anthropic, proxy, Grok) received the request (phase 2) | `worker/route`, `policy`; `worker` |
| AC-4 | Passthrough fidelity for 12 named bodies: 4 MiB valid JSON; two top-level `model` members whose last value selects Anthropic; nested duplicate keys; ill-formed UTF-8 in a string; not JSON; empty; an integer above 2^53; `1e400`; an escaped lone surrogate; CRLF-heavy formatting; a leading byte-order mark; JSON sent to a path outside `/v1/messages`. The SHA-256 of what the fake Anthropic upstream receives equals that of what was sent. Sentinel values placed in `authorization`, `proxy-authorization`, `x-api-key`, `cookie` and the control-token header appear in no header or body received by the fake proxy or the fake Grok endpoint; the control-token sentinel reaches no fake at all | `worker/upstream` integration tests |
| AC-5 | Streams. For the worker path with Anthropic and proxy upstreams and for the supervisor path: when the fake upstream closes mid-stream, the client has status 200 and the earlier data, and its HTTP response is incomplete (transport error, not a clean end) within 1 s (`test/relay-stream-abort.test.js:84-87,119-122,172-175`). For Grok the translated stream ends as the source ends it (`lib/grok-backend.js:384-389`). An uninterrupted stream delivers its terminal event. `goleak` reports nothing. 20 concurrent compaction streams each deliver a complete terminal event (`test/compact-stream.test.js:670`) | `worker/sse`, `supervisor/relay`; source tests at their assigned gates |
| AC-6 | Ownership matrix below. Each decoy is checked for survival and unchanged birth time after the command (a count of delivered signals is kept in addition) | Tier 2: T3.7 (start, stop, recover), T4.7 (ensure, setup), Linux and macOS |
| AC-7 | Migration in an isolated home that holds the pinned Node plugin (Tier 2). Refusals that change no file: Node supervisor still listening; old plugin not disabled; a journal in progress for the other end state. After the Node CLI's `stop`: `migrate --start` ends with the Go supervisor answering `/healthz`, no stop marker, exec-form hooks, both launchers as trampolines; `migrate --keep-stopped` ends with the marker present and nothing listening. In both, every settings file differs from its pre-image only in readiness-hook entries. A legacy `node <launcher> readiness-gate` then exits 0 on a healthy gateway. `migrate --rollback` restores every untouched file, the stop marker and the previous stable binary byte for byte, keeps an edit made after migration and reports it, starts nothing, and prints the commands that re-enable the old plugin and start the Node gateway. A process kill injected before and after each journaled effect leaves a home from which rerunning the same command completes. The hook commands of both plugins, invoked as the host invokes them, behave as documented before migration, after it and after rollback | T5.3, T6.0 |
| AC-8 | `hack/parity-check structure` passes globally at every gate: `docs/port/parity-matrix.md` (T0.7) holds every row of INV-05 §4 split into single assertions, its row ids equal `parity-matrix.lock`, and every `conformance` and `go-port` case of `classification.tsv` is named by at least one row. Run `mapping <phase> [package]` (select by `phase`, `package` and `gate`) and `results <phase>` over the nonempty due ledger only. Each due row is `passed` (every named test ran, passed and was not skipped), `difference:<slug>` (the replacement test passed), or `unsupported:<reason>` with a listed reason and reviewer approval. Independently of the matrix, each `go-port` pair in `assertion-sites.tsv` needs passing evidence for its reviewed target or an individually approved `not-ported` reason when its assertion is due (T0.7b), not necessarily when the case is due. Do not assign Go-launch conformance evidence before the adapter decision at T2.0b. Later rows may remain open until their assigned gate. At phase 5 also check every earlier ledger and require no open row | every phase gate |
| AC-9 | Not slower than Node, both runtimes measured in the same job on the same runner with `hack/bench` (T0.6), after one warm-up run: (a) healthy `readiness-gate` p95 wall time over 50 runs; (b) p99 time to first response byte over 200 runs at the client, through supervisor and worker, for a 1 MiB request answered by a 2,000-event SSE stream; (c) summed resident memory of supervisor and worker after 60 s idle. Each Go value ≤ the Node value; raw samples are kept | phase 4 gate |
| AC-10 | Shadow soak, at least 20 sessions or 3 days. `shadow report` exits 0, which requires, from `soak.jsonl` alone: request records for at least one Anthropic model, one Codex model and one compaction; no `gateway-fault`; every upstream submission after a request's first carries a cause from the closed list of section 3.7, and a `relay-resend` only when the request's delivery list names two worker generations with a `worker-exit` record of the first between them; replaying the state table of INV-02 §5.3 over the request records of each session and model key reproduces every recorded sentry state and refusal: a request is refused exactly when it is a non-compaction request on a path the sentry covers and the state before it was armed-high (the last usage observation exceeded the recorded trigger and nothing had fired since), and a compaction request is forwarded in every state (`lib/request-worker.js:1360-1364`); every `worker-exit` and `supervisor-exit` record is preceded, within the same supervisor generation, by a `stop-request` or `restart-request` record | the user, T6.1 |
| AC-11 | Every source test case, identified by file, static site and full expanded name, has exactly one class in `classification.tsv`. `conformance`: it passed at the task or phase exit specified by the `gate` column and `docs/port/test-ownership.md`, no earlier than launch-adapter proof, and the launch log attributes at least one Go launch to that test name. `go-port`: its named Go test passed and was authored before the implementing task by a lane other than the implementer, and every assertion site of the case is mapped or individually exempted by its due phase (AC-8). `difference:<slug>`: the replacement test passed. `js-only` and `unsupported`: the reason is in T0.7's lists and the T0.7 reviewer approved the row. A test leaves `conformance` only through the decision of T2.0b or with the lead's recorded reason that names the missing seam | `make conformance`; `hack/parity-check` |
| AC-12 | CLI contract in `testscript`, one scenario per row of the INV-01 §3.2 outcome table and of INV-01 §1: `ensure --quiet` exits 0 for the wrapper notices, a missing binary, a cut-short wait, a failed start and completion, and 1 for a lock timeout, a thrown error or a fatal error; its stdout is one JSON object when a notice or log line was buffered and empty for the wrapper notices, with the expected text asserted per scenario. `catalog --json` writes exactly one JSON document to stdout. Retired `env --mode` and `--show-mode` exit 2. An unknown command prints the usage and exits 1; no command exits 0; an unknown flag is ignored, also when a positional argument follows it. `doctor` exits 1 under each of its six conditions (INV-01 §3.6) | T4.7 |
| AC-13 | Both security reviews report no open finding of high or critical severity at the reviewed commit; fixes are re-reviewed | grok lane reports |
| AC-14 | Relay, with a fake worker and a fake upstream that count separately. Transient failure before delivery, then a worker appears: one worker delivery, one upstream submission. Permanent failure before delivery: no delivery, a retryable 503 (`test/shim-request.test.js:194-206`). Delivered to a worker that stays current and loses the connection: no second delivery, 503 with `x-should-retry: false` (`:107`). Delivered to a worker that is then replaced: delivered again to the new worker (`:84`). Worker death after its headers: client connection reset, no second delivery (`:210`) | `supervisor/relay` |
| AC-15a | Install path, simulated. In a fresh isolated home with no Node on `PATH` and no stable binary: the plugin's SessionStart hook command exits 0 and prints one JSON object with the install commands. After build and `setup` against a fake release server and fake proxy: the stable binary exists, `env --write-project` writes exec-form hooks, `readiness-gate` exits 0, `doctor` exits 0. A kill at each point of the stable-binary replacement (before the hard link, between link and rename, after the rename) leaves a working binary at the stable path. Installing a higher version replaces the binary and the supervisor. Setting the plugin id to `false` makes the gate exit 0 without starting anything | T5.2 (phase 5 gate) |
| AC-15b | Install path, real host. The user's checklist at cutover records that Claude Code lists the marketplace, loads the plugin and both skills, and runs the SessionStart hook | T6.2 checklist (phase 6) |
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

1. **Per task.** The author lane runs its package-scoped gates in its own worktree and hands over. A review lane that did
   not write the code (`gpt62-sol-fast-code-reviewer` for S and M, `gpt62-astra-fast-code-reviewer` for L) reads the diff
   against the cited inventory sections and source ranges. The author never approves its own work.
2. **Per phase.** A verifier lane (`gpt62-astra-fast-verifier`) runs the phase gate from a clean checkout of the phase
   branch: every tier in the Linux venue and on the `xcode-27` CI runner (OD-8). It stores each command, exit code
   and raw output under `.omc/artifacts/verify/<phase>/` and runs `hack/parity-check`.
3. **Lead check per phase.** For each difference D1-D13 in scope and for the relay, sentry and ownership code of that phase,
   the lead compares the Go code with the pinned source lines directly, not with the inventory.
4. **Before phase 6.** `fd -H -I -t d '^\.omc$'` shows no stray state directory; every spawned lane has acknowledged
   shutdown; the tripwire is clean.

---

## 7. Expanded test plan

| Level | What | Tools | Tier |
|---|---|---|---|
| Unit | policy and classification tables; `jsNumber`; `jsonx` wire and file corpora against Node goldens; hosts block; pins compare; tool-schema reason codes against the Node/Ajv corpus; sentry state table; dispatch cache; Grok translation; lifecycle lock stale rules; frame scanner | table tests (`map[string]struct`), `go-cmp`, `testing/synctest` for every timer, fuzz targets for `toolschema`, the frame scanner, the hosts block and `jsonx` | 1 |
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
| K11 | The tool-schema validator may accept or reject an original ECMAScript Unicode-mode pattern differently from V8 | medium / low | compare verdicts against the pinned Node corpus (T1.5a); add cases for later mismatches. `regexp2` alone is not the validator (§12). | T1.5b |
| K12 | An inventory statement is imprecise although its citation resolves: the verification of the pin move read about 240 citations and found six such statements (corrected, VER-R) | medium / medium | the pinned source is the authority and the inventories are leads: every brief says so; test lanes write expectations from the pinned tests and source (T0.9, T1.0); the reviewer reads the diff against the cited source lines (section 6); a wrong statement is corrected in the inventory with a dated note | review; AC-8, AC-11 |

---

## 9. Execution routing

| Role | Agent type (model) | Used for |
|---|---|---|
| Implementation, simple | `gpt62-sol-fast-executor` (`claude-gpt-6.2-sol-fast[1m]`) | tasks marked **sol** |
| Implementation, complex | `gpt62-astra-fast-executor` (`claude-gpt-6.2-astra-fast[1m]`) | tasks marked **astra**; test-first lanes of the fallback |
| Code review | `gpt62-sol-fast-code-reviewer` (S, M), `gpt62-astra-fast-code-reviewer` (L) | every task; never the author lane |
| Verification | `gpt62-sol-fast-verifier` per task, `gpt62-astra-fast-verifier` per phase gate | section 6 |
| Architecture questions | `gpt62-astra-fast-architect` | read-only |
| Security review | `grok-security-reviewer` (`claude-grok-4.7-fast[1m]`). A two-line probe lane runs first; if its model line is not the grok id, the review runs on `oh-my-claudecode:security-reviewer` (Opus) and the substitution is recorded | T3.8, T5.7; never the lead |
| Steps with live credentials, and all of phase 6 after T6.0 | the user | live tests, soak, cutover |

**Tests first.** For every implementing task, the `go-port` tests that T0.7 assigned to it are written beforehand by a
separate test lane (`gpt62-astra-fast-executor`) from the source tests and the inventories; the implementing lane may add
tests but may not change those expectations without the lead's recorded approval. The handoffs are tasks of their own (T0.9, T1.0, and the first task of each later phase), merged before the tasks they serve. Each CLI command lives in its own package
under `internal/cli/`, and the lead owns the root registration file, so parallel command tasks do not share a package.

**Lane brief rules** (briefs are in English): the task, its inventory sections and pinned source ranges, the package it
owns, its gate commands; read the source only from the pinned export, which is the authority where an inventory and the source disagree (the lane reports the disagreement); run from the repository root with absolute paths and
`go -C` / `git -C`, never `cd`; never call `go test` or Node directly, the make targets run them in a venue; never read `control-token` or auth files; any
time comes from `date`; line 1 of the report is the model the lane ran as, and the lead checks it against the pin; the
gateway must be on (`/gateway on`) or the pin is silently ignored. A lane is shut down in the turn its report is accepted;
a run is complete only when every lane has acknowledged shutdown. Data files under `test/` and `hack/` name phases,
packages and difference slugs, never task, decision or inventory ids.

**Branches and worktrees.** The lead creates one integration branch per phase (`port/phase-0` from `main`; `port/phase-N` from the verified tip of the previous phase branch, checked with `git merge-base --is-ancestor`, or from `main` once the user has merged that phase into it) and one worktree
with its own branch per lane (`git worktree add`). A lane formats and tests only its own package paths. When a lane
reports, the lead rebases its branch onto the phase branch with `git r port/phase-N` **before** review; review and
verification refer to those commit hashes; after approval the lead fast-forwards the phase branch (`git merge --ff-only`)
and removes the worktree. Push the phase branch to `origin` for each completed small task. Merging into `main`, pushing
`main` and opening a pull request still need the user's word.

**Commits.** Commit each small task separately on its phase branch and push it to `origin/port/phase-N`. Signed, message
from a file (`git commit --gpg-sign -F <file>`), subject and body lines at most 72 columns,
the subject says why the change exists; no task, decision or inventory id anywhere; code written by a GPT lane carries
`Co-Authored-By: Codex <noreply@openai.com>` beside the Claude trailer.

**Parallelism.** Phase 0: T0.0; T0.1; T0.4; T0.7; T0.7b; T0.9; then T0.2, T0.3 and T0.8 together; then T0.4b; then T0.5 and T0.6. Phase 1: the handoffs T1.0 first; then T1.2, T1.3, T1.8, T1.12b,
T1.13 and the corpus job T1.5a; then T1.5b and T1.9; T1.1 after T1.2; then T1.4, T1.6, T1.7, T1.11, T1.12a, T1.12c, T1.12d; T1.10 last. Phase 2: T2.0, then T2.0b and its decision; then
T2.1 and T2.2; then T2.3 and T2.4; then T2.5, T2.6, T2.7, T2.8. Phase 3: T3.1 and T3.3; then T3.2, T3.4, T3.5; then T3.6.
Phase 4: T4.0 and T4.3; then T4.1, T4.2, T4.4, T4.5, T4.6. Two lanes never own the same package at the same time.

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
  independently of the implementation elsewhere, and by a soak before cutover. Thirteen differences are declared in advance.
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
| OD-3 | Differences D1-D13 | accept all | each can be dropped separately; dropping D2 keeps Node on the per-prompt hook path; dropping D10 needs another rule for two instances of one binary |
| OD-4 | Source of truth after cutover | Go; the fork's `plugins/model-gateway` is frozen and later upstream changes are ported by hand | keeping both alive needs a sync cadence |
| OD-5 | Release archives and `self update` | no; `go install` plus `setup` | adds T5.5 and `go-selfupdate` |
| OD-6 | Plugin identity | marketplace `claude-code-model-gateway` in this repository, plugin id `model-gateway@claude-code-model-gateway` | another marketplace changes one constant and the skill text |
| OD-7 | First version | `0.57.0` | any value above `0.56.3` |
| OD-8 | Where macOS process tests run | decided: `xcode-27` hosted CI runner through pushed phase branches | — |
| OD-9 | Agent definitions `gpt62-*.md` | decided: not committed; `~/.claude/agents/` makes the definitions available from any checkout | — |
| OD-10 | Inventories, reviews and this plan under `docs/port/` | done: committed there as the authoritative porting specification; copies under `.omc/` are not maintained | — |
| OD-11 | Tests written first by a separate lane (section 9) | yes, for every `go-port` test | implementers port their own tests; AC-11 then loses its independence clause |
| OD-13 | Tool-schema pattern validator | Awaiting the user's choice between a checker written in this repository and a library that reproduces the original pattern's ECMAScript Unicode-mode syntax verdict. The lead recommends the in-repository checker. | Either choice must handle the relevant ECMA-262 property names and match the pinned corpus. |
| OD-14 | Hosted-CI attestation wording in AC-2 | Awaiting the user. Say that CI runs `hack/venue-attest-ci` first and applies the same predicate, with no exemption. | The locked acceptance criterion is unchanged pending approval. |
| OD-15 | Unknown-flag wording in AC-12 | Awaiting the user. Limit the unknown-flag assertion to presence checks; keep raw option/value validation and `remote-control`'s first-token behavior (§3.10). | The locked acceptance criterion is unchanged pending approval. |
| OD-16 | Clock-independent process start token in the pid record | Awaiting the user. Add a clock-independent start token before phase 3 so the lifecycle signaling gate can distinguish a reused PID without relying on wall-clock time. | A different stable process discriminator would need to be agreed before the signaling gate. |
| OD-17 | Early phase-1 test-first handoffs | Awaiting the user's confirmation of the recorded deviation. Test-only lanes independent of `procinfo` started before the phase-0 prerequisites had all merged; no phase-1 work merges or is pushed before the phase-0 gate passes. | The deviation remains on record. |
| OD-18 | Additional deliberate differences | Awaiting the user's confirmation of the added §3.10 rows and launcher-guidance coverage, separately from D1–D13. | Raise objections before phase 2; resolve them before treating the disputed behavior as accepted. |

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

The port follows the code. These are corrected in the rewritten skills and README (T5.2, T5.6).

| # | Documentation says | Code does | Evidence |
|---|---|---|---|
| A1 | `hooks/hooks.json:2`: SessionStart "waits up to 12 seconds for the local Codex gateway" | the hook runs only `registry-writer.js`, which writes launchers and the breadcrumb | `hooks/hooks.json:9`; `hooks/registry-writer.js:192-202` |
| A2 | `README.md:110`, `skills/model-gateway/SKILL.md:60,127`: SessionStart runs `ensure --quiet` | removed by commit `101a64fd2`; readiness comes from settings-installed `readiness-gate` hooks | INV-01 §3.2, §4.3 |
| A3 | `README.md:74`: "Anthropic requests remain byte-identical" | true for the body; `traceparent`, `tracestate`, `baggage` and hop-by-hop headers are removed and `content-length` recomputed | `lib/request-worker.js:1827-1839` |
| A4 | `lib/request-worker.js:2703-2704`: the hosts entry is "never written by this plugin" | `remote-control enable --confirm` writes it | `lib/remote-control.js:219` |
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
user's separate authorization); matching the executing image of an unlinked executable on hosted macOS (T0.5);
whether the chosen tool-schema validator reproduces the original pattern's ECMAScript Unicode-mode verdict (T1.5b,
§12).

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
| Worker descriptors fixed at 3 and 4; the source's RC rule and preconfigured-port rule kept; missing port report still ends the supervisor | architect 11 |
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
| Phase 0 is in progress; the user adopted the sixth check without another round and authorized execution through T6.0, with a phase table at each gate and a stop before the user's soak, cutover and plugin removal | user's decision |
| Push each completed small task to its phase branch; a pull request, main-branch merge or push to `main` still needs permission. CI runs on pushed branches from the first gate; macOS uses `xcode-27`, Linux the approved current account with the no-write stop rule | user's decisions |
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
