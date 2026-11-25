model: claude-opus-5-5

# 05 — Test suite, skills and changelog inventory (model-gateway, Node.js)

Source: `/Users/zchee/src/github.com/Eigenwise/eigenwise-toolshed/plugins/model-gateway`, commit `a8501f251755` (written against `4ffb2f672`; moved to this commit on 2026-11-25, see `07-repin.md`), branch `dev`
(both from `git log -1` / `git branch --show-current`, 2026-11-25 21:30:54 JST by `date`). Plugin version 0.56.3
(`.claude-plugin/plugin.json:3`). All `path:line` citations are relative to that plugin root. Nothing was executed.

Method notes, so the numbers can be judged:

- Test counts are static: `grep -caE "^\s*test\("` per file. A `test()` inside a `for` loop counts once, so the run-time
  count is higher (loops at e.g. `test/gateway-process-isolation.test.js:726,943`, `test/relay-stream-abort.test.js:66`,
  `test/shim-request.test.js:144-269`, `test/context-sentry-lifecycle.test.js:216,232`). Static total over the 57
  `*.test.js` files: **693**.
- `test/tool-schema-compat.test.js` contains non-text bytes (`file` reports `data`), so plain `grep` returned nothing;
  `grep -a` gives 13 tests.
- Per-file classification used titles plus a scan of each file for: in-process `require('../lib/…')`, `'-e'` child
  scripts that `require()` lib modules, `startGateway(…,'serve-shim'|'serve-worker')`, `[CLI, '<command>', …]`
  invocations, fake servers (`http.createServer`/`net.createServer`), and platform skips. Bodies were read only where noted.
- lib/ files were opened only to explain a harness contract: `lib/commands.js:153-215,2870-2905,3020-3040`,
  `lib/process-supervision.js:1017-1028`, `lib/request-worker.js:1210,2693`, `lib/pins.js:212-213`,
  `lib/settings-wiring.js:89`, `bin/model-gateway.js:1-19`.

---

## 1. Harness

### 1.1 Runner configuration

| Item | Value | Cite |
|---|---|---|
| Command | `node --require ./test/isolate-home.js --test --test-concurrency=2 --test-timeout=300000 "test/*.test.js"` | `package.json:6` |
| Coverage | same, with `NODE_V8_COVERAGE` (default `coverage`) | `package.json:7` |
| Node engine | `>=22.5.0` | `package.json:10` |
| Only dev dependency | `ajv` 8.20.0 (Draft 2020-12 oracle for tool-schema tests) | `package.json:13`, `test/tool-schema-compat.test.js:89-90` |
| File concurrency | 2 test files at once; tests inside a file run sequentially (node:test default) | `package.json:6` |
| Per-test timeout | 300 s default; overrides 15 s–150 s (table 1.6) | `package.json:6` |
| Suite budget in manifest | `"suiteTimeout": 300000` (consumer UNREAD) | `.claude-plugin/plugin.json:12` |

### 1.2 `test/isolate-home.js` (preloaded into the runner and every test child)

| Effect | Cite |
|---|---|
| `mkdtemp(os.tmpdir()/model-gateway-test-home-*)` and sets `HOME`, `USERPROFILE` to it | `test/isolate-home.js:12-14` |
| `CLAUDE_CONFIG_DIR=<home>/.claude` | `:15` |
| `CODEX_GATEWAY_CLAUDE_BIN=<home>/claude-is-not-installed-in-tests` (a real `claude` launched by a pin probe once rewrote the real settings file) | `:16-18` |
| Removes the home on process `exit` | `:19-21` |
| Reason: on 28 Sep 2026 a test run overwrote a developer's real `~/.claude/settings.json` | `:4-7` |

Product code also detects the runner: `lib/pins.js:212-213` refuses to launch a real `claude` when `NODE_TEST_CONTEXT`
is set and `CODEX_GATEWAY_CLAUDE_BIN` is not; `lib/settings-wiring.js:89` guards settings writes only under
`NODE_TEST_CONTEXT`. `test/settings-write-guard.test.js` pins both. A Go port needs its own equivalent switch.

### 1.3 `test/support.js` exports (`test/support.js:858-860`)

| Export | What it does | Cite |
|---|---|---|
| `gatewayTestEnvironment(t, overrides, isolatedOverrides)` | Builds an isolated env (1.4), creates and registers an owned temp home, and in `t.after` stops every tracked process for that home, then removes the home with ownership/identity checks; cleanup failure is reported via `t.diagnostic` and a deferred `t.after` throw | `:750-764`, `:483-488` |
| `isolateInProcessGatewayEnvironment(t)` | Rewrites *this* process's `process.env` to an isolated env before `require(CLI)` (runtime.js reads state when loaded) | `:744-752` |
| `spawnGatewayProcess(t, cmd, args, opts)` | `spawn` with an isolated env (reuses an existing test env by object or by `HOME`), registers the child for teardown | `:766-777` |
| `spawnGatewayProcessSync(cmd, args, opts)` | `spawnSync` in a fresh isolated env, then SIGTERM/SIGKILLs PIDs from the home's pid files and removes the home | `:779-788` |
| `startGateway(t, command, env, {cliPath, isolatedOverrides})` | `spawn(process.execPath, [cliPath ?? bin/model-gateway.js, command], stdio ['ignore','pipe','pipe','ipc'])`; waits ≤5 s for stdout `listening on 127.0.0.1:(\d+)`, then polls `GET /healthz` until 200 (≤5 s); resolves `{child, port}` | `:790-831`, `:490-525` |
| `stopGatewayChild(child)` | One 5 s budget: `serve-shim` gets IPC `{type:'fixture-shutdown'}`; `serve-worker` gets an authenticated `POST /drain`; other children get `disconnect()`/SIGTERM (taskkill on Windows); on timeout SIGKILL (`taskkill /T /F` on Windows) and record escalation | `:158-176`, `:105-156` |
| `startCountingProxy(t)` | Raw TCP server that records the target of each HTTP-proxy request line (CONNECT or absolute URI) and answers 502; used to prove no egress via `HTTP(S)_PROXY` | `:833-856` |
| `waitForFixtureProxy(child, {home, port, startedAt, timeout=15000, …})` | Polls `proxy.pid` + `GET /v1/models` (each probe ≤500 ms) until the recorded proxy answers 200; fails fast on supervisor exit; on failure dumps the tail (2 KiB) of 12 state files | `:637-657`, `:527-530`, `:583-588` |
| `removeOwnedGatewayTestHome(home, {shutdown,…})` | Validates registration + root identity (realpath, dev, inode, birthtime), refuses if any tracked PID is still alive or a control failed, retries transient `EBUSY/EPERM/ENOTEMPTY/EMFILE/ENFILE` within 6 s | `:466-481`, `:401-441`, `:16` |
| `observeGatewayChildClose`, `waitForGatewayChildClose` | Track the child `close` event (not just `exit`) | `:51-99` |
| `requestFixtureWorkerDrain(child, deadline)` | Worker drain: `POST http://127.0.0.1:<worker port>/drain`, header `x-model-gateway-control-token: <64 hex>`, body `{}`, expects 202 `{"ok":true,"draining":true}` (≤4 KiB) | `:321-399`, `:299-311` |
| `processIsRunning(pid)` | `kill(pid,0)`; `ESRCH` ⇒ gone; Windows `EPERM` ⇒ treated as foreign/gone | `:30-39` |
| `requireFixtureShutdown(home, shutdown)` | Throws `ERR_GATEWAY_FIXTURE_CONTROL_FAILED` / `…_CLOSE_TIMEOUT` / `…_PROCESS_ALIVE` | `:401-407` |

### 1.4 Isolated environment (`createGatewayTestEnvironment`, `test/support.js:688-734`)

First deletes every inherited `CODEX_GATEWAY_*` key and every key in `ISOLATED_GATEWAY_ENVIRONMENT_KEYS`
(`:659-676`, `:695-697`), then forces:

| Key | Value | Purpose |
|---|---|---|
| `HOME`, `USERPROFILE` | owned temp dir | state root `~/.claude/model-gateway` moves with it |
| `CLAUDE_CONFIG_DIR` | `<home>/.claude` | Claude settings, plugin registry, discovery cache |
| `CODEX_HOME` | `<home>/.codex` | Codex auth state |
| `MODEL_GATEWAY_REQUEST_BODY_DIR` | `<home>/.claude/model-gateway/request-body` | usage high-water files |
| `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` (+ lowercase) | `''` | no outbound proxy |
| `NO_PROXY`, `no_proxy` | `*` | |
| `CODEX_GATEWAY_PORT`, `CODEX_GATEWAY_WORKER_PORT`, `CODEX_GATEWAY_PROXY_PORT`, `CODEX_GATEWAY_COMPAT_PORT` | `0` | ephemeral ports; never 18764/18765/80 |
| `CODEX_GATEWAY_SOCKET_PATH` | `<home>/gateway.sock`, or `\\.\pipe\model-gateway-test-<basename>` on Windows | `:683-686` |
| `CODEX_GATEWAY_GROK_HOME` | `<home>/.grok` | Grok CLI auth/catalog |
| `CODEX_GATEWAY_GROK_ENDPOINT` | `http://127.0.0.1:9/v1/responses` | discard port |
| `CODEX_GATEWAY_ANTHROPIC_UPSTREAM` | `http://127.0.0.1:9` | api.anthropic.com replacement |
| `CODEX_GATEWAY_CLAUDE_BIN` | `<home>/missing-claude` | never the real CLI |
| `CODEX_GATEWAY_HOSTS_FILE` | `<home>/hosts` | never `/etc/hosts` |
| `CODEX_GATEWAY_DISPATCH_CACHE_PATH` | `<home>/dispatch-routes.json` | |
| `CODEX_GATEWAY_REQUEST_LOG_PATH` | `<home>/request-routes.jsonl` | |
| `CODEX_GATEWAY_TELEMETRY_ENDPOINT` | `http://127.0.0.1:9` | OTLP sink discarded |

Overrides are applied twice (before and after the forced block), so a test can override any forced key; values equal
to the parent's inherited value are dropped so a leaked parent override cannot win (`:678-681`, `:698-732`).
`isolatedOverrides` are applied last unconditionally (`:732`). Keys deleted but not forced: `ANTHROPIC_BASE_URL`,
`ANTHROPIC_UNIX_SOCKET`, `CLAUDE_CODE_PLUGIN_CACHE_DIR` (`:659-676`).

Other env keys tests set to steer the product (files using each, from a `grep -l` count): `CODEX_GATEWAY_PROXY_PORT` 29,
`CODEX_GATEWAY_REQUEST_LOG` 24, `CODEX_GATEWAY_PORT` 21, `CODEX_GATEWAY_ANTHROPIC_UPSTREAM` 17, `ANTHROPIC_BASE_URL` 15,
`CODEX_GATEWAY_WORKER_PORT` 14, `CODEX_GATEWAY_USAGE_ENDPOINT` 8, `CODEX_GATEWAY_SENTRY` 8,
`CODEX_GATEWAY_TELEMETRY_ENDPOINT` 7, `CODEX_GATEWAY_CLAUDE_BIN` 6, `CODEX_GATEWAY_COMPACT_TRIGGER` 5,
`CODEX_GATEWAY_GROK_HOME`/`_ENDPOINT` 5, `CODEX_GATEWAY_MODELS_REFRESH_MS` 4, `CODEX_GATEWAY_HOSTS_FILE` 4,
`CODEX_GATEWAY_COMPAT_PORT` 4, `CODEX_GATEWAY_CONTEXT_WINDOW` 4, `CLAUDE_CODE_PLUGIN_CACHE_DIR` 4,
`MODEL_GATEWAY_CLAUDE_HOME` 3, `CODEX_GATEWAY_SOCKET_PATH` 3, `ANTHROPIC_UNIX_SOCKET` 3, and once or twice:
`CODEX_GATEWAY_{SSE_HEARTBEAT_S,SSE_HEARTBEAT_MS,PROBE_TIMEOUT_MS,COMPACT_STREAM_RETRIES,COMPACT_STREAM_RETRY_DELAY_MS,
COMPACT_STREAM_GUARD,WS_UPGRADE_RETRIES,WS_UPGRADE_RETRY_DELAY_MS,PROXY_RECOVERY_INTERVAL_MS,PIN_PROBE_TIMEOUT_MS,
LMSTUDIO_FIRST_EVENT_WAIT_MS,LIST_DISPATCH_MODEL,REQUEST_COMPRESSION}`, `CCP_{LLAMA_PORT,LMSTUDIO_ENABLED,
LMSTUDIO_BASE_URL,LMSTUDIO_API_KEY,VERTEX_PROJECT,OPENCODE_API_KEY,CONFIG_DIR,CODEX_REQUEST_COMPRESSION}`.
This list is the product's configuration surface that the suite depends on.

### 1.5 Fakes

| Fake | How it is built | Cite |
|---|---|---|
| claude-code-proxy (HTTP) | In-test `http.createServer` on `listen(0,'127.0.0.1')` answering `GET /v1/models` with `{data:[{id:'gpt-…'}]}` and scripted `/v1/messages` (JSON or SSE); wired in via `CODEX_GATEWAY_PROXY_PORT` | e.g. `test/context-sentry-lifecycle.test.js:33-57`, `test/relay-stream-abort.test.js:19-31`, `test/context-window.test.js:144-167` |
| claude-code-proxy (process) | `process.execPath` copied to `<home>/.claude/model-gateway/bin/claude-code-proxy[.exe]`; the product spawns it as `claude-code-proxy serve --no-monitor`, so the copied Node runs a `serve` script the test wrote into the cwd | `test/gateway-process-isolation.test.js:198-203,1527-1528`, `lib/process-supervision.js:1017-1021` |
| Anthropic upstream | HTTP server set as `CODEX_GATEWAY_ANTHROPIC_UPSTREAM` | `test/anthropic-catalog.test.js`, `test/thread-negotiation.test.js` (titles) |
| Grok backend | HTTP server as `CODEX_GATEWAY_GROK_ENDPOINT=…/v1/responses` plus `<grok home>/auth.json` with a fixture OIDC token | `test/context-sentry-lifecycle.test.js:47-56` |
| OTLP collector | HTTP server receiving `/v1/traces` (telemetry) and `/v1/logs` (usage) | `test/gateway-telemetry.test.js`, `test/gateway-usage.test.js`, `test/compact-stream.test.js` (path greps) |
| LM Studio / llama-server | HTTP servers for `/api/v1/models` and `/props` | `test/lmstudio.test.js`, `test/local-llama.test.js` (titles) |
| `claude` CLI | `<home>/bin/fake-claude.js` + `claude` sh wrapper (`claude.cmd` on Windows) that prints a version or a stream-json `system/init` line; pointed at by `CODEX_GATEWAY_CLAUDE_BIN` | `test/wiring-mode.test.js:54-68` |
| Codex auth status | `NODE_OPTIONS=--require=test/codex-authenticated-preload.cjs` patches `child_process.execFile` so `… codex auth status` returns `account: isolated-test-fixture` | `test/codex-authenticated-preload.cjs:12-17`, `test/discovery-cache.test.js:358` |
| Installed plugin | `<home>/.claude/plugins/installed_plugins.json` with `installPath` = the checkout, or a full `cpSync` of the checkout into `plugins/cache/eigenwise-toolshed/model-gateway/<ver>` with a rewritten `plugin.json` version | `test/readiness-gate.test.js:14-22`, `test/gateway-process-isolation.test.js:217-230` |
| PID records | Hand-written `guardian.pid` and `guardian.pid.json {pid, command:'node "<install>/bin/model-gateway.js" serve-shim'}` | `test/readiness-fast-path.test.js:25-26` |
| Hosts file | temp file via `CODEX_GATEWAY_HOSTS_FILE` (30 references in rc-compat-mode) | `test/rc-compat-mode.test.js` |
| Egress observer | `startCountingProxy` set as `HTTP(S)_PROXY` | `test/support.js:842-856`, `test/context-window.test.js:1493-1538` (titles) |

### 1.6 Ports, timeouts, platform gates

- Ports: every listener binds `0` on `127.0.0.1`; gateway ports via env `0`; the shim reports its own port on stdout.
  Some tests reserve a port by bind-and-close (`test/gateway-process-isolation.test.js:1506-1513`); changelog 0.54.2
  records a flake from that pattern (`CHANGELOG.md:129-132`). One test asserts the upstream port is not
  18764/18765/18766/29113 (`test/gateway-process-isolation.test.js:1342`).
- Explicit per-test timeouts: 15 s `test/lifecycle-lock.test.js:114`; 20 s `test/anthropic-catalog.test.js:183`,
  `test/shim-upload-abort.test.js:181`, `test/windows-detached.test.js:95`; 30 s `test/windows-detached.test.js:18`;
  40 s `test/readiness-gate.test.js:169`; 45 s `test/recovery-lifecycle.test.js:85`; 150 s `:109`;
  10 s `test/gateway-process-isolation.test.js:1898`.
- Platform skips: Windows-only `test/windows-detached.test.js:18,95`, `test/usage-observability.test.js:368`; POSIX-only
  `test/gateway-process-isolation.test.js:1326` (real reparenting), `test/gateway-readiness.test.js:393` (socket file);
  conditional `test/readiness-gate.test.js:338`. Runtime skip when no real `claude` on PATH:
  `test/picker-guard.test.js:115-121`.

### 1.7 Cleanup guarantees

1. Every child spawned against an owned home is tracked in a Set per home, not only the last (`test/support.js:178-191`; SQ-3075).
2. Spread copies of an environment are matched by `HOME` (`:20-24`, `:768`; SQ-3142).
3. Teardown order: graceful control → wait for `close` → SIGKILL/taskkill → then pid-file PIDs (`shim`, `guardian`, `proxy`) get SIGTERM then SIGKILL (`:193-208`).
4. Home removal refuses a symlinked or replaced root and any live tracked PID (`:252-281`, `:401-407`).
5. A cleanup error never masks the primary assertion; it is emitted as a diagnostic and fails the test afterwards (`:483-488`).

---

## 2. Per-file table

Surface codes: **R** = requires lib/bin in-process; **E** = child `node -e`/`--require` script that loads lib internals;
**C** = spawns the CLI with a command; **H** = HTTP to a running `serve-shim`/`serve-worker`; **P** = real process trees.
Port class per the lead's set. "mixed" lists the split.

| File | Lines | Tests | Behavior pinned | Surface | Needs | Class |
|---|---:|---:|---|---|---|---|
| anthropic-catalog | 256 | 9 | Anthropic catalog merged first, cached without credentials; stale cache serves on upstream failure; `ensure` advances catalog defaults forward-only; invalid persisted pins ignored | C,H (`pin`, `ensure --quiet`, `serve-worker`) | fake Anthropic upstream | blackbox |
| atomic-file | 77 | 3 | Atomic write retries a locked target, leaves no temp sibling, sweeps stale temps | R | fs | unit |
| catalog-refresh | 192 | 7 | `catalog --refresh` exits non-zero with reason when shim unhealthy / lists no gateway ids / errors (#227); stale refresh advisory exit 0 | C (`catalog`), R (`gw.buildCatalog` for expected value) | fake shim (http+net) | blackbox (expected value computed in-process) |
| codex-upstream-cases | 280 | 7 | 429 block expiry (Retry-After, reset header, 60 s), 401 until setup (#190); 30 s transient hold (#175); empty Codex turn → `end_turn` (#192); lost `message_stop` closed; `/v1/models` timer refresh and burst (#238); 5 s auth stall does not block `/healthz` (#367) | E (requires state/runtime), H (`serve-worker`) | fake proxy, slow fake auth | mixed: 4 blackbox / 3 integration |
| compact-stream | 801 | 22 | Compaction stream guard: lost terminal inferred only for end-of-generation stops; retries truncated compaction streams; byte-identical synthesized terminal; passthrough of healthy/non-compaction/Claude-native streams; in-band error on retry; diagnostics metadata-only; `/healthz` reports guard settings | H (`serve-shim`) | fake proxy, fake OTLP | blackbox |
| context-sentry-lifecycle | 244 | 9 (+loops) | Sentry latch/rearm per session×agent×model; compaction bypass; rearm only on completed ordinary stops (`end_turn`,`stop_sequence`,`tool_use`) | H (`serve-shim`) | fake proxy, fake Grok + auth.json | blackbox |
| context-window-setting | 297 | 13 | `context-window` saved value > env > default; validation; report text (272000 / 187000 / 2× billing); Claude cap writes only gateway-owned `autoCompactWindow`; compact-at semantics | R (runtime, context-window, commands, request-worker), C (`context-window`), H, E | none beyond isolation | mixed: ~4 blackbox / ~9 unit |
| context-window | 2021 | 47 | Window policy, aliases `[1m]`, advertised `max_input_tokens`, sentry 413 shape and latching, genuine-413 rewriting, WS-upgrade retries, auth rewrite, plan-mode tool stripping, request-route log metadata-only, env wiring/pins/doctor text, Claude passthrough byte-identical, SSE heartbeat, proxy semver floor | H 21, C 9, R/E ~17 (awk pass) | fake proxy, fake claude, counting proxy | mixed: ~30 blackbox / ~17 unit+integration |
| control-auth | 69 | 3 | Lifecycle requests need valid token, local Host, JSON, no Origin; token stable, fail-closed; worker replacement never downgrades or leaves cache root | R | fs | unit |
| discovery-cache | 489 | 14 | Claude Code discovery cache schema from shim list; unchanged → no rewrite; foreign cache untouched; untouched in RC mode; `CLAUDE_CONFIG_DIR` honored; fallback catalog never persisted; legacy cache normalization | R (runtime, settings-wiring), C (`ensure`), H, E (`NODE_OPTIONS` preload) | fake proxy, preload | mixed: ~5 blackbox / ~9 unit+integration |
| dispatch-model | 749 | 15 | Dispatch (`codex-auto`) routable when unlisted; marker parsing only in user text, not tool_result; usage marker ticket per agent; catalog v4 contract; subset writes preserve models/readiness; future schema refused; `catalog --refresh --json` stdout stays JSON (SQ-2208) | C (`serve-shim`, `catalog`, `doctor`), H, R | fake proxy | mixed: ~7 blackbox / ~8 unit |
| gateway-drain | 967 | 29 | Drain finishes in-flight request; supervisor keeps listener while worker hard-killed/restarted; orderly-signal record; restart refuses older/foreign/unknown owners and dev-checkout switches; adopts newer sibling; control auth refuses browser Origin; token not leaked upstream; singleton listener; version-based supervisor replacement; proxy recovery single-flight, confirmation probe, foreign listener kept | R (process-supervision, control-auth), E, H, C (`doctor`, `serve-shim`), P | fake proxy, cached CLI copies | mixed: ~6 blackbox / ~23 integration |
| gateway-process-isolation | 1950 | 60 | 33 tests are harness self-tests (owned removal, drain, close observation); the other 27 pin ownership resolution (same-install / unowned / foreign / unknown, start-time identity), probe timeout budget (2 s, 8 s Windows), foreign supervisor/proxy/junction preservation, stale guardian PID not killed, sibling-version records, stale orphan reaping with live stream | R, E (`--require` preloads), C (`status`,`ensure --quiet`,`stop`,`doctor`,`env`,`serve-shim`), H, P | Node-as-proxy, cached CLI copies, real ps | mixed: 33 js-only / ~6 blackbox / ~21 integration |
| gateway-ps-locale | 188 | 3 | POSIX probes pin `LC_ALL=C`; Windows probes inherit env; `netstat` columns parsed numerically in German/French (#296) | E (module loaded with env) | none | unit |
| gateway-ps-width | 272 | 1 (loop) | `ps` keeps full command lines and ownership at narrow `COLUMNS` | R, P (real `ps`) | POSIX `ps` | integration |
| gateway-readiness | 412 | 11 | Local failure states; setup clears upstream records; `upstream-blocked` survives health checks, clears on success; catalog readiness metadata; Grok auth readiness; `/healthz` single-flight checks with pending/confirmed/stale (#367); socket takeover rules | R, E, H (`serve-shim`) | fake proxy, unix socket (POSIX) | mixed: ~4 blackbox / ~7 integration |
| gateway-telemetry | 454 | 7 | Metadata-only `codex_gateway.route` span linked to traceparent; trace/auth stripped before Codex; failed/retry spans; dispatch/cached-route attrs; telemetry failure cannot change response; Anthropic keeps its credential | H (`serve-shim`) | fake proxy, fake OTLP `/v1/traces` | blackbox |
| gateway-usage | 464 | 6 | Counts-only usage records (JSON, SSE, multi-block = one record), header-only limit evidence, slow collector never delays inference; spool reader ignores truncated tail | C (`serve-shim`), H, R (1 test) | fake proxy, fake OTLP `/v1/logs` | mixed: 5 blackbox / 1 unit |
| github-release | 114 | 10 | Token order `GH_TOKEN`→`GITHUB_TOKEN`→`gh auth token --hostname github.com`; bearer only to API; rate-limit advice on anonymous 403/429; Authorization dropped on downgrade/cross-host redirect | R | local http server | unit |
| grok-backend | 374 | 13 | Anthropic↔Responses translation (messages, tools, effort, web search, streaming tool ids, block order); auth refresh not exposed; CLI version header; catalog from Grok CLI cache; shim lists/routes Grok `[1m]` aliases | R, H (`serve-worker`, 1 test) | fake Grok | mixed: 12 unit / 1 blackbox |
| hook-ensure-delegation | 261 | 9 | `hooks/ensure.js` delegates to the newest recorded install across both registries; falls back to itself; forwards exit status; names searched registries | spawns `hooks/ensure.js` with fixture CLIs | fixture `bin/model-gateway.js` | integration (hook entry point) |
| hook-startup-budget | 47 | 2 | Quiet ensure stops at 12 s; router-only readiness; asserts text in `hooks/hooks.json` and `.claude/skills/verify/SKILL.md`; `login` message wording | R (`bin/model-gateway.js` in-process) | none | unit (+ doc-text assertions) |
| keep-alive-listener-leak | 61 | 1 | 15 keep-alive requests on one socket do not emit `MaxListenersExceededWarning` | H (`serve-worker`) | none | js-only (Node EventEmitter warning) |
| lifecycle-lock | 123 | 8 | Lock refuses live owner, recovers stale PID generation, leaves malformed owners, async-scoped reentrancy, native identity, mutual exclusion across processes; intentional stop persists until resumed | R, E | none | unit (+1 multi-process integration) |
| lmstudio | 764 | 21 | Settings resolved like the proxy; probe reads loaded context; stopped server keeps windows; row window = loaded context; readiness never a proxy sign-in; token sent only to LM Studio; cached tokens subtracted once; overflow → real 413 (JSON and 200-stream first event, 10 s wait); heartbeat during wait | R, H (`serve-shim`) | fake LM Studio, fake proxy | mixed: ~13 blackbox / ~8 unit |
| local-llama | 380 | 12 | `CCP_LLAMA_PORT` parsing; `/props` n_ctx probe; window kept when unreachable; sentry = window − min(⅓, 40000), none below 32768; overflow → 413 with counts; restart moves trigger | R, H (`serve-shim`) | fake llama-server, fake proxy | mixed: ~6 blackbox / ~6 unit |
| native-budget-probe | 421 | 22 | Self-tests of `test/native-budget-probe.js` (a probe that runs a real `claude --print` against a fixture upstream) using `fixtures/native-budget-client.js`; no gateway process involved | spawns the fixture client | none | js-only (developer probe; no product counterpart) |
| owned-worker-cleanup | 349 | 27 | Stale worker reaped only with exact birth record, lost birth parent, ancestry outside supervisor; PID reuse retires record without signal; single-target termination, no escalation; Windows/POSIX parent-chain rules; table parsers fail closed | R (injected process tables) | none | unit |
| picker-guard | 146 | 7 | Stale Fable server row fails the picker check; previous rows relabelled; Fable option follows pin; one check per Claude Code version; optional real-`claude` test | R; real `claude` (opt-in) | real `claude` optional | unit (+1 opt-in external) |
| plugin-registry | 157 | 8 | `CLAUDE_CODE_PLUGIN_CACHE_DIR` searched before config-dir layout; both registries read; malformed skipped; cache identity | R | fs | unit |
| proxy-models | 153 | 8 | Proxy listing attribution; malformed rows refused; `models.json` keeps only Codex ids; provider labels; 200k unmeasured policy; transport preserved; `proxyRouteFromId` | R | none | unit |
| proxy-providers | 707 | 17 | Every proxy provider row advertised without Claude aliases; Vertex Gemini bare id only; Fable rows not spelled `claude-fable-`; backend id + credential/plan-tool stripping; unknown ids → Anthropic; non-Codex rejection leaves Codex readiness; overflow normalization; provider auth states; Vertex readiness without status process; Grok Fast/Muse/Vertex compaction at window − 40000 | R, H (`serve-shim`) | fake proxy, fake Anthropic | mixed: ~9 blackbox / ~8 unit |
| proxy-upgrade | 261 | 9 | Proxy binary replaced by rename, rollback on failure, undeletable old binaries swept later; version change without supervisor stops proxy; stable launcher follows registry, forwards exit codes, finds other marketplaces (GH-380), honors `CLAUDE_CODE_PLUGIN_CACHE_DIR` | R (commands, hooks/registry-writer), C (via launcher: `status --json`, `doctor`) | registry fixtures | mixed: ~4 blackbox (launcher) / ~5 unit |
| rc-compat-mode | 1006 | 28 | Hosts entry/block parsing (Windows/macOS/Linux syntax), managed block add/remove, enable refusals (port conflict, HTTPS process URL, unbindable) before any write, rollback of own bytes only, wiring attestation; DNS bypass resolver errors closed; `serve-shim` compat listener only with hosts entry; RC control only on main loopback; `ANTHROPIC_UNIX_SOCKET`; bodyless request | R (remote-control, request-worker, settings-wiring, in-process CLI), C (`remote-control …`), H (`serve-shim`) | temp hosts file, compat port | mixed: ~6 blackbox / ~22 unit+integration |
| readiness-fast-path | 102 | 3 (+loop) | Healthy installed gate does not load `commands.js` (observed via `Module._load` preload); falls through to recovery on intentional stop / foreign PID record | spawns launcher with `NODE_OPTIONS` preload | fake health server, pid records | js-only observable (behavior itself → integration) |
| readiness-gate | 354 | 21 | Owned hooks merge/dedup/unwire exactly; no `onFailure`, older `block` rewritten; truncated stdin exits 0; non-gateway endpoint no-op before CLI resolution; deleted cwd; exit 2 guidance; plugin `false` disables; upgrade/downgrade registry; 25 s child deadline; intentional-stop handoff; startup failure scoped to generation | R, E (monkeypatched commands), spawns launcher `readiness-gate` | registry fixture, launcher | mixed: ~10 blackbox (launcher+stdin) / ~11 integration |
| recovery-lifecycle | 198 | 2 | Installed gates leave a foreign listener untouched; recover stale boot state once, keep a live stream, recover supervisor death, honor persistent stop | launcher + preload monkeypatch, H, P | fake proxy, fake release download | integration |
| registry | 61 | 3 | Atomic registry breadcrumb; future schema preserved; SessionStart never fails on registry init error | R (hooks/registry-writer) | fs | unit |
| relay-stream-abort | 177 | 3 (×2 commands = 6) | Upstream death mid-SSE (Codex, Anthropic passthrough, Grok) terminates the client response for worker and supervisor (GH-820, SQ-2846) | H (`serve-worker`, `serve-shim`) | dying fake upstreams | blackbox |
| release-verification | 65 | 5 (+loops) | Checksum asset required; invalid/HTTP errors fail closed; digest mismatch rejected | R | none | unit |
| request-worker | 212 | 7 | Overflow phase wording (`first_turn`, `continuation`, `compaction`, `UNVERIFIED`); per-agent sentry; legacy proxy context error with sentry off | H (`serve-shim`) 6, R 1 | fake proxy, OTLP | mixed: 6 blackbox / 1 unit |
| session-route-cache | 615 | 7 | Dispatch route cache TTL/LRU; route survives compaction and shim restart; native Agent route across compaction identity; parent-only child route inheritance; cache-miss logging | R (DispatchSessionRouteCache) 2, C (`serve-shim` spawned directly) 5 | fake proxy, dispatch cache file | mixed: 5 blackbox / 2 unit |
| settings-write-guard | 114 | 10 | Suite home is temp; settings writes outside temp refused under runner; real `claude` never probed (incl. PATH); symlink escape refused | R, E | `NODE_TEST_CONTEXT` | unit (guard is runner-specific) |
| shim-lifecycle | 482 | 24 | Slow proxy waited out (#251); ungraceful proxy replaced after grace; four shim states; failed ownership probe = unidentified; ensure waits for starting shim (#230); stop judged by PID gone (GH-371); start timeout semantics (GH-360); foreign listener refused; same state line in status/doctor/ensure (#275); RC port owner parsing (#296); compression env forwarding (GH-782) | R (injected deps), E | fake servers | mixed: ~2 blackbox / ~22 unit+integration |
| shim-request | 298 | 10 (+loops) | Relay resend only after worker instance gone; 503 otherwise; aborted uploads discarded; 64 MiB cap; health metadata | R (`createShimRelay`) | none | unit |
| shim-upload-abort | 226 | 3 (+loops) | Aborting an upload to `/v1/messages` or the RC endpoint keeps supervisor and a sibling SSE alive | H (`serve-shim`), control token | fake proxy | blackbox |
| stable-command-path | 83 | 5 | Doctor/readiness advice falls back to CLI path when the launcher is missing, else the launcher | R, E (doctor) | none | unit |
| start-gateway-timeout | 247 | 12 | Self-tests of `waitForFixtureProxy` and `startGateway` error reporting (`test-shim` fake CLI) | R (support.js) | fake CLI | js-only (harness) |
| supervisor-security | 110 | 6 | Browser `Origin` and foreign/missing `Host` refused; Origin-less local Host relayed; >64 MiB chunked body 413 without worker; 413 flushed before close | H (`serve-shim`) 4, R (`createShimRelay`) 2 | none | mixed: 4 blackbox / 2 unit |
| support | 71 | 8 | Harness `processIsRunning`/`requireFixtureShutdown` semantics | R (support.js) | none | js-only (harness) |
| thread-negotiation | 199 | 7 (+loop) | Thread continuations refused locally with 400 for Codex and Grok (before auth); self-contained thread creation forwarded; Anthropic bytes unchanged; count_tokens path | H (`serve-shim`) | fake proxy, fake Anthropic | blackbox |
| tool-schema-compat | 376 | 13 | Codex tool-schema adaptation of Unicode property escapes; supported applicators; refusals with JSON Pointer + reason; prior-adapter counterexamples | R; requires `../../sidequest/lib/suite-resolver.js`, `ajv/dist/2020.js`, `fixtures/prior-adapters/*` | ajv 8.20.0, monorepo sibling | unit (JS-only oracle/fixtures) |
| tool-search | 330 | 2 | Codex tool search resolves references while Anthropic passthrough byte-identical; proxy routes turn `tool_addition` blocks into `tools` | C (`serve-shim`), H | fake proxy, fake Anthropic | blackbox |
| usage-observability | 638 | 21 | Request composition counts, largest-remainder attribution, per-agent snapshots, cache TTL/thinking/server tools, SSE merge, limit evidence, loopback-only OTLP, project name from transcript (Windows casing), MCP footprint, bounded buffers, `event.name` attribute | R | Windows-only case | unit |
| windows-detached | 101 | 2 | Detached Windows service survives `KILL_ON_JOB_CLOSE`; invalid executable yields no PID | E, P | Windows only | integration (Windows) |
| windows-hide | 19 | 2 | Every production child_process call passes `windowsHide` (lint over JS source) via `../../test-support/windows-hide.js` | static source scan | monorepo sibling | js-only (JS source lint) |
| wiring-mode | 1096 | 49 | `env --write-project/--write-user/--remove/--reconcile`, retired `--mode` exit 2, project registry, pin propagation, doctor effective/shadowed/contradiction lines, effectiveBaseUrl precedence, SessionStart notices (SQ-1901), migration of committed settings, unwired cleanup, updater never wires its cwd | C 20, hook (`ensure --quiet`) 3, cli+hook 1, launcher 1, internal (`runNode` scripts) 24 | fake claude, temp project | mixed: ~25 blackbox / ~24 integration |

Support files (not counted as test files): `test/support.js` 860, `test/isolate-home.js` 21,
`test/native-budget-probe.js` 494 (probe library and CLI, `:484-491`), `test/codex-authenticated-preload.cjs` 17.

---

## 3. Black-box feasibility

### 3.1 Counts

File level (57 files, 693 static tests), each file counted once under its dominant class (section 2 gives the per-file split, so a few files listed here as unit or integration show as mixed there):

| Class | Files | Static tests |
|---|---:|---:|
| blackbox (whole file, 9; plus request-worker and gateway-usage at 6/7 and 5/6) | 11 — anthropic-catalog, catalog-refresh, compact-stream, context-sentry-lifecycle, gateway-telemetry, relay-stream-abort, shim-upload-abort, thread-negotiation, tool-search, (request-worker, gateway-usage are 6/7 and 5/6) | 82 (69 + 7 + 6) |
| unit (whole file) | 20 — atomic-file, control-auth, gateway-ps-locale, github-release, hook-startup-budget, lifecycle-lock, owned-worker-cleanup, picker-guard, plugin-registry, proxy-models, registry, release-verification, settings-write-guard, shim-request, stable-command-path, tool-schema-compat, usage-observability, grok-backend (12/13), proxy-upgrade (5/9 unit), shim-lifecycle (mostly) | 192 |
| integration (whole file) | 6 — gateway-ps-width, hook-ensure-delegation, recovery-lifecycle, windows-detached, gateway-drain (mostly), readiness-gate (half) | 64 |
| mixed (split above) | 14 — codex-upstream-cases, context-window, context-window-setting, discovery-cache, dispatch-model, gateway-process-isolation, gateway-readiness, lmstudio, local-llama, proxy-providers, rc-compat-mode, session-route-cache, supervisor-security, wiring-mode | 307 (includes the 33 harness self-tests of gateway-process-isolation) |
| js-only (whole file) | 6 — keep-alive-listener-leak, native-budget-probe, readiness-fast-path, start-gateway-timeout, support, windows-hide | 48 |

Test level, adding the mixed splits from section 2: roughly **200 blackbox**, **≈300 unit**, **≈110 integration**,
**≈81 js-only**. These are estimates from the surface scan, ±15 per class.

### 3.2 How black-box tests locate the CLI

| Mechanism | Where | Cite |
|---|---|---|
| Interpreter is always Node: `spawn(process.execPath, [cliPath, command])` | `startGateway` | `test/support.js:792` |
| Default entry path `CLI = path.join(__dirname,'..','bin','model-gateway.js')` | support.js and 14 test files (e.g. `test/wiring-mode.test.js:11`, `test/context-window.test.js:12`) | `test/support.js:11` |
| `startGateway` accepts `cliPath` but not another interpreter | | `test/support.js:790` |
| ~90 direct `[CLI, '<command>', …]` spawns via `process.execPath` (185 `process.execPath` references in tests) | `grep -c` over tests | — |
| Launcher form: `[launcher, 'readiness-gate']`, `[launcher, 'status', '--json']`, launcher written by `hooks/registry-writer.js writeCommandLauncher({home})` | | `test/readiness-gate.test.js:20,26`, `test/proxy-upgrade.test.js:152-196` |
| In-process `require(CLI)` (not black-box at all) | catalog-refresh, dispatch-model, rc-compat-mode, gateway-readiness, context-window, gateway-drain | `grep -l "require(CLI)"` |
| Readiness handshake: stdout must contain `listening on 127.0.0.1:<port>`; supervisor prints `model-gateway shim supervisor listening on 127.0.0.1:<port>`, worker `model-gateway shim listening on 127.0.0.1:<port> …` | | `test/support.js:812`, `lib/commands.js:2879`, `lib/request-worker.js:2693` |
| Then `GET /healthz` → 200 within 5 s | | `test/support.js:490-525` |

### 3.3 Harness contract a Go binary must satisfy (each: trivially satisfiable / protocol to implement / fixture change)

| # | Contract | Cite | Verdict for a Go binary |
|---|---|---|---|
| 1 | Launched as `node <cliPath> <command>` | `test/support.js:792` | **fixture change** or a JS trampoline at `bin/model-gateway.js` that execs the Go binary |
| 2 | Stdout line `listening on 127.0.0.1:<port>` after binding port 0, then `/healthz` 200 | `test/support.js:812,490-525` | trivially satisfiable |
| 3 | `serve-shim` stops on Node IPC message `{type:'fixture-shutdown'}` over the `'ipc'` stdio slot | `test/support.js:105-122,796,798`; product `lib/commands.js:2900-2903` | **protocol to implement**: Node IPC (`NODE_CHANNEL_FD` + newline-delimited JSON) in Go, or a trampoline translating `message` → SIGTERM. Without it each `serve-shim` teardown waits the 5 s budget, then SIGKILLs (`test/support.js:158-176`): slower, and a teardown that relies on orderly exit to stop the worker may leave it to the pid-file sweep. Tests that assert an orderly stop send SIGTERM themselves (`test/gateway-drain.test.js:390-424` asserts `lifecycle.jsonl` records `supervisor-stop-requested` with `signal: SIGTERM` and `supervisor-exit` with `exitCode: 0`), so a Go binary must handle SIGTERM as an orderly stop; which other teardowns depend on the IPC path was not verified |
| 4 | `serve-worker` exits on IPC `disconnect` (non-fixture children) | `test/support.js:146-150`; `lib/request-worker.js:1210` | same as 3 |
| 5 | Worker drain: `POST /drain` on the worker port with `x-model-gateway-control-token`, 202 `{"ok":true,"draining":true}` | `test/support.js:321-375` | trivially satisfiable (HTTP) |
| 6 | Control token file `~/.claude/model-gateway/control-token`, exactly 64 lowercase hex, ≤65 bytes | `test/support.js:299-311` | trivially satisfiable |
| 7 | PID files `~/.claude/model-gateway/{shim,guardian,proxy}.pid` (decimal PID) for teardown | `test/support.js:26-28,193-202` | trivially satisfiable |
| 8 | `guardian.pid.json` `{pid, command}` where fixtures hand-write `command: node "<install>/bin/model-gateway.js" serve-shim` and ownership compares against it | `test/readiness-fast-path.test.js:25-26` | **product decision + fixture change**: what command string a Go supervisor records and what the gate's ownership check matches |
| 9 | Installed plugin registry `installed_plugins.json {plugins:{'model-gateway@…':[{version, installPath}]}}`; cached copies under `plugins/cache/eigenwise-toolshed/model-gateway/<ver>/bin/model-gateway.js` | `test/readiness-gate.test.js:17-19`, `test/gateway-process-isolation.test.js:217-230` | **fixture change** (copies JS tree; Go needs a binary per version dir) |
| 10 | Env surface of 1.4 honored | `test/support.js:701-728` | trivially satisfiable if the port keeps the names |
| 11 | Fake proxy = copied Node binary run as `claude-code-proxy serve --no-monitor` with a `serve` script in the cwd | `test/gateway-process-isolation.test.js:198-203,1527-1528`; `lib/process-supervision.js:1019` | works unchanged against Go if argv (`serve --no-monitor`), path and cwd inheritance are kept |
| 12 | `NODE_TEST_CONTEXT` disables real-`claude` probes and out-of-temp settings writes | `lib/pins.js:213`, `lib/settings-wiring.js:89` | **protocol to implement**: Go must honor the same variable (Node sets it in test children) or the guard tests fail |
| 13 | `--require` / `NODE_OPTIONS=--require` monkeypatches of lib modules in the child | `test/discovery-cache.test.js:358`, `test/gateway-process-isolation.test.js:1497-1502`, `test/readiness-fast-path.test.js:33-54`, `test/readiness-gate.test.js:226-314`, `test/recovery-lifecycle.test.js:69-72` | **blocks**: these tests cannot run against Go; need Go-side fault injection (env hooks, a fake release URL) |
| 14 | On-disk JSON/JSONL state shapes are asserted directly: `catalog.json` v4 (`test/dispatch-model.test.js:448`), Claude Code discovery cache `~/.claude/cache/gateway-models.json` (`test/discovery-cache.test.js:101`), `wiring.json` and `wired-projects.json` (`test/wiring-mode.test.js:88-93`), `pins.json`, `context-window.json`, `dispatch-routes.json`, `request-routes.jsonl`, `logs/lifecycle.jsonl` events (`test/gateway-drain.test.js:415-423`), `worker-births/`, `picker-guard.json`, `*.pid` / `*.pid.json`, `request-body/` high-water files | — | trivially satisfiable only if the Go port keeps the exact file names and shapes; the suite freezes them, not just the HTTP/CLI surface |

Things that make a file non-black-box although it spawns: `-e` scripts that `require()` lib (flag E), preload
monkeypatches (#13), in-process `require(CLI)`, assertions on Node-only artifacts (`MaxListenersExceededWarning`,
`Module._load`), and monorepo escapes: `../../sidequest/lib/suite-resolver.js` (`test/tool-schema-compat.test.js:8`),
`../../test-support/windows-hide.js` (`test/windows-hide.test.js:6`), `ajv` (`package.json:13`), and the prior JS
adapters executed as fixtures (`test/fixtures/prior-adapters/snapshots.json:1-10`).

### 3.4 Practical options (for the planner; not chosen here)

| Option | What it takes | Covers | Cost / risk |
|---|---|---|---|
| A. JS trampoline `bin/model-gateway.js` → exec Go binary, forwarding argv/env/stdio and mapping IPC `message`/`disconnect` to SIGTERM | ~50 lines of JS; Go handles SIGTERM as orderly stop | all ~200 black-box tests with zero test edits | keeps a Node dependency in CI; PID/command identity (#8) still differs |
| B. Native Node IPC in Go (read `NODE_CHANNEL_FD`, JSON lines) | small Go package; tests unchanged except `process.execPath` launch | same as A | couples Go product to a Node test protocol |
| C. Port the harness to Go (`testing` + `httptest` fakes), keep JS suite only as reference | rewrite ~200 black-box tests | everything, long-term | largest effort; loses direct conformance check unless run in parallel during the port |

Confidence that A gives a working conformance run for the black-box subset: performance 0.8, scalability 0.7,
reliability 0.6 (ownership/PID-command fixtures, #8–#9, are the uncertain part), cost 0.8.

---

## 4. Behavior catalogue (acceptance checklist)

### 4.1 Routing

| Invariant | Pinned by |
|---|---|
| `claude-gpt-*` → proxy as backend id; proxy-listed ids → proxy un-prefixed; measured `grok-*` → own Grok backend; everything else → Anthropic | `dispatch-model:691`, `proxy-providers:255,301`, `proxy-models:124,148` |
| Proxy's Claude-style aliases never advertised or routed to proxy | `proxy-models:35`, `proxy-providers:138` |
| Claude passthrough byte-identical; never window/error rewritten | `context-window:775,1883`, `thread-negotiation:163`, `tool-search:75` |
| Credentials and plan-mode tools stripped for non-Claude rows. The escape hatch `CODEX_GATEWAY_KEEP_PLAN_TOOLS=1` (`lib/request-worker.js:2546-2547`) is set by no test of the suite | `proxy-providers:255`, `context-window:1063` |
| `count_tokens` for Codex routes to proxy | `context-window:1932`, `thread-negotiation:177` |
| Dispatch marker (`codex-auto`) parsed only from user text; tool_result echoes ignored; route cache TTL/LRU, survives compaction and restart, child inherits trusted parent | `dispatch-model:255,306,379`, `session-route-cache:82-469` |
| Vertex Gemini bare id only advertised; Fable proxy rows not spelled `claude-fable-` | `proxy-providers:176,203` |
| Thread continuations refused locally 400 for every gateway backend, before auth | `thread-negotiation:111-189`, `keep-alive-listener-leak:37` |

### 4.2 Streaming

| Invariant | Pinned by |
|---|---|
| Upstream dying mid-SSE terminates the client response (Codex, Anthropic, Grok; worker and supervisor) | `relay-stream-abort:67,90,125` |
| SSE heartbeat comments while upstream silent | `context-window:1969`, `lmstudio:602` |
| Lost `message_stop` closed only for finished turns | `codex-upstream-cases:190`, `compact-stream:268,356` |
| Empty Codex turn 503 → empty `end_turn` | `codex-upstream-cases:166` |
| Grok streaming function calls keep identity; text/tool order | `grok-backend:237,258` |
| Upload abort never kills supervisor or sibling SSE | `shim-upload-abort:181`, `shim-request:144` |

### 4.3 Sentry and compaction

| Invariant | Pinned by |
|---|---|
| Synthetic 413 `request_too_large` with `(N tokens > M tokens)` then `[model-gateway: phase …]` and `error.overflow{phase,refused_by,request_bytes,tokens,tokens_from,limit_tokens}` | `context-sentry-lifecycle:72-82`, `request-worker:99-191`, `context-window:661,685` |
| State per session × agent × model; compaction bypass; latch until a completed ordinary turn; JSON rearm only for `end_turn`/`stop_sequence`/`tool_use` | `context-sentry-lifecycle:84-232`, `context-window:721` |
| Usage summed across all SSE input fields; message_start-only usage counted (llama) | `context-window:637`, `local-llama:285` |
| Genuine 413 lowers only the receiving model; exact `n_ctx` never lowers trigger | `context-window:836`, `local-llama:338`, `lmstudio:666` |
| `CODEX_GATEWAY_SENTRY=0` disables gating and rewriting | `context-window:745` |
| Compaction stream guard: retry truncated, replay real errors, no fabricated terminal, request-shaped failures not retried, Claude-native never buffered, 20 concurrent all complete, guard switchable, `/healthz` reports settings | `compact-stream:268-788` |
| Context-window setting precedence and compact-at rules (272000 cap, −85000 legacy, −40000 ceiling) | `context-window-setting:30-269` |

### 4.4 Catalog and discovery

| Invariant | Pinned by |
|---|---|
| `/v1/models` from proxy listing, refreshed on timer; burst does not refresh or block | `codex-upstream-cases:205` |
| Fallback catalog when proxy down, never persisted to discovery cache | `discovery-cache:239`, `proxy-providers:559` |
| Discovery cache schema, no rewrite when unchanged, foreign cache untouched, none in RC mode | `discovery-cache:101-166` |
| Anthropic catalog merged first, cached without secrets, stale cache served, refresh error reported | `anthropic-catalog:54-229` |
| Catalog v4 contract; subset writes preserve; future schema refused; `--json` stdout pure | `dispatch-model:448-666` |
| `catalog --refresh` fails loudly on unhealthy/empty/error | `catalog-refresh:80-178` |

### 4.5 Wiring

| Invariant | Pinned by |
|---|---|
| `env --write-project` → `.claude/settings.local.json` only; `--write-user` shared fallback preserving user values; no-flag `env` writes nothing | `wiring-mode:96-125`, `context-window:1845` |
| Retired `--mode`/`--show-mode` exit 2 | `wiring-mode:133`, `.claude/skills/verify/SKILL.md:15` |
| Precedence: process env > project-local > project > user; doctor `[effective]`, shadowed file, mode contradiction fails | `wiring-mode:423,568,642,663` |
| `env --remove` clears owned keys and pins, names preserved user pins | `wiring-mode:1050,1068`, `rc-compat-mode:745` |
| Project registry records, dedups, prunes; updater never wires its cwd | `wiring-mode:152,286,994` |
| Orphan aliases repaired by `ensure`; doctor only reports | `wiring-mode:1013,1037` |
| SessionStart notice shown to user only for actionable states (SQ-1901) | `wiring-mode:952-981` |

### 4.6 Pins

| Invariant | Pinned by |
|---|---|
| Overrides in `pins.json` always win; `pin --<alias> default` removes | `context-window:1575` |
| Detected alias older than shipped default loses (SQ-3205); retired shipped values rewritten | `wiring-mode:239,263`, `context-window:1338` |
| Failed probe keeps last good pin; after a CLI version change the stale pin stays in the cache but is no longer the effective default (`:1398-1401`) | `context-window:1372` |
| Probe makes no egress (`context-window:1457-1519`, where `:1519` starts the positive control) and is credential-free (`context-window:1647-1681`) | `context-window:1457-1519,1647-1681`, `settings-write-guard:56-89` |
| Fable custom-option and "(previous)" relabelling | `picker-guard:33-92` |

### 4.7 Lifecycle and supervision

| Invariant | Pinned by |
|---|---|
| Supervisor keeps listener while worker restarts; drain finishes in-flight | `gateway-drain:285-460` |
| Version-based supervisor replacement; newer supervisor tells stale sessions to reload | `gateway-drain:662-721` |
| Restart adopts newer sibling, refuses older and dev→install switches | `gateway-drain:116,460,541,562` |
| Proxy recovery: fresh-connection confirmation, two failed probes, single-flight, cancellation | `gateway-drain:815-946` |
| Shim states `running-ours`/`running-foreign`/`starting`/`stopped`; same line in status/doctor/ensure | `shim-lifecycle:117,380` |
| Start timeout semantics; stop judged by PID gone (GH-371) | `shim-lifecycle:268-363` |
| Lifecycle lock and intentional stop persistence | `lifecycle-lock:27-114`, `readiness-gate:190,226,338` |

### 4.8 Ownership safety

| Invariant | Pinned by |
|---|---|
| Never stop a foreign or unknown owner (port, proxy, junction, configured port) | `gateway-process-isolation:521,1577,1614,1647,1720`, `shim-lifecycle:371`, `gateway-drain:188,905` |
| PID reuse: stale guardian PID discarded without killing the reused process; start-time identity | `gateway-process-isolation:392,1750`, `owned-worker-cleanup:106-159` |
| Orphan worker reaping needs a birth record and lost parent; single target, no escalation | `owned-worker-cleanup:48-344`, `gateway-process-isolation:1326` |
| Probe timeout budget 2 s / 8 s Windows, refusal names it | `gateway-process-isolation:347,436` |
| Test fixtures never touch the installed gateway | `gateway-process-isolation:598`, `settings-write-guard:21-104` |

### 4.9 Readiness gate (hooks)

| Invariant | Pinned by |
|---|---|
| Gate checks router health only; Codex auth/upstream never blocks | `readiness-fast-path:63`, `.claude/skills/verify/SKILL.md:25` |
| Healthy gate is cheap (no CLI load, no lock) | `readiness-fast-path:63` |
| Exit 2 with guidance; host-could-not-run lets prompt through; truncated stdin exit 0; no `onFailure` | `readiness-gate:45,57,113,121` |
| Non-gateway endpoint no-op before CLI resolution; plugin `false` disables; absent entry keeps recovery | `readiness-gate:87,129-145` |
| 25 s child deadline below 35 s hook timeout | `readiness-gate:169` |
| Hook delegates to the recorded install | `hook-ensure-delegation:59-244` |
| SessionStart waits ≤12 s, always exits 0 | `hook-startup-budget:10` |

### 4.10 RC compatibility

| Invariant | Pinned by |
|---|---|
| Managed hosts block parse/add/remove preserves other content; partial blocks refused | `rc-compat-mode:159-225` |
| Enable refuses before any write on port conflict, HTTPS process URL, unbindable serving; rolls back only its own bytes | `rc-compat-mode:234-560` |
| Hosts path overridable, real file never touched | `rc-compat-mode:608` |
| Second listener only with hosts entry; safe fallback when compat port taken | `rc-compat-mode:829-857,982` |
| RC control only on main loopback listener | `rc-compat-mode:876` |
| DNS bypass resolver: never hosts-aware fallback, AAAA fallback, errors closed | `rc-compat-mode:776-814` |

### 4.11 Usage observability and telemetry

| Invariant | Pinned by |
|---|---|
| Counts only, never content; exact attribution; per-agent snapshots; bounded buffers | `usage-observability:78-616` |
| One usage record per request (not per block); slow collector never delays inference | `gateway-usage:286,333` |
| Route span metadata-only; trace/auth stripped before Codex; telemetry failure cannot change response | `gateway-telemetry:114-410` |
| Request-route log metadata-only; `CODEX_GATEWAY_REQUEST_LOG=0` disables | `context-window:558,603` |
| OTLP endpoints loopback only | `usage-observability:312` |

### 4.12 Tool schema compatibility

| Invariant | Pinned by |
|---|---|
| Only approved property atoms rewritten, other regex bytes preserved; unsupported compositions refused with JSON Pointer + reason; unaffected sibling tools unchanged | `tool-schema-compat:108-360` |
| Codex tool search references resolved; `tool_addition` → `tools` | `tool-search:75,271` |

### 4.13 Local providers

| Invariant | Pinned by |
|---|---|
| llama-server window from `/props`; sentry rule; overflow 413 | `local-llama:141-359` |
| LM Studio window from loaded context; token only to LM Studio; cached tokens subtracted; overflow 413 incl. 200-stream first event | `lmstudio:221-754` |

### 4.14 Security of the local surface

| Invariant | Pinned by |
|---|---|
| Browser Origin / foreign Host refused; 64 MiB cap 413 flushed before close | `supervisor-security:33-105` |
| Control token never leaks upstream; lifecycle requests authenticated | `gateway-drain:582,605`, `control-auth:17-49` |
| GitHub token only to API, dropped on cross-host redirect | `github-release:12-95` |
| Release archive checksum fail-closed | `release-verification:31-60` |

---

## 5. Skills contract

### 5.1 Invocation form

All skill commands run as `node ~/.claude/model-gateway/model-gateway.js <command>` (`skills/model-gateway/SKILL.md:27,71-74`;
`skills/remote-control-compatibility/SKILL.md:18,39,74,96`). SessionStart recreates this launcher from the installed-plugin
registry (`skills/model-gateway/SKILL.md:27`). **A Go port either keeps a JS launcher at that path or changes both skills.**

### 5.2 Commands and flags the skills tell Claude to run

| Command | Flags | Skill cite | Usage cite |
|---|---|---|---|
| `setup` | `--preserve-wiring` (updater) | `skills/model-gateway/SKILL.md:37,71,74` | `lib/commands.js:155` |
| `login` | `--device` | `:73` | `:156` |
| `start`, `stop` | | `:35,289` | `:157` |
| `ensure` | `--quiet` (SessionStart) | `:60,288` | `:158` |
| `recover` | | `:35,39` | `:159` |
| `status` | `--json` (tests) | `:286` | `:160` |
| `models` | | `:100` | `:161` |
| `catalog` | `--json`, `--refresh` | `:370` | `:162-164` |
| `pin` | `--opus/--sonnet/--fable <model|default>` | `:127,172-173` | `:165-167` |
| `context-window` | `--claude/--codex/--grok <tokens|full>`, `--codex-compact-at/--grok-compact-at <tokens|cap>` | `:158-161` | `:168-185` |
| `env` | `--write-project`, `--write-user`, `--remove`, `--reconcile` | `:43,45,94,279,290` | `:186-190` |
| `doctor` | | `:287` | `:191` |
| `remote-control` | `doctor`, `enable --confirm`, `disable --confirm` | `skills/remote-control-compatibility/SKILL.md:39,74,96` | `:192-194` |
| `serve-shim`, `serve-worker` | internal | — | `:195`, `lib/commands.js:3039-3040` |
| `readiness-gate` | JSON stdin `{hook_event_name, cwd}` | — | `bin/model-gateway.js:5`, `test/readiness-gate.test.js:25-29` |
| proxy sign-in | `"~/.claude/model-gateway/bin/claude-code-proxy" <kimi|cursor|grok|muse> auth login` | `skills/model-gateway/SKILL.md:136` | — |

### 5.3 Paths named by the skills

| Path | Cite |
|---|---|
| `~/.claude/model-gateway/model-gateway.js` (stable launcher) | `skills/model-gateway/SKILL.md:27` |
| `~/.claude/model-gateway/control-token` (never print) | `:68` |
| `~/.claude/model-gateway/context-window.json` | `:158` |
| `~/.claude/model-gateway/pins.json` | `:175` |
| `~/.claude/model-gateway/picker-guard.json` | `:134` |
| `~/.claude/model-gateway/models.json` (fallback, Codex `gpt-*` only) | `:103,186` |
| `~/.claude/model-gateway/catalog.json` (Sidequest catalog) | `lib/commands.js:162` |
| `~/.claude/model-gateway/worker-births/` | `skills/model-gateway/SKILL.md:301` |
| `~/.claude/model-gateway/logs/{guardian.log,lifecycle.jsonl,request-routes.jsonl}` | `:113-114,232,323` |
| `~/.claude/model-gateway/request-body/` (per-session high-water JSON) | `:240` |
| `~/.claude/model-gateway/bin/claude-code-proxy` | `:136` |
| `~/.claude/cache/gateway-models.json` (picker cache) | `:125` |
| `~/.claude.json` `additionalModelOptionsCache` (read) | `:133` |
| `.claude/settings.local.json` (project), `.claude/settings.json`, `~/.claude/settings.json` | `:43-45,84-86` |
| `/etc/hosts`, `C:\Windows\System32\drivers\etc\hosts` | `skills/remote-control-compatibility/SKILL.md:64` |
| `.claude/agents/<name>.md` with gateway id in `model:` | `skills/model-gateway/SKILL.md:208-221` |

### 5.4 Output strings and values the skills tell Claude to look for

| String / value | Cite |
|---|---|
| `shim (model router) on :<port>: running-ours (serving <version>)` \| `running-foreign (PID <pid>, <install root or owner unidentified>)` \| `starting (PID <pid> since <time>)` \| `stopped` | `skills/model-gateway/SKILL.md:293-297` |
| `[effective]` marker on winning settings file | `:45,80` |
| `fallback catalog (proxy unreachable)` (status) | `:104-105` |
| `upstream-unavailable`, `upstream-blocked`, `upstreamBlocked.expiresAt` | `:341-350` |
| `proxy <provider> auth:`, `proxy local server:`, `proxy lmstudio server:`, `proxy local window: MISMATCH (...)`, `authenticated`/`MISSING (...)`/`NOT CONFIGURED (...)`/`NOT RUNNING (...)`/`status unavailable` | `:136,145,151,155` |
| Catalog `providers.<p>.state`: `ready`, `auth-missing`, `config-invalid`, `backend-down`, `auth-unknown` | `:145,151,155` |
| `leftover gateway settings: aliases pinned to <ids>, no gateway routing`, `wired-but-unregistered project` | `:281` |
| 413: `(N tokens > M tokens)` then `[model-gateway: phase …, refused by …; request … bytes, … tokens, limit … tokens; recovery: …]`; `error.overflow` fields; phases `first_turn`/`continuation`/`compaction`/`UNVERIFIED`; `refused_by` `gateway_sentry`/`upstream` | `:351-364` |
| Compaction telemetry outcomes `completed`, `empty_summary`, `incomplete`, `compaction_terminal_code`, `unknown_error` | `:244` |
| Model-window table `PASS`/`FAIL` naming missing/extra ids | `:312-321` |
| RC serving result `bound` / `bindable` / `unavailable (CODE)` / `unknown` | `skills/remote-control-compatibility/SKILL.md:42` |
| Hosts block `# >>> model-gateway RC compatibility >>>` / `127.0.0.1 api.anthropic.com` / `# <<< model-gateway RC compatibility <<<` | `:58-62` |
| Picker label "From gateway"; "<name> (previous)" | `skills/model-gateway/SKILL.md:82,133` |
| `/healthz` fields `compat.hostsDetected`, `compat.port80Bound`, `reason` | `.claude/skills/verify/SKILL.md:16` |

### 5.5 Exit codes and numeric constants in the contract

| Item | Value | Cite |
|---|---|---|
| `ensure --quiet` (SessionStart) | always 0 | `skills/model-gateway/SKILL.md:59`, `.claude/skills/verify/SKILL.md:14` |
| Readiness gate refusal | exit 2 with guidance; host failure → prompt passes | `skills/model-gateway/SKILL.md:35`, `.claude/skills/verify/SKILL.md:27` |
| Retired `env --mode` / `--show-mode` | exit 2 | `.claude/skills/verify/SKILL.md:15` |
| `catalog --refresh` declining to write | non-zero, reason on stderr | `skills/model-gateway/SKILL.md:371-374` |
| Ports | shim 18764, proxy 18765, RC 80 | `:270,324` |
| Gate timings | health 750 ms; SessionStart wait 12 s; recovery child 25 s; hook 35 s | `:31,35,60` |
| Probe budget | 2 s default, 8 s Windows (`CODEX_GATEWAY_PROBE_TIMEOUT_MS`) | `:376` |
| Body cap | 64 MiB → 413 | `:330` |
| Windows | Codex cap 272000; legacy −85000 (187000); ceiling −40000; local sentry floor 32768, −min(⅓, 40000) | `:149,157-159` |
| Upstream holds | transient 30 s; 429 Retry-After → reset header → 60 s | `:341-348` |
| Catalog cache | Anthropic 1 h; listing refresh 1 min; retry 1 s doubling to 30 s | `:125,186` |

### 5.6 Workflows

| Workflow | Steps (skill) | Cite |
|---|---|---|
| First-time setup | install → reload plugins → `setup` → if needed `login` then `setup` → full Claude Code restart | `skills/model-gateway/SKILL.md:47-55` |
| Re-auth on wired install | `login` + `setup`; no Claude Code restart needed | `:108-121` |
| Diagnose | `status`, `doctor`, logs under `logs/`; Astra missing → check proxy version, `setup` | `:283-327,387` |
| Pins | `pin`, `pin --<alias> <id|default>`, restart affected sessions | `:170-179` |
| Context window | `context-window …`, then `stop`+`ensure`, restart sessions | `:158-161` |
| RC compatibility | `remote-control doctor` → explain block → direct yes → `enable --confirm` → restart; disable symmetric | `skills/remote-control-compatibility/SKILL.md:34-101` |
| Turn gateway off for a project | `env --remove`, restart | `:21-28` |
| Unwire before uninstall | `env --remove` (project), `env --remove --write-user` (user) | `skills/model-gateway/SKILL.md:39,279,290` |

### 5.7 Verify skill (`.claude/skills/verify/SKILL.md`)

Prescribes a black-box procedure: isolated home, random ports, `bin/model-gateway.js serve-shim` against a fake proxy
(`:8`); drive `GET /v1/models`, `POST /v1/messages` with current and legacy ids (`:12-13`); `ensure --quiet` (`:14`);
`env --write-project`/`--write-user`, doctor precedence, retired flags exit 2 (`:15`); RC via
`CODEX_GATEWAY_HOSTS_FILE` + `CODEX_GATEWAY_COMPAT_PORT` with `/healthz` `compat.*` (`:16`); DNS guard via
`createHostsBypassResolver` in-process (`:17`, not black-box); stop-marker semantics and gate scenarios (`:19-27`).
`test/hook-startup-budget.test.js:34-37` asserts wording in this file and in `hooks/hooks.json`, so doc text is under test.

---

## 6. Changelog digest

Current version **0.56.3** (2026-11-17) (`CHANGELOG.md:5`, `.claude-plugin/plugin.json:3`). 102 version headings, 195
bullets. Skill text covers Muse, Vertex AI, local GGUF and LM Studio rows that the changelog does not yet list (they ride
on unreleased proxy `dev` builds, `skills/model-gateway/SKILL.md:139-155`).

### 6.1 Most recent entries

| Version (date) | Entry | Cite |
|---|---|---|
| 0.56.3 (11-17) | Downstream stream destroyed when passthrough/Grok upstream closes mid-response (GH-820) | `CHANGELOG.md:11-12` |
| 0.56.3 | Gate the host could not run no longer blocks with no message (GH-847) | `:13-14` |
| 0.56.2 (11-12) | Unwiring removes owned pins; ensure repairs orphan settings (SQ-4105) | `:22-23` |
| 0.56.2 | Current Fable in /model via `ANTHROPIC_CUSTOM_MODEL_OPTION`, "(previous)" relabel, picker check (SQ-4106) | `:24-25` |
| 0.56.1 (11-10) | Anthropic catalog refreshed from inference requests (SQ-4102) | `:33-34` |
| 0.56.0 (11-10) | Anthropic discovery merged; pins advance from catalog (SQ-4098) | `:42-43` |
| 0.55.4 (11-10) | GitHub token order, rate-limit advice, Authorization dropped cross-origin (GH-718) | `:51` |
| 0.55.4 | `CODEX_GATEWAY_REQUEST_COMPRESSION` → `CCP_CODEX_REQUEST_COMPRESSION` (GH-782) | `:52-53` |
| 0.55.3 (10-25) | Test-only: Windows PID reuse flake; wait for exit event (SQ-3912) | `:61-62` |
| 0.55.2 (10-24) | Birth-identified stale worker reaping without interrupting streams (GH-577) | `:70-71` |
| 0.55.2 | Gate survives deleted session cwd (GH-618) | `:72-73` |
| 0.55.1 (10-21) | Test cleanup trusts observed close; Windows EPERM (SQ-3821) | `:81-82` |
| 0.55.1 | Healthy gate skips CLI load (SQ-3845) | `:83-84` |
| 0.55.1 | 413 flushed before closing oversized upload, 250 ms grace (SQ-3846) | `:85-86` |
| 0.55.0 (10-21) | Recovery after reboot: lifecycle lock, intentional stop, SessionStart + UserPromptSubmit hooks, `recover` (SQ-3839) | `:94` |
| 0.54.5 (10-20) | Tests run in throwaway home, refuse outside writes (GH-319); aborted body reads contained (GH-578) | `:103-104` |
| 0.54.4 (10-17) | Origin/Host refusal, 64 MiB cap at supervisor (SQ-3717) | `:112-113` |
| 0.54.3 (10-12) | Sentry rearm after failed compaction (SQ-3567); Codex JSON incomplete turns stay latched (SQ-3573) | `:121-123` |
| 0.54.2 (10-08) | Test-only: thread-negotiation port race (SQ-3489) | `:131-132` |
| 0.54.1 (10-07) | Catalog test isolated from real home (SQ-3435); overflow errors name phase, per-agent sentry (SQ-3446) | `:141-143` |
| 0.54.0 (10-04) | Direct Codex/Grok compact-at maxima (SQ-3292) | `:151-152` |
| 0.53.1 (10-02) | Lagging CLI alias no longer pulls projects back (SQ-3205); native budget probe (SQ-3248/3253/3256) | `:175-182` |
| 0.53.0 (10-01) | Sonnet 5.5 pin; per-backend `context-window` with 272k Codex cap; GPT-6.1 Sol row | `:190-200` |
| 0.52.2 (09-30) | `/healthz` not waiting on auth, stale `gateway.sock` reclaimed (#367); Windows stop judged by PID gone (SQ-3170); any marketplace (#380) | `:210,216,222` |
| 0.52.0–0.51.7 | Fixture teardown tracks every process (SQ-3075, SQ-3142); status/doctor/ensure agree (SQ-3124); 429 expiry/empty turn/models stall (SQ-3125); fallback list kept out of discovery cache (SQ-3126) | `:260,266,279,297,299` |

### 6.2 Recurring bug classes and hard-won invariants (most likely to regress in a rewrite)

| Class | Evidence | Pinned by |
|---|---|---|
| Stream left open after upstream death | SQ-2855 (`CHANGELOG.md:383`), GH-820 (`:11`) | `relay-stream-abort` |
| Compaction stream truncation / late abort mis-reported | SQ-2870 (`:458`), SQ-2822 (`:478`) | `compact-stream` |
| Sentry latch stuck or rearmed too early | SQ-2477 (`:779`), SQ-3567/3573 (`:121-123`), SQ-3446 (`:143`) | `context-sentry-lifecycle`, `context-window` |
| Killing a foreign install or reused PID | SQ-2460 (`:771`), SQ-2481 (`:745`), SQ-2483 (`:728`), GH-577 (`:70`), SQ-3912 (`:61`) | `gateway-process-isolation`, `owned-worker-cleanup` |
| Windows process semantics (taskkill exit, elevation, EPERM, netstat locale) | SQ-3170 (`:216`), SQ-2810 (`:498`), SQ-3821 (`:81`), #296 | `shim-lifecycle`, `gateway-ps-locale`, `support` |
| Port binding races and ownership of listeners | SQ-1307 (`:1045`), SQ-3489 (`:131`), #367 socket reclaim (`:210`) | `gateway-readiness`, `thread-negotiation` |
| Health endpoint blocked by slow auth checks | #238/#367 (`:210,297`) | `codex-upstream-cases`, `gateway-readiness` |
| Upstream state over-blocking (429 forever, 503 retry loop) | #190/#192 (`:297`) | `codex-upstream-cases` |
| Tests touching the developer's real home / installed gateway | GH-319 (`:103`), SQ-3435 (`:141`), `test/isolate-home.js:4-7` | `settings-write-guard`, isolation harness |
| Hook budget overruns killing the supervisor tree | SKILL `:332`; 0.55.0 (`:94`) | `hook-startup-budget`, `readiness-gate`, `windows-detached` |
| Pins moving backwards | SQ-3205 (`:175`), GH-253 (`:259`) | `wiring-mode`, `context-window` |
| Browser-origin quota spend / oversized uploads | SQ-3717 (`:112`), SQ-3846 (`:85`), GH-578 (`:104`) | `supervisor-security`, `shim-upload-abort` |
| `--json` stdout polluted by logs | SQ-2208 (`lib/commands.js:201-205`) | `dispatch-model:596` |

---

## 7. Fixtures

| File | Lines | Content | Used by |
|---|---:|---|---|
| `test/fixtures/native-budget-client.js` | 148 | Stand-in for the `claude` CLI: reads a prompt on stdin, emits stream-json events, calls `$ANTHROPIC_BASE_URL/v1/messages` and `/count_tokens` with a `Task` tool, writes a synthetic `autocompact:` debug file; 17 modes (`complete`, `refuseTool`, `timeout`, `oversizedOutput`, …) | `test/native-budget-probe.test.js:13,15-30` via `test/native-budget-probe.js` (`fixtures/native-budget-client.js:139-148`) |
| `test/fixtures/prior-adapters/20ef3d38-tool-schema-compat.js` | 90 | Old `lib/tool-schema-compat.js` from commit `20ef3d38…` | `test/tool-schema-compat.test.js:254` (executed against Ajv counterexamples SQ-2551/2561) |
| `test/fixtures/prior-adapters/d68e8ec5-tool-schema-compat.js` | 222 | Old version from commit `d68e8ec5…` | same |
| `test/fixtures/prior-adapters/snapshots.json` | 10 | Source commit and sha256 pin for each snapshot | `test/tool-schema-compat.test.js:27-34` |
| `test/codex-authenticated-preload.cjs` | 17 | `execFile` patch faking `codex auth status` | `test/discovery-cache.test.js:358` |

For a Go port the prior-adapter snapshots are regression oracles, not code to port: the counterexample schemas they are run
against can be lifted into Go test tables; the JS snapshots themselves stay js-only.

---

## 8. Open questions

1. `hooks/hooks.json`, `hooks/ensure.js`, `hooks/registry-writer.js` (launcher generation) — UNREAD beyond what tests show; they define
   the SessionStart/UserPromptSubmit contract and the stable launcher format. Another lane presumably owns them.
2. `../../sidequest/lib/suite-resolver.js` and `../../test-support/windows-hide.js` live outside the plugin root — UNREAD. The Go
   repo cannot depend on them; decide whether the cold-setup convention test (`test/tool-schema-compat.test.js:95`) and the
   `windowsHide` lint have Go equivalents (a `go vet`-style check on `exec.Cmd.SysProcAttr.HideWindow`?).
3. Consumer of `plugin.json` `suiteTimeout: 300000` — UNREAD (probably Toolshed CI).
4. `SQ3248_SCRATCHPAD` env (`test/native-budget-probe.test.js:12`): who runs the native probe for real and where its receipts go — UNREAD.
5. What command line will a Go supervisor record in `guardian.pid.json`, and does ownership keep matching on the
   install root path? Fixtures hard-code `node "<install>/bin/model-gateway.js" serve-shim` (`test/readiness-fast-path.test.js:25`).
6. Will the Go product keep the Node IPC stop path (`lib/commands.js:2900-2903`) for compatibility with the JS harness, or
   switch to signals only? This decides whether the black-box subset runs unchanged (section 3.4).
7. Is `NODE_TEST_CONTEXT` (product test-runner detection, `lib/pins.js:213`, `lib/settings-wiring.js:89`) to be kept, renamed,
   or replaced by an explicit env switch in Go?
8. `readiness-fast-path` pins "healthy gate does not load commands.js" by Module._load interception; the Go analogue
   (e.g. no lifecycle lock file touched, latency bound) needs a new observable.
9. Per-test counts for mixed files other than the three large ones are estimates from greps, not from reading every body.
