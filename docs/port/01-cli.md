model: claude-opus-5-5

# model-gateway CLI slice: behavior inventory for a Go port

Source: `/Users/zchee/src/github.com/Eigenwise/eigenwise-toolshed/plugins/model-gateway`, commit `a8501f251755` (written against `4ffb2f672`; moved to this commit on 2026-11-25, see `07-repin.md`), branch `dev`, plugin version `0.56.3` (`.claude-plugin/plugin.json`).
All citations are `path:line`, relative to that plugin root. `commands.js` means `lib/commands.js`.
Read in full: `lib/commands.js`, `bin/model-gateway.js`, `hooks/hooks.json`, `hooks/ensure.js`, `hooks/registry-writer.js`, `lib/readiness-gate.js`, `lib/readiness-hooks.js`, `lib/recovery-policy.js`, `lib/lifecycle-diagnostics.js`, `lib/github-release.js`, `lib/release-verification.js`, `lib/proxy-config.js`.
Read only to resolve a call from this slice (marked **[other lane]** where cited): `lib/lifecycle-lock.js`, `lib/shim-state.js`, `lib/control-auth.js`, `lib/windows-detached.js`, `lib/atomic-file.js`, `lib/remote-control.js`, and selected functions of `lib/process-supervision.js`, `lib/runtime.js`, `lib/settings-wiring.js`, `lib/request-worker.js`, `lib/pins.js`, `lib/context-window.js`.
UNREAD (other lanes): `createProxyRecovery` internals (`lib/process-supervision.js:1027-1210`), worker reaping/birth records (`lib/process-supervision.js:660-837`), `runWorker` and the request path (`lib/request-worker.js`), picker guard (`lib/picker-guard.js`), pins detection (`lib/pins.js` except `envBlockFor`/`ourBaseUrls`/`ownedPinValues`), anthropic catalog, local-llama, lmstudio, proxy-models, grok-backend, codex-upstream-state, usage-observability, context-window validation rules beyond the flag set.

---

## 0. Process-level conventions (apply to every command)

| Topic | Behavior | Citation |
|---|---|---|
| Entry | `bin/model-gateway.js` reads `argv[2]`. `readiness-gate` is dispatched **before** `commands.js` is required; any other name looks up `commands[name]`. | `bin/model-gateway.js:4-10` |
| Unknown / missing command | Prints `usage` to **stdout**; exit 1 if a name was given, 0 if none. | `bin/model-gateway.js:8-9` |
| Uncaught rejection | `console.error('model-gateway: ' + error.message)`, exit 1. | `bin/model-gateway.js:13-16` |
| Required as a module | `module.exports = require('../lib/commands.js')`. | `bin/model-gateway.js:17-18` |
| Argument model | Global, positional: `args = process.argv.slice(3)`; `flag(f) = args.includes(f)`. No parser, no `--x=y` form, unknown flags silently ignored. | `commands.js:199-201` |
| `log()` routing | If hook buffering is active, push to buffer; else if `--json` is anywhere in args, **stderr**; else stdout. Applies to every command. | `commands.js:215-218` |
| `die(m, code)` | Flushes buffered hook output, prints `model-gateway: <m>` to stderr, `process.exit(code ?? 1)`. Called inside lifecycle locks, so the lock directory is left behind (reclaimed later by dead-owner detection, `lib/lifecycle-lock.js:35-41,65-73` **[other lane]**). | `commands.js:241` |
| Module-load side effects of `commands.js` | Reads `plugin.json` version (`:85,242-247`); reads context-window settings via `runtime.js:319` (**[other lane]**); calls `configureRemoteControl({...})` (`:313`). | `commands.js:85,309` |
| State root | `STATE = ~/.claude/model-gateway` from `os.homedir()` (not `CLAUDE_CONFIG_DIR`); `LOGS = STATE/logs`, `BIN_DIR = STATE/bin`. | `commands.js:80-82`, `lib/runtime.js:15-18` |
| Ports | `PUBLIC_SHIM_PORT = CODEX_GATEWAY_PORT ‖ 18764`; `SHIM_PORT = CODEX_GATEWAY_WORKER_PORT ‖ PUBLIC_SHIM_PORT`; `PROXY_PORT = CODEX_GATEWAY_PROXY_PORT ‖ 18765`; `COMPAT_PORT = CODEX_GATEWAY_COMPAT_PORT ‖ 80`. | `commands.js:88-90`, `lib/runtime.js:45` |
| Base URLs owned | `DEFAULT_BASE_URL = http://127.0.0.1:<PUBLIC_SHIM_PORT>`, `COMPAT_BASE_URL = http://api.anthropic.com`. | `lib/runtime.js:46,50`, `lib/pins.js:476` |
| Lifecycle lock | `withLifecycleLock` = directory lock `STATE/lifecycle.lock/` holding one file named by a UUID token with `{token,pid,startedAt,host}`; acquired by `rename(candidate, lock)`; 15 s acquire timeout; re-entrant inside the same async context (`AsyncLocalStorage`). Failure message: `gateway lifecycle is busy or ownership is unknown (<dir>); retry recover or run doctor`. | `lib/lifecycle-lock.js:13,57-94` **[other lane]** |
| Intentional stop marker | `STATE/intentionally-stopped.json` = `{"stoppedAt": ISO}` mode 0600; presence is the only signal. | `lib/lifecycle-lock.js:12,96-108` **[other lane]** |

---

## 1. Command table

For `remote-control` only: Not ported: user decision of 2026-11-26; see the plan's differences table.

`commands` map: `commands.js:2935-3041`; `ensure` is rewrapped by `automaticLifecycle` at `commands.js:3043-3044`.
**There is no `restart` command.** "Restart" exists only as the supervisor's `POST /restart` endpoint (`commands.js:2831-2857`) and the client helper `restartWorkerWithDrain` (`lib/process-supervision.js:945-961`).

| Command | Args / flags (default) | Purpose | Exit codes | stdout / stderr | State read / written | Child processes | Network | Env | Entry |
|---|---|---|---|---|---|---|---|---|---|
| `setup` | `--preserve-wiring` (false) | Download latest claude-code-proxy, swap binary, (re)start gateway, wire project if authed | 0 success (also when not authed / still starting); 1 via `die` | progress lines via `log` (stdout) | writes `BIN_DIR/*`, `proxy-serving-version.txt`, settings files, `catalog.json`, launcher; reads stop marker | `tar -xf`, `claude-code-proxy --version` ×3 (no timeout), `codex auth status` (15 s), detached supervisor | GitHub API, asset downloads, shim `/healthz`, `/restart`, `/v1/models`, proxy `/v1/models` | `GH_TOKEN`, `GITHUB_TOKEN`, `SystemRoot`, ports | `commands.js:2936` → `:439-527` |
| `login` | `--device` (false → `login`) | Run proxy OAuth | child's status; 1 if null; 1 via `die` when binary missing | child inherits stdio; success line on stdout | reads `PROXY_BIN` | `claude-code-proxy codex auth login\|device`, `stdio: inherit`, **no timeout** | shim `/healthz` (2 s) after success | — | `commands.js:2937-2943` |
| `start` | — | Start supervisor, resume intentional stop | 0; 1 via `die(reason)` | status report on stdout | lock, stop marker removed when a supervisor is launched | detached `node <cli> serve-shim` | `/healthz`, `/v1/models`, port probes | ports, `CODEX_GATEWAY_DRAIN_TIMEOUT_MS` | `commands.js:2944-2948` |
| `stop` | — | Record intentional stop, stop shim/guardian/proxy | 0; 1 on stop failure | `stopped` (stdout) or `model-gateway: <reason>.` (stderr) | writes stop marker **before** stopping; removes pid files | `kill`/`taskkill`, `ps`/`lsof`/PowerShell probes | port probes | ports | `commands.js:2951-2960` |
| `recover` | — | Restore readiness only (no settings, pins, catalog) | 0; 1 on throw (bin prints `model-gateway: <reason>`) | normally silent; stale-session notice on stderr | as `start` but does **not** clear the stop marker | detached supervisor if needed | as `start` | ports | `commands.js:2949` → `:633-638` |
| `ensure` | `--quiet` (false) | Maintain wiring, start if wired, report | see §3.2 exit table | `--quiet`: single JSON object on stdout; else text | many (see §3.2) | as `start`, plus `--version` (10 s), picker probe | as `start`, plus `/v1/models` | `ANTHROPIC_BASE_URL`, ports | `commands.js:2961-3026`, wrapper `:577-586,3043-3044` |
| `status` | — | Show what is running | 0 if Codex readiness `ready`, else 1 | report on stdout | reads pid records, settings, context-window | `codex auth status` (15 s, async, in `getCodexReadiness`) | `/healthz`, proxy `/v1/models` | ports | `commands.js:3027` → `:955-968` |
| `models` | — | Dump shim `/v1/models` JSON | 0; 1 via `die('shim not running (start it first)')` on network error; 1 via bin on bad JSON | pretty JSON on stdout | — | — | `GET http://127.0.0.1:<SHIM_PORT>/v1/models` 3 s | ports | `commands.js:3028-3032` |
| `catalog` | `--json`, `--refresh` | Print sidequest catalog; refresh if stale (>5 min) or asked | 0; 1 when `--refresh` and write refused; 1 via `die` when no catalog at all | `--json`: compact JSON + `\n` on stdout, human lines on stderr; else pretty JSON via `log` | reads/writes `STATE/catalog.json`, writes `$CLAUDE_CONFIG_DIR/cache/gateway-models.json` | `codex auth status` | `/healthz`, shim and proxy `/v1/models`, local/LM Studio probes **[other lane]** | — | `commands.js:3033` → `:2078-2101` |
| `pin` | `--opus\|--sonnet\|--fable <id\|default>` pairs | Show or persist alias pin overrides | 0; 2 on bad flag/value | report lines | `STATE/pins.json`, registered project settings | — | — | — | `commands.js:3034` → `:1088-1103` |
| `context-window` | `--claude`, `--codex`, `--grok` `<tokens\|full>`; `--codex-compact-at`, `--grok-compact-at` `<tokens\|cap>` (pairs) | Show or persist per-backend windows | 0; 2 on invalid pair or Claude cap without project wiring | report lines | `STATE/context-window.json`, `autoCompactWindow` in registered project `settings.local.json` | — | — | `CLAUDE_CODE_AUTO_COMPACT_WINDOW` | `commands.js:3035` → `:1001-1018`, flags `lib/context-window.js:20-30` |
| `env` | none \| `--write-project` \| `--write-user` \| `--remove`, `--reconcile`; retired `--mode`, `--show-mode` | Print or write/remove wiring | 0; 2 on retired/conflicting flags; 1 on verify failure (throw) | env block JSON + guidance, or `written to`/`removed from` | settings files, `wired-projects.json`, deletes `wiring.json`, launcher | none directly | `/healthz` (mode) | `ANTHROPIC_BASE_URL` | `commands.js:3036` → `:1134-1171` |
| `doctor` | — | Full health check | 0 or 1 (six independent failure conditions, §3.6) | report on stdout, `ERROR`/`FAIL` lines on stderr | read-mostly; may retire/rewrite pid records (stderr) | `--version` (10 s), provider `auth status` (15 s), picker probe | many local GETs | many | `commands.js:3037` → `:1397-1518` |
| `remote-control` | `enable\|disable\|doctor`, `--confirm` | Opt-in hosts-file compat mode | 0; 1 via `die` | report lines | hosts file + timestamped `.bak` | — | `/healthz`, `POST /internal/rc-compatibility`, DNS lookup | `CODEX_GATEWAY_HOSTS_FILE`, `SystemRoot` | `commands.js:3038` → `lib/remote-control.js:137-269` **[other lane]** |
| `serve-shim` | — (internal) | Run supervisor in foreground | 0 on SIGTERM/SIGINT; 1 on bind failure / missing worker port | listen lines stdout, errors stderr (redirected to `logs/guardian.log` when launched detached) | control-token, pid files, lifecycle log, failure file, proxy serving version | `fork(<cli> serve-worker)`, proxy via `createProxyRecovery` | listens `127.0.0.1:<PUBLIC_SHIM_PORT>`, `<ip>:<COMPAT_PORT>` | many (§3.8) | `commands.js:3039` → `:2456-2905` |
| `serve-worker` | — (internal, not in usage) | Worker HTTP server | UNREAD | UNREAD | UNREAD | UNREAD | UNREAD | UNREAD | `commands.js:3040` → `runWorker` (`lib/request-worker.js`, UNREAD) |
| `readiness-gate` | stdin JSON hook payload | Prompt/session readiness gate | 0 ok; 2 (UserPromptSubmit) or 1 (other events) on recovery failure | stdout empty in normal paths; guidance on stderr | reads settings, stop marker, pid record | via `recoverGateway` | `/healthz` 750 ms | `ANTHROPIC_BASE_URL`, `CODEX_GATEWAY_PORT` | `bin/model-gateway.js:5` → `lib/readiness-gate.js:52-63`; duplicate map entry `commands.js:2950` (unreachable via bin) |

### 1.1 State files (consolidated)

`STATE` = `<homedir>/.claude/model-gateway`. "Other lane" = shape not read here.

| Path | Shape | Written by | Read by | Citation |
|---|---|---|---|---|
| `STATE/bin/claude-code-proxy[.exe]` | executable | `setup` (rename swap) | everything that spawns the proxy | `commands.js:86,368-379` |
| `STATE/bin/stage-XXXXXX/` | temp dir with archive + extracted tree | `setup`, removed at end | `setup` | `commands.js:456,484` |
| `STATE/bin/claude-code-proxy.old-<ver\|unknown>-<epochms>` | previous binary | `setup` | swept by `setup`, `ensure` | `commands.js:355-366,443,2972` |
| `STATE/proxy-serving-version.txt` | `<x.y.z>\n` | `setup` (old version), supervisor on proxy start | `ensure` (`restartProxyIfOutdated`) | `commands.js:381-388,404,2508` |
| `STATE/shim-supervisor-failure.txt` | JSON `{supervisorPid, generation, message}`, atomic | supervisor (bind failure, uncaught error) | `startAll` wait; deleted on launch and on listen | `commands.js:614-625,761,2878,2893` |
| `STATE/intentionally-stopped.json` | `{"stoppedAt": ISO}`, 0600 | `stop` | gate, `startAll`, `setup`, `ensure`, doctor; deleted by `launchSupervisor` when `resumeIntent` | `lib/lifecycle-lock.js:12,96-108`, `commands.js:764` |
| `STATE/lifecycle.lock/<uuid>` (+ `lifecycle.lock.candidate-*`) | `{token,pid,startedAt,host}`, 0600 | every locked operation | lock waiters (dead-owner check) | `lib/lifecycle-lock.js:13,57-73` |
| `STATE/control-token` | 64 lower-hex + `\n`, 0600, created `wx` | supervisor | CLI (`controlRequestHeaders`), supervisor | `lib/control-auth.js:8-39` |
| `STATE/{guardian,shim,proxy}.pid` + `.pid.json` | pid text; `{pid,command,startedAt}` | `spawnDetached`, supervisor | owner checks, stop, doctor | `lib/process-supervision.js:61-104` |
| `STATE/worker-births/` | other lane | supervisor | reaper | `lib/process-supervision.js:660` |
| `STATE/launch-XXXXXX/result.json` | `{pid}` or `{error}` (Windows only, temp) | Windows launcher | `spawnWindowsDetached` | `lib/windows-detached.js:29-57` |
| `STATE/catalog.json` | catalog schema 4 (§3.9), atomic | `writeCatalog` | `catalog`, doctor, sidequest | `commands.js:1572,1950-1964` |
| `STATE/model-gateway.js`, `STATE/update.js` | Node launchers, 0600, atomic | registry-writer, `migrateReadinessGates`, `writeEnv` | settings hooks, updater | `hooks/registry-writer.js:137-156` |
| `STATE/wiring.json` | retired | — | deleted by `env` writes/removes | `commands.js:83,1161`, `lib/settings-wiring.js:23-28` |
| `STATE/logs/{guardian,shim,proxy}.log` | appended stdout/stderr | supervisor launch, worker fork, proxy spawn | humans | `lib/process-supervision.js:962-991,1017-1025`, `commands.js:2618` |
| `STATE/logs/lifecycle.jsonl` | §4.5, capped 200 lines | CLI + supervisor | doctor | `lib/runtime.js:37`, `lib/lifecycle-diagnostics.js` |
| `STATE/logs/request-routes.jsonl` | other lane (worker) | worker | — | `commands.js:127` |
| `STATE/{pins,detected-pins,context-window,wired-projects,codex-upstream-blocked,dispatch-routes}.json`, `picker-guard.json` | other lane | pins / context-window / settings-wiring / worker / picker guard | various | `lib/runtime.js:20,22,38,244,545-546`, README:134 |
| `<homedir>/.claude/toolshed/registry/model-gateway.json` | breadcrumb (§4.2) | registry-writer | sidequest (other plugin) | `hooks/registry-writer.js:133-198` |
| `${CLAUDE_CONFIG_DIR‖~/.claude}/cache/gateway-models.json` | discovery cache, other lane | `writeGatewayDiscoveryCache` | doctor, Claude Code | `lib/runtime.js:552`, `commands.js:1986-2016` |
| `<cwd>/.claude/settings.local.json` | Claude Code settings | `env --write-project`, `setup`, `ensure`, `pin`, `context-window --claude` | policy, wiring, doctor | `lib/settings-wiring.js:30-36,107-114` |
| `<cwd>/.claude/settings.json` | Claude Code settings | legacy migration only | policy, wiring, doctor | `lib/settings-wiring.js:30-36` |
| `<homedir>/.claude/settings.json` | Claude Code settings | `env --write-user`, readiness-gate migration | policy, wiring, doctor | `lib/settings-wiring.js:33-35`, `commands.js:602` |
| `installed_plugins.json` (registry dirs, §4.2) | Claude Code registry | — | launcher, `hooks/ensure.js`, `installScope` | `lib/runtime.js:682-716` |
| proxy `config.json` (§5) | claude-code-proxy config | — | provider readiness | `lib/proxy-config.js:12-22` |
| hosts file + `<hosts>.model-gateway-<ts>.bak` | text | `remote-control --confirm` | supervisor compat listener, doctor | `lib/remote-control.js:10-15,214-221,295-299` |

---

## 2. Usage text (verbatim, `commands.js:153-197`)

Template literal; `${BIN_DIR}`, `${LOGS}`, `${path.join(STATE, 'catalog.json')}`, `${PIN_OVERRIDE_PATH}`, `${CONTEXT_WINDOW_PATH}`, `${REQUEST_ROUTE_LOG_PATH}` are interpolated at module load.

```text
usage: model-gateway.js <command>

  setup            download the claude-code-proxy binary (v-latest) into ${BIN_DIR}
  login [--device] run the ChatGPT OAuth flow (--device for headless device-code)
  start | stop     start/stop the proxy + shim (detached, logs in ${LOGS})
  ensure [--quiet] maintain installed wiring and start unless intentionally stopped
  recover          restore readiness without settings, pin or cache maintenance
  status           show what's running
  models           show the model list the shim advertises to Claude Code
  catalog [--json] [--refresh] print the sidequest-readable model catalog (${path.join(STATE, 'catalog.json')});
                   --refresh exits non-zero and says why on stderr when it could not write a
                   fresh one, and prints the retained catalog unchanged
  pin [--opus|--sonnet|--fable <model|default>]
                   show pin sources and newest catalog ids, or persist an override (${PIN_OVERRIDE_PATH});
                   default removes the override and follows detected/catalog defaults, never backwards
  context-window [--claude <tokens|full>] [--codex <tokens|full>] [--grok <tokens|full>]
                   show or persist the context window per backend (${CONTEXT_WINDOW_PATH}); the
                   orchestrator and every executor share it. Defaults: claude full, codex 272000,
                   grok full. Without compact-at, a gateway cap compacts 85000 below the cap.
                   --codex-compact-at <tokens|cap> and --grok-compact-at <tokens|cap> set a direct
                   maximum before compaction (positive whole tokens); cap removes the override.
                   The cap and backend window minus 40000 still limit the effective maximum.
                   A saved compact-at ignores CODEX_GATEWAY_COMPACT_TRIGGER, reported unchanged.
                   Example: --codex-compact-at 242000 leaves the 272000 cap unchanged. OpenAI bills
                   input above 272k tokens at 2x; the crossing turn and compaction request can still
                   exceed 272k. Existing installations keep their current policy until configured.
                   --claude sets the native autoCompactWindow in project-wired settings; native
                   engine headroom applies and the exact trigger is unverified. Claude compact-at
                   is unsupported. No same-model main/subagent split is applied.
                   CODEX_GATEWAY_CONTEXT_WINDOW is superseded and only applies when no codex value is saved.
  env [--write-project | --write-user | --remove] [--reconcile]
                   print the Claude Code env block, or merge/remove wiring
                   --remove clears owned routing, discovery, static env and alias pins; preserves and names user pins
                   (--write-project writes .claude/settings.local.json; --write-user
                   is an opt-in shared fallback in ~/.claude/settings.json)
  doctor           full health check, Anthropic catalog age/fallback, pin sources and newest ids
  remote-control <enable|disable|doctor> [--confirm]
                   manage the opt-in hosts-file compatibility mode; enable refuses an effective
                   HTTPS api.anthropic.com process URL before hosts changes
  serve-shim       (internal) run the router in the foreground

  Request route logging (on by default; set CODEX_GATEWAY_REQUEST_LOG=0 to opt out):
    CODEX_GATEWAY_REQUEST_LOG=0
    Writes JSONL route metadata to ${REQUEST_ROUTE_LOG_PATH}. Override the path with
    CODEX_GATEWAY_REQUEST_LOG_PATH. It records no request bodies, prompts, tools, or auth.
```

Not listed in usage: `readiness-gate`, `serve-worker`. Listed but not a single command: `start | stop`.

---

## 3. Flows

### 3.1 `setup` (`commands.js:439-527`, wrapped in the lifecycle lock at `:2936`)

| # | Step | Citation |
|---|---|---|
| 1 | `mkdirs()` creates `STATE`, `LOGS`, `BIN_DIR`. | `:440,248` |
| 2 | `refreshCatalogPinsAndWiring()`: snapshot owned pins and effective pins, `refreshCatalogPins()`, log moved pins / outdated overrides, sync pins into registered projects. | `:441,1048-1054,1056-1066,1031-1033` |
| 3 | `migrateReadinessGates()`: write stable launcher, then for every registered project settings file plus `~/.claude/settings.json`, if its `env.ANTHROPIC_BASE_URL` is ours, `syncReadinessHooks(settings,true)` and write if changed. | `:442,599-612` |
| 4 | `sweepOldProxyBinaries()`: delete every `BIN_DIR/claude-code-proxy*.old-*`. | `:443,359-366` |
| 5 | Asset name `claude-code-proxy-<plat>-<arch>.<ext>`; see §5. | `:444-447` |
| 6 | `fetchLatestRelease('raine/claude-code-proxy')`; any error → `die`. | `:449-451`, `lib/github-release.js:45-51` |
| 7 | `downloadVerifiedArchive` (checksum first, then archive, sha256 compare); error → `die`. | `:452-454`, `lib/release-verification.js:5-28` |
| 8 | `mkdtemp(BIN_DIR/stage-)`, write archive there. | `:456-458` |
| 9 | Extract: `tar -xf <archive>` with `cwd: stage` (Windows: `%SystemRoot%\System32\tar.exe`, default `C:\Windows`); non-zero → `die('extract failed: <stderr|status>')`. | `:459-463` |
| 10 | Find the binary by recursive `readdirSync` whose basename equals `claude-code-proxy[.exe]`; none → `die`. POSIX `chmod 0755`. | `:464-468` |
| 11 | Compare `--version` of current and staged binaries (first `x.y.z` in stdout); `proxyChanged` if either unparsable or they differ. Neither spawn has a timeout. | `:469-471`, `lib/shim-state.js:55-58` |
| 12 | `maintenanceStopped = preserveWiring && intentionallyStopped()`. | `:473` |
| 13a | Changed: `replaceProxyBinary` renames current → `<bin>.old-<ver\|unknown>-<epochms>`, staged → current; on failure renames back and throws `proxy upgrade failed (...); original proxy restored.` | `:475-478,355-379` |
| 13b | Then `restartSetupProxy`: returns false if stopped; else writes the **previous** version to `proxy-serving-version.txt`, stops the recorded proxy if `:PROXY_PORT` listens, waits ≤ 7×100 ms for release, returns whether `:PUBLIC_SHIM_PORT` owner is `same-install` (= "proxy recovery handoff" to the running supervisor). If false, logs `proxy on disk: X   serving: Y   restarts on next \`ensure\``. | `:479-480,398-408,425-428,390-396` |
| 13c | Unchanged: logs `model-gateway: proxy unchanged; keeping its authenticated process running.` | `:481-483` |
| 14 | `rm -rf stage`. | `:484` |
| 15 | If `maintenanceStopped`: log `Model Gateway updated; intentional stop preserved. Run start to resume.` and return (exit 0). | `:485` |
| 16 | `restartAfterSetup(handoff)`: skipped if handoff or stopped; else `restartShimIfOutdated({operation:'setup'})` (stop + `startAll` when `/healthz` lacks `proxyRecovery:true` or serving version < installed); if no supervisor restart was needed, `restartWorkerWithDrain()` (POST `/restart {script}`), failure → `die('could not restart shim worker: ...')`. | `:486,430-437,897-909,888-895` |
| 17 | Supervisor restart failed and not still starting → `die('could not restart shim supervisor: ...')`. | `:487` |
| 18 | Log `installed: <proxy --version output>`. | `:489-490` |
| 19 | Supervisor still starting → `warnShimStillStarting` (one warning line) and return 0. | `:492,2054-2056` |
| 20 | `startAll({lifecycleOperation:'setup', resumeIntent: !preserveWiring, preserveRunningSupervisor: handoff})`; still starting → warn, return; not ok → `die(reason)`. | `:494-496` |
| 21 | Clear upstream blocked/unavailable markers. | `:497-498` **[other lane: codex-upstream-state]** |
| 22 | Login check `isAuthed()`: `claude-code-proxy codex auth status`, 15 s, success = status 0 AND `/account/i` in stdout+stderr. Not authed → log `next: node "<stable>" login   (ChatGPT browser sign-in), then setup again to wire Claude Code`, return 0. | `:499-502,327-330` |
| 23 | Log `ChatGPT auth: valid`; `refreshDetectedPinsAndWiring({force:true})` (also syncs registered Claude window). | `:503-504,1038-1046` |
| 24 | `--preserve-wiring` → `finishUpdateWithoutWiring()`: log that it never wires the cwd, `writeCatalog()` (errors swallowed), `statusReport()`. | `:505,2058-2062` |
| 25 | `resolveIntendedMode()`: `compat` iff `/healthz` `compat.hostsDetected && compat.port80Bound`. | `:506,915-919` |
| 26 | Already wired: (a) `wiredMode()` has no file scope → log the "shell-only, run env --write-project" line; (b) mode differs → `writeEnv(scope,false,{mode,quiet:true})` + switch line; (c) same → rewrite quietly + `already wired; refreshed Claude alias pins...`. Then `statusReport()`, return. | `:507-523`, `lib/settings-wiring.js:470-484` |
| 27 | Not wired → `writeEnv('project', false, {mode})` (scope from `selectedWiringScope()`, constant `'project'`), `writeCatalog()` (errors swallowed), `statusReport()`. | `:524-526`, `lib/settings-wiring.js:15-19` |

Notes: `setup` ignores `statusReport().ok`, so it exits 0 when not ready. The stop marker is removed only inside `launchSupervisor` (`:764`), i.e. only if a new supervisor is spawned with `resumeIntent`.

### 3.2 `ensure` (`commands.js:2961-3026`) and the wrapper

Wrapper `automaticLifecycle` (`:577-586`): (1) explicit plugin disable → stderr notice, return (exit 0); (2) acquire lifecycle lock; (3) stop marker present → `model-gateway is intentionally stopped; run start to resume.` on stderr, return (exit 0). The wrapper runs **before** `bufferedHookLines` is set, so with `--quiet` these two notices go to stderr and no JSON is printed.

| # | Step | Citation |
|---|---|---|
| 1 | `--quiet` → `bufferedHookLines = []`; `finish(code)` = flush JSON then `process.exit(quiet ? 0 : code)`. | `:2962-2968` |
| 2 | `migrateReadinessGates`, `cleanLegacyEnvSettings`, `cleanLegacyGatewayModelCache`, `sweepOldProxyBinaries`, `repairProjectWiring(noticeForUser)`. | `:2969-2973` |
| 3 | `wired = isWired()` (effective `ANTHROPIC_BASE_URL` ∈ our URLs; env first, then project-local, project-shared, user). | `:2974`, `lib/settings-wiring.js:38-53,444-446` |
| 4 | `refreshCatalogPinsAndWiring()`. | `:2975` |
| 5 | `getCodexReadiness()`; binary missing: wired → notice (stderr in plain mode; while buffering, `noticeForUser` ignores `toStderr` and the text goes to the JSON, `:220-225`) + `finish(1)`; unwired → notice `model-gateway is installed but not set up. Offer to run its setup ...` + `finish(0)`. | `:2976-2984` |
| 6 | `ensureWiredGateway`: unwired or disabled → `{ok:true}` **without starting anything**; else `restartProxyIfOutdated({quiet})` (on-disk `--version` vs `proxy-serving-version.txt`) then `startAll({quiet,'ensure',preserveRunningSupervisor})`. | `:2985,571-575,410-423` |
| 7 | Not ok + quiet + `waitCutShort`: if a shim was launched, append lifecycle record `ensure-hook-wait-cut-short` `{component:'SessionStart',pid,outcome:'supervisor-started'}`; notice `model-gateway is still starting; retry the Codex model in a few seconds`; `finish(0)`. | `:2986-2997` |
| 8 | Not ok otherwise: notice `model-gateway could not start: <reason>. Run \`node "<stable>" doctor\` ...`; `finish(1)`. | `:2998-2999` |
| 9 | Readiness again; quiet and not ready → notice its message. | `:3001-3002` |
| 10 | `warnIfProxyOutdated()`: `--version` with 10 s timeout; below `0.1.14` → **direct `console.error`** (bypasses the hook buffer). | `:3003,554-563,121` |
| 11 | Unwired: notice from `sessionStartWiringNotice` (precedence: env bypass → not signed in → locally wired → sibling project wiring → generic), then `repairProjectWiring(log)`. | `:3005-3011,2908-2926,1130-1132` |
| 12 | Wired: if source is env with no file definition → "shell-only" notice; `syncCompatMode()`; `syncGatewayWiring()` (refresh pins; rewrite settings if any expected env key differs); `reportStalePickerRowsOncePerCli()` (8 s probe, once per `claude --version`). | `:3012-3023,1226-1245,1119-1128,1111-1117,1395` |
| 13 | Not quiet → `statusReport({readiness})`. Then `flushHookOutput()`. | `:3024-3025` |

Startup wait: quiet = `QUIET_STARTUP_WAIT_MS` 12000; non-quiet = `max(12000, (CODEX_GATEWAY_DRAIN_TIMEOUT_MS‖30000)+12000)` = 42000 by default. Poll interval 300 ms; returns early on `/healthz` 200 (1 s timeout) or on a failure file matching this supervisor pid and generation. `commands.js:91,565-569,645-663`, `lib/process-supervision.js:54-59`.

Hook JSON (quiet only, `commands.js:227-238`): one object, no trailing newline, omitted entirely when both parts are empty:

```json
{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"<buffered lines joined by \\n>"},
 "systemMessage":"<first notice>[ (+N more: run `node \"<stable launcher>\" doctor`)]"}
```

`systemMessage` is the first `noticeForUser` text; the rest are counted. Every `log()` line during the run lands in `additionalContext`.

| `ensure` outcome | `--quiet` exit | plain exit |
|---|---|---|
| plugin explicitly disabled / intentionally stopped (wrapper) | 0, stderr only, no JSON | 0 |
| lock acquire timeout or any thrown error | 1 (bin catch) | 1 |
| `die()` anywhere | 1 (JSON flushed first) | 1 |
| binary missing, wired | 0 | 1 |
| binary missing, unwired | 0 | 0 |
| start not ok, wait cut short | 0 | — (non-quiet sets `waitCutShort:false`) |
| start not ok otherwise | 0 | 1 |
| completed (ready or not) | 0 | 0 |

**SessionStart reachability at this commit:** `hooks/hooks.json:4-14` runs only `registry-writer.js` (timeout 10). Commit `101a64fd2` (2026-09-26, `feat(model-gateway): remove ensure hooks`) deleted the `node "${CLAUDE_PLUGIN_ROOT}/hooks/ensure.js" ensure --quiet` entry (timeout 30). The `ensure --quiet` JSON contract and the `ensure-hook-wait-cut-short` record (read by doctor, `:1438-1441`) are now reachable only by manual invocation; `hooks/ensure.js` is referenced only by `test/hook-ensure-delegation.test.js`. Session start readiness now comes from the settings-installed `readiness-gate` hook (§4).

`hooks/ensure.js` (dormant delegator, `hooks/ensure.js:35-53`): resolve the registered install's CLI (`runtime.resolveRegisteredCliPath`), fall back to `$CLAUDE_PLUGIN_ROOT/bin/model-gateway.js` (or `../bin`), else print the missing-install message; `spawnSync(process.execPath, [cli, ...argv], {stdio:'inherit', windowsHide:true})`, no timeout; with `--quiet` every failure exits 0; otherwise child status (null → 1).

### 3.3 `start` / `stop` / `recover` and the shared `startAll`

`startAllLocked` (`commands.js:811-839`):

1. Stop marker present and not `resumeIntent` → `{ok:false, reason:'gateway intentionally stopped; run start to resume'}` (`:830`).
2. Proxy binary missing → `{ok:false, reason:'proxy binary missing (run setup)'}` (`:831`).
3. `mkdirs`; build context (timeout from §3.2; one-shot lifecycle recorder `<op>-recovery-started/finished` with `outcome` `ready|starting|failed`, `:669-698`).
4. `readSettledShimState`: `probeShimState` (`lib/shim-state.js:28-42` **[other lane]**: `running-foreign` if owner is a foreign install or `unknown`; `running-ours` if `/healthz.ok`; else `starting` if a same-install or recorded guardian pid exists; else `stopped`). If `starting`, wait the startup window ignoring the old failure file, then probe again (`:702-708`).
5. Plan (`:710-714`):

| Shim state | Plan | Behavior | Citation |
|---|---|---|---|
| `running-foreign` | `refuseForeignShim` | foreign install → `PID <p> owns :<port> from a different install root (<image>)`; unknown owner → record `<op>-owner-unknown`, reason from `unknownPortOwnerReason` | `:716-723` |
| `running-ours` and serving version ≥ installed with `proxyRecovery:true` | `keepRunningShim` | reap orphans if same-install; stale-session notice if serving is newer; with `preserveRunningSupervisor` wait for readiness, else refresh catalog and return ok | `:725-733,802-805,544-548` |
| `starting` and `preserveRunningSupervisor` | `waitForRunningShim` | wait the startup window for the existing pid | `:735-737` |
| anything else | `replaceShim` | `stopped` → reap orphans; else `stopRunningSupervisor` (SIGTERM / `taskkill /T /F`, confirm exit ≤ 3 s, wait shim port release ≤ 3 s); then `launchSupervisor` | `:739-766`, `lib/process-supervision.js:859-888` |

`launchSupervisor` (`:757-766`): new UUID `startupGeneration`; delete failure file; `spawnDetached('guardian', process.execPath, [resolveNewestInstalledCliPath(), 'serve-shim'], {MODEL_GATEWAY_STARTUP_GENERATION})`; if `resumeIntent`, delete the stop marker. `resolveNewestInstalledCliPath` picks the newest sibling `<cache>/<version>/bin/model-gateway.js` that `canReplaceInstalledCliPath` allows (`lib/runtime.js:648-671,749-764`).

`finishStartingShim` (`:768-784`): ready → `started: <list>` (non-quiet) + catalog refresh; failure file → `{ok:false, reason:<file message>}`; timeout → `still starting after Ns; supervisor pid P is running` (`stillStarting:true`) when `process.kill(pid,0)` succeeds or fails with EPERM (`:786-794`), else `not healthy after Ns (check logs in <LOGS>)`; `waitCutShort = quiet`.

| Command | Lock | `resumeIntent` | Catalog refresh | On failure |
|---|---|---|---|---|
| `start` | outer + inner (re-entrant) | true | `writeCatalog` | `die(reason)` exit 1 |
| `recover` | inner only | false | no-op | throws `Error(reason)`; disabled plugin → ok |
| `ensure` | wrapper + inner | false | `writeCatalog` | §3.2 |
| `setup` | outer + inner | `!preserveWiring` | `writeCatalog` | `die` |

`stop` (`:2951-2960`): lock; `recordIntentionalStop()` first; `stopAll()` (`lib/process-supervision.js:838-858` **[other lane]**): refuse foreign/unknown port owner; record `stop-<component>-stop-requested` for shim, guardian, proxy; stop each recorded pid (owned-by-this-install check, SIGTERM or `taskkill /pid N /T /F`, confirm exit ≤ 3 s); kill the port owner pid if still alive; reap orphans. Failure leaves the stop marker in place and exits 1.

### 3.4 `readiness-gate`

Two layers run for every gated event:

| Layer | What it does | Citation |
|---|---|---|
| Settings hook | `{"type":"command","command":"node \"<HOME>/.claude/model-gateway/model-gateway.js\" readiness-gate","timeout":35}` on `SessionStart` (matcher `startup\|resume`) and `UserPromptSubmit` (no matcher); no `onFailure`. | `lib/readiness-hooks.js:5-7,25-29` |
| Stable launcher (gate mode) | Reads all stdin; `chdir(payload.cwd)` if it is a string (errors ignored); runs inlined `recoveryPolicy()`; `unwired` → `repairProjectWiring()` from the registered CLI's `lib/settings-wiring.js`, exit 0; `disabled` → stderr notice, exit 0; no registered CLI → message, exit 2; else `spawnSync(node, [cli, 'readiness-gate'], {stdio:['pipe','inherit','inherit'], input, timeout:25000, killSignal:'SIGKILL', windowsHide:true})`; spawn error/timeout → deadline message, exit 2; else child status (null → 1). | `hooks/registry-writer.js:72-111` |
| CLI `readinessGate()` | Re-reads stdin (`fs.readFileSync(0)`), unparsable → `{}`; `chdir(cwd)`; `recoveryPolicy()` again; `unwired` → `repairProjectWiring()` (stderr report), exit 0; `disabled` → stderr, exit 0; else `recoverUnlessHealthy`. | `lib/readiness-gate.js:39-63` |
| Healthy fast path | Not intentionally stopped, guardian pid record exists and its recorded command resolves to this plugin's install, `/healthz` (750 ms) `supervisorPid === record.pid`, serving version string **equals** `PLUGIN_VERSION`, and `servingCurrentVersion` → return without recovering. | `lib/readiness-gate.js:10-32` |
| Recovery | `require('./commands.js').recoverGateway()` (§3.3). Throw → stderr `Model Gateway isn't ready: <reason>. Run node "<stable>" start (resumes an intentional stop), ... recover, ... doctor. To remove project wiring, run ... env --remove. Then resubmit your prompt. Hooks disabled by the host and failures inside an active turn aren't covered by the prompt gate.`; `exitCode = 2` if `hook_event_name === 'UserPromptSubmit'`, else 1. | `lib/readiness-gate.js:34-37,52-63` |

Payload fields used: `cwd`, `hook_event_name`. Nothing else. Stdout: none on the normal paths (recovery runs quiet with a no-op catalog refresh).

`recoveryPolicy()` (`lib/recovery-policy.js:3-32`): reads `<cwd>/.claude/settings.local.json`, `<cwd>/.claude/settings.json`, `~/.claude/settings.json` (os.homedir, each `{}` on error). Endpoint = `process.env.ANTHROPIC_BASE_URL` if it is a string, else the first settings `env.ANTHROPIC_BASE_URL` string. Not in `['http://127.0.0.1:' + (CODEX_GATEWAY_PORT‖18764), 'http://api.anthropic.com']` → `unwired`. Else first settings file with a boolean `enabledPlugins['model-gateway@eigenwise-toolshed']`; `false` → `disabled`; otherwise `recover`.

Deadlines in play: lock acquire 15 s, quiet startup wait 12 s, launcher SIGKILL 25 s, host hook timeout 35 s. One `recover` can spend two startup windows: `readSettledShimState` waits a full window when the probe says `starting` (`commands.js:702-708`), and `finishStartingShim` waits another after a launch or for the running pid (`:768-769`). 12 + 12 s plus lock acquisition and the stop of a stale supervisor exceeds the 25 s SIGKILL, and the user then sees the launcher's generic "recovery failed or exceeded its 25s deadline" (`hooks/registry-writer.js:107-109`) instead of the real reason. Consequences: an intentionally stopped gateway makes recovery return `gateway intentionally stopped` → every prompt in a wired project is blocked with exit 2 until `start`; a supervisor still starting after 12 s is `ok:false` → exit 2, the prompt is blocked.

### 3.5 `status` (`commands.js:955-968`) output sections, in order

| # | Line(s) | Check | Citation |
|---|---|---|---|
| 1 | `proxy (claude-code-proxy) on :<PROXY_PORT>: answering /v1/models\|DOWN` | proxy `/v1/models` 200 (2 s) | `:958`, `lib/request-worker.js:56-61` |
| 2 | `models advertised to Claude Code: <n>[; fallback catalog (proxy unreachable)]` and `proxy recovery: ...` | only if `/healthz.ok` | `:959,927-934` |
| 3 | `shim (model router) on :<SHIM_PORT>: <state>(detail)` + conflict / pid-record / unreadable-command lines | `probeShimState` | `:960,936-942`, `lib/shim-state.js:44-53` |
| 4 | RC-compat lines: `hosts entry: not present` or `detected (<line>)`, `127.0.0.1:<COMPAT_PORT> bound: yes\|no (reason)`, transport disclaimer | `/healthz.compat` | `:961,944-953` |
| 5 | `Codex readiness: <state>` (+ message if not ready). States: `binary-missing`, `proxy-down`, `auth-missing`, `shim-down`, `serving-version-mismatch`, `upstream-blocked`, `upstream-unavailable`, `ready` | `getCodexReadiness` | `:962-963`, `lib/request-worker.js:63-73,139-163` |
| 6 | `context window <line>` per line of `contextWindowReport` | saved windows + configured `autoCompactWindow` (env `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, then project-local, project-shared, user `autoCompactWindow`) | `:964,970-974,1249-1263` |
| 7 | Anthropic catalog report | **[other lane]** | `:965` |
| 8 | `<alias>: <value> (<provenance>); newest catalog: <id\|unavailable>` per pin + outdated override line | `effectivePins` | `:966,1068-1073` |

`checks` shape (`lib/request-worker.js:142-150`): `{proxyBinary, proxyModels, codexAuth, shimRunning, servingVersion, installedVersion, servingVersionMatches}`; `servingVersionMatches = shimRunning && serving ≥ installed`.

### 3.6 `doctor` (`commands.js:1397-1518`) output sections, in order

| # | Section | Detail | Citation |
|---|---|---|---|
| 1 | Readiness gate | `readiness gate installed: <bool>; hooks disabled: <bool>; settings: <file>`, `intentional stop: <bool>; state: <STOP_PATH>`, `explicit plugin disable: <bool>`, `recovery diagnostics: <LOGS>; startup failures: <failure file>` | `:1398,588-597` |
| 2 | Project wiring issues | `model-gateway: <msg> in <file>; run ensure to repair.` | `:1399,1391-1393` |
| 3 | Pid record hygiene | `recordedGatewayPids()` may print `pid record <name> retired/rewritten ...` on stderr and delete/rewrite pid files | `:1400`, `lib/process-supervision.js:117-192` |
| 4 | Binary | `binary: <path>\|MISSING (run setup)`, `version: ...` (10 s), `codex auth: authenticated\|MISSING` | `:1402-1407` |
| 5 | Grok auth | `grok auth: present (<version>)` or error message | `:1408-1413` |
| 6 | Proxy providers | per non-codex provider from proxy `/v1/models`: `proxy <p> auth\|server: <description>`; local provider window mismatch line | `:1414-1424,1680-1693,1839-1849,1702-1708` |
| 7 | Status report | all of §3.5 | `:1425` |
| 8 | `serving shim version: <v\|unavailable>` | | `:1426-1427` |
| 9 | Picker guard report lines | **[other lane]** | `:1428` |
| 10 | Lifecycle | `lifecycle evidence: <logs/lifecycle.jsonl>`; last `supervisor-exit\|worker-exit\|proxy-exit` record; last `ensure-hook-wait-cut-short` record | `:1429-1441`, `lib/lifecycle-diagnostics.js:53-59` |
| 11 | Fixed advice line about `CLAUDE_CODE_NO_MODEL_FALLBACK=true` | | `:1442` |
| 12 | `VERSION MISMATCH` line when shim running and versions differ | | `:1443-1445` |
| 13 | `catalog: <n> models at <path> (writtenBy: <v>)` or `not written yet` | | `:1446-1449` |
| 14 | Discovery cache: `<path>; <age>s old, <n> models; matches live list\|stale\|live list unavailable` (+ OAuth note) | | `:1450,2003-2016` |
| 15 | `Claude <alias> pin: ...` | | `:1451` |
| 16 | Registered project pin disagreements (stderr `ERROR`) | | `:1452,1381-1389` |
| 17 | Model window policy table (`backend id \| picker/client id \| backend window \| advertised window \| client window \| client compact point \| sentry \| effective trigger \| evidence date`) and `live shim model ids: PASS\|FAIL` | | `:1453,1339-1379` |
| 18 | Wiring: effective source, default target, one line per source `env`, `project-local`, `project-shared`, `user` with `[effective]`, `[default write target]`, mode tags; precedence note | | `:1454-1485` |
| 19 | Process env bypass, not wired, mode-mismatch errors (stderr) | | `:1486-1510` |
| 20 | `install scope: project\|user` (nothing for `unknown`) | | `:1511-1516` |

`process.exitCode = 1` when any of: live ids differ from policy (`:1377`), registered pin disagreement (`:1452`), env bypass (`:1489`), not wired (`:1493`), effective/shadowed mode mismatch (`:1509`), Codex readiness not ready (`:1517`).

### 3.7 `env` (`commands.js:1134-1171`) and `writeEnv` (`:1176-1218`)

| Invocation | Behavior | Citation |
|---|---|---|
| `--mode` or `--show-mode` | `die('env <flag> was removed. Use env --write-project ...', 2)` | `:1135-1139` |
| `--write-project --write-user` | `die(..., 2)` | `:1145` |
| `--reconcile` without `--write-user` | `die(..., 2)` | `:1146` |
| none of write/remove | print `{"env": envBlockFor('default')}` (2-space JSON) and five guidance lines; no writes | `:1147-1155` |
| `--write-project` | `recordProjectWiring()`; `refreshDetectedPinsAndWiring({force:true})`; `writeEnv('project', false, {mode: resolveIntendedMode()})`; delete `STATE/wiring.json` | `:1157-1161` |
| `--write-user [--reconcile]` | as above with scope `user`; then compare registered project wirings against the user URL; list conflicts; with `--reconcile` remove gateway-owned wiring from them | `:1162-1170` |
| `--remove [--write-user]` | scope `project` (or `user` if `--write-user`); no pin refresh; `writeEnv(scope, true, {mode:'default'})`; delete `wiring.json` | `:1157-1162` |

Env block (`lib/pins.js:428-430,472-474`, `lib/runtime.js:522-532`): `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`, `CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK=1`, `ENABLE_TOOL_SEARCH=true`, `CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000`, `ANTHROPIC_DEFAULT_{OPUS,SONNET,FABLE}_MODEL`, plus Fable custom-option keys (`fableOptionBlock`, **[other lane]**).

`writeEnv` steps: migrate legacy project `settings.json` (write only; its mode wins) → target file (`project` = `<cwd>/.claude/settings.local.json`, `user` = `~/.claude/settings.json`) → remove on a missing file is a no-op → write stable launcher → `syncReadinessHooks(settings, !remove)` → delete `env.ANTHROPIC_UNIX_SOCKET` → remove: `removeGatewayOwnedSettings` (logs preserved user pins); write: merge env block, delete legacy `CLAUDE_CODE_AUTO_COMPACT_WINDOW=950000` / `CLAUDE_CODE_MAX_CONTEXT_TOKENS=920000` only when equal, clean legacy model cache → unchanged remove returns early → `writeSettings` (2-space JSON + `\n`, non-atomic, re-syncs readiness hooks itself) → re-read and verify `ANTHROPIC_BASE_URL`, else throw `Could not verify gateway settings in <file>` → `recordProjectWiring()` for project scope → log. `commands.js:1179-1217`, `lib/settings-wiring.js:107-114`.

### 3.8 `serve-shim` (`commands.js:2456-2905`)

For the compatibility listener and its control endpoint only: Not ported: user decision of 2026-11-26;
see the plan's differences table.

| Aspect | Behavior | Citation |
|---|---|---|
| Startup | `mkdirs`; `ensureControlToken()` (64 hex + `\n`, 0600, `wx`); lifecycle `supervisor-started`; `exit` hook records `supervisor-exit` with `exitCode`; `uncaughtExceptionMonitor` records `supervisor-uncaught-failure` and writes failure file | `:2457-2486`, `lib/control-auth.js:23-33` |
| Main listener | `http.createServer` on `127.0.0.1:<PUBLIC_SHIM_PORT>`; `requestTimeout=0`, `headersTimeout=120000`, `keepAliveTimeout=75000`. On listen: delete failure file, print `model-gateway shim supervisor listening on 127.0.0.1:<port>`, reconcile compat listener, start worker, run proxy recovery now and every `max(1000, CODEX_GATEWAY_PROXY_RECOVERY_INTERVAL_MS‖5000)` ms (unref'd) | `:2867-2885,2504` |
| Bind failure | `EADDRINUSE` → look up owning pid; write failure file `{supervisorPid, generation, message}`; stderr; `stopSupervisor(1)` | `:2886-2897,623-625` |
| Worker | `fork(workerScript, ['serve-worker'], {stdio:['ignore', shim.log, shim.log, 'ipc'], windowsHide:true, env:{...env, CODEX_GATEWAY_WORKER_PORT: String(configured‖0)}})`; port arrives as IPC `{type:'listening', port}` within 5 s or supervisor exits 1; pid record `shim`; on exit record `worker-exit`, remove pid, restart after 50 ms unless stopping | `:2612-2655,2606-2610` |
| Request gate | Host header must be `127.0.0.1:<localPort>` or `localhost:<localPort>` (plus `api.anthropic.com[:port]` on the compat listener) and no `Origin` header, else 403 | `:2801-2813`, `lib/control-auth.js:41-43` |
| `/restart`, `/drain` | Require POST, `content-type: application/json`, host `127.0.0.1:<port>`, `x-model-gateway-control-token` equal (constant-time) to the 64-hex token, not on compat listener; else 403 | `:2815-2821`, `lib/control-auth.js:45-54` |
| `/restart` body | ≤ 4096 bytes (else 413); JSON object; optional `script` must pass `canReplaceInstalledCliPath`, else 400 `{"ok":false,"reason":"restart requires this CLI or a non-older sibling in the same plugin cache"}`; success 202 `{"ok":true,"restarting":true}` and `restartWorker` (POST worker `/drain {timeout}`, force-kill after `timeout`, new worker on exit) | `:2831-2857,2680-2713` |
| `/drain` | After auth it falls through to the relay, so the **worker** implements drain | `:2815-2858` |
| `/internal/rc-compatibility` | POST, JSON, host `127.0.0.1:<PUBLIC_SHIM_PORT>`, header `x-model-gateway-control: rc-compatibility`, not compat listener; else 404. Body `{action:'probe'\|'reconcile', expectedSupervisorPid}`; pid mismatch → `{status:'unknown'}`; reply 200 `{status, code?, supervisorPid}` | `:2715-2726,2783-2799,305-307` |
| Relay | Body cap 64 MiB (413, `x-should-retry:false`); up to 80 attempts × 50 ms to reach the worker; `keepAlive:false` agent; `/healthz` response is augmented with `supervisorVersion`, `supervisorPid`, `proxyRecovery:true`, `compat:{hostsDetected,hostsLine,port80Bound,reason}`; worker lost after taking the body → 503 with `x-should-retry:false` | `:2297-2309,2332-2454,2499,2661-2678` |
| Compat listener | If hosts maps `api.anthropic.com` to loopback, bind `<ip>:<COMPAT_PORT>` (same handler); failure recorded in `compatState.reason` | `:2756-2781` |
| Shutdown | SIGTERM/SIGINT/IPC `fixture-shutdown` → `stopSupervisor(0, signal)`: stop proxy recovery, await pending pid writes, disconnect worker, wait 3 s, kill, close servers, `process.exit` | `:2560-2598,2898-2904` |

Supervisor launch (`lib/process-supervision.js:962-991` **[other lane]**): POSIX `spawn(node, [cli,'serve-shim'], {detached:true, stdio:['ignore', guardian.log, guardian.log], env:{...env, MODEL_GATEWAY_STARTUP_GENERATION}})` + `unref`; Windows: WMI `Win32_Process.Create` via PowerShell with a base64 stdin payload and a file acknowledgement (`lib/windows-detached.js:9-57`). Pid files `STATE/guardian.pid` and `guardian.pid.json {pid,command,startedAt}`.

### 3.9 `login`, `pin`, `context-window`, `catalog`, `models`

| Command | Flow | Citation |
|---|---|---|
| `login` | binary missing → `die('proxy binary missing, run setup first')`; run `codex auth <login\|device>` inheriting stdio; status 0 → `signed in; the proxy picks up the new credential on the next request` when wired and the shim does not need a restart, else `signed in; run setup once more to finish wiring Claude Code`; exit = child status | `:2937-2943,2928-2933` |
| `pin` | no args → `reportEffectivePins()`; else snapshot owned pins, read `pins.json`, consume `(args[i], args[i+1])` pairs: option must be `--opus\|--sonnet\|--fable` and value non-null else exit 2; `default` deletes the override; `isValidPin` else exit 2; write overrides; sync registered project pins; two confirmation lines | `:1075-1103` |
| `context-window` | no args → report; else pairs via `contextWindowUpdate` (`--claude\|--codex\|--grok` window, `--codex-compact-at\|--grok-compact-at`; error → exit 2); a non-`full` Claude value with no registered project wiring → exit 2 and nothing saved; Claude change synced into each registered `settings.local.json` `autoCompactWindow`; write `context-window.json`; report + restart advice | `:1001-1018,978-990`, `lib/context-window.js:11-30` |
| `catalog` | stale = missing or `updatedAt` older than 300000 ms; refresh or stale → require `/healthz` 200 (3 s) then `writeCatalog()`; failure kept as `refusal`; no catalog → `die`; refusal → stderr line; `--refresh` + refusal → exit 1 | `:2078-2101,1573,2070-2076` |
| `writeCatalog` | load proxy routes (proxy `/v1/models` 3 s); shim `/v1/models` (3 s, one retry after 300 ms when empty); write discovery cache unless shim serves a stand-in catalog; keep gateway ids; zero → throw; build catalog with readiness; `writeCatalogFile` refuses a stored schema > 4 and merges a strict-subset response with the stored models | `:2033-2046,1966-1977,1986-1996,1885-1964` |
| `models` | GET shim `/v1/models`, pretty-print; does not check status code | `:3028-3032` |

Catalog JSON (`commands.js:1900-1908`, schema 4, file written atomically with 2-space JSON + `\n`): `{schemaVersion:4, source:'model-gateway', updatedAt, writtenBy:<plugin version>, providers:{<p>:{ready,state,message}}, codexReadiness:<providers.codex|null>, models:[{slug,id,label,provider,contextWindow?,contextWindowNote?}]}`. Slug rule `^[a-z0-9][a-z0-9-]{1,31}$`, sha1-6 suffix on overflow, `-N` on collision (`:1583-1608`).

### 3.10 Remote Control compatibility (`remote-control`, `lib/remote-control.js:137-269` **[other lane]**)

Not ported: user decision of 2026-11-26; see the plan's differences table.

| Action | Behavior |
|---|---|
| any | action not in `enable\|disable\|doctor` → `die('usage: remote-control <enable\|disable\|doctor>')` (`:142-145`) |
| `enable` precheck | effective process env URL is an unsupported HTTPS api.anthropic.com URL → `die` (`:146-151`) |
| `doctor` | hosts file path, plugin block state, loopback mapping, conflicts, write access, serving compat (`POST /internal/rc-compatibility {action:'probe'}`), port 80 holder (`lsof`/`netstat` + `ps`/`tasklist`, Docker named), DNS lookup, then full `doctor()` (`:157-176`, `commands.js:269-303`) |
| `enable`/`disable` | unreadable hosts / non-loopback conflict → `die`; no change → "already enabled/disabled"; print the block; without `--confirm` print elevation hint and a question, exit 0 (`:178-200`) |
| `--confirm` | enable requires serving status `bound\|bindable`; re-read hosts and refuse if changed; copy to `<hosts>.model-gateway-<ISO with :. → ->.bak`; write; enable → `reconcile`, verify `/healthz` compat, `syncCompatMode({requiredMode:'compat'})`; disable → `startAll()`, `reconcile`, verify, `syncCompatMode()`; enable failure restores the original bytes if untouched (`:202-268`) |

Managed block: `# >>> model-gateway RC compatibility >>>` / `127.0.0.1 api.anthropic.com` / `# <<< model-gateway RC compatibility <<<` (`lib/runtime.js:51-53`). Hosts path `CODEX_GATEWAY_HOSTS_FILE` ‖ `/etc/hosts` ‖ `%SystemRoot%\System32\drivers\etc\hosts` (`lib/remote-control.js:10-15`).

---

## 4. Hook contract

### 4.1 Plugin hooks (`hooks/hooks.json`)

| Event | Command | Timeout | Notes |
|---|---|---|---|
| `SessionStart` (no matcher) | `node "${CLAUDE_PLUGIN_ROOT}/hooks/registry-writer.js"` | 10 s | Only entry at this commit. The `description` string still says it "waits up to 12 seconds for the local Codex gateway", which no longer happens. |

### 4.2 `hooks/registry-writer.js` (main: `writeBreadcrumb()`, all errors swallowed, `:200-202`)

| File | Path | Content | Citation |
|---|---|---|---|
| Stable command launcher | `<homedir>/.claude/model-gateway/model-gateway.js` | `COMMAND_LAUNCHER` Node script (below) | `:11-112,141-149` |
| Update launcher | `<homedir>/.claude/model-gateway/update.js` | runs `node <dir>/model-gateway.js setup --preserve-wiring`, stdio inherit; throws on spawn error; exit = status (null → 1) | `:114-123,137-156` |
| Breadcrumb | `<homedir>/.claude/toolshed/registry/model-gateway.json` | `{"schemaVersion":1,"name":"model-gateway","version":<plugin.json version>,"root":<plugin root>,"capabilities":["model-catalog"],"catalog":{"path":"<home>/.claude/model-gateway/catalog.json","schemaVersion":4}}`; skipped (launchers still written) when the existing file has an integer `schemaVersion > 1` | `:158-198` |

All three use `writeAtomically`: mkdir -p, write `<file>.<pid>.<uuid>.tmp` mode 0600, rename, unlink tmp (`:167-176`). JSON is 2-space + `\n`. Plugin root = `CLAUDE_PLUGIN_ROOT` ‖ `..` (`:125-127`). Launchers are also rewritten by `migrateReadinessGates` and `writeEnv` (`commands.js:600,1186`).

Stable launcher resolution of the installed CLI (`hooks/registry-writer.js:19-70`):

1. Registries: `MODEL_GATEWAY_CLAUDE_HOME` set → only `<it>/plugins/installed_plugins.json`; else `CLAUDE_CODE_PLUGIN_CACHE_DIR/installed_plugins.json` (if set) and `<CLAUDE_CONFIG_DIR‖~/.claude>/plugins/installed_plugins.json`, deduplicated.
2. From each parsable registry, every install under keys starting `model-gateway@` with an `installPath` whose `bin/model-gateway.js` exists.
3. Sort by version (split on non-digits, numeric compare), descending; tie → `lastUpdated` string descending. First wins.
4. Non-gate mode: `spawnSync(node, [cli, ...argv], {stdio:'inherit'})`, exit = status; no CLI → message, exit 1.

`runtime.resolveRegisteredCliPath` (`lib/runtime.js:718-747`) implements the same ordering but ignores `MODEL_GATEWAY_CLAUDE_HOME`. The launcher embeds the text of `lib/recovery-policy.js` at write time through template interpolation (`hooks/registry-writer.js:80-84`), so a launcher written by one version carries that version's policy.

### 4.3 Settings-installed readiness hooks (`lib/readiness-hooks.js`)

| Function | Behavior | Citation |
|---|---|---|
| `readinessCommand()` | Literal `node "<homedir>/.claude/model-gateway/model-gateway.js" readiness-gate` | `:5-7` |
| `ownedReadinessHook(h)` | `type === 'command'` and `command` **string-equal** to the above | `:9-11` |
| `syncReadinessHooks(settings, enabled)` | For `SessionStart` and `UserPromptSubmit`: drop owned hooks (and groups left empty), then append `{matcher:'startup\|resume'(SessionStart only), hooks:[{type:'command', command, timeout:35}]}` if enabled and `settings.disableAllHooks !== true`; delete empty events and an empty `hooks` key | `:13-46` |
| `readinessHookPresent(settings)` | Both events contain an owned hook | `:48-51` |
| Callers | `writeEnv` (`enabled = !remove`), `writeSettings` (`enabled` = base URL is ours; runs on every settings write), `migrateReadinessGate` | `commands.js:1189,610`, `lib/settings-wiring.js:107-109` |

### 4.4 `lib/recovery-policy.js` decision table

| Input precedence | Value |
|---|---|
| Base URL | `process.env.ANTHROPIC_BASE_URL` (any string, including empty) > `<cwd>/.claude/settings.local.json` > `<cwd>/.claude/settings.json` > `~/.claude/settings.json` |
| Enabled flag | first of the same three files with boolean `enabledPlugins["model-gateway@eigenwise-toolshed"]` (env not consulted) |

| Base URL ∈ {`http://127.0.0.1:<CODEX_GATEWAY_PORT‖18764>`, `http://api.anthropic.com`} | Enabled flag | Policy |
|---|---|---|
| no | any | `unwired` |
| yes | `false` | `disabled` |
| yes | `true` or absent | `recover` |

`effectivePluginEnabled()` is also used by `recoveryExplicitlyDisabled()` and doctor (`commands.js:627-631,595`). The plugin id is hard-coded to the `eigenwise-toolshed` marketplace.

### 4.5 Lifecycle diagnostics (`lib/lifecycle-diagnostics.js`)

`STATE/logs/lifecycle.jsonl`, one JSON per line `{at, event, component, pid, startedAt?, outcome?, errorType?, signal?, exitCode?, child?}` (`:13-27`). Before each append, a file with ≥ 200 lines is rewritten to its last 199 lines, non-atomically (`:29-43`). All errors swallowed. Events emitted in this slice: `supervisor-started`, `supervisor-exit`, `supervisor-uncaught-failure`, `supervisor-stop-requested`, `worker-stop-requested`, `worker-started`, `worker-exit`, `worker-request-lost`, `restart-worker-requested`, `restart-worker-stop-requested`, `<op>-recovery-started`, `<op>-recovery-finished` (extra `supervisorPid` field is dropped by `lifecycleRecord`), `<op>-owner-unknown`, `ensure-hook-wait-cut-short` (`commands.js:2462-2486,2565-2578,2626-2650,2669-2676,2683-2705,680-694,721,2989`).

---

## 5. Release acquisition

| Item | Behavior | Citation |
|---|---|---|
| Repository | `raine/claude-code-proxy` | `commands.js:114` |
| API call | `GET https://api.github.com/repos/<repo>/releases/latest`, header `accept: application/vnd.github+json`, `user-agent: model-gateway`, optional `authorization: Bearer <token>`; timeout = `fetchUrl` default 15000 ms | `lib/github-release.js:30-34,45-51`, `lib/process-supervision.js:25-42` |
| Token | `GH_TOKEN`, then `GITHUB_TOKEN` (trimmed); else `gh auth token --hostname github.com` (5 s, stdout only, status 0) | `lib/github-release.js:8-28` |
| Non-200 | `GitHub API returned <s>`; with a token `... using the token from <source>; check that it is valid for github.com`; unauthenticated 403/429 → rate-limit hint | `lib/github-release.js:36-43,49` |
| Asset name | `claude-code-proxy-<darwin\|linux\|windows>-<arm64\|amd64>.<tar.gz\|zip>`; arch is `arm64` only when `process.arch === 'arm64'`, everything else `amd64`; `linux` for every non-win32/non-darwin OS | `commands.js:444-447` |
| Checksum asset | asset name with `.zip`/`.tar.gz` replaced by `.sha256`; missing → refuse | `lib/release-verification.js:9-11` |
| Checksum format | trimmed body must match `/^([a-f0-9]{64})(?:[ \t]+\*?([^\r\n]+))?$/i`; a filename, if present, must equal the asset name | `lib/release-verification.js:15-18` |
| Downloads | checksum via `browser_download_url` (15 s), archive (120000 ms), both must be 200; sha256 of the body compared lower-case | `lib/release-verification.js:13-26` |
| Redirects | followed recursively for 3xx with `Location`; `authorization` dropped when origin changes | `lib/process-supervision.js:18-33` |
| Extraction | external `tar -xf` for both `.tar.gz` and `.zip` (that Windows `tar.exe` reads `.zip` is assumed by the code, not verified from source) | `commands.js:459-463` |
| Version comparison | No tag comparison: `latest` is always downloaded. "Changed" = parsed `--version` of staged vs current differ. Floor `MIN_PROXY_VERSION = 0.1.14` checked only by `ensure` | `commands.js:469-471,121,554-563` |
| Serving version | `STATE/proxy-serving-version.txt` (`<version>\n`), written by setup with the old version before a restart and by the supervisor when its proxy starts | `commands.js:381-388,404,2508` |

`lib/proxy-config.js` only reads: directory `CCP_CONFIG_DIR` ‖ win32 `%APPDATA%\claude-code-proxy` (fallback `~/AppData/Roaming`) ‖ darwin `~/.config/claude-code-proxy` ‖ other `${XDG_CONFIG_HOME‖~/.config}/claude-code-proxy`; file `config.json`, `null` when missing or not JSON (`:12-22`). Keys this slice reads: `vertex.project`, `opencode.apiKey` (`commands.js:1674-1685`); LM Studio keys via `lmStudioSettings` **[other lane]**.

---

## 6. Helper inventory (`commands.js`, grouped)

| Concern | Function (line) |
|---|---|
| Output | `log` 215, `noticeForUser` 220, `flushHookOutput` 227, `die` 241, `readPluginVersion` 242, `mkdirs` 248 |
| RC port diagnostics | `compatibilityPortOwnerIdentifiers` 269, `processNameForIdentifier` 278, `compatibilityPortConflict` 288 |
| Install / auth | `installScope` 318, `isAuthed` 327, `noteCodexRequestSuccess` 332 (exported only), `providerReadiness` 337 |
| Proxy binary | `proxyVersion` 347, `currentProxyVersion` 351, `oldProxyPath` 355, `sweepOldProxyBinaries` 359, `replaceProxyBinary` 368, `read/writeProxyServingVersion` 381/385, `waitForProxyExit` 390, `restartProxyForVersionChange` 398, `restartProxyIfOutdated` 410, `restartSetupProxy` 425, `restartAfterSetup` 430, `warnIfProxyOutdated` 554 |
| Version helpers | `servingVersionIsCurrentOrNewer` 531 (unused), `servingVersionIsNewer` 538, `staleSessionReloadNotice` 544 |
| Lifecycle | `startupWaitMsFor` 565, `ensureWiredGateway` 571, `automaticLifecycle` 577, `reportReadinessGate` 588, `migrateReadinessGates` 599, `migrateReadinessGate` 606, `startupFailureFor` 614, `writeStartupFailure` 623, `recoveryExplicitlyDisabled` 627, `recoverGateway` 633, `readObservedStartupFailure` 640, `waitForStartupReadiness` 645, `reportSiblingSupervisorReplacement` 665, `recoveryOutcome` 669, `lifecycleRecovery` 674, `readSettledShimState` 702, `shimStartPlan` 710, `refuseForeignShim` 716, `keepRunningShim` 725, `waitForRunningShim` 735, `replaceShim` 739, `clearShimForStart` 745, `launchSupervisor` 757, `finishStartingShim` 768, `processAlive` 786, `reportShimReady` 796, `noticeStaleSession` 802, `startAll` 807, `startAllLocked` 811, `restartSupervisorForVersionMismatch` 888, `restartShimIfOutdated` 897 |
| RC control client | `unknownServingCompatibility` 841, `hasServingCompatibilityHealth` 845, `requestServingCompatibility` 852 (POST 3 s), `resolveIntendedMode` 915, `syncCompatMode` 1226 |
| Status | `reportAdvertisedModels` 927, `reportShimState` 936, `reportCompatibilityListener` 944, `statusReport` 955, `reportContextWindows` 970 |
| Context window | `applyClaudeWindowChange` 978, `reportClaudeWindowSync` 987, `refreshRegisteredClaudeWindow` 994, `contextWindowCommand` 1001, `configuredAutoCompactWindow` 1249 |
| Pins / wiring | `reportRegisteredPinSync` 1022, `refreshRegisteredProjectPins` 1031, `refreshDetectedPinsAndWiring` 1038, `refreshCatalogPinsAndWiring` 1048, `reportPinChanges` 1056, `reportOutdatedOverride` 1063, `reportEffectivePins` 1068, `updatePinOverride` 1075, `pinCommand` 1088, `wiredGatewayEnv` 1105, `reportStalePickerRowsOncePerCli` 1111, `syncGatewayWiring` 1119, `syncUnwiredPins` 1130, `envCommand` 1134, `writeEnv` 1176, `sessionStartWiringNotice` 2908, `loginSuccessMessage` 2928 |
| Doctor policy | `windowValue` 1265, `evidenceDate` 1270, `nativeModelPolicyIds` 1277, `modelWindowPolicyRow` 1284, `policyGatewayModelIds` 1304, `modelWindowPolicyRows` 1318, `expectedShimModelIds` 1324, `modelIdDifference` 1330, `reportModelWindowPolicy` 1339, `reportLiveShimModelPolicy` 1360, `reportRegisteredProjectPinDisagreements` 1381, `reportProjectWiringIssues` 1391, `doctor` 1397 |
| Provider readiness | `unavailableProviderReadiness` 1637, `getGrokReadiness` 1645, `vertexProjectIsSet` 1674, `proxyProviderAuthState` 1680, `localWindowMismatch` 1702, `localServerReadinessState` 1710, `localServerRestartHint` 1717, `lmStudioReadinessState` 1741, `lmStudioSettingPlaces` 1749, `proxyProviderReason` 1781, `proxyProviderCredentialCheck` 1805, `describeLocalServer` 1813, `describeLmStudioServer` 1824, `describeProxyProviderAuth` 1839, `getProxyProviderReadiness` 1853, `providerCatalogReadiness` 1870; tables at 1723, 1753, 1774, 1788, 1793 |
| Catalog | `displayName` 1534, `gatewayModel` 1555 (unused), `slugFor` 1583, `labelFor` 1611, `modelCatalogDetails` 1622, `catalogContextWindow` 1878, `buildCatalog` 1885, `readJsonFile` 1911, `catalogSchemaVersion` 1915, `catalogModelIds` 1921, `mergeSubsetCatalog` 1929, `writeCatalogFile` 1950, `fetchShimModels` 1966, `discoveryCacheBaseUrl` 1979, `writeGatewayDiscoveryCache` 1986, `cacheAgeLabel` 1998, `reportGatewayDiscoveryCache` 2003, `loadProxyModelRoutes` 2021, `writeCatalog` 2033, `warnShimStillStarting` 2054, `finishUpdateWithoutWiring` 2058, `readCatalog` 2064, `requireShimHealth` 2070, `catalogCommand` 2078 |
| Route telemetry (dead here) | `loopbackTelemetryEndpoint` 2103, `parseIncomingTraceparent` 2129, `safeMetadataId` 2139, `otlpAttribute` 2144, `postRouteSpan` 2154, `routeStatus` 2182, `buildRouteTelemetry` 2190, `createRouteTelemetry` 2254 — never called in `commands.js` (`grep` shows only definitions) |
| Shim relay | `requestHeader` 2262, `uploadResponseUnavailable` 2269, `sendUploadError` 2273, `rejectOversizedUpload` 2285, `collectShimRequestBody` 2297, `readShimRequestBody` 2311, `parseCompatibilityControl` 2325, `createShimRelay` 2332, `runShim` 2456 (inner: `trackSupervisorWrite`, `closeServer`, `serializeCompatibility`, `stopCompatibilityProbe`, `waitForWorkerExit`, `stopSupervisor`, `clearWorkerPortReportTimeout`, `stopForMissingWorkerPort`, `startWorker`, `workerReady` (unused), `restartWorker`, `controlRequestAllowed`, `sendControlResponse`, `probeCompatibilityListener`, `reconcileCompatibilityListener`, `handleCompatibilityControl`, `supervisorRequestAllowed`, `handle`, `monitorProxy`, `listen`) |

Dependency edges (command → lib module):

| Command | Modules |
|---|---|
| `setup` | github-release, release-verification, process-supervision (fetchUrl, stopProcessAsync, resolvePortOwner, restartWorkerWithDrain, stopRunningSupervisor, spawnDetached, reapGatewayOrphans), shim-state, lifecycle-lock, pins, settings-wiring, readiness-hooks, hooks/registry-writer, codex-upstream-state, request-worker (getCodexReadiness), context-window, runtime, atomic-file, previous-picker-rows, anthropic-catalog, proxy-models, local-llama, lmstudio |
| `ensure` | all of `setup` except github-release/release-verification, plus picker-guard, remote-control (detectHostsCompat via shim health only), lifecycle-diagnostics |
| `start`/`recover`/`stop` | process-supervision, shim-state, lifecycle-lock, lifecycle-diagnostics, runtime, request-worker (via catalog refresh) |
| `status`/`doctor` | request-worker, shim-state, process-supervision, context-window, anthropic-catalog, pins, settings-wiring, readiness-hooks, lifecycle-lock, lifecycle-diagnostics, picker-guard, grok-backend, proxy-models, local-llama, lmstudio, proxy-config, runtime |
| `env`/`pin`/`context-window` | settings-wiring, pins, readiness-hooks, hooks/registry-writer, context-window, runtime |
| `catalog`/`models` | process-supervision (fetchUrl), shim-state, proxy-models, local-llama, lmstudio, runtime, request-worker, atomic-file |
| `remote-control` | remote-control, settings-wiring, process-supervision |
| `serve-shim` | control-auth, process-supervision (createProxyRecovery, createProbeChildRegistry, writePidRecordAsync, killPidAsync, processOwningPortAsync, removePid, portListening, postJson), remote-control, lifecycle-diagnostics, runtime |
| `readiness-gate` | recovery-policy, runtime, lifecycle-lock, process-supervision, shim-state, settings-wiring, commands |

---

## 7. Node-specific constructs and platform branches

| Construct | Where | What the Go port must reproduce |
|---|---|---|
| Self re-exec with `process.execPath` | `commands.js:763`; launcher `hooks/registry-writer.js:106,120`; `hooks/ensure.js:47` | Supervisor launch runs `<node> <newest sibling cli> serve-shim`, so the command line must keep a path that `gatewayInstallRootFromCommand` (`lib/process-supervision.js:510-514`) recognizes (`.../model-gateway[/x.y.z]/bin/model-gateway.js`). A Go binary needs a new, recognizable path shape or ownership checks break against running Node supervisors. |
| Detached spawn (POSIX) | `lib/process-supervision.js:981-991` | `setsid`/new process group, stdin `/dev/null`, stdout+stderr appended to `logs/guardian.log`, no wait. |
| Detached spawn (Windows) | `lib/windows-detached.js:9-57` | PowerShell `Invoke-CimMethod Win32_Process Create` with `CreateFlags 16778240` (`0x01000400` = `CREATE_BREAKAWAY_FROM_JOB \| CREATE_UNICODE_ENVIRONMENT`), env passed through stdin, a node launcher that writes `result.json`, 5 s acknowledgement poll with `Atomics.wait`. Purpose: escape the hook's Job Object. |
| `fork` + IPC channel | `commands.js:2619-2639,2900-2904` | Worker port arrives as IPC `{type:'listening',port}`; supervisor also accepts IPC `fixture-shutdown`; `child.disconnect()` on shutdown. Go needs a replacement channel (pipe/fd, or a port file) and a matching worker. |
| `spawnSync` with `input`, `timeout`, `killSignal:'SIGKILL'` | `hooks/registry-writer.js:106` | Gate child gets stdin bytes, 25 s hard kill; timeout surfaces as `result.error`. |
| `spawnSync` without timeout | `commands.js:352,469,470,489,2940` | `--version` and login have no deadline; decide whether to keep. |
| `stdio: 'inherit'` | `commands.js:2940`, `hooks/ensure.js:47`, launchers | Interactive OAuth must own the terminal. |
| `windowsHide: true` | every spawn | `SysProcAttr.HideWindow` on Windows. |
| Signals | `commands.js:2898-2899` SIGTERM/SIGINT → graceful stop; `process.kill(pid,0)` with EPERM = alive (`:786-794`, `lib/lifecycle-lock.js:25-28`) | `syscall.Kill(pid,0)` semantics on POSIX; the Windows equivalent of Node's `process.kill(pid, 0)` has to be chosen explicitly (Node's Windows behavior not verified from source). |
| Process termination | `lib/process-supervision.js:193-200` | POSIX SIGTERM/SIGKILL; Windows `taskkill /pid N /T /F` (tree kill). Success judged by the target being gone, not the kill status (`:219-224`). |
| Port owner lookup | `commands.js:269-276`, `lib/process-supervision.js:242-289` | POSIX `lsof -nP -iTCP:<port> -sTCP:LISTEN -t` then a `/proc` socket-inode fallback (`processOwningPortInProc`, `:242-270`, body not read in detail); Windows `netstat -ano -p tcp` parsed language-independently (`:287-290`). Probes run with `LC_ALL=C`, async probe children `detached: !WIN`. |
| Process info | `lib/process-supervision.js:307-348` | `ps -ww -p <pid> -o pid=,ppid=,lstart=,args=` / `ps -ww -eo ...`; Windows `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "Get-CimInstance Win32_Process ... \| ConvertTo-Json -Compress"`. `lstart` text is the process identity used by the lock and pid records. |
| Process name | `commands.js:278-286` | `ps -p <pid> -o comm=` / `tasklist /FI "PID eq N" /FO CSV /NH` (first CSV field). |
| `process.platform` / `process.arch` | `commands.js:79,444-447,459-461,468`; `lib/proxy-config.js:14-16`; `lib/remote-control.js:12-14,121-126` | Asset naming, `.exe`, tar path, chmod, config dir, hosts path, elevation text. |
| Shell quoting | `lib/windows-detached.js:25-27` (wraps in `"`, doubles backslashes before `"` and at the end, escapes `"` as `\"`); hook command strings embed `"<path>"` (`lib/readiness-hooks.js:6`) | Paths with `"` or spaces; settings hook must be the identical string to be recognized. |
| Sync sleep `Atomics.wait` | `lib/atomic-file.js:24`, `lib/windows-detached.js:46` | Rename retry on `EPERM/EACCES/EBUSY` with 20/60/140/300 ms delays (Windows file sharing). |
| Stdin read `fs.readFileSync(0)` | `lib/readiness-gate.js:46`, `hooks/registry-writer.js:75` | Read all of stdin; empty or partial input is tolerated. |
| `process.chdir(payload.cwd)` | `lib/readiness-gate.js:48`, `hooks/registry-writer.js:79` | All later relative settings paths derive from cwd (`lib/settings-wiring.js:30-36`). |
| `process.exit` inside `withLifecycleLock` | `commands.js:241,2968` | Exits skip the `finally` release; Go `os.Exit` skips `defer` the same way, so choose explicitly. |
| `process.exitCode` accumulation | `commands.js:1377,1452,1489,1493,1509,1517,2098` | Several checks set 1 and the command continues; exit at the end. |
| `AsyncLocalStorage` re-entrant lock | `lib/lifecycle-lock.js:11,88-94` | Nested `startAll` inside `setup`/`start` must not deadlock; pass a lock token through `context.Context`. |
| HTTP server knobs | `commands.js:2869-2871,2499` | No request timeout, 120 s header timeout, 75 s keep-alive; client agent without keep-alive to the worker. |
| `http.get` redirect following | `lib/process-supervision.js:25-42` | Unbounded redirect recursion; strip `Authorization` across origins. |
| `fs.readdirSync({recursive:true})` | `commands.js:464-465` | `filepath.WalkDir` over the stage directory. |
| `fs.mkdtempSync` | `commands.js:456`, `lib/lifecycle-lock.js:60`, `lib/windows-detached.js:30` | `os.MkdirTemp` with the same prefixes (sweepers rely on them). |
| `fs.writeFileSync(..., {flag:'wx', mode:0o600})` | `lib/control-auth.js:28` | `O_CREATE\|O_EXCL`, 0600. |
| Unref'd timers | `commands.js:2290,2884` | Timers must not keep a CLI alive; in Go only relevant for the long-running supervisor. |
| `uncaughtExceptionMonitor` | `commands.js:2476-2486` | Record crash + write failure file; Go equivalent is a top-level `recover` in each goroutine. |

---

## 8. Open questions, dead code, doc/code inconsistencies

| # | Item | Evidence |
|---|---|---|
| 1 | No `restart` command, although the lead's brief and README advice ("`stop`, then `ensure`") describe restart flows. Port only what exists, or add one? | `commands.js:2935-3041`; README:68,161 |
| 2 | SessionStart no longer runs `ensure --quiet` in this fork. `hooks/hooks.json:2` description, README:110, SKILL.md:60 and SKILL.md:127 still say it does. `hooks/ensure.js`, the hook JSON (`commands.js:227-238`) and `ensure-hook-wait-cut-short` are reachable only by hand. Decide whether to port them. | commit `101a64fd2`; `hooks/hooks.json:4-14` |
| 3 | `readiness-gate` map entry is shadowed by bin's early dispatch. | `commands.js:2950`, `bin/model-gateway.js:5` |
| 4 | Dead constants/imports in `commands.js` (one occurrence = definition only): `LEGACY_CODEX_PREFIX`, `DISPATCH_ROUTE_CACHE_PATH`, `LIST_DISPATCH_MODEL`, `TRACE_HEADERS`, `AUTH_HEADERS`, `ANTHROPIC_UPSTREAM`, `GROK_ENDPOINT`, `REQUEST_ROUTE_LOG`, `PLAN_TOOLS`, `DEFAULT_MODELS`, `DEFAULT_GROK_MODELS`, `cmd`, `zlib`, `StringDecoder`, `createGatewayUsageEmitter`, `recordRequestBodyHighWater`, `STATIC_ENV_BLOCK`, `detectedPinDefaults`, `stalePinUpdates`, `gatewayEnvBlock`, `waitForShimExit`; dead functions `gatewayModel`, `servingVersionIsCurrentOrNewer`, `workerReady`, the route-telemetry group (2103-2260). Most duplicate `lib/runtime.js:27-43`. | `grep -c` per symbol; `commands.js:102-133,1543-1564,2103-2260,2657` |
| 5 | `restartShimIfOutdated` passes `restartWorker` to `restartSupervisorForVersionMismatch`, which ignores it. | `commands.js:888,901,908` |
| 6 | `models`, `catalog` (`requireShimHealth`, `fetchShimModels`) and shim health use `SHIM_PORT`, not `PUBLIC_SHIM_PORT`; they differ only if `CODEX_GATEWAY_WORKER_PORT` is set, in which case the CLI bypasses the supervisor (and its `/healthz` augmentation). Intended? | `commands.js:89,1970,2072,3029`, `lib/shim-state.js:9` |
| 7 | State root uses `os.homedir()` while registry lookup honors `CLAUDE_CONFIG_DIR`; recovery policy reads `~/.claude/settings.json` from homedir too. A relocated config dir splits state. | `lib/runtime.js:15-16`, `lib/recovery-policy.js:10`, `lib/settings-wiring.js:33` |
| 8 | Launcher registry search honors `MODEL_GATEWAY_CLAUDE_HOME`; `runtime.resolveRegisteredCliPath` does not. The two can pick different installs. | `hooks/registry-writer.js:42-51`, `lib/runtime.js:682-696` |
| 9 | `ensure --quiet` reports "still starting" whenever the wait timed out, even if the supervisor died (`waitCutShort = quiet` regardless of `stillStarting`). | `commands.js:775-783,2987-2997` |
| 10 | `warnIfProxyOutdated` writes to stderr directly, so in `--quiet` mode its line is outside the JSON contract. | `commands.js:560` |
| 11 | `automaticLifecycle` notices (disabled / intentionally stopped) are emitted before buffering starts, so `--quiet` prints no JSON for them. | `commands.js:577-586,2963,3043-3044` |
| 12 | If the stop marker exists but `startAll` keeps a running current shim (`keepRunningShim`), `resumeIntentionalStop` is never called, so `start` can succeed and leave the marker; the next gate then blocks. Not verified whether reachable. | `commands.js:725-733,757-766` |
| 13 | Readiness gate fast path requires serving version **equal** to the installed version, while `startAll` accepts serving ≥ installed; a newer serving shim forces a `recover` on every prompt (which then keeps it). Cost only, but measurable. | `lib/readiness-gate.js:28-32`, `lib/shim-state.js:67-76` |
| 14 | `setup` always downloads `releases/latest`; there is no pinned or minimum proxy version and no offline path. | `commands.js:449-454` |
| 15 | `lifecycleRecord` drops `supervisorPid` passed by `lifecycleRecovery.finish`. | `commands.js:692`, `lib/lifecycle-diagnostics.js:13-27` |
| 16 | Hook command recognition is exact string equality on a path built from `os.homedir()`; a Go port changing the command (or the user's home path changing) leaves orphan hooks that `syncReadinessHooks` cannot remove. Launchers are Node scripts, so Node stays a runtime dependency unless the launcher scheme changes. | `lib/readiness-hooks.js:5-11`, `hooks/registry-writer.js:11-123` |
| 17 | `recordGatewayLifecycle` trims non-atomically while the supervisor and CLI may append concurrently; lines can be lost. | `lib/lifecycle-diagnostics.js:29-43` |
| 18 | `createProxyRecovery`, worker drain, `runWorker`, picker guard and pin detection behavior are UNREAD here; their timeouts and outputs feed `doctor`, `status` and `serve-shim`. | other lanes |
