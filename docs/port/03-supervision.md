model: claude-opus-5-5

# 03 — Process supervision and host integration (behavior inventory)

Source: `/Users/zchee/src/github.com/Eigenwise/eigenwise-toolshed/plugins/model-gateway`, commit `a8501f251755` (written against `4ffb2f672`; moved to this commit on 2026-11-25, see `07-repin.md`), branch `dev`.
All citations are `path:line` relative to that plugin root. Nothing was executed; everything below comes from reading source.
Written 2026-11-25 21:42:33 JST (from `date` in the same command).

Conventions: `#N`, `GH-N` and `SQ-N` are the plugin source's own tracker ids quoted from its comments; none refer to this repository. **UNREAD** = not read, not asserted. **EXT** = statement from outside knowledge (OS/Node/PowerShell semantics), not from this source; flagged so a planner can verify independently.

## 0. Files read

| File | Lines read | Why |
|---|---|---|
| lib/process-supervision.js | 1-1216 (all) | slice subject |
| lib/lifecycle-lock.js, lib/windows-detached.js, lib/control-auth.js, lib/atomic-file.js, lib/picker-guard.js, lib/remote-control.js | all | slice |
| lib/commands.js | 241-530, 531-920, 1111-1118, 2262-2908, 2930-3130 | the supervisor (`runShim`), relay, HTTP layer, start/stop orchestration live here, not in process-supervision.js |
| lib/request-worker.js | 1180-1275, 2340-2440, 2670-2734 | worker side of `/drain`, listeners, IPC port report |
| lib/runtime.js | 1-144, 549-776 | constants (ports, paths), install-path helpers |
| lib/shim-state.js, lib/lifecycle-diagnostics.js, lib/readiness-gate.js | all | callers/callees of the slice |
| hooks/hooks.json, hooks/ensure.js | all | hook entry points |
| hooks/registry-writer.js | 60-130, 170-204 | stable launcher text (spawns CLI) |
| lib/pins.js | 54-56, 185-195, 472-476 | `terminateProbe`, `envBlockFor`, `isGatewayModelId` used by picker-guard |
| test/gateway-ps-width.test.js | grep only (lines 3-15, 47, 127-131) | why `ps -ww`; lstart shape |

Everything else in lib/ (request routing, catalog, settings-wiring, readiness-hooks, recovery-policy, github-release) is UNREAD beyond grep hits.

---

## 1. Process topology

### 1.1 Process table

| Process | Spawned by | Command line | Env additions | Listens on | Cite |
|---|---|---|---|---|---|
| **CLI** (`model-gateway.js <cmd>`) | user / hooks / stable launcher (`~/.claude/model-gateway/model-gateway.js`) | `node <install>/bin/model-gateway.js <cmd>` | — | nothing | bin/model-gateway.js:4-16; hooks/registry-writer.js:106 |
| **Supervisor** a.k.a. guardian (`serve-shim`) | CLI `startAll` → `launchSupervisor` → `spawnDetached('guardian', …)` | `<process.execPath> <newest sibling install>/bin/model-gateway.js serve-shim` | `MODEL_GATEWAY_STARTUP_GENERATION=<uuid v4>` merged over full `process.env` | TCP `127.0.0.1:PUBLIC_SHIM_PORT` (default 18764); optionally `<hosts-ip>:COMPAT_PORT` (default 80) | commands.js:757-766, 2876, 2756-2781; process-supervision.js:962-991 |
| **Worker** a.k.a. shim (`serve-worker`) | Supervisor `startWorker` via `child_process.fork` | `<execPath> <workerScript> serve-worker` (`workerScript` starts as the supervisor's own `CLI_PATH`; `/restart` may switch it) | `CODEX_GATEWAY_WORKER_PORT=String(configuredWorkerPort)` → `"0"` when unset | TCP `127.0.0.1:<ephemeral>` (reported over IPC); Unix socket `~/.claude/model-gateway/gateway.sock` or Windows pipe `\\.\pipe\model-gateway` (override `CODEX_GATEWAY_SOCKET_PATH`) | commands.js:2612-2655; request-worker.js:2688-2701; runtime.js:47-49 |
| **claude-code-proxy** | Supervisor `createProxyRecovery` → `spawnSupervisedProxy` | `~/.claude/model-gateway/bin/claude-code-proxy[.exe] serve --no-monitor` | `PORT=<PROXY_PORT>` (default 18765); `CCP_CODEX_REQUEST_COMPRESSION` copied from `CODEX_GATEWAY_REQUEST_COMPRESSION` when only the latter is set | TCP `PROXY_PORT` (bind address chosen by the proxy, UNREAD) | process-supervision.js:1009-1025; runtime.js:23, 26 |
| **Probe children** (ps / lsof / netstat / powershell) | Supervisor and CLI | see §5.2 | POSIX: `LC_ALL=C` over full env; Windows: inherited | — | process-supervision.js:236-241, 436-475 |
| **RC-compat bind probe** (in-process `net.Server`, not a child) | Supervisor on `/internal/rc-compatibility` `probe` | — | — | `<hosts-ip or 127.0.0.1>:COMPAT_PORT`, closed immediately | commands.js:2728-2754 |
| **Windows launcher** (transient) | WMI `Win32_Process.Create` called from the CLI's PowerShell | `"<node.exe>" "<…>/lib/windows-detached.js" "<base64 payload>"` | WMI environment block = full CLI env + additions | — | windows-detached.js:29-57, 59-82 |
| **Picker probes** (`claude -p …`) | CLI (`ensure`, `doctor`) | see §10 | native env / gateway env | — | picker-guard.js:21, 32-57 |

Ports: `PUBLIC_SHIM_PORT = Number(CODEX_GATEWAY_PORT || 18764)`, `SHIM_PORT = Number(CODEX_GATEWAY_WORKER_PORT || PUBLIC_SHIM_PORT)`, `PROXY_PORT = Number(CODEX_GATEWAY_PROXY_PORT || 18765)`, `COMPAT_PORT = Number(CODEX_GATEWAY_COMPAT_PORT || 80)` (runtime.js:24-26, 45; duplicated at commands.js:88-90).

**Worker port subtlety.** The supervisor passes `CODEX_GATEWAY_WORKER_PORT="0"` (commands.js:2622). `"0"` is a non-empty, truthy string, so in the worker `SHIM_PORT = Number("0") = 0` → the worker binds an ephemeral port (request-worker.js:2690) and reports it with `process.send({type:'listening', port})` (request-worker.js:2692). The same truthy string makes the worker skip its own compat-port listener (`!process.env.CODEX_GATEWAY_WORKER_PORT` is false, request-worker.js:2713); the supervisor owns the compat listener instead (commands.js:2756-2781). In the CLI process (no env var) `SHIM_PORT == PUBLIC_SHIM_PORT`, so CLI health checks `GET 127.0.0.1:18764/healthz` hit the supervisor (shim-state.js:7-12; process-supervision.js:54-59).

### 1.2 Relay path

| Path | Transport | Cite |
|---|---|---|
| Client → supervisor | HTTP/1.1 over TCP `127.0.0.1:18764` | commands.js:2876 |
| Supervisor → worker | **new TCP connection per request** (`http.Agent({keepAlive:false})`) to `127.0.0.1:<workerPort>`; `Host` rewritten to `127.0.0.1:<workerPort>`; all other headers copied | commands.js:2499, 2373-2376 |
| Client → worker directly | Unix socket / named pipe `SOCKET_PATH` — owned by the worker, **not relayed by the supervisor**; rebinds on each worker restart | request-worker.js:2696-2701, 1180-1206 |
| Worker → proxy / upstreams | HTTP (request-worker lane; UNREAD here) | — |
| Supervisor ↔ worker control | Node IPC channel (fork `'ipc'` stdio slot): worker→supervisor `{type:'listening',port}`; supervisor → worker `disconnect()` on shutdown | commands.js:2620, 2633-2639, 2587-2589; request-worker.js:1210 |

No `net.Server` handle passing and no socket transfer is used anywhere in the slice (grep for `send(` with a handle: none; the only `process.send` is the port report, request-worker.js:2692).

### 1.3 Diagram

```
 Claude Code ──HTTP──▶ 127.0.0.1:18764 ┐                      ┌─ ~/.claude/model-gateway/gateway.sock
 (ANTHROPIC_BASE_URL)                  │                      │  (or \\.\pipe\model-gateway)  ◀── ANTHROPIC_UNIX_SOCKET clients
 api.anthropic.com:80 (hosts compat) ─┤                      │
                                       ▼                      │
            ┌──────────────── SUPERVISOR (serve-shim, detached, guardian.pid) ────────────────┐
            │ Host/Origin filter · /restart · /drain auth · /internal/rc-compatibility        │
            │ 64 MiB body buffer · relay (fresh TCP conn per request, 81 attempts × 50 ms)     │
            │ proxy recovery timer (5 s) · compat :80 listener                                │
            └───────┬──────────────────────────────────────────────┬──────────────────────────┘
          fork + IPC│ stdout/stderr → logs/shim.log               │ spawn (not detached) stdout/stderr → logs/proxy.log
                    ▼                                              ▼
       WORKER (serve-worker, shim.pid)                   claude-code-proxy serve --no-monitor (proxy.pid)
       127.0.0.1:<ephemeral> + gateway.sock              :18765  (GET /v1/models = health)
       exits on IPC disconnect
 CLI (start/ensure/recover/setup/stop) ── spawnDetached ──▶ SUPERVISOR   (POSIX: setsid child; Windows: WMI → launcher → child)
 CLI ── ps/lsof/netstat/powershell probes; lifecycle.lock; pid records
```

### 1.4 Stdio and logs

| Process | stdin | stdout+stderr | Cite |
|---|---|---|---|
| Supervisor (POSIX) | ignore | `logs/guardian.log`, opened `'a'`, fd closed in parent after spawn | process-supervision.js:962-963, 981-991 |
| Supervisor (Windows) | ignore | `logs/guardian.log` opened `'a'` by the launcher | windows-detached.js:71-74 |
| Worker | ignore | `logs/shim.log` (`'a'`), 4th slot `'ipc'` | commands.js:2618-2624 |
| Proxy | ignore | `logs/proxy.log` (`'a'`) | process-supervision.js:1017-1023 |
| Probe children | ignore | pipes captured | process-supervision.js:456 |

`LOGS = ~/.claude/model-gateway/logs` (runtime.js:17).

---

## 2. State files

`STATE = path.join(os.homedir(), '.claude', 'model-gateway')` — **home directory, not `CLAUDE_CONFIG_DIR`** (runtime.js:16; commands.js:80). Plugin registry lookup does honour `CLAUDE_CONFIG_DIR` / `CLAUDE_CODE_PLUGIN_CACHE_DIR` (runtime.js:15, 682-689). A port must keep these two roots distinct.

| Path (under STATE) | Format / schema | Writer, when | Readers | Mode | Atomic? | Cite |
|---|---|---|---|---|---|---|
| `guardian.pid` | decimal PID text, no newline | `spawnDetached` right after launch (sync) | `readPid` everywhere; readiness gate | default (umask) | no (`writeFileSync`) | process-supervision.js:61, 80-88, 970 |
| `guardian.pid.json` | `{pid:int, command:string\|null, startedAt:string\|null}` from `processInfoSync(pid)` | same | `readPidRecord` (valid only if `record.pid === guardian.pid`) | default | no | process-supervision.js:62, 72-88 |
| `shim.pid` / `shim.pid.json` | same schema | supervisor `startWorker` → `writePidRecordAsync('shim', child.pid)`: `.pid` immediately, `.pid.json` after async inspect, skipped if the supervisor stopped or the `.pid` changed meanwhile | stop paths, `recordedGatewayPids` | default | no | commands.js:2640; process-supervision.js:98-104 |
| `proxy.pid` / `proxy.pid.json` | same schema | supervisor `onStarted` after each proxy spawn | stop paths, `restartProxyForVersionChange` | default | no | commands.js:2507-2510 |
| removal of `<name>.pid` + `.pid.json` | — | `removePid(name, expectedPid?)` — skipped if `.pid` no longer holds `expectedPid`; worker exit (`removePid('shim', child.pid)`), stop paths, retire | — | — | two separate `rmSync` | process-supervision.js:63-68; commands.js:2652 |
| `worker-births/<pid>-<sha256hex(startedAt)>.json` | `{schemaVersion:1, pid, command, startedAt, installRoot, version, parent:{pid,command,startedAt}}` | supervisor, inside `writePidRecordAsync('shim')`, only if worker's `parentPid === process.pid`, parent identity readable, and worker command ends `\sserve-worker\s*$` | `reapOwnedWorkerBirths` | **0600** | no | process-supervision.js:660-711, 687-689 |
| `proxy-serving-version.txt` | `"<x.y.z>\n"` (proxy `--version` parsed) | supervisor on each proxy start (`currentProxyVersion()` = sync `proxy --version`); setup before a version-change restart writes the *previous* version | `restartProxyIfOutdated` (compares on-disk binary version vs this) | default | no | commands.js:87, 381-388, 404, 2508 |
| `gateway.sock` | Unix socket | worker listen; stale one unlinked only if connect → `ECONNREFUSED` **and** `lstat().isSocket()` | clients | OS default (UNREAD umask) | — | request-worker.js:1180-1206 |
| `control-token` | 64 lowercase hex + `\n` (`randomBytes(32)`) | `ensureControlToken()` by supervisor (commands.js:2458) and worker (request-worker.js:1209), created with `flag:'wx'`, EEXIST tolerated | CLI `controlRequestHeaders()`; servers | **0600** | create-exclusive | control-auth.js:9-33 |
| `shim-supervisor-failure.txt` | **JSON** despite `.txt`: `{supervisorPid, generation: $MODEL_GATEWAY_STARTUP_GENERATION, message}` | supervisor on bind failure / uncaught exception | `waitForStartupReadiness` → `startupFailureFor` (matches pid **and** generation) | default | yes (`writeFileAtomically`) | runtime.js:21; commands.js:614-625, 2485, 2893 |
| — removal | — | CLI before launch (commands.js:761); supervisor once listening (commands.js:2878) | — | — | — | — |
| `intentionally-stopped.json` | `{stoppedAt: ISO-8601}` | `stop` command (before stopping) | `startAll`, `automaticLifecycle`, setup, readiness gate | **0600** | no | lifecycle-lock.js:12, 96-108; commands.js:2952 |
| — removal | `unlink`, ENOENT ignored | `launchSupervisor` when `resumeIntent` (start, setup without `--preserve-wiring`) | — | — | — | commands.js:764 |
| `lifecycle.lock/` (directory) + `lifecycle.lock/<uuid>` file | `{token:uuid, pid, startedAt, host: os.hostname()}` | `acquireLifecycleLock` | `readLockOwner` | file 0600 | dir rename | §6 |
| `lifecycle.lock.candidate-XXXXXX/` | staging dir | `prepareLock` | — | — | — | lifecycle-lock.js:57-63 |
| `logs/lifecycle.jsonl` | §7 | everyone | doctor | default | append | lifecycle-diagnostics.js |
| `logs/{guardian,shim,proxy}.log` | free text | child stdio | humans | default | append | §1.4 |
| `launch-XXXXXX/{result.json,result.tmp}` (Windows) | `{pid}` or `{error}` | launcher (tmp + rename) | CLI | default | rename | windows-detached.js:30-31, 64-68 |
| `picker-guard.json` | §10 | CLI | CLI | default | `writeFileAtomically` | picker-guard.js:13, 138-147 |
| `bin/claude-code-proxy[.exe]`, `bin/…old-<ver>-<ms>` | binary | setup (rename swap) | supervisor | 0755 (POSIX) | rename | commands.js:355-379, 468 |

`atomic-file.js` (`writeFileAtomically`): temp `<target>.<pid>.<12 hex>.tmp`, `writeFileSync(mode)`, `rename` with retries on `EPERM|EACCES|EBUSY` after 20, 60, 140, 300 ms (sync sleep via `Atomics.wait`), temp unlinked in `finally`, then sweep siblings `<basename>.*.tmp` older than 1 h (atomic-file.js:7-59). No fsync.

---

## 3. Supervisor HTTP layer

Server settings (supervisor and worker identical): `requestTimeout=0`, `headersTimeout=120000`, `keepAliveTimeout=75000` (commands.js:2867-2874; request-worker.js:2680-2686).

### 3.1 Dispatch order in `handle(req,res,compatibilityListener)` (commands.js:2808-2859)

| # | Condition | Result |
|---|---|---|
| 1 | `supervisorRequestAllowed` fails | `403`, empty body |
| 2 | path (`req.url` before `?`) is `/restart` or `/drain` and `authenticatedControlRequest` fails | `403`, empty body |
| 3 | path is `/internal/rc-compatibility` and `controlRequestAllowed` fails | `404`, empty body |
| 3b | `/internal/rc-compatibility` allowed | §3.4 |
| 4 | path is `/restart` (authenticated) | §3.3 |
| 5 | anything else, **including authenticated `/drain`** | relay to worker (§3.5) |

The supervisor has no `/drain` handler: it authenticates the request and relays it; the worker re-checks the token against its own port (the relay's `Host` rewrite to `127.0.0.1:<workerPort>` makes the worker's Host check pass) and drains (request-worker.js:2364-2373). Effect: authenticated `POST :18764/drain` restarts the worker (the supervisor respawns it 50 ms after exit, commands.js:2653) without changing `workerScript`.

### 3.2 Host / Origin / token rules

| Check | Rule | Cite |
|---|---|---|
| `localRequestAllowed(req, hosts)` | `req.headers.origin === undefined` **and** `hosts.includes(req.headers.host)` — exact, case-sensitive string match | control-auth.js:41-43 |
| Supervisor allowed hosts | `127.0.0.1:<socket.localPort>`, `localhost:<localPort>`; on the compat listener also `api.anthropic.com`, `api.anthropic.com:<localPort>` | commands.js:2801-2806 |
| `authenticatedControlRequest(req, token, ports, compat)` | all of: not compat listener; `POST`; Host exactly `127.0.0.1:<port>` for a positive integer port in `ports` (supervisor passes `[req.socket.localPort]`); no Origin; `content-type === 'application/json'` exactly (a `; charset=…` suffix fails); server token matches `/^[a-f0-9]{64}$/`; header `x-model-gateway-control-token` is a string matching the same pattern; `crypto.timingSafeEqual` on the two buffers | control-auth.js:8, 10, 45-54 |
| Worker `/drain` | same function, `ports=[localPort]`, `compat = port80Bound && localPort === COMPAT_PORT`; over the Unix socket `localPort` is undefined → always 403 | request-worker.js:2365 |
| Worker ordinary requests | **no Host/Origin check** (grep for `headers.host`, `headers.origin`, `localRequestAllowed` in request-worker.js: no hits). A client on the ephemeral worker port or on `gateway.sock` bypasses the supervisor's filter. | grep result |
| Client side | `controlRequestHeaders()` returns `{x-model-gateway-control-token: token}` or `{}` if the file is missing (legacy supervisors); throws on a malformed token file | control-auth.js:12-21, 35-39 |

### 3.3 `/restart` (commands.js:2831-2856)

| Step | Behavior |
|---|---|
| Body | streamed; >4096 bytes → `413` empty (reading continues; `end` handler returns early) |
| Parse | JSON must be a non-array object, else `400 {ok:false, reason:'restart requires this CLI or a non-older sibling in the same plugin cache'}` |
| `script` | optional; if present (any value other than `undefined`, incl. `null`) must satisfy `canReplaceInstalledCliPath(workerScript, script)` else same 400 |
| Success | `restartWorker(script)` (async, not awaited) and `202 {ok:true, restarting:true}` |

`canReplaceInstalledCliPath(current, candidate)` (runtime.js:648-671): both strings, both absolute; both `realpath` resolvable; if equal lexically → realpaths must be equal; else all four paths (lexical and real) end `bin/model-gateway.js`; same cache root (`dirname³`) lexically **and** after realpath; both parent dirs parse as `X.Y.Z`; candidate's lexical and real version-dir basenames equal; candidate version ≥ current.

### 3.4 `/internal/rc-compatibility` (commands.js:2715-2799, 852-886)

| Item | Value |
|---|---|
| Gate (`controlRequestAllowed`) | not compat listener; POST; Host exactly `127.0.0.1:${PUBLIC_SHIM_PORT}` (the configured port, not `localPort`); no Origin; `content-type: application/json`; header `x-model-gateway-control: rc-compatibility`. **No token.** Failure → 404 |
| Body | read with the 64 MiB relay reader; bad JSON → `{}` |
| Request | `{action: 'probe'\|'reconcile', expectedSupervisorPid:int}`; pid ≠ `process.pid` → `{status:'unknown'}` |
| `probe` | serialized; if compat server listening and `port80Bound` → `bound`; else temp `net.Server` bind on `<hosts ip or 127.0.0.1>:COMPAT_PORT`, 2 s timer: listening → `bindable`; error → `unavailable` + `code`; timer → `unknown`/`ETIMEDOUT`; shutdown → `unknown`/`ESHUTDOWN` |
| `reconcile` | serialized; close compat server; re-read hosts; reset `compatState`; no hosts entry → `{status:'bound'}` (means "default mode reconciled"); else listen `<ip>:COMPAT_PORT` → `bound` / `unavailable`+`code` |
| Response | `200 {…result, supervisorPid}` |
| Client | `requestServingCompatibility` resolves the port owner (must be `same-install`, pid = expected), checks `/healthz` (`ok`, matching `supervisorPid`, boolean compat fields), short-circuits `probe` to `bound` when health says bound, POSTs with 3 s timeout, validates status set and `code` presence |

### 3.5 Relay (commands.js:2297-2454)

| Behavior | Value | Cite |
|---|---|---|
| Body buffer limit | 64 MiB (`> 64*1024*1024` → reject) | commands.js:2302 |
| Oversize reply | `413`, `connection: close`, `content-length: 0`, `x-should-retry: false`; flush headers, keep reading, `end()` on request end, force `destroy()` after 250 ms | commands.js:2285-2295 |
| Read error | `ECONNRESET`, `ERR_STREAM_PREMATURE_CLOSE`, `ABORT_ERR` → silent; other → `400` JSON `{type:'error', error:{type:'invalid_request_error', message:'model-gateway could not read the request body'}}` with `connection: close`, `x-should-retry: false` | commands.js:2273-2283, 2311-2323 |
| Attempts | `attempt(80)` → up to 81 tries, 50 ms apart (~4 s) | commands.js:2357-2389 |
| No worker port yet | wait; at exhaustion → 503 "could not deliver" (retryable) | commands.js:2367-2371 |
| Error before connect | retry (body never reached a worker) | commands.js:2382-2386 |
| Error after connect to worker W | never resent to W; keep waiting while `getWorker() === W`; resend only to a different worker; at exhaustion → reject with `bodyTakenByWorker` | commands.js:2353-2366 |
| Failure response | `503` JSON `{type:'error', error:{type:'api_error', message}}`; adds `x-should-retry: false` iff body was taken by a worker; lifecycle `worker-request-lost` | commands.js:2420-2441, 2668-2677 |
| Response relay | status + headers copied; `pipe`; upstream `error`/`aborted` → `res.destroy()` (client sees reset) | commands.js:2393-2418 |
| `/healthz` rewrite | worker JSON + `supervisorVersion=PLUGIN_VERSION`, `supervisorPid`, `proxyRecovery:true`, `compat={hostsDetected, hostsLine, port80Bound, reason}` | commands.js:2402-2414 |

Worker `/healthz` base fields (request-worker.js:2383-2406): `ok, version, models, catalog, served, draining, activeRequests, compat, compaction{…}, usage{…}, codexReadiness`. `shimNeedsRestart` treats a missing `proxyRecovery === true` as legacy (shim-state.js:67-72).

---

## 4. Lifecycle protocols

### 4.1 Start (`startAll` → `startAllLocked`, commands.js:807-839)

| Step | Action | Cite |
|---|---|---|
| 1 | Take lifecycle lock (re-entrant) | commands.js:808 |
| 2 | Refuse if `intentionally-stopped.json` exists and not `resumeIntent` | commands.js:830 |
| 3 | Refuse if proxy binary missing | commands.js:831 |
| 4 | `mkdirs` (STATE, logs, bin) | commands.js:832, 248 |
| 5 | `probeShimState` = `resolvePortOwner(18764)` ∥ `GET /healthz` (2 s): foreign-install → `running-foreign`; `health.ok` → `running-ours`; owner unknown → `running-foreign`; else `starting` (if same-install owner pid or a recorded guardian pid) or `stopped` | shim-state.js:28-42 |
| 6 | If `starting`: wait for readiness (timeout §4.2, failure file ignored), re-probe | commands.js:702-708 |
| 7 | Plan: `running-foreign` → refuse (foreign or unknown reason; unknown records `<op>-owner-unknown`); `running-ours` and not outdated → keep (reap birth-identified orphans beside it; `preserveRunningSupervisor` → wait for readiness); `starting` + `preserveRunningSupervisor` → wait; else replace | commands.js:710-743 |
| 8 | Replace: if `stopped` → `reapGatewayOrphans(null)`; else `stopRunningSupervisor` (§4.4) | commands.js:745-755 |
| 9 | Launch: new `startupGeneration = randomUUID()`; rm failure file; `spawnDetached('guardian', execPath, [resolveNewestInstalledCliPath(), 'serve-shim'], {MODEL_GATEWAY_STARTUP_GENERATION})`; if `resumeIntent` remove stop marker | commands.js:757-766 |
| 10 | Readiness wait; on success optional catalog refresh | commands.js:768-800 |

`resolveNewestInstalledCliPath` scans the sibling version directories of the current install (`<cache>/<mkt>/model-gateway/*/bin/model-gateway.js`) and picks the newest one accepted by `canReplaceInstalledCliPath` (runtime.js:749-764).

**Detached launch, POSIX** (process-supervision.js:981-991): `open(logs/guardian.log,'a')`; `spawn(command, args, {detached:true, stdio:['ignore',fd,fd], env:{...process.env,...env}, windowsHide:true})`; `'error'` → appended to log as `launch error: <msg>`; `unref()`; close fd; return `child.pid`. `spawnDetached` throws `"<name> launch produced no process id …"` when pid is not a positive integer (process-supervision.js:967-969), then writes the pid record synchronously (970).

**Detached launch, Windows**: §8.

**Supervisor boot** (`runShim`, commands.js:2456-2905): mkdirs → `ensureControlToken` → lifecycle `supervisor-started` → handlers (`exit`, `uncaughtExceptionMonitor`) → create proxy recovery → `listen(127.0.0.1:PUBLIC_SHIM_PORT)`. On listening: rm failure file, log line, `reconcileCompatibilityListener()` (awaited), `startWorker()`, `monitorProxy()` once, then `setInterval(monitorProxy, recoveryIntervalMs).unref()`. On listen error: `EADDRINUSE` → async port-owner probe; write failure file with remedy text naming `resolveStableCommandPath()`; `stopSupervisor(1)`.

### 4.2 Readiness wait (`waitForStartupReadiness`, commands.js:645-663)

| Item | Value |
|---|---|
| Poll | `GET 127.0.0.1:SHIM_PORT/healthz` 1000 ms timeout, status 200 = ready (process-supervision.js:54-59) |
| Failure short-circuit | failure file exists and its `supervisorPid` and `generation` match this launch → `{ok:false, reason}` |
| Interval | 300 ms |
| Timeout | quiet (hooks): 12000 ms; otherwise `max(12000, (CODEX_GATEWAY_DRAIN_TIMEOUT_MS‖30000) + 12000)` = 42000 by default (commands.js:91, 565-569) |
| On timeout | `processAlive(pid)` (`kill(pid,0)`; EPERM counts as alive) → "still starting … supervisor pid N is running" + `stillStarting`; else "not healthy after Ns (check logs in …)" (commands.js:772-794) |

### 4.3 Worker start / crash / replacement

| Event | Behavior | Cite |
|---|---|---|
| `startWorker` | no-op if stopped or a live worker exists; if port not configured: `workerPort=0`, arm 5000 ms "no port report" timer (`||=`) → `stopSupervisor(1)`; fork; lifecycle `worker-started`; accept first IPC message `{type:'listening', port}` (1..65535, same child) → `workerPort`; `writePidRecordAsync('shim')`; `child.unref()` | commands.js:2606-2641 |
| Worker exit | lifecycle `worker-exit` (exitCode, signal); clear `worker`; `removePid('shim', pid)`; if not stopped → `setTimeout(startWorker, 50)`. **No backoff, no crash counter.** | commands.js:2642-2654 |
| Worker IPC disconnect | worker `process.exit(0)` | request-worker.js:1210 |
| `restartWorker(script)` | adopt `script` if absolute and `canReplaceInstalledCliPath`; lifecycle `restart-worker-requested` (`drain`/`start`); no worker → start now; else `POST 127.0.0.1:<workerPort>/drain {timeout}` with token, client timeout `timeout+1000`, errors ignored; arm `timeout` (default 30000, `CODEX_GATEWAY_DRAIN_TIMEOUT_MS`) → if same worker still alive: log, lifecycle `restart-worker-stop-requested` (`drain-timeout`), `killPidAsync(pid,{probeChildren})` (ownership-checked); on worker `exit` → `startWorker()` | commands.js:2680-2713 |

The crash-path respawn (50 ms timer) and the restart-path `once('exit') → startWorker()` both fire on a drained worker; `startWorker` is idempotent (`worker && exitCode == null` guard), so only one new worker results (commands.js:2613, 2653, 2709-2712).

### 4.4 Drain (worker side, request-worker.js:1230-1260, 2364-2381)

| Step | Behavior |
|---|---|
| Accept | authenticated POST `/drain` → `draining=true`, `202 {ok:true, draining:true, activeRequests}`; after the response finishes, `setImmediate(beginDrain)` |
| During drain | every request except `/healthz` → `503 {type:'error', error:{type:'api_error', message:'model-gateway shim is restarting; retry this request shortly'}}` (no `x-should-retry`) |
| `beginDrain` | `server.close()` on every listener (TCP, socket, compat) + `closeIdleConnections()`; `process.exit(0)` when all close callbacks ran (immediately if no servers) |
| In-flight | counted per non-health request (`finish`/`close`); when it reaches 0 while draining → `closeAllConnections()` |
| Deadline | the worker has none; the supervisor's `timeout` timer kills it (§4.3) |

CLI-side drain helpers:

| Function | Used by | Behavior | Cite |
|---|---|---|---|
| `restartWorkerWithDrain` | setup (`restartAfterSetup`) | public port not listening → `{ok, running:false}`; owner foreign/unknown → refuse; unowned → not running; `POST :18764/restart {script: newest CLI}` (2 s): 202 → `{ok, draining}`; 404 → legacy, `stopShimWithDrain`; other → error | process-supervision.js:945-961; commands.js:430-437 |
| `stopShimWithDrain` | legacy upgrade | `SHIM_PORT` not listening → rm shim pid; owner checks; `POST :SHIM_PORT/drain {timeout}` expects 202, else force `stopProcessAsync('shim')`; wait `waitForShimExit(timeout)` (poll connect 100 ms every 50 ms); timeout → force stop | process-supervision.js:918-944 |

### 4.5 Stop

`stop` command (commands.js:2951-2960): lock → **write `intentionally-stopped.json` first** → `stopAll()` → failure prints reason, exit code 1.

`stopAll` (process-supervision.js:838-858):

| Step | Action |
|---|---|
| 1 | `resolvePortOwner(18764)`: `foreign-install` → refuse; `unknown` → refuse + lifecycle `stop-owner-unknown` |
| 2 | lifecycle `stop-{shim,supervisor,proxy}-stop-requested` for each recorded, verified pid |
| 3 | `stopProcessAsync` for `shim`, `guardian`, `proxy` in that order: verified record → `killPidAsync` (§5.4) → `waitForProcessExit(3000)` → `removePid(name, pid)` |
| 4 | if the port owner pid is still alive → `stopAndConfirmExit(owner.pid)` (ownership-checked) |
| 5 | `reapGatewayOrphans(null)` (sync SIGTERM to any remaining owned recorded pid) |

Ordering race: killing `shim` first makes the still-running supervisor respawn a worker 50 ms later (commands.js:2653) until the guardian stops; the guardian's own shutdown then disconnects/kills the new worker (commands.js:2587-2593). The newly respawned worker's `shim.pid` is written by the supervisor; `removePid('shim', pid)` with the old pid leaves it (process-supervision.js:216).

`stopRunningSupervisor` (process-supervision.js:859-888), used by replace/upgrade: owner checks as above (`<op>-owner-unknown`); lifecycle `<op>-supervisor-stop-requested`; stop the port owner pid (as `guardian` with record) or, if none, the recorded guardian; then require `waitForProcessExit(target,3000)` **and** `waitForShimExit(3000)`; `reapGatewayOrphans(null)`; return `siblingInstallRoot` when the stopped owner was same-plugin but a different root ("replaced older sibling shim version").

Supervisor shutdown (`stopSupervisor`, commands.js:2560-2598), once-only: `stopped=true`; lifecycle `supervisor-stop-requested` and `worker-stop-requested` when signalled; clear port-report timer and proxy interval; abort compat probe; `await proxyRecovery.stop()` (halt, kill probe children, await running recovery, stop the proxy *only if this supervisor spawned it*: trusted kill + `waitForPortRelease` 20×100 ms); await pending pid-record writes and compat actions; `worker.disconnect()`; wait 3000 ms for exit, else trusted `killPidAsync` + wait 3000 ms; `server.close()` compat and main; `process.exit(code)`.

### 4.6 Proxy health and recovery (`createProxyRecovery`, process-supervision.js:1027-1209)

| Parameter | Value | Cite |
|---|---|---|
| Tick | `max(1000, CODEX_GATEWAY_PROXY_RECOVERY_INTERVAL_MS ‖ 5000)` ms, timer unref'd; first tick right after listen | commands.js:2504, 2882-2884 |
| Probe | `GET http://127.0.0.1:PROXY_PORT/v1/models`, 2000 ms, `agent:false` (fresh connection, no pooling); healthy iff status 200 | process-supervision.js:993-997 |
| Backoff | `min(30000, 1000·2^(attempt-1))` → 1, 2, 4, 8, 16, 30, 30 … s; reset on any healthy probe | 1050-1051, 1102-1104 |
| Startup grace | 30000 ms after spawning a proxy: report `starting`, do nothing (proxy binds before model discovery) | 1052-1054, 1072-1074 |
| Single flight | concurrent `recover()` calls share one promise | 1083-1086, 1198 |

Recovery decision sequence (one tick):

| # | Condition | Outcome (`state`) / action |
|---|---|---|
| 1 | halted earlier | `foreign-port-owner` (permanent for this supervisor's lifetime) |
| 2 | probe OK | `healthy`, reset backoff |
| 3 | own proxy within grace | `starting` |
| 4 | `now < nextRestartAt` | `backing-off` |
| 5 | port listening and second fresh probe OK | `healthy` (the "fresh-connection confirmation") |
| 6 | — | `attempt++`, schedule `nextRestartAt`, lifecycle `proxy-recovery-started` (`attempt-N`) |
| 7 | binary missing | `binary-missing` |
| 8 | port listening, owner probe timed out | `owner-unknown` (never kills an unknown owner) |
| 9 | port listening, no owner pid found | `port-owned` |
| 10 | owner pid: `ownsProxy` timed out | `owner-unknown` |
| 11 | owner is not our proxy (command must contain PROXY_BIN **and** be a descendant of *this* supervisor) and process readable | `halted=true`, `foreign-port-owner` |
| 11b | not ours, process unreadable | `owner-unknown` |
| 12 | ours | lifecycle `proxy-stop-requested`; trusted kill; wait release 20×100 ms; still bound → `port-stuck` |
| 13 | port not listening | stop our previously spawned proxy, if any |
| 14 | spawn proxy; record `supervisedProxy` + start time; `onStarted(pid)` → write `proxy-serving-version.txt` + `proxy.pid(.json)`; lifecycle `proxy-started`; on `exit` → lifecycle `proxy-exit` |
| 15 | immediate probe OK → `recovered`; else `starting` |

Every terminal outcome writes `proxy-recovery-finished` with `outcome` ∈ {binary-missing, owner-unknown, port-owned, foreign-port-owner, port-stuck, recovered, starting}.

Consequence of rule 11: a proxy started by an earlier supervisor (reparented after that supervisor died) is adopted while healthy, but once unhealthy it is not a descendant of the current supervisor and recovery halts for good with "different install root (unknown)" (1036-1041, 1141-1145).

### 4.7 Version-upgrade takeover

| Situation | Behavior | Cite |
|---|---|---|
| Serving supervisor version < installed (or health lacks `proxyRecovery:true`) | `shimNeedsRestart` → replace: stop it, launch newest sibling CLI | shim-state.js:67-76; commands.js:710-713, 888-909 |
| Serving version > installed | kept; stderr notice "reload plugins" | commands.js:538-548, 725-733, 802-805 |
| Port owner is another install root of the **same plugin identity** | `same-install` → may be stopped and replaced | process-supervision.js:537-543, 884-887 |
| Port owner is a different plugin identity | `foreign-install` → refuse, never stopped | 588, 840-844, 861 |
| Same identity rule | `installRoot` normalized-equal to ours, or both roots are `<P>/cache/<marketplace>/model-gateway/<X.Y.Z>` with identical `…/model-gateway` dir, where `<P>` is a directory named `plugins` or a registry dir from `pluginRegistryDirectories()` | 515-543 |
| Worker-only upgrade | setup → `/restart {script}` with non-older sibling | §4.4 |
| Proxy binary upgrade | setup renames binary aside (`.old-<ver>-<ms>`), writes previous version to serving-version file, stops `proxy` by record, waits ≤7×100 ms for port release, lets the supervisor's recovery respawn it; `ensure` repeats this when on-disk ≠ serving version | commands.js:368-428, 571-575 |

Installs from different marketplaces have different `…/<marketplace>/model-gateway` roots and are therefore **foreign** to each other (inference from 525-536).

### 4.8 Orphan reaping

| Mode | Trigger | Behavior | Cite |
|---|---|---|---|
| `reapGatewayOrphans(null)` | stopped-state start, stopAll, stopRunningSupervisor | for guardian/shim/proxy: `recordedGatewayPid` (verifies/retires records) → sync `killPid` (ownership-checked, SIGTERM / taskkill /T /F) | 829-837 |
| `reapGatewayOrphans(supervisorPid)` | healthy same-install supervisor kept | birth-record reap: requires full process table containing the supervisor with identical birth; for each `worker-births/*.json` that is valid (schema, identities, `installRoot`/`version` strings, command ends `serve-worker`, same install, **recorded version strictly older than current and still equal to the version on disk in its installRoot**), and file name matches `<pid>-<sha256(startedAt)>.json`: if the worker is gone (null, or different `startedAt`) → delete the file; else if the live worker matches (same startedAt, command, ppid as table), its recorded parent is gone, and it is outside the supervisor's tree and a confirmed orphan (ancestry walk reaches ppid 0/null without cycles or unknown nodes; on Windows also "lost parent still recorded as ppid") → `SIGTERM` that single pid, then delete the file if it is gone | 660-810, 812-828 |

`probeWorkerIdentity`: `processInfoSync`; if null, `kill(pid,0)`: only `ESRCH` means gone (`null`); EPERM/other → `undefined` (unknown) (673-680).

---

## 5. Ownership proof

### 5.1 Identity record

Identity = `{pid, command, startedAt}` captured from the OS at record time (process-supervision.js:80-91). `startedAt` is compared only with `===`; it is never parsed.

### 5.2 OS commands

| Purpose | POSIX (macOS, Linux) | Windows | Cite |
|---|---|---|---|
| One process | `ps -ww -p <pid> -o pid=,ppid=,lstart=,args=` | `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "Get-CimInstance Win32_Process -Filter \"ProcessId = <pid>\" \| Select-Object ProcessId,ParentProcessId,CommandLine,CreationDate \| ConvertTo-Json -Compress"` | 307-313, 482-489 |
| Process table | `ps -ww -eo pid=,ppid=,lstart=,args=` | same CIM query without `-Filter` | 343-348, 490-499 |
| Port listener | `lsof -nP -iTCP:<port> -sTCP:LISTEN -t` → first non-zero pid; **sync only**: Linux fallback parses `/proc/net/tcp`,`/proc/net/tcp6` (field1 ends `:%04X` port, field3 `0A`=LISTEN, field9 inode) and scans `/proc/*/fd/*` for `socket:[inode]` | `netstat -ano -p tcp`, regex `^\s*TCP\s+\S*:<port>\s+(?:0\.0\.0\.0\|\[::\]):0\s.*\s(\d+)\s*$` (gim) — locale-independent: keys on the unconnected foreign address and trailing PID, not the localized state word | 242-290, 476-481 |
| Probe env | `{...process.env, LC_ALL:'C'}` (fixes `lstart` to `Www Mmm d hh:mm:ss yyyy`) | inherited | 236-238 |
| Width | `-ww` = unlimited; procps otherwise truncates `args` to `$COLUMNS` even on a pipe | — | test/gateway-ps-width.test.js:3-15 |

Parsing:

| Platform | Rule | Cite |
|---|---|---|
| POSIX | regex `^\s*(\d+)\s+(\d+)\s+(\w{3}\s+\w{3}\s+\d+\s+\d+:\d+:\d+\s+\d{4})\s+(.*)$` → pid, ppid, startedAt (the lstart text), command (rest of line). No fixed column widths. Table: same per line; ppid 0 → `null` ("proven root"), negative/non-integer → `undefined` | 304-305, 314-319, 333-341 |
| Windows | JSON object (single) or array (table); `ProcessId` required; `CommandLine` null → `''`; `CreationDate` → startedAt string as serialized; `ParentProcessId` 0 → null | 291-303, 320-332 |

EXT: PowerShell 5.1 `ConvertTo-Json` serializes `DateTime` as `"\/Date(ms)\/"` while PowerShell 7 emits ISO text; since only equality is used, the port must use the same shell/serializer for record and check, or normalize. The only fixture seen uses `CreationDate: 'birth'` (test/owned-worker-cleanup.test.js:308), so the real shape is UNREAD.

EXT: `netstat -p tcp` on Windows lists IPv4 TCP only (IPv6 needs `-p tcpv6`); the `[::]` alternative in the regex would then never match.

### 5.3 Probe execution and timeouts

| Item | Value | Cite |
|---|---|---|
| `CODEX_GATEWAY_PROBE_TIMEOUT_MS` | default 2000 (POSIX) / 8000 (Windows), floor 1 | 349-354 |
| Async child | `spawn(cmd, args, {detached: !WIN, windowsHide, env, stdio:['ignore','pipe','pipe']})`; on timeout: POSIX `kill(-pid,SIGTERM)` (group; falls back to the child), wait ≤250 ms for close, then `SIGKILL` group; Windows `taskkill /pid N /T /F`; result `{status:null, timedOut:true}` | 391-402, 436-475 |
| Registry | `createProbeChildRegistry`: probes run **one at a time** (promise queue); `stop()` kills all tracked children and makes queued probes return `timedOut`; `stopSync()` (from `exit`) sends SIGTERM / taskkill only | 408-435 |
| Sync probes | `spawnSync` with **no timeout** (processInfoSync, processTableSync, lsof/netstat sync) | 239-241 |
| Exit wait | `waitForProcessExit(pid, ms)`: `processInfoSync` every 50 ms (on Windows each poll is a PowerShell launch, measured ~1.8-2.4 s per the comment at 350-351) | 902-909 |
| Lock identity | `processInfoAsync(pid, {timeout: 8000})` | lifecycle-lock.js:76 |
| Async return | `undefined` = timed out (unknown), `null` = query ran and found nothing / non-zero exit | 480, 487-488, 497 |

### 5.4 Ownership predicates

`processIsOwnedByThisInstall(pid, {record, name})` (sync, 149-159):

| # | Condition | Result |
|---|---|---|
| 1 | process not found | false |
| 2 | command empty (protected/elevated process) and record matches (same pid and same `startedAt`) | true |
| 3 | command contains a `…model-gateway[/X.Y.Z]/bin/model-gateway.js` path whose install root is the same plugin identity (§4.7) | true iff no record or record matches (`command ===` **or** `startedAt ===`) |
| 4 | `name==='proxy'` and command contains `PROXY_BIN` (normalized: realpath, `/` separators, lower-cased) | true iff record matches, or record absent/empty-identity, **or the process descends from the recorded, owned guardian** (sync table walk) |
| 5 | otherwise | false |

The async variant (636-645) has rules 1-4 except the guardian-ancestry arm, and returns `undefined` when the probe timed out. Install-root extraction regex: 510-514 (double-quoted, single-quoted, or bare token; case-insensitive; root = two directories above `model-gateway.js`).

`recordedGatewayPid(name)` (160-186):

| Observation | Action | Return |
|---|---|---|
| no `.pid` | — | null |
| process gone (inspect null) | retire record (rm both files, log "PID N is gone") | null |
| owned (table above) | rewrite `.pid.json` if missing | pid |
| not owned by record but command is this install (PID reused by another of our processes) | rewrite record | pid |
| foreign process on the PID (reused) | retire record ("no longer belongs to this gateway: <command>") | null |

`resolvePortOwner(port)` (550-597), budget = probe timeout shared by both attempts:

| Observation | State |
|---|---|
| owner probe timed out / budget exhausted | `unknown` (`timeout`) |
| no pid and port not listening | `unowned` |
| no pid but port listening | `unknown` (`unidentified`) |
| pid, process vanished, port free | `unowned` |
| pid, command has no model-gateway.js; command empty and pid is a verified recorded pid | `same-install` (`identity:'pid-record'`) |
| … command empty, not recorded | `unknown` (`unreadable-command`) |
| … command non-empty | `unknown` (`unidentified`) |
| command names an install root | `same-install` or `foreign-install` |

Retry: if the first result is `unknown` and budget remains, probe again; accept `foreign-install`/`unowned`, or `same-install` with the same pid; otherwise `unknown`. Refusal texts: 603-614 (timeout text names `CODEX_GATEWAY_PROBE_TIMEOUT_MS`).

Async port probes have **no `/proc` fallback** (476-481 vs 280-284): on Linux without `lsof`, a live listener resolves to `unknown` and start/stop refuse.

### 5.5 Kill primitives

| Function | POSIX | Windows | Cite |
|---|---|---|---|
| `terminateProcess` | `kill(pid,SIGTERM)` | `taskkill /pid N /T /F` (sync) | 193-196 |
| `forceTerminateProcess` | `SIGKILL` | same taskkill | 197-200 |
| `killPid` (sync) | ownership check → terminate | same | 201-204 |
| `killPidAsync` | ownership (unless `trusted`) → SIGTERM → wait 3000 ms → re-check ownership → SIGKILL | ownership → async taskkill /T /F | 646-659 |
| `stopAndConfirmExit` | kill, then success = process gone within 3000 ms (kill result ignored; taskkill exits non-zero when a tree member vanishes, GH-371) | same | 219-224 |
| `signalOrphanWorker` | `SIGTERM` single pid (no tree) | Node `process.kill` = TerminateProcess (comment) | 770-772 |

---

## 6. Lifecycle lock (lifecycle-lock.js)

| Item | Value | Cite |
|---|---|---|
| Path | directory `~/.claude/model-gateway/lifecycle.lock` | 13 |
| Owner file | `lifecycle.lock/<token>` where token = `crypto.randomUUID()`; JSON `{token, pid, startedAt, host}`; mode 0600 | 57-63 |
| Prepare | `mkdir -p` parent; `mkdtemp("lifecycle.lock.candidate-")`; write owner file inside | 57-63 |
| Acquire | `rename(candidateDir, lockDir)`; `EEXIST`/`ENOTEMPTY`/`EPERM`/`EACCES` = busy; other errors throw | 49-55 |
| Identity | `startedAt` of the caller from `processInfoAsync(process.pid, {timeout:8000})`; missing → throw "could not establish lifecycle lock process identity; run doctor" | 75-78 |
| Poll | every 50 ms until `timeout` (default 15000 ms) → throw "gateway lifecycle is busy or ownership is unknown (<dir>); retry recover or run doctor" | 65-73 |
| Owner read | lock dir must contain exactly one file whose name equals its `token`, else owner unknown | 15-23 |
| Stale rule | owner on same `os.hostname()`, positive integer pid, string `startedAt`; dead iff `kill(pid,0)` → ESRCH, or a readable current `startedAt` differs. Other hosts / unreadable identity → never stale | 25-41 |
| Break stale | unlink `<lock>/<token>` then `rmdir` (only if empty) | 43-47, 69 |
| Release | same `removeOwnedDirectory(lockDir, token)` | 82 |
| Cleanup | candidate dir removed in `finally` (if rename failed it still holds the file) | 83-85 |
| Re-entrancy | `AsyncLocalStorage` store `{active:true}`; nested `withLifecycleLock` in the same async context runs directly | 11, 88-94 |
| Takers | `setup`, `start`, `stop`, `ensure` (via `automaticLifecycle`), every `startAll` (so `recover`/readiness gate too) | commands.js:579, 808, 2936, 2944, 2951, 3044 |
| Not locked | the supervisor process itself; `remote-control` except its `startAll`; `doctor`/`status` | grep |

EXT: on POSIX `rename(dir, existingEmptyDir)` succeeds (replaces it); only a non-empty target fails with ENOTEMPTY/EEXIST. So an empty leftover `lifecycle.lock/` does not block acquisition. On Windows `MoveFileEx` onto an existing directory fails with access denied, which this code maps to busy (EPERM/EACCES) — an empty leftover dir there would block until timeout.

Intentional-stop marker functions live in the same module (96-108, see §2).

---

## 7. Lifecycle log (`logs/lifecycle.jsonl`, lib/lifecycle-diagnostics.js)

| Item | Value | Cite |
|---|---|---|
| Path | `~/.claude/model-gateway/logs/lifecycle.jsonl` (no env override) | runtime.js:37 |
| Write | `mkdir -p`; trim; `appendFileSync(JSON + '\n')`; every error swallowed | 36-43 |
| Bound | before each append: if the file has ≥ 200 non-empty lines, rewrite it with the last 199 (non-atomic `writeFileSync`); so the file holds at most 200 records | 7, 29-34 |
| Concurrency | no lock; supervisor and CLIs append concurrently; a concurrent trim can drop records (inference) | — |
| Read | `readLifecycleRecords` skips unparsable lines; doctor shows the last `supervisor-exit`/`worker-exit`/`proxy-exit` and last `ensure-hook-wait-cut-short` | 45-59; commands.js:1430, 1438 |

Record schema (`lifecycleRecord`, 13-27): always `at` (ISO-8601 UTC), `event`, `component`, `pid` (keys present even if undefined → dropped by JSON); optional `startedAt`, `outcome`, `errorType` (when non-null); `signal`, `exitCode` (when not `undefined`, so `null` is kept); `child: {component, pid}` (when truthy). **Any other field is dropped** — e.g. `supervisorPid` passed at commands.js:692 never reaches the file.

Event catalogue:

| Event | component | Extra fields | Emitted at |
|---|---|---|---|
| `supervisor-started` | supervisor | startedAt | commands.js:2462 |
| `supervisor-exit` | supervisor | startedAt, exitCode | commands.js:2469 |
| `supervisor-uncaught-failure` | supervisor | startedAt, outcome=origin, errorType | commands.js:2478 |
| `supervisor-stop-requested` | supervisor | startedAt, signal (SIGTERM/SIGINT/fixture-shutdown) | commands.js:2565 |
| `worker-stop-requested` | supervisor | child worker, signal (TASKKILL on Windows) | commands.js:2572 |
| `worker-started` | supervisor | startedAt, child | commands.js:2626 |
| `worker-exit` | supervisor | startedAt, child, exitCode, signal | commands.js:2643 |
| `worker-request-lost` | supervisor | startedAt, child?, outcome=response-lost, errorType=code/name | commands.js:2669 |
| `restart-worker-requested` | supervisor | child?, outcome drain/start | commands.js:2683 |
| `restart-worker-stop-requested` | supervisor | child, signal, outcome=drain-timeout | commands.js:2699 |
| `proxy-recovery-started` | supervisor | outcome=`attempt-N` | process-supervision.js:1105 |
| `proxy-recovery-finished` | supervisor | outcome (binary-missing, owner-unknown, port-owned, foreign-port-owner, port-stuck, recovered, starting) | 1113-1195 |
| `proxy-stop-requested` | supervisor | child proxy, signal | 1147 |
| `proxy-started` / `proxy-exit` | supervisor | child; exit adds exitCode, signal | 1170, 1178 |
| `stop-{shim,supervisor,proxy}-stop-requested` | controller | child, signal | 225-235, 851 |
| `stop-owner-unknown` | stop | outcome=owner-unknown | 848 |
| `<op>-owner-unknown` (op ∈ restart, ensure, recover, start, setup) | op | outcome | 864; commands.js:721 |
| `<op>-supervisor-stop-requested` | controller | child supervisor, signal | 870 |
| `<op>-recovery-started` / `<op>-recovery-finished` | op | startedAt / outcome ready·starting·failed | commands.js:674-698 |
| `ensure-hook-wait-cut-short` | SessionStart | outcome=supervisor-started | commands.js:2989 |

---

## 8. Windows-specific behavior (lib/windows-detached.js, full)

Why: WMI creates the launcher outside the calling hook/terminal's process tree and Job Object; Node `detached:true` alone does not escape Windows job cleanup (5-8).

Parent side, `spawnWindowsDetached(command, args, {env, logPath, state})` (29-57), called as `spawnWindowsDetached(execPath, [cli,'serve-shim'], {env:{...process.env, ...extra}, logPath: logs/guardian.log, state: STATE})` (process-supervision.js:974-977):

| Step | Detail |
|---|---|
| 1 | `mkdtemp(STATE/launch-)`; `resultPath = <dir>/result.json` |
| 2 | argv payload = base64(JSON `{command, args, logPath, resultPath}`) — launch metadata only |
| 3 | stdin payload = base64(JSON `{commandLine, cwd: process.cwd(), environment: ["K=V", …]}`) with `commandLine = quote(process.execPath) quote(__filename) quote(payload)`; env entries with `null`/`undefined` values dropped. Environment never appears on a command line or in a temp file (32-33) |
| 4 | `spawnSync('powershell.exe', ['-NoProfile','-NonInteractive','-Command', CREATE_PROCESS], {input, encoding:'utf8', windowsHide:true, timeout:15000})`; `error` or non-zero → throw `Windows detached launch failed: <error.message or stderr>` |
| 5 | busy-wait (`Atomics.wait` 25 ms slices) up to 5000 ms for `result.json`; missing → throw "did not acknowledge startup" |
| 6 | parse ack: `{pid}` → return pid; else throw `Windows detached launch failed: <ack.error>` |
| 7 | `finally`: unlink `result.json`, `result.tmp`; `rmdir` the dir (signals cancellation to a late launcher) |

PowerShell script `CREATE_PROCESS` (9-23): `$ErrorActionPreference='Stop'`; decode stdin base64 → UTF-8 → `ConvertFrom-Json`; `New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly` with `ShowWindow=[uint16]0`, `CreateFlags=[uint32]16778240`, `EnvironmentVariables=[string[]]environment`; `Invoke-CimMethod Win32_Process Create {CommandLine, CurrentDirectory, ProcessStartupInformation}`; `ReturnValue ≠ 0` → throw `WMI process creation failed: <n>`.

EXT decoding: `16778240 = 0x01000400 = CREATE_BREAKAWAY_FROM_JOB (0x01000000) | CREATE_UNICODE_ENVIRONMENT (0x400)`; `ShowWindow 0 = SW_HIDE`. The `EnvironmentVariables` array replaces (does not extend) the WMI provider's environment.

`quoteArgument` (25-27): wrap in `"`; double backslashes preceding a `"` and escape the quote; double trailing backslashes (MSVCRT `CommandLineToArgvW` rules).

Launcher side (`require.main === module`, 59-82):

| Step | Detail |
|---|---|
| 1 | decode `argv[2]`; if `dirname(resultPath)` no longer exists → `process.exit(1)` (caller timed out; do not start an unacknowledged service) |
| 2 | `open(logPath,'a')`; `spawn(command, args, {detached:true, stdio:['ignore',fd,fd], env: process.env, windowsHide:true})` |
| 3 | `'spawn'` → ack `{pid}` (write `result.tmp`, rename to `result.json`), then `unref()`; ack failure → `child.kill()` |
| 4 | `'error'` or sync throw → ack `{error: message}` |
| 5 | close fd; launcher exits when its loop drains |

Other Windows branches in the slice:

| Concern | Windows behavior | Cite |
|---|---|---|
| Stop | always `taskkill /pid N /T /F`; no graceful stop (measured: `/T` without `/F` exits 255 and the process keeps running) | process-supervision.js:190-200, 650-653 |
| Orphan worker | `process.kill(pid,'SIGTERM')` single process, never `/T` | 770-772 |
| Probes | PowerShell CIM (§5.2), not detached ("Windows detached children lose PowerShell probe output") | 455-456 |
| Lost parent | worker whose recorded parent died still shows that ppid → counted as outside the supervisor | 755-757 |
| Probe budget | 8000 ms default | 349-354 |
| Socket | named pipe `\\.\pipe\model-gateway` | runtime.js:47-49 |
| Hosts | `%SystemRoot%\System32\drivers\etc\hosts` | remote-control.js:10-15 |
| Port-80 owner names | `tasklist /FI "PID eq N" /FO CSV /NH`, first CSV field | commands.js:278-286 |
| `claude` probe | `shell:true` when `CLAUDE_BIN` ends `.cmd`/`.bat`; not detached | picker-guard.js:35 |

---

## 9. Remote Control compatibility (lib/remote-control.js, full)

Constants (runtime.js:44-53): `COMPAT_HOST='api.anthropic.com'`, `COMPAT_PORT=80` (`CODEX_GATEWAY_COMPAT_PORT`), `COMPAT_BASE_URL='http://api.anthropic.com'`, block start `# >>> model-gateway RC compatibility >>>`, end `# <<< model-gateway RC compatibility <<<`, line `127.0.0.1 api.anthropic.com`.

| Item | Rule | Cite |
|---|---|---|
| Hosts path | `CODEX_GATEWAY_HOSTS_FILE` if set; Windows `%SystemRoot%‖C:\Windows\System32\drivers\etc\hosts`; else `/etc/hosts` | 10-15 |
| Entry match | per line: strip `#…`, trim, split on whitespace; IP must be exactly `127.0.0.1` or `::1`; any name equal (case-insensitive, trailing dot stripped) to `api.anthropic.com` → `{ip, line}` | 8, 23-34 |
| Block state | `absent` (neither marker), `partial` (one missing or end before start), `valid` (block contains a loopback entry), `invalid` | 36-47 |
| Conflicts | lines (outside marker lines) mapping the host to a non-loopback IP | 89-101 |
| Add | valid → unchanged; partial/invalid → throw; else adopt the first unmarked loopback line by wrapping it in markers; else append block (EOL = CRLF if the file contains any CRLF; add separator if file lacks trailing newline) | 69-78, 53-67, 49-51 |
| Remove | absent → unchanged; not valid → throw; cut block, drop one leading newline after it, collapse ≥3 consecutive newlines to 2 | 80-87 |
| Write check | `access(W_OK)` → "available" or "missing (run from an elevated terminal)" | 109-114 |

`remote-control <enable|disable|doctor>` (`remoteControlCommand`, 137-269); dependencies injected from commands.js:309.

| Phase | enable | disable |
|---|---|---|
| Refuse first | effective process `ANTHROPIC_BASE_URL` unsafe (`unsafeRemoteControlProcessEnv`, settings-wiring lane, UNREAD) → die | — |
| Read | hosts unreadable → die; any conflict → die | same |
| Transform | `addManagedHostsBlock` (errors → die); unchanged → "already enabled" | `removeManagedHostsBlock` |
| Preview | print the exact block; without `--confirm`: print elevation hint if not writable, ask to re-run with `--confirm`, return | same |
| Precheck | `requestServingCompatibility(probe)` must be `bound` or `bindable` else die (with port-80 holder diagnostic from `lsof`/`netstat` + `ps -o comm=`/`tasklist`, Docker Desktop named) | — |
| Race guard | re-read file; bytes differ from the first read → die, untouched | same |
| Write | backup `<hosts>.model-gateway-<ISO with :/. → ->.bak` via `copyFileSync`; `writeFileSync(hosts)` (not atomic); failure → die naming backup | same |
| Activate | `reconcile` with expected supervisor pid → must be `bound`; `remoteControlVerify(requireCompatibility:true)`; `syncCompatMode({requiredMode:'compat'})` must return mode `compat` | `startAll()` ok; `reconcile` must be `bound`; verify with `requireCompatibility:false`; `syncCompatMode()` |
| Rollback (enable only) | if the file still equals what this command wrote → restore original bytes; else keep and point to backup; then `reconcile` again and report; finally die "hosts file changed, but gateway reconciliation failed" | disable: no rollback, die with backup name |

`remoteControlVerify` (271-292): ready iff `health.ok`, supervisor pid matches (when expected), and (compat) hosts entry present ∧ `compat.hostsDetected` ∧ `compat.port80Bound`, or (default) all three false. Prints DNS lookup of the host, entry, port state, health, model count; for compat it also states Remote Control end-to-end is not verified (session creation uses HTTPS while the transport is HTTP).

`doctor` (157-176): prints hosts path, block state (notes an adoptable unmarked entry), entry, conflicts, write access, serving-compat status from a `probe`, port-80 diagnostic, DNS lookup, then the general doctor. Read-only except the in-supervisor bind probe.

Port-80 bindability check = the supervisor's temp `net.Server` bind on `<hosts ip or 127.0.0.1>:80` (commands.js:2728-2754), 2 s timeout. The plugin never adds the block without `--confirm` and the comment at request-worker.js:2703-2704 says the entry is "never written by this plugin" — contradicted by the enable path, which does write it after confirmation (remote-control.js:219).

---

## 10. picker-guard.js

Purpose: verify that Claude Code's `/model` Fable/Opus/Sonnet rows behind the gateway resolve to the same models as without it, without sending a model request (11-12).

| Item | Value | Cite |
|---|---|---|
| Probe | `spawn(CLAUDE_BIN, ['-p','--input-format','stream-json','--output-format','stream-json','--verbose','--settings','{"disableAllHooks":true}'], {cwd: tmpdir, env, stdio:['pipe','pipe','ignore'], windowsHide:true, detached:!WIN, shell: WIN && .cmd/.bat})`; `CLAUDE_BIN = CODEX_GATEWAY_CLAUDE_BIN ‖ 'claude'` | 21, 32-36; runtime.js:549 |
| Request | stdin line `{"type":"control_request","request_id":"picker-guard","request":{"subtype":"initialize"}}` | 18, 55 |
| Response | first stdout JSON line with `type:'control_response'` → `response.response.models` | 23-30 |
| Kill | `terminateProbe`: SIGTERM, after 250 ms SIGKILL to group (POSIX) or `taskkill /t /f` (Windows) | pins.js:185-195 |
| Timeout | 20000 ms default; 8000 ms from `ensure` | 14; commands.js:1395, 1113 |
| Envs | native = base env minus every key of the gateway env block and minus `CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_SSE_PORT`, `CLAUDE_CODE_SESSION_ID`; gateway = native + `envBlockFor('default')` (`ANTHROPIC_BASE_URL=http://127.0.0.1:18764` + gateway block) | 17, 90-99; pins.js:472-474 |
| Run | both probes in parallel in one `mkdtemp(os.tmpdir()/model-gateway-picker-)`, removed afterwards with 20 retries × 100 ms | 94-107 |
| Compare | per family (fable, opus, sonnet): native row with `value===family` or first non-"(previous)" row whose first display word is the family; expected = its `resolvedModel` minus `[1m]`; actual = same lookup in gateway rows; mismatch → failure. Also lists "(previous)" rows and counts gateway model ids | 59-88 |
| Cache | `STATE/picker-guard.json` = `{cliVersion, checkedAt: ms, ...result}`, pretty JSON + newline, written atomically; reused while `cliVersion` unchanged (unverified results cached too) | 13, 131-147 |
| Callers | `ensure` (wired) via `reportStalePickerRowsOncePerCli`; `doctor` runs uncached | commands.js:1111-1117, 1428, 3022 |

---

## 11. Signals and exit paths

| Process | Trigger | Handler / cleanup | Exit code | Cite |
|---|---|---|---|---|
| Supervisor | `SIGTERM` (once) | `stopSupervisor(0,'SIGTERM')` (§4.5) | 0 | commands.js:2898 |
| Supervisor | `SIGINT` (once) | `stopSupervisor(0,'SIGINT')` | 0 | 2899 |
| Supervisor | IPC `{type:'fixture-shutdown'}` (only if it has an IPC channel, i.e. test harness) | `stopSupervisor(0,'fixture-shutdown')` | 0 | 2900-2904 |
| Supervisor | main listen error | failure file, `stopSupervisor(1)` | 1 | 2886-2897 |
| Supervisor | no worker port within 5 s | `stopSupervisor(1)` | 1 | 2606-2610 |
| Supervisor | `uncaughtExceptionMonitor` | lifecycle `supervisor-uncaught-failure`; failure file; then Node's default crash | EXT: 1 | 2476-2486 |
| Supervisor | `exit` | `proxyRecovery.stopSync()` (SIGTERM probe groups / taskkill); lifecycle `supervisor-exit` | — | 2467-2475 |
| Supervisor | SIGHUP / SIGKILL | no handler; with SIGKILL the worker exits on IPC disconnect, the proxy is left running (not detached from the supervisor's group, no parent-death hook) | — | grep; request-worker.js:1210 |
| Worker | IPC `disconnect` | `process.exit(0)` — no drain | 0 | request-worker.js:1210 |
| Worker | drain complete | `process.exit(0)` | 0 | 1237, 1241 |
| Worker | SIGTERM | no handler in the read ranges → Node default termination (UNREAD elsewhere in request-worker.js beyond grep, which found none) | — | grep |
| CLI | `die(msg, code)` | flush buffered hook lines, stderr `model-gateway: msg` | code ‖ 1 | commands.js:241 |
| CLI | `ensure` finish | flush; `--quiet` always 0 (hook contract), else code | 0 / code | 2968 |
| CLI | `stop` failure | `exitCode=1` | 1 | 2954-2957 |
| CLI | unknown command | usage | 1 (0 if none) | bin/model-gateway.js:7-9 |
| CLI | thrown error | stderr `model-gateway: <message>` | 1 | bin/model-gateway.js:13-16 |
| Readiness gate | recovery throws | guidance on stderr | 2 for `UserPromptSubmit`, else 1 | readiness-gate.js:57-62 |
| Stable launcher (gate mode) | child exceeds 25 s | `SIGKILL` child (spawnSync `timeout`, `killSignal`) | 2 | hooks/registry-writer.js:106-110 |
| Windows launcher | result dir gone | exit before spawning | 1 | windows-detached.js:63 |

---

## 12. Node-specific constructs and what a Go port must reproduce

| Node construct | Where | Semantics relied on | Go equivalent / note |
|---|---|---|---|
| `spawn(..., {detached:true})` + `unref()` (supervisor) | process-supervision.js:984-986 | new session + process group (POSIX `setsid`); parent may exit without waiting | `exec.Cmd` with `SysProcAttr{Setsid:true}`; `Start()` then `Process.Release()` (never `Wait`). Redirect stdout/stderr to the opened log file; stdin `/dev/null` |
| Deferred spawn `'error'` event | 979-986 | ENOENT etc. arrive later; pid may be `undefined` → caller throws "produced no process id" | `Start()` returns the error synchronously; write it to the launch log to keep the same diagnostics |
| `fork(script, ['serve-worker'], {stdio:[ignore,fd,fd,'ipc']})` | commands.js:2619-2623 | IPC channel used for (a) port report message, (b) `disconnect()` → worker exits | No handle passing needed. Use an inherited pipe (`ExtraFiles`) on which the worker writes its port; worker exits on EOF of that pipe (covers supervisor crash too). Linux could add `Pdeathsig`; macOS has none, so the pipe is the portable mechanism. Or re-exec the same Go binary with a `serve-worker` subcommand |
| `child.unref()` on worker | 2641 | worker does not keep the supervisor's event loop alive | irrelevant in Go; keep a goroutine on `Wait()` to observe exit (needed for respawn and `worker-exit` record) |
| Proxy spawn without `detached` | 1019-1021 | shares supervisor's process group; survives supervisor SIGKILL | same default in Go; decide explicitly whether to set `Pdeathsig`/group-kill (current behavior: proxy survives) |
| Probe `detached:!WIN` + `kill(-pid)` | 395-397, 456 | kill whole probe tree via its process group | `SysProcAttr{Setpgid:true}`, `syscall.Kill(-pid, SIGTERM)`, then SIGKILL after 250 ms; or `exec.CommandContext` with `Cancel`/`WaitDelay` |
| `spawnSync` without timeout | 239-241 | blocks the event loop until the probe exits | Go: use `CommandContext` with a timeout anyway; blocking a goroutine is fine but an unbounded hang is a latent bug |
| `setTimeout(...).unref()` | 384, 473, 2289-2290, 2884 | timer does not keep process alive | Go processes exit when `main` returns; model the supervisor's lifetime explicitly (context cancel) |
| `setInterval` proxy recovery | commands.js:2883 | 5 s tick, single-flight promise | `time.Ticker` + mutex/`singleflight` |
| Promise queue probe registry | 408-435 | probes serialized one at a time; tracked children killed on stop | `chan struct{}` semaphore of 1 + set of `*exec.Cmd` |
| `AsyncLocalStorage` lock re-entrancy | lifecycle-lock.js:11, 88-94 | nested lock calls in the same logical call chain skip acquisition | pass a lock token in `context.Context`, or acquire once at the command entry |
| `Atomics.wait` sync sleep | atomic-file.js:24; windows-detached.js:46 | blocking sleep | `time.Sleep` |
| `fs.renameSync` for lock / atomic writes | lifecycle-lock.js:50; atomic-file.js:19 | dir rename as mutex; file replace | `os.Rename`; Windows replace-on-open retries (EPERM/EACCES/EBUSY → 20/60/140/300 ms) must be kept |
| `crypto.timingSafeEqual` | control-auth.js:53 | constant-time compare of equal-length tokens | `crypto/subtle.ConstantTimeCompare` |
| `http.Agent({keepAlive:false})` | commands.js:2499 | one TCP connection per relayed request, so a failed write is unambiguous | `http.Transport{DisableKeepAlives:true}` or raw `net.Dial` per request |
| `agent:false` on `/v1/models` | process-supervision.js:995 | fresh connection per probe | same as above |
| `server.closeIdleConnections()` / `closeAllConnections()` | request-worker.js:1243, 1255 | drain semantics | `http.Server.Shutdown(ctx)` closes idle conns and waits for active; force with `Close()` |
| `requestTimeout=0`, `headersTimeout=120s`, `keepAliveTimeout=75s` | commands.js:2869-2871 | long streaming responses allowed | `ReadHeaderTimeout:120s`, `IdleTimeout:75s`, no `ReadTimeout`/`WriteTimeout` |
| `req.socket.localPort` in auth | commands.js:2802, 2816 | Host must match the listener actually hit | `http.LocalAddrContextKey` from the request context |
| `process.kill(pid, 0)` ESRCH/EPERM | lifecycle-lock.js:25-28; process-supervision.js:678; commands.js:786-794 | liveness without permission | `syscall.Kill(pid, 0)`; on Windows `OpenProcess` + exit code (Node maps differently; behavior there is UNREAD) |
| `fs.realpathSync.native` | process-supervision.js:505 | case/8.3/symlink normalization for path equality | `filepath.EvalSymlinks` (does not normalize Windows case/8.3; port must add `GetFinalPathNameByHandle` or lower-casing as done here) |
| `process.execPath` | commands.js:763 | the node binary re-runs the CLI script | Go: `os.Executable()`; the "newest sibling install" selection then targets a binary path, not `bin/model-gateway.js` — the install-root regex (process-supervision.js:511) and `serve-worker` suffix rule (688) must be redefined for the Go binary name |

---

## 13. Open questions, ambiguities, dead code

| # | Observation | Cite |
|---|---|---|
| 1 | `start` against an already-serving current shim never removes `intentionally-stopped.json`: `resumeIntentionalStop` runs only in `launchSupervisor`. After a partly failed `stop`, `start` can leave the marker, and `ensure`/readiness gate keep refusing. Intended? | commands.js:725-733, 764, 830, 2952 |
| 2 | `stop` writes the stop marker before `stopAll`; if `stopAll` refuses (foreign/unknown owner), the marker stays and automatic recovery is disabled although nothing was stopped | commands.js:2952-2958 |
| 3 | Worker-birth files for current-version workers are never valid (`workerBirthVersionStale` requires a strictly older recorded version) and are therefore never retired; one file per worker start accumulates until a newer plugin version runs the reap | process-supervision.js:725-740, 776-779, 700-711 |
| 4 | `lifecycleRecord` drops `supervisorPid` (and any unlisted field) | lifecycle-diagnostics.js:13-27; commands.js:692 |
| 5 | `shim-supervisor-failure.txt` holds JSON | commands.js:623-625 |
| 6 | Worker crash respawn has no backoff (50 ms) — a worker that crashes at boot loops at ~20 Hz, each writing pid records, birth files and two lifecycle records (the log stays bounded at 200 lines, the birth dir does not) | commands.js:2642-2654 |
| 7 | Async port-owner probe lacks the Linux `/proc` fallback that the sync one has; without `lsof`, start/stop/restart refuse with `unknown` | process-supervision.js:280-284, 476-481 |
| 8 | Async ownership lacks the guardian-ancestry rule for the proxy (sync has it). `stopProcessAsync('proxy')` therefore refuses a proxy whose record was rewritten with a different identity even if it runs under our guardian | 143-159 vs 636-645 |
| 9 | An adopted (inherited) proxy that becomes unhealthy halts proxy recovery permanently ("different install root (unknown)") | 1036-1041, 1134-1145 |
| 10 | `stopAll` stops the worker before the supervisor, so the supervisor respawns a worker in the gap | 851-853; commands.js:2653 |
| 11 | Worker's ordinary requests have no Host/Origin check; `gateway.sock` and the ephemeral TCP port bypass the supervisor's DNS-rebinding filter. Socket file mode is not set explicitly | request-worker.js:2361-2416, 2696-2701 |
| 12 | Doc vs code: `hooks/hooks.json:2` describes a SessionStart hook that "waits up to 12 seconds for the local Codex gateway", but the only command it runs is `registry-writer.js` (hooks.json:9), whose main path only writes the breadcrumb and launchers (hooks/registry-writer.js:192-202). The wait happens through the `readiness-gate` hooks that are installed into settings (lib/readiness-hooks.js:6, 26, 40). `hooks/ensure.js` is referenced by no manifest, only by test/hook-ensure-delegation.test.js:21; how `ensure` is invoked in practice is outside this slice |
| 13 | Comment says the hosts entry is "never written by this plugin", but `remote-control enable --confirm` writes it | request-worker.js:2703-2704; remote-control.js:219 |
| 14 | `reconcile` with no hosts entry returns `{status:'bound'}` meaning "reconciled to default", overloading `bound` | commands.js:2765 |
| 15 | `/internal/rc-compatibility` is unauthenticated except for a fixed header; any local process can trigger a port-80 bind/unbind (bounded to the hosts-file IP) | commands.js:2715-2721 |
| 16 | Windows `CreationDate` serialization (PS 5.1 vs 7) and `netstat -p tcp` IPv6 coverage are not determinable from source (EXT notes in §5.2) | process-supervision.js:288, 310 |
| 17 | `waitForShimExit` in the CLI polls `SHIM_PORT`, which equals the public port unless `CODEX_GATEWAY_WORKER_PORT` is set in the CLI's env; with that variable set in a user shell, the CLI would check the wrong listener | process-supervision.js:910-917; runtime.js:25 |
| 18 | `writePidRecord` (sync) writes `.pid` then `.pid.json` non-atomically; a reader between the two sees a pid with a stale/mismatched record and `readPidRecord` returns null → ownership falls back to command matching | process-supervision.js:69-88 |
| 19 | `lifecycle.jsonl` trim rewrites the file without a lock while other processes append | lifecycle-diagnostics.js:29-42 |
| 20 | Worker SIGTERM handling, socket-file permissions, and proxy bind address: UNREAD | — |
| 21 | `stopShimWithDrain` reports "drained" when `resolveOwner` says `unowned` without removing the pid record | process-supervision.js:926 |
