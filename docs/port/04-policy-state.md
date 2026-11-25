model: claude-opus-5-5

# model-gateway port inventory 04: policy, configuration and persisted state

Source: `/Users/zchee/src/github.com/Eigenwise/eigenwise-toolshed/plugins/model-gateway`, commit `a8501f251755` (written against `4ffb2f672`; moved to this commit on 2026-11-25, see `07-repin.md`), branch `dev`.
Written 2026-11-25 21:36 JST (time from `date` in the shell call that checked the line numbers just before this file was written).
All citations are `path:line` relative to the plugin root. Nothing was executed; every statement comes from reading.
Files other lanes own were opened only to resolve calls from this slice: `lib/atomic-file.js`, `lib/readiness-hooks.js`,
`lib/proxy-config.js`, `hooks/registry-writer.js`, and short ranges of `lib/request-worker.js`, `lib/commands.js` and
`lib/context-window.js`. Each such use is cited. "UNREAD" marks anything outside what was read.

Notation: `STATE` = `$HOME/.claude/model-gateway` (see section 2). "load" = read once when the module is first `require`d;
"call" = read each time the function runs.

---

## 1. Environment variables

### 1.1 Read in this slice

| Name | Default | Type / parse | Meaning | When | Citation |
|---|---|---|---|---|---|
| `CLAUDE_CONFIG_DIR` | `$HOME/.claude` | path, `\|\|` fallback (empty string means unset) | Claude Code config dir; used for discovery cache path only in runtime | load | `lib/runtime.js:15` |
| `CLAUDE_CONFIG_DIR` | `$HOME/.claude` | path | base of `plugins/` registry search | call | `lib/runtime.js:685` |
| `CLAUDE_CONFIG_DIR` | `$HOME/.claude.json` when unset | path | global config `.claude.json` location (`$CLAUDE_CONFIG_DIR/.claude.json`) | call | `lib/previous-picker-rows.js:13-17` |
| `CLAUDE_CONFIG_DIR` | `$HOME/.claude` | path | transcripts dir `<dir>/projects` for project-name lookup | per resolver construction | `lib/usage-observability.js:134` |
| `CLAUDE_CODE_PLUGIN_CACHE_DIR` | unset | path, `path.resolve`d | extra plugin registry dir, searched first | call | `lib/runtime.js:684` |
| `CODEX_GATEWAY_PORT` | `18764` | `Number(env \|\| 18764)` (NaN possible) | public shim port; also builds `DEFAULT_BASE_URL` | load | `lib/runtime.js:24,46` |
| `CODEX_GATEWAY_WORKER_PORT` | = public port | `Number` | worker listen port | load | `lib/runtime.js:25` |
| `CODEX_GATEWAY_PROXY_PORT` | `18765` | `Number` | claude-code-proxy port | load | `lib/runtime.js:26` |
| `CODEX_GATEWAY_GROK_ENDPOINT` | `grokBackend.GROK_ENDPOINT` | string | Grok backend URL (value UNREAD, grok lane) | load | `lib/runtime.js:31` |
| `CODEX_GATEWAY_ANTHROPIC_UPSTREAM` | `https://api.anthropic.com` | URL string | upstream for passthrough and the catalog fetch | load | `lib/runtime.js:34`, `lib/anthropic-catalog.js:61` |
| `CODEX_GATEWAY_REQUEST_LOG` | on | `!== '0'` | route log enable | load | `lib/runtime.js:35` |
| `CODEX_GATEWAY_REQUEST_LOG_PATH` | `STATE/logs/request-routes.jsonl` | path | route log path | load | `lib/runtime.js:36` |
| `CODEX_GATEWAY_DISPATCH_CACHE_PATH` | `STATE/dispatch-routes.json` | path | dispatch route cache file | load | `lib/runtime.js:38` (also re-derived at `lib/commands.js:128`) |
| `CODEX_GATEWAY_LIST_DISPATCH_MODEL` | off | `=== '1'` | list the virtual `auto` dispatch row | load | `lib/runtime.js:39`, used `lib/request-worker.js:1511` |
| `CLAUDE_CODE_PROPAGATE_TRACEPARENT` | off | `=== '1'` | route telemetry enable | load | `lib/runtime.js:40` |
| `CODEX_GATEWAY_COMPAT_PORT` | `80` | `Number` | RC-compat listener port | load | `lib/runtime.js:45` |
| `CODEX_GATEWAY_SOCKET_PATH` | `STATE/gateway.sock`; Windows `\\.\pipe\model-gateway` | path | control socket | load | `lib/runtime.js:47-49` |
| `CODEX_GATEWAY_CONTEXT_WINDOW` | none | `parseContextWindowValue('codex', v)` (section 3.4) | legacy Codex cap, below the saved file | load (`CONTEXT_WINDOWS`) and per call when commands re-read settings (`lib/commands.js:995,1006`) | `lib/runtime.js:272-279,319` |
| `CODEX_GATEWAY_COMPACT_TRIGGER` | none (NaN) | `Number(env)`; used only if finite and > 0 | global compact trigger candidate for every synthetic-413 row | load | `lib/runtime.js:320,339,358` |
| `CODEX_GATEWAY_PIN_CACHE_TTL_MS` | `86400000` (24 h) | `Number(env) \|\| default` (0/NaN fall back) | detected-pin cache TTL | load | `lib/runtime.js:547` |
| `CODEX_GATEWAY_PIN_PROBE_TIMEOUT_MS` | `5000` | `Number(env) \|\| default` | `claude` probe timeout; retry uses 2x | load | `lib/runtime.js:548`, `lib/pins.js:323` |
| `CODEX_GATEWAY_CLAUDE_BIN` | `claude` | command name/path | Claude CLI used by pin probes | load | `lib/runtime.js:549`; batch detection `:550` (`/\.(?:cmd\|bat)$/i` on Windows → `shell: true`) |
| `NODE_TEST_CONTEXT` | unset | truthy | under node's test runner: refuse to run real `claude` (unless `CODEX_GATEWAY_CLAUDE_BIN` set); refuse settings writes outside `os.tmpdir()` | call | `lib/pins.js:212-214`, `lib/settings-wiring.js:88-105` |
| `NO_PROXY`, `no_proxy` | unset | comma list | prepended to `127.0.0.1,localhost` for the probe child | call | `lib/pins.js:248-250` |
| (whole `process.env`) | — | — | probe child inherits env minus an exclusion set (section 5.6) | call | `lib/pins.js:246-247` |
| `ANTHROPIC_BASE_URL` | unset | string; any `typeof === 'string'` (even empty) counts | highest-precedence base-URL definition (`source: 'env'`) | call | `lib/settings-wiring.js:40-42` |
| `HOME` | — | — | only interpolated into an error message | call | `lib/settings-wiring.js:104` |
| `CCP_LMSTUDIO_ENABLED` | → config → `true` | `1/true/yes/on` → true, `0/false/no/off` → false (trim, lowercase), else unset | LM Studio provider on/off | call | `lib/lmstudio.js:17,28-32,75` |
| `CCP_LMSTUDIO_BASE_URL` | → config `lmstudio.baseUrl` → `http://127.0.0.1:1234` | trimmed, blank = unset | LM Studio server origin | call | `lib/lmstudio.js:16,18,78-80` |
| `CCP_LMSTUDIO_API_KEY` | → `LM_API_TOKEN` → config `lmstudio.apiKey` | trimmed, blank = unset | bearer token for LM Studio probe | call | `lib/lmstudio.js:19,84` |
| `LM_API_TOKEN` | — | as above | second-choice LM Studio token | call | `lib/lmstudio.js:21,84` |
| `CCP_CONFIG_DIR`, `APPDATA`, `XDG_CONFIG_HOME` | platform default | path | claude-code-proxy config dir (resolved in `proxy-config.js`, called from lmstudio) | call | `lib/proxy-config.js:12-17` |
| `CCP_LLAMA_PORT` | unset | `/^\+?\d+$/` then `Number`, 1..65535 | llama-server port for `local/gguf` | call | `lib/local-llama.js:13,17-22` |
| `MODEL_GATEWAY_REQUEST_BODY_DIR` | `$HOME/.claude/model-gateway/request-body` | path, `\|\|` fallback | high-water file dir | load | `lib/usage-observability.js:23-24` |
| `CODEX_GATEWAY_USAGE_ENDPOINT` | — | `'0'` disables; else loopback URL | OTLP logs endpoint, 1st choice | per emitter creation | `lib/usage-observability.js:621-623` |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | — | loopback URL | 2nd choice | per emitter | `lib/usage-observability.js:624-626` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | loopback URL + `/v1/logs` appended | 3rd choice | per emitter | `lib/usage-observability.js:627-629` |
| `CODEX_GATEWAY_USAGE_MAX_RESPONSE_BYTES` | `4194304` | `Number(...) \|\| default`, then `max(1024, …)` | side-buffer ceiling for response parsing | per emitter | `lib/usage-observability.js:986-988` |
| `CODEX_GATEWAY_DISPATCH_CACHE_TTL_MS` | `14400000` (4 h) | finite and > 0 | context-snapshot TTL (shared name with dispatch cache) | per emitter | `lib/usage-observability.js:989-992` |
| `CODEX_GATEWAY_DISPATCH_CACHE_MAX_SESSIONS` | `500` | `Number.isInteger` and > 0 | context-snapshot identity cap | per emitter | `lib/usage-observability.js:993-996` |

`proxy-models.js` and `anthropic-catalog.js` read no environment variables directly.

### 1.2 Read elsewhere (names only; from one grep over `lib/` plus `hooks/registry-writer.js`)

| File | Variables |
|---|---|
| `lib/commands.js` | `ANTHROPIC_BASE_URL`, `ANTHROPIC_UNIX_SOCKET`, `CCP_OPENCODE_API_KEY`, `CCP_VERTEX_PROJECT`, `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, `CLAUDE_CODE_PROPAGATE_TRACEPARENT`, `CODEX_GATEWAY_ANTHROPIC_UPSTREAM`, `CODEX_GATEWAY_DISPATCH_CACHE_PATH`, `CODEX_GATEWAY_DRAIN_TIMEOUT_MS`, `CODEX_GATEWAY_GROK_ENDPOINT`, `CODEX_GATEWAY_LIST_DISPATCH_MODEL`, `CODEX_GATEWAY_PORT`, `CODEX_GATEWAY_PROXY_PORT`, `CODEX_GATEWAY_PROXY_RECOVERY_INTERVAL_MS`, `CODEX_GATEWAY_REQUEST_LOG`, `CODEX_GATEWAY_REQUEST_LOG_PATH`, `CODEX_GATEWAY_TELEMETRY_ENDPOINT`, `CODEX_GATEWAY_WORKER_PORT`, `GOOGLE_CLOUD_PROJECT`, `MODEL_GATEWAY_STARTUP_GENERATION`, `OPENCODE_API_KEY`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` |
| `lib/request-worker.js` | `ANTHROPIC_BASE_URL`, `CODEX_GATEWAY_COMPACT_STREAM_GUARD`, `CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES`, `CODEX_GATEWAY_COMPACT_STREAM_RETRIES`, `CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS`, `CODEX_GATEWAY_DISPATCH_CACHE_MAX_SESSIONS`, `CODEX_GATEWAY_DISPATCH_CACHE_TTL_MS`, `CODEX_GATEWAY_KEEP_PLAN_TOOLS`, `CODEX_GATEWAY_LMSTUDIO_FIRST_EVENT_WAIT_MS`, `CODEX_GATEWAY_MODELS_REFRESH_MS`, `CODEX_GATEWAY_SENTRY`, `CODEX_GATEWAY_SSE_HEARTBEAT_S`, `CODEX_GATEWAY_TELEMETRY_ENDPOINT`, `CODEX_GATEWAY_WORKER_PORT`, `CODEX_GATEWAY_WS_UPGRADE_RETRIES`, `CODEX_GATEWAY_WS_UPGRADE_RETRY_DELAY_MS`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` |
| `lib/context-window.js` | `CODEX_GATEWAY_COMPACT_TRIGGER` |
| `lib/grok-backend.js` | `CODEX_GATEWAY_GROK_HOME` |
| `lib/process-supervision.js` | `CODEX_GATEWAY_DRAIN_TIMEOUT_MS`, `CODEX_GATEWAY_PROBE_TIMEOUT_MS` (also passes whole env to children, `LC_ALL=C` override at `:237`) |
| `lib/recovery-policy.js` | `ANTHROPIC_BASE_URL`, `CODEX_GATEWAY_PORT` |
| `lib/remote-control.js` | `CODEX_GATEWAY_HOSTS_FILE` |
| `lib/github-release.js` | `GH_TOKEN`, `GITHUB_TOKEN` (`:8`) |
| `lib/proxy-config.js` | `CCP_CONFIG_DIR`, `APPDATA`, `XDG_CONFIG_HOME` |
| `hooks/registry-writer.js` | `MODEL_GATEWAY_CLAUDE_HOME` (`:43`), `CLAUDE_CODE_PLUGIN_CACHE_DIR`, `CLAUDE_CONFIG_DIR` (`:42-51`), `CLAUDE_PLUGIN_ROOT` (`:126`) |

The grep matched `env.NAME`, `env?.NAME`, `environment.NAME` and `env[NAME]`; a name built dynamically would be missed.
`bin/` and the rest of `hooks/` were not grepped.

---

## 2. Paths

### 2.1 Home and config directory resolution

| Concept | Resolution | Citation |
|---|---|---|
| Gateway state root `STATE` | `path.join(os.homedir(), '.claude', 'model-gateway')`. Ignores `CLAUDE_CONFIG_DIR`, not overridable. | `lib/runtime.js:16` (duplicated `lib/commands.js:80`) |
| `CLAUDE_CONFIG_DIR` constant | `env.CLAUDE_CONFIG_DIR \|\| $HOME/.claude` (not `path.resolve`d) | `lib/runtime.js:15` |
| Plugin registry dirs | `[resolve(CLAUDE_CODE_PLUGIN_CACHE_DIR)?, resolve(CLAUDE_CONFIG_DIR \|\| $HOME/.claude)/plugins]`, deduplicated | `lib/runtime.js:682-689` |
| Same, in the hook launcher | `MODEL_GATEWAY_CLAUDE_HOME` set → only `<it>/plugins/installed_plugins.json`; else same list as above | `hooks/registry-writer.js:40-51` |
| User settings | `os.homedir()/.claude/settings.json` (ignores `CLAUDE_CONFIG_DIR`) | `lib/settings-wiring.js:33` |
| Project-local settings | `process.cwd()/.claude/settings.local.json` | `lib/settings-wiring.js:31` |
| Project-shared settings (also "legacy-project") | `process.cwd()/.claude/settings.json` | `lib/settings-wiring.js:32` |
| Claude global config | `$CLAUDE_CONFIG_DIR/.claude.json` if set, else `$HOME/.claude.json` | `lib/previous-picker-rows.js:13-17` |
| Transcripts | `$CLAUDE_CONFIG_DIR/projects` or `$HOME/.claude/projects` | `lib/usage-observability.js:134` |
| Readiness hook launcher | `os.homedir()/.claude/model-gateway/model-gateway.js` | `hooks/registry-writer.js:141-143` |
| `os.homedir()` | Node: `$HOME` on POSIX (then passwd), `USERPROFILE` on Windows | Node behaviour, not plugin code |

So the home resolution is split three ways: state, user settings and the hook command use `os.homedir()`. The discovery cache,
transcripts and `.claude.json` use `CLAUDE_CONFIG_DIR`. `MODEL_GATEWAY_CLAUDE_HOME` exists only in the hook launcher.

### 2.2 Path constants

| Constant | Value | Overridable | Citation |
|---|---|---|---|
| `LOGS` | `STATE/logs` | no | `lib/runtime.js:17` |
| `BIN_DIR` | `STATE/bin` | no | `lib/runtime.js:18` |
| `WIRING_CONFIG_PATH` | `STATE/wiring.json` | no | `lib/runtime.js:19` |
| `PROJECT_WIRING_REGISTRY_PATH` | `STATE/wired-projects.json` | no | `lib/runtime.js:20` |
| `SHIM_FAILURE_PATH` | `STATE/shim-supervisor-failure.txt` | no | `lib/runtime.js:21` |
| `CODEX_UPSTREAM_BLOCK_PATH` | `STATE/codex-upstream-blocked.json` | no | `lib/runtime.js:22` |
| `PROXY_BIN` | `STATE/bin/claude-code-proxy` (`.exe` on Windows) | no | `lib/runtime.js:23` |
| `REQUEST_ROUTE_LOG_PATH` | `STATE/logs/request-routes.jsonl` | `CODEX_GATEWAY_REQUEST_LOG_PATH` | `lib/runtime.js:36` |
| `LIFECYCLE_LOG_PATH` | `STATE/logs/lifecycle.jsonl` | no | `lib/runtime.js:37` |
| `DISPATCH_ROUTE_CACHE_PATH` | `STATE/dispatch-routes.json` | `CODEX_GATEWAY_DISPATCH_CACHE_PATH` | `lib/runtime.js:38` |
| `SOCKET_PATH` | `STATE/gateway.sock`; Windows `\\.\pipe\model-gateway` | `CODEX_GATEWAY_SOCKET_PATH` | `lib/runtime.js:47-49` |
| `CONTEXT_WINDOW_PATH` | `STATE/context-window.json` | no (function takes a `file` arg) | `lib/runtime.js:244` |
| `PIN_OVERRIDE_PATH` | `STATE/pins.json` | no | `lib/runtime.js:545` |
| `PIN_CACHE_PATH` | `STATE/detected-pins.json` | no | `lib/runtime.js:546` |
| `GATEWAY_MODELS_CACHE` | `CLAUDE_CONFIG_DIR/cache/gateway-models.json` | via `CLAUDE_CONFIG_DIR` | `lib/runtime.js:552` |
| `CLI_PATH` | `<plugin root>/bin/model-gateway.js` (`__dirname/../bin`) | no | `lib/runtime.js:553` |
| `STABLE_COMMAND_PATH` | `STATE/model-gateway.js` | no | `lib/runtime.js:554` |
| plugin manifest | `<plugin root>/.claude-plugin/plugin.json` (`version` read at load into `PLUGIN_VERSION`) | no | `lib/runtime.js:766-773` |
| anthropic catalog cache | `STATE/anthropic-models.json` | no | `lib/anthropic-catalog.js:10` |
| legacy model list | `STATE/models.json` (read by request-worker) | no | `lib/request-worker.js:237-239` |
| sidequest catalog | `STATE/catalog.json` (commands lane) | no | `lib/commands.js:1572`, `hooks/registry-writer.js:186` |
| request-body high water | `<REQUEST_BODY_STATE_DIR>/<base64url(sessionId)>.json` | `MODEL_GATEWAY_REQUEST_BODY_DIR` | `lib/usage-observability.js:23-24,177-180` |
| proxy config | `<proxyConfigDirectory>/config.json`; dir = `CCP_CONFIG_DIR`, else Windows `APPDATA\claude-code-proxy` (fallback `~/AppData/Roaming`), macOS `~/.config/claude-code-proxy`, other `${XDG_CONFIG_HOME:-~/.config}/claude-code-proxy` | env | `lib/proxy-config.js:12-22` |

`mkdirs()` creates `STATE`, `LOGS`, `BIN_DIR` recursively (`lib/runtime.js:775-777`).

### 2.3 Network constants

| Constant | Value | Citation |
|---|---|---|
| `DEFAULT_BASE_URL` | `http://127.0.0.1:${PUBLIC_SHIM_PORT}` (default `http://127.0.0.1:18764`) | `lib/runtime.js:46` |
| `COMPAT_HOST` / `COMPAT_BASE_URL` | `api.anthropic.com` / `http://api.anthropic.com` | `lib/runtime.js:44,50` |
| hosts block | start `# >>> model-gateway RC compatibility >>>`, end `# <<< model-gateway RC compatibility <<<`, line `127.0.0.1 api.anthropic.com` | `lib/runtime.js:51-53` |
| `TRACE_HEADERS` | `traceparent`, `tracestate`, `baggage` | `lib/runtime.js:42` |
| `AUTH_HEADERS` | `authorization`, `proxy-authorization`, `x-api-key`, `cookie` | `lib/runtime.js:43` |
| `ROUTE_TELEMETRY_TIMEOUT_MS` | `500` | `lib/runtime.js:41` |
| `REPO` / `MIN_PROXY_VERSION` | `raine/claude-code-proxy` / `0.1.14` | `lib/runtime.js:32-33` |
| prefixes | `PREFIX='claude-'`, `GROK_PREFIX='claude-grok-'`, `LEGACY_CODEX_PREFIX='claude-codex-'`, `DISPATCH_MODEL_ID='claude-codex-auto'` | `lib/runtime.js:27-30` |

---

## 3. Model window policy

### 3.1 Constants

| Constant | Value | Meaning | Citation |
|---|---|---|---|
| `CODEX_UNKNOWN_MODEL_WINDOW` | 200000 | Claude Code's window for an unknown model; threshold for `[1m]` alias | `lib/runtime.js:54` |
| `PICKER_ALIAS_CLIENT_WINDOW` | 1000000 | client window a `[1m]` alias gives; advertised cap | `lib/runtime.js:57` |
| `CONTEXT_WINDOW_BACKENDS` | `['claude','codex','grok']` | backends with a persisted window | `lib/runtime.js:245` |
| `COMPACT_AT_BACKENDS` | `['codex','grok']` | backends that accept `compactAt` | `lib/runtime.js:246` |
| `DEFAULT_CONTEXT_WINDOWS` | `{claude:'full', codex:272000, grok:'full'}` | defaults; Codex is capped by default | `lib/runtime.js:247` |
| `CODEX_BILLING_RULE` | `'OpenAI bills input above 272k tokens at 2x'` | message text | `lib/runtime.js:248` |
| `CODEX_DOUBLE_BILLING_THRESHOLD` | 272000 | | `lib/runtime.js:249` |
| `CODEX_COMPACT_HEADROOM` | 40000 | one turn of growth | `lib/runtime.js:251` |
| `COMPACTION_PROMPT_TOKENS` | 5000 | | `lib/runtime.js:253` |
| `CAP_COMPACTION_MARGIN` | `2*40000+5000 = 85000` | cap → trigger margin | `lib/runtime.js:260` |
| `CONTEXT_WINDOW_MAX` | 1000000 | | `lib/runtime.js:261` |
| `CONTEXT_WINDOW_MIN` | `{claude:100000, codex:185000, grok:185000}` | | `lib/runtime.js:264` |
| `LOCAL_MIN_SENTRY_WINDOW` | 32768 | below this a live row gets no sentry | `lib/runtime.js:414` |
| `CODEX_SERVICE_TIER_SUFFIX` | `/-(?:ultra)?fast$/` | stripped from `gpt-*` ids to find the base row | `lib/runtime.js:395` |
| `CODEX_CONTEXT_WINDOWS` | alias of `MODEL_WINDOW_POLICY` | | `lib/runtime.js:240` |

### 3.2 `MODEL_WINDOW_POLICY` rows (`lib/runtime.js:109-239`)

The object is frozen; each row is frozen. Key insertion order is the row order below: `default`, the Codex rows, the Grok rows,
the five Muse rows, the 49 generated Vertex rows, `anthropic` last. One caller depends on that order today:
`lib/context-window.js:67` takes
`Object.values(MODEL_WINDOW_POLICY).find((entry) => entry.backend === backend)` as a backend's representative row. `policyIdForAdvertisedPart` also builds its map in key order (`lib/runtime.js:386-388`; a later key would replace an earlier one with the same advertised part), but the respelled keys have distinct advertised parts today, so its result does not depend on the order.
"transport" blank means the row has no `transport` field and `policyTransport` derives it (section 3.3).

Hand-written rows:

| key | backend | transport | backendId | backendWindow | advertisedWindow | pickerAlias / template | sentry | measurement (verbatim) | line |
|---|---|---|---|---|---|---|---|---|---|
| `default` | codex | | `default` | 920000 | 920000 | template `claude-{backendId}[1m]` | synthetic-413 | `unmeasured default for Codex proxy rows absent from this table` | 110-118 |
| `gpt-5.6-sol` | codex | | `gpt-5.6-sol` | 920012 | 920000 | `claude-gpt-5.6-sol[1m]` | synthetic-413 | `measured 2026-09-05: 920012 accepted; 935012 refused` | 119-127 |
| `gpt-5.6-terra` | codex | | `gpt-5.6-terra` | 920012 | 920000 | `claude-gpt-5.6-terra[1m]` | synthetic-413 | same as above | 128-136 |
| `gpt-5.6-luna` | codex | | `gpt-5.6-luna` | 920012 | 920000 | `claude-gpt-5.6-luna[1m]` | synthetic-413 | same | 137-145 |
| `gpt-6-astra` | codex | | `gpt-6-astra` | 920012 | 920000 | `claude-gpt-6-astra[1m]` | synthetic-413 | same | 146-154 |
| `gpt-6.1-sol` | codex | | `gpt-6.1-sol` | 920012 | 920000 | `claude-gpt-6.1-sol[1m]` | synthetic-413 | `measured 2026-09-30: 920012 accepted; 935012 refused` | 155-163 |
| `grok-4.7` | grok | | `grok-4.7` | 500000 | 500000 | `claude-grok-4.7[1m]` | synthetic-413 | `models.dev xai/grok-4.7 2026-10-17 (context 500000) and Grok CLI models_cache.json 2026-09-23 (500000); the Grok CLI README calls its 256000 default an auto-compact threshold with 500000 selectable, not a backend limit; not a live probe` | 164-172 |
| `grok-4.7-fast` | grok | `proxy` | `grok-4.7-fast` | 500000 | 500000 | `claude-grok-4.7-fast[1m]` | synthetic-413 | `same model as grok-4.7 per claude-code-proxy grok-4.7-fast branch; window from grok-4.7 (models.dev 2026-10-17, Grok CLI README 2026-10-16); not a live probe` | 176-185 |
| `grok-4.7-build-fast` | grok | `proxy` | `grok-4.7-build-fast` | 500000 | 500000 | `claude-grok-4.7-build-fast[1m]` | synthetic-413 | same as `grok-4.7-fast` | 186-195 |
| `grok-4.6` | grok | | `grok-4.6` | 500000 | 500000 | `claude-grok-4.6[1m]` | synthetic-413 | `measured 2026-09-15 from Grok CLI models_cache.json (context_window 500000)` | 196-204 |
| `grok-4.5` | grok | | `grok-4.5` | 500000 | 500000 | `claude-grok-4.5[1m]` | synthetic-413 | `measured 2026-09-05 from grok-backend.js GROK_MODELS` | 205-213 |
| `anthropic` | anthropic | | `anthropic` | `null` | `null` | `claude-anthropic` | `none` | `upstream-managed native Anthropic passthrough` | 230-238 |

Muse rows (`lib/runtime.js:218-228`), one shape for `muse-spark-1.3`, `muse-spark-1.2`, `muse-wonder-1.0`, `muse-wonder-full-1.0`,
`muse-ember-1.0` (in that order): backend `muse`, transport `proxy`, backendId = key, backendWindow 1007997, advertisedWindow
1000000, pickerAlias `claude-<key>[1m]`, sentry synthetic-413, measurement
`claude-code-proxy dev 1b2e80b 2026-11-10 docs/providers/muse.md (context 1007997, max output 128000 for every Muse model); not a live probe`.

Generated Vertex rows (`vertexWindowPolicyRows`, `lib/runtime.js:67-107`, spread at `:229`). Every row: backend `vertex`, transport
`proxy`, backendId = key, `pickerAlias = 'claude-' + key + '[1m]'`, `advertisedWindow = min(backendWindow, 1000000)`, sentry
synthetic-413, `measurement = '<source> (context <backendWindow>); not a live probe'`. Rows are generated in the order below.

| # | source string (verbatim) | backendWindow | advertised | keys (in order) |
|---|---|---|---|---|
| 1-18 | `models.dev google-vertex 2026-11-12` | 1048576 | 1000000 | for each of `gemini-4-argon`, `gemini-4-carbon`, `gemini-3.8-flash`, `gemini-3.7-flash`, `gemini-3.5-flash`, `gemini-3.5-flash-lite`, `gemini-3.1-flash-lite`, `gemini-3.1-pro-preview`, `gemini-3-flash-preview`: the bare id then `vertex/<id>` (interleaved: `gemini-4-argon`, `vertex/gemini-4-argon`, `gemini-4-carbon`, …) |
| 19-30 | same | 1000000 | 1000000 | `vertex/anthropic/` + `claude-fable-5-2`, `claude-fable-5-1`, `claude-fable-5`, `claude-opus-5-5`, `claude-opus-5`, `claude-opus-4-8`, `claude-opus-4-7`, `claude-sonnet-5-5`, `claude-sonnet-5`, `claude-haiku-5-5`, `claude-opus-4-6`, `claude-sonnet-4-6` |
| 31-32 | same | 524288 | 524288 | `vertex/xai/grok-4.7`, `vertex/xai/grok-4.6` |
| 33-34 | same | 2000000 | 1000000 | `vertex/xai/grok-4.20-reasoning`, `vertex/xai/grok-4.20-non-reasoning` |
| 35 | same | 1000000 | 1000000 | `vertex/zai-org/glm-5.2-maas` |
| 36 | same | 262144 | 262144 | `vertex/google/gemma-4-26b-a4b-it-maas` |
| 37 | same | 524288 | 524288 | `vertex/meta/llama-4-maverick-17b-128e-instruct-maas` |
| 38 | same | 1310720 | 1000000 | `vertex/meta/llama-4-scout-17b-16e-instruct-maas` |
| 39-42 | `models.dev google-vertex 2026-11-25` | 1048576 | 1000000 | for each of `gemini-4-hydrogen`, `gemini-4-neon`: the bare id then `vertex/<id>` (`gemini-4-hydrogen`, `vertex/gemini-4-hydrogen`, `gemini-4-neon`, `vertex/gemini-4-neon`); the comment at `lib/runtime.js:85` says claude-code-proxy registered the two at dev ef6b0bc, after the first catalog date |
| 43-46 | `claude-code-proxy dev c31326c 2026-11-12 docs/providers/vertex.md, quoting Google's input limit` | 922000 | 922000 | `vertex/openai/` + `gpt-6-astra`, `gpt-6.1-sol`, `gpt-6.1-terra`, `gpt-6-luna` |
| 47-49 | same | 1048576 | 1000000 | `vertex/meta/` + `muse-spark-1.3`, `muse-wonder-1.0`, `muse-ember-1.0` |

No generated key collides with a hand-written key (the hand-written Muse keys have no `vertex/meta/` prefix). The comment at
`lib/runtime.js:59-66` says Mistral, gpt-oss, Grok 4.3, two dated Claude ids, DeepSeek and `vertex/endpoint/<name>` are deliberately
absent (they fall to the unmeasured proxy policy, 3.3).

### 3.3 Policy functions (input → output)

**`gatewayBackendModelId(id)`** (`lib/runtime.js:376-380`)
1. `part = id.replace(/\[1m\]$/, '').replace(/^claude-/, '')`; a non-string id gives `''`.
2. If `proxyModelProvider(part)` is non-null (part starts with `gpt-`, or is a key in the live route map) → `part`.
3. Else `proxyRouteIdForAdvertisedPart(part)` (route id behind a respelled advertised id, section 7) ?? `policyIdForAdvertisedPart(part)` ?? `part`.

**`policyIdForAdvertisedPart(part)`** (`lib/runtime.js:382-390`): `null` if `part` is itself a policy key. Otherwise looks `part` up in
a lazily built, memoized `Map(advertisedProxyModelPart(policyId) → policyId)` over policy keys whose advertised spelling differs
(in practice the three `vertex/anthropic/claude-fable-*` keys → `vertex/anthropic/fable-*`). The map is built once, on the first
call, against the route table as it is then (`??=`); later route changes do not rebuild it.

**`codexContextWindowModelId(id)`** (`lib/runtime.js:400-404`): backend id; if it starts `gpt-`, strip `-fast`/`-ultrafast`; `''` → `'default'`.

**`policyTransport(policy)`** (`lib/runtime.js:473-476`): `policy.transport` if set; else `policy.backend` when it is `anthropic` or `grok`; else `'proxy'`.

**`resolveGatewayModelPolicy(id)`** (`lib/runtime.js:478-497`)
1. `backendId = gatewayBackendModelId(id)`; `policyId = backendId` with the tier suffix stripped when it starts `gpt-`; `provider = proxyModelProvider(backendId)`.
2. Row selection, first non-null wins:
   a. `MODEL_WINDOW_POLICY[policyId]` if `policyId !== 'default'` and is an own key;
   b. `MODEL_WINDOW_POLICY.default` if `backendId` starts `gpt-`;
   c. `proxyProviderPolicy(provider, backendId)` if a provider is known;
   d. `MODEL_WINDOW_POLICY.anthropic` if the original `id` is a string starting `claude-` and not `claude-codex-`;
   e. otherwise return `null`.
3. `transport = policyTransport(policy)`; `pickerId = transport === 'proxy' ? advertisedProxyModelPart(backendId) : backendId`.
4. `pickerAlias`: if `policy.backendId === backendId` and the row has `pickerAlias` → `policy.pickerAlias.replace(backendId, pickerId)`
   (JS string replace: first occurrence, `$` patterns in `pickerId` interpreted); else if the row has `pickerAliasTemplate` →
   `template.replace('{backendId}', pickerId)`; else `` `claude-${pickerId}[1m]` ``.
5. Returns `{ ...policy, backendId, pickerAlias, transport }` (a new unfrozen object; `backendId` overwritten with the real id).

**`proxyProviderPolicy(provider, backendId)`** (`lib/runtime.js:453-468`): for `local` → `localServerPolicy(localServerState())`; for
`lmstudio` → `lmStudioModelPolicy(lmStudioServerState(), backendId)`; if either returns a policy, use it. Otherwise:
`{backend: provider, transport:'proxy', backendId:null, backendWindow:null, measurement:'unmeasured default for claude-code-proxy <provider> rows absent from this table', pickerAliasTemplate:'claude-{backendId}[1m]', advertisedWindow:200000, sentry:'none'}`.

**`liveWindowPolicy(backend, backendWindow, measurement)`** (`lib/runtime.js:421-435`): `null` unless `backendWindow` is a positive safe
integer. `guarded = backendWindow >= 32768`. Returns `{backend, transport:'proxy', backendId:null, backendWindow, measurement,
pickerAliasTemplate:'claude-{backendId}[1m]', advertisedWindow:min(backendWindow,1000000), compactHeadroom (only if guarded) =
min(40000, floor(backendWindow/3)), sentry: guarded ? 'synthetic-413' : 'none'}`. Key order: `compactHeadroom` sits before `sentry`.

**`localServerPolicy(state)`** (`lib/runtime.js:437-440`): `liveWindowPolicy('local', state?.contextWindow, 'llama-server /props n_ctx <w> on 127.0.0.1:<port>, read live')`.

**`lmStudioModelPolicy(state, backendId)`** (`lib/runtime.js:444-448`): `null` unless `backendId` starts `lmstudio/`; window =
`state.models.get(backendId.slice(9)).contextWindow`; measurement `LM Studio /api/v1/models loaded context_length <w> at <baseUrl>, read live`.

**`gatewayAdvertisedWindow(id)`** (`lib/runtime.js:499-504`): policy null → null. `cap = contextWindowCap(policy.backend)`. If `cap` and
`policy.advertisedWindow` are both truthy → `min(advertisedWindow, cap)`, else `advertisedWindow` (may be null).

**`gatewayClientModelId(id)`** (`lib/runtime.js:506-512`): policy null → `id` unchanged. If `gatewayAdvertisedWindow(id) > 200000` →
`policy.pickerAlias`; else `'claude-' + (transport === 'proxy' ? advertisedProxyModelPart(policy.backendId) : policy.backendId)`
(no `[1m]`). `codexClientModelId` is an alias (`:518-520`).

**`codexContextWindow(id)`** (`lib/runtime.js:514-516`): `gatewayAdvertisedWindow(id) || 920000`.

**`contextWindowCap(backend, windows = CONTEXT_WINDOWS)`** (`lib/runtime.js:322-325`): `windows[backend].value` if it is an integer, else
null. Only `claude`, `codex`, `grok` exist in `CONTEXT_WINDOWS`, so `muse`, `vertex`, `local`, `lmstudio` and `anthropic` never have
a cap. The `claude` cap is not applied to `anthropic` rows here; commands.js writes it as `autoCompactWindow` (other lane,
`lib/commands.js:976-999`).

**`contextWindowCompactAt(backend)`** (`lib/runtime.js:327-329`): `windows[backend].compactAt ?? null`.

**`gatewayCompactTrigger(cap, compactAt)`** (`lib/runtime.js:331-334`): `compactAt` non-null → `min(compactAt, cap ?? Infinity)`; else `cap ? cap - 85000 : null`.

**`gatewayCompactionCandidates(compactTrigger, cap, compactAt)`** (`lib/runtime.js:336-342`): `compactAt !== null` → only
`[{compactTrigger: gatewayCompactTrigger(cap, compactAt), source:'compact-at'}]` (env trigger ignored). Else: env trigger if finite and
> 0 (`source:'env'`), then `cap - 85000` if cap (`source:'cap'`). Note `compactAt === undefined` takes the first branch, because the
test is `!== null`; the default argument at `:358` always yields null or a number, so that case does not arise there.

**`sentryHeadroom(policy)`** (`lib/runtime.js:346-348`): `policy.compactHeadroom` if a positive safe integer, else 40000.
**`sentryBackendWindow(policy)`** (`:350-356`): throws `model-gateway: invalid sentry backend window for <backendId>` if the window is not
finite or ≤ headroom.

**`effectiveSentryPolicy(policy, compactTrigger = env trigger, cap = contextWindowCap(policy.backend), compactAt = contextWindowCompactAt(policy.backend))`**
(`lib/runtime.js:358-365`): null unless `sentry === 'synthetic-413'`. Candidates = `[{backendWindow - headroom, 'derived'}, ...gatewayCompactionCandidates]`;
sorted ascending by trigger (stable sort, so on a tie `derived` wins, then the order above); returns `{backendWindow, compactTrigger, source}` of the lowest.
`CODEX_GATEWAY_COMPACT_TRIGGER` is global: it enters the candidates of every synthetic-413 row (Codex, Grok, Muse, Vertex, guarded local/LM Studio).

**`codexBillingNote(cap, compactAt = null)`** (`lib/runtime.js:367-372`): text only; "can still exceed 272k and pay double" when compactAt
non-null or `cap <= 272000`, else "requests past 272k pay double".

### 3.4 Context-window settings

| Function | Rule | Citation |
|---|---|---|
| `parseContextWindowValue(backend, v)` | `'full'` → `'full'`; else `Number(v)` must be an integer in `[CONTEXT_WINDOW_MIN[backend], 1000000]`, else null | `lib/runtime.js:266-270` |
| `resolveContextWindow(backend, saved, env)` | saved value (parsed) → `{value, source:'saved'}`; else for codex only `CODEX_GATEWAY_CONTEXT_WINDOW` → `source:'CODEX_GATEWAY_CONTEXT_WINDOW'`; else default → `source:'default'` | `lib/runtime.js:273-279` |
| `parseCompactAtValue(v)` | string or number; `Number(v)` safe integer > 0, else null | `lib/runtime.js:285-289` |
| `validateCompactAtSettings(compactAt = {})` | every key must be `codex`/`grok` (else throws `unsupported compactAt.<k>: …`); every value must be a positive safe integer of type number (else throws `invalid compactAt.<k>: use a positive whole token count`) | `lib/runtime.js:291-298` |
| `resolveContextWindowSetting` | adds `compactAt` (parsed) only when `saved.compactAt[backend] !== undefined` | `lib/runtime.js:300-305` |
| `readContextWindowSettings({file, env})` | read and parse file (any error → null); validate `compactAt` (throws); returns `{claude:{value,source,compactAt?}, codex:…, grok:…}` | `lib/runtime.js:307-311` |
| `CONTEXT_WINDOWS` | `readContextWindowSettings()` evaluated at module load | `lib/runtime.js:319` |

Worked numbers with defaults (no file, no env):

| Row | cap | compactAt | candidates | effective trigger |
|---|---|---|---|---|
| Codex `gpt-6.1-sol` | 272000 | null | derived 920012−40000 = 880012; cap 272000−85000 = 187000 | 187000 (`cap`) |
| Codex unknown `gpt-x` (`default` row) | 272000 | null | 880000; 187000 | 187000 |
| Grok `grok-4.7` | null (`full`) | null | 460000 | 460000 (`derived`) |
| Muse | null | null | 1007997−40000 = 967997 | 967997 |
| Vertex Gemini | null | null | 1048576−40000 = 1008576 | 1008576 (above the 1000000 client window) |
| local, n_ctx 65536 | null | null | headroom floor(65536/3)=21845 → 43691 | 43691 |
| local, n_ctx 16384 | — | — | sentry `none` | no sentry |

Advertised window and picker id with defaults: Codex rows advertise `min(920000, 272000) = 272000` > 200000, so their picker id
keeps `[1m]`.

### 3.5 Worked `resolveGatewayModelPolicy` / `gatewayClientModelId` examples (default settings, empty route table unless stated)

| Input id | backendId | row | transport | pickerAlias | advertised | `gatewayClientModelId` |
|---|---|---|---|---|---|---|
| `claude-opus-5-5[1m]` | `opus-5-5` | `anthropic` | anthropic | `claude-opus-5-5[1m]` (fallback form) | null | `claude-opus-5-5` (the `[1m]` is dropped: null is not > 200000) |
| `claude-gpt-6.1-sol-fast[1m]` | `gpt-6.1-sol-fast` | `gpt-6.1-sol` | proxy | `claude-gpt-6.1-sol-fast[1m]` (row's backendId differs, row has no template → fallback form) | 272000 | `claude-gpt-6.1-sol-fast[1m]` |
| `claude-gpt-9` | `gpt-9` | `default` | proxy | `claude-gpt-9[1m]` (template) | 272000 | `claude-gpt-9[1m]` |
| `claude-grok-4.6` | `grok-4.6` | `grok-4.6` | grok | `claude-grok-4.6[1m]` | 500000 | `claude-grok-4.6[1m]` |
| `claude-vertex/anthropic/claude-fable-5-2` (route listed) | same | vertex row | proxy | `claude-vertex/anthropic/fable-5-2[1m]` (Fable respelling, section 7) | 1000000 | `claude-vertex/anthropic/fable-5-2[1m]` |
| `claude-vertex/anthropic/fable-5-2` | `vertex/anthropic/claude-fable-5-2` (reverse map) | vertex row | proxy | as above | 1000000 | as above |
| `claude-local/gguf`, n_ctx 131072 | `local/gguf` | live | proxy | `claude-local/gguf[1m]` | 131072 | `claude-local/gguf` (≤ 200000 → no `[1m]`) |
| `claude-kimi/k3` (listed under `kimi`, unmeasured) | `kimi/k3` | unmeasured | proxy | `claude-kimi/k3[1m]` | 200000 | `claude-kimi/k3` |
| `claude-codex-gpt-5.6-sol` | `codex-gpt-5.6-sol` | none (not `gpt-`, not listed, legacy prefix excluded from anthropic) | — | — | — | returned unchanged |
| `gpt-6.1-sol` (no prefix) | `gpt-6.1-sol` | `gpt-6.1-sol` | proxy | `claude-gpt-6.1-sol[1m]` | 272000 | `claude-gpt-6.1-sol[1m]` |

### 3.6 Static env block (`lib/runtime.js:522-527,551`)

`STATIC_ENV_BLOCK` (insertion order): `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY: '1'`, `CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK: '1'`,
`ENABLE_TOOL_SEARCH: 'true'`, `CLAUDE_CODE_MAX_OUTPUT_TOKENS: '64000'`. `LEGACY_ENV_BLOCK`: `CLAUDE_CODE_AUTO_COMPACT_WINDOW: '950000'`
(only ever removed).

### 3.7 Readiness messages (`lib/runtime.js:563-587`)

`CODEX_READINESS_MESSAGES` keys: `binary-missing`, `auth-missing`, `proxy-down`, `shim-down`, `serving-version-mismatch`,
`upstream-blocked`, `upstream-unavailable`. The texts name `node "<resolveStableCommandPath()>" setup|login|ensure` and
`STATE/logs/guardian.log`. Verbatim strings are at the cited lines; `upstreamBlockedMessage` uses `blocked.expiresAt`,
`upstreamUnavailableMessage` uses `observedAt`, `statusCode`, `expiresAt` (default text `about 30 seconds later`).

### 3.8 Discovery cache sync (`lib/runtime.js:589-629`)

- `gatewayDiscoveryModels(models)`: keep entries whose `id` is a string matching `/(claude|anthropic)/i`; map to
  `{id: gatewayClientModelId(id), display_name?}` (display_name only if a string). Non-array → `[]`.
- `syncGatewayDiscoveryCache({models, baseUrl = DEFAULT_BASE_URL, cachePath, now, force})`:
  `baseUrl === COMPAT_BASE_URL` → `{state:'skipped', reason:'rc-compatibility'}`; `baseUrl !== DEFAULT_BASE_URL` → `skipped`,
  `gateway-not-wired`. If not `force` and the existing file's `models` equal the new list (same length, same `id` and
  `display_name` at each index; `baseUrl` is not compared) → `unchanged`. Else write `{baseUrl, fetchedAt: now(), models}`.
- Caller passes `baseUrl: effectiveBaseUrl().value || null` (`lib/request-worker.js:1530`) or `discoveryCacheBaseUrl()` (`lib/commands.js:1992`, UNREAD).

---

## 4. Persisted JSON files

### 4.1 Write primitive

`writeFileAtomically(target, contents, {mode})` (`lib/atomic-file.js:48-59`): write `<target>.<pid>.<12 hex>.tmp` with `mode`,
`renameSync` onto the target, retrying on `EPERM`/`EACCES`/`EBUSY` after 20, 60, 140, 300 ms (blocking `Atomics.wait`,
`:7-27`); always unlink the temp file; after a successful replace, delete sibling `<basename>.*.tmp` files older than 1 h
(`:29-46`). No fsync of file or directory. Node's `mode` option applies only when the file is created; an existing file keeps
its mode (the temp file is always new, so atomic writes do get `mode`).

All writers below serialize with `JSON.stringify(value, null, 2) + '\n'` unless stated.

### 4.2 Files

| File | Writer (this slice) | Reader | Atomic | Mode | Schema version |
|---|---|---|---|---|---|
| `STATE/context-window.json` | `writeContextWindowSettings` `lib/runtime.js:313-317` (called from `lib/commands.js:1014`) | `readContextWindowSettings` `lib/runtime.js:307-311`, at load `:319` | yes | default (umask) | none |
| `STATE/pins.json` (override) | `writePinOverrides` `lib/pins.js:356-359` (called `lib/commands.js:1099`) | `readPinOverrides` `lib/pins.js:346-354` | no (`fs.writeFileSync`) | default | none |
| `STATE/detected-pins.json` | `writeDetectedPinCache` `lib/pins.js:180-183` | `readDetectedPinCache` `lib/pins.js:79-93` | yes | 0600 | none |
| `STATE/anthropic-models.json` | `writeCatalog` `lib/anthropic-catalog.js:72-75` | `readAnthropicCatalog` `:26-31` | yes | 0600 | none |
| `CLAUDE_CONFIG_DIR/cache/gateway-models.json` | `syncGatewayDiscoveryCache` `lib/runtime.js:614-629`; `cleanLegacyGatewayModelCache` `lib/settings-wiring.js:424-442` | Claude Code itself; `readGatewayDiscoveryCache` `lib/runtime.js:599-601` | yes | 0600 + explicit `chmodSync(0600)` (runtime only) | none |
| `STATE/wired-projects.json` | `writeProjectWiringRegistry` `lib/settings-wiring.js:201-204` | `recordedWiringProjects` `:206-215` | no | 0600 on create only | none |
| `STATE/wiring.json` | never written; deleted by `retireWiringModeConfig` `lib/settings-wiring.js:23-28` | not read in this slice | — | — | legacy; shape known only from the comment `{"mode":"local"}` at `lib/settings-wiring.js:21` |
| Claude settings files (section 6) | `writeSettings` `lib/settings-wiring.js:107-112` | `readSettingsForWrite` `:76-83`, ad hoc `JSON.parse` at `:47,237,321,409` | no | default on create | — |
| `<REQUEST_BODY_STATE_DIR>/<b64url>.json` | `recordRequestBodyHighWater` `lib/usage-observability.js:191-208` | `existingHighWater` `:182-189` | no | 0600 on create | none |
| `STATE/models.json` (legacy) | not written in this slice | `lib/request-worker.js:237-239` → `modelsFileRoutes` `lib/proxy-models.js:57-61` | — | — | — |
| `<proxy config dir>/config.json` | not written here | `readProxyConfig` `lib/proxy-config.js:20-22`, section `lmstudio` | — | — | — |
| `CLAUDE_CONFIG_DIR/.claude.json` | never written (comment: "The file is only read.") | `readServerPickerRows` `lib/previous-picker-rows.js:19-24` | — | — | Claude Code owned |
| `installed_plugins.json` | never written | `readInstalledPluginEntries` `lib/runtime.js:697-712` | — | — | Claude Code owned (section 11) |
| `STATE/dispatch-routes.json` | other lane: `persist()` `lib/request-worker.js:788-794` | `load()` `:766-779` | yes | 0600 | `version: 1` |
| `STATE/catalog.json` | other lane: `lib/commands.js:2045` (`writeCatalogFile`), path `:1572` | `lib/commands.js:2065`, `hooks/registry-writer.js:186` | UNREAD | UNREAD | `schemaVersion: CATALOG_SCHEMA_VERSION` (`lib/commands.js:1901`, reader accepts `schemaVersion ?? schema` `:1917`); rest UNREAD |

### 4.3 Schemas

**context-window.json**
```json
{ "claude": "full" | 100000..1000000,
  "codex":  "full" | 185000..1000000,
  "grok":   "full" | 185000..1000000,
  "compactAt": { "codex": <positive safe int>, "grok": <positive safe int> } }
```
Every key optional. The writer (`lib/context-window.js:32-52`, other lane) only persists backends whose current source is
`saved`, and `compactAt` only when non-empty. Out-of-range or malformed window values are ignored on read (fall to env/default);
a malformed `compactAt` throws (section 13). A string window such as `"300000"` is accepted (`Number()`), a string `compactAt`
throws.

**pins.json** — `{ "opus"?: string, "sonnet"?: string, "fable"?: string }`. On read, a non-object or array → `{}`; each alias value
kept only when `isValidPin` (section 5.3). Unknown keys ignored.

**detected-pins.json**
```json
{ "cliVersion": "<claude --version output, trimmed>" | "unknown",
  "updatedAt": <ms since epoch>,
  "pins":        { "<alias>": "claude-…[1m]" },
  "detectedFor": { "<alias>": "<cliVersion when this alias was probed>" },
  "retired":     { "<alias>": ["claude-…[1m]", …] },      // most recent first, max 16
  "catalogPins": { "<alias>": "claude-…[1m]" } }
```
Read normalisation (`lib/pins.js:79-93`): non-object/array → null; `cliVersion` must be a string else null; each pin passes
`normalizedDetectedPin`; `detectedFor[alias]` defaults to `cliVersion`; `updatedAt` = `Number(...) || 0`; `retired` normalised
for every alias (array, `[].concat` so a scalar becomes one element, each normalised); `catalogPins` filtered by
`validatedCatalogPins` (`:123-127`). `refreshCatalogPins` writes the normalised object back, so the file can lack `cliVersion`
and `updatedAt` when it was created by that path (`:163`).

**anthropic-models.json**
```json
{ "data": [ <upstream /v1/models rows, kept whole> ], "updatedAt": <ms>, "error": null | "<message>", "attemptedAt"?: <ms> }
```
Success writes `{data, updatedAt: now, error: null}` (`lib/anthropic-catalog.js:86`). Failure writes `{...cached, error, attemptedAt}`
where `cached` is the normalised previous read (so `updatedAt` is unchanged) (`:91`). Read filters rows to
`/^claude-[a-z0-9._-]+$/.test(id)` and string `display_name`; a non-array `data` throws inside the try → `{data:[], updatedAt:0}`.

**gateway-models.json** (Claude Code discovery cache) — `{ "baseUrl": "http://127.0.0.1:18764", "fetchedAt": <ms>, "models": [ {"id": "...", "display_name"?: "..."} ] }`.
`cleanLegacyGatewayModelCache` only rewrites `models[].id` (spread keeps other fields and key order) and requires `baseUrl` to be one
of ours.

**wired-projects.json** — `{ "projects": [ "<absolute, path.normalize'd project dir>" ] }`. A parse error, or `projects` not an
array, marks it for rewrite.

**request-body high water** — `{"value":<bytes>,"observed_at":"<ISO-8601 UTC ms>"}` compact, no trailing newline
(`lib/usage-observability.js:202`). Filename `base64url(UTF-8 sessionId)` without padding + `.json`; session id must match
`/^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,255}$/`. Written only when the new value exceeds the remembered one; the in-process `Map` remembers
every path seen (unbounded). No reader in the gateway apart from its own `existingHighWater`; README/SKILL say no retention period
is promised (`README.md:88`, `skills/model-gateway/SKILL.md:239-242`).

**dispatch-routes.json** (other lane) — `{"version":1,"routes":[["<identity>",{"model","effort","ticket","lastUsedAt"}],…]}`
compact plus `\n` (`lib/request-worker.js:792`); TTL 4 h / 500 entries via the two `CODEX_GATEWAY_DISPATCH_CACHE_*` vars (`:715-722`).

---

## 5. Pins (`lib/pins.js`, constants in `lib/runtime.js:528-551`)

### 5.1 Constants (verbatim)

| alias | settings key (`PIN_ALIASES`) | shipped `KNOWN_GOOD_PINS` | `RETIRED_SHIPPED_PINS` |
|---|---|---|---|
| `opus` | `ANTHROPIC_DEFAULT_OPUS_MODEL` | `claude-opus-5-5[1m]` | `claude-opus-5[1m]`, `claude-opus-4-8[1m]` |
| `sonnet` | `ANTHROPIC_DEFAULT_SONNET_MODEL` | `claude-sonnet-5-5[1m]` | `claude-sonnet-5[1m]` |
| `fable` | `ANTHROPIC_DEFAULT_FABLE_MODEL` | `claude-fable-5-1[1m]` | (none) |

Alias iteration order is `opus`, `sonnet`, `fable` everywhere (`Object.keys(PIN_ALIASES)`). `RETIRED_PIN_LIMIT = 16` (`lib/pins.js:104`).
Fable picker-row keys (`FABLE_OPTION_KEYS`, `lib/pins.js:394-398`): `ANTHROPIC_CUSTOM_MODEL_OPTION`,
`ANTHROPIC_CUSTOM_MODEL_OPTION_NAME`, `ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION`.

### 5.2 Version parsing and comparison

- `pinVersion(v)` (`:35-45`): drop one trailing `[1m]` (case-sensitive), split on `-`; need exactly 3 or 4 segments with segment 0
  `=== 'claude'`; minor defaults `'0'`; `parseInt` of major/minor must print back to the same text (rejects `05`, `5a`, empty);
  returns `[family.toLowerCase(), major, minor]`. Dated ids (`claude-opus-4-8-20251101`, 5 segments) → null.
- `comparePinVersions(a, b)` (`:47-52`): null if either is unparseable or the families differ; else sign of major difference, then
  sign of minor difference (-1/0/1).
- `modelDisplayName(id)` (`:400-406`): `"<Family> <major>.<minor>"` when the bare id has 4 segments, `"<Family> <major>"` with 3;
  unparseable → id. E.g. `claude-fable-5-1[1m]` → `Fable 5.1`.
- `normalizedDetectedPin(alias, v)` (`:63-68`): remove every `[1m]` (global, case-insensitive), trim; must match
  `/^claude-[a-z0-9][a-z0-9._-]*$/i`; must contain the alias as a whole dash-delimited segment
  (`new RegExp('(?:^|-)' + alias + '(?:-|$)', 'i')`); returns `<concrete>[1m]`.
- `isValidPin(v)` (`:58-61`): non-empty string, `v.trim() === v`, no char from `/[\s\x00-\x1F\x7F"'`$\;&|<>(){}]/`.

### 5.3 Precedence and the catalog-following rule

| Function | Rule | Citation |
|---|---|---|
| `currentDetectedPins(cache)` | pins whose `detectedFor[alias] === cache.cliVersion`; `{}` when no `cliVersion` | `:95-99` |
| `detectedPinOutranksShipped(alias, pin)` | `pin` not in `RETIRED_SHIPPED_PINS[alias]` and `comparePinVersions(pin, KNOWN_GOOD) !== -1` (an incomparable pin, null, counts as outranking) | `:119-121` |
| `detectedPinDefaults()` | `{...KNOWN_GOOD_PINS, ...currentDetectedPins filtered by outranks}` | `:174-178` |
| `catalogPinDefaults(defaults)` | per alias: saved `catalogPins[alias]` if strictly newer (`=== 1`) than `defaults[alias]`, else `defaults[alias]` | `:155-160` |
| `newestCatalogPins()` | over `readAnthropicCatalog().data`: skip `lifecycle` `retired`/`deprecated`; family from `pinVersion` must be an alias; pin normalised; must outrank shipped; must not be in the cache's `retired[alias]`; keep the strictly newest per alias | `:129-153` |
| `refreshCatalogPins()` | `defaults = catalogPinDefaults(detectedPinDefaults())`; for each newest pin strictly newer than its default: push the default into `retired[alias]` and set `catalogPins[alias]`; write the cache if `catalogPins` has any key | `:162-172` |
| `effectivePins()` | per alias `{default, shipped, detected, override, newest, value: override \|\| default, source}`; `source` = `override` if an override, else `catalog` if `default === catalogPins[alias]`, else `detected` if `default === detected[alias]`, else `shipped` | `:366-381` |

Effective value precedence: override (`pins.json`) > catalog pin (strictly newer than detected/shipped) > detected pin for the
current CLI version (if it outranks shipped) > shipped. `pinProvenance(pin)` (`:383-389`) texts: `overridden; without it <default>`,
`catalog default`, `detected for this CLI`, `shipped default; this CLI resolves <detected>, which it replaces`,
`shipped fallback, not detected for this CLI`.

### 5.4 Env blocks

- `pinEnvBlock()` (`:420-426`): `{ANTHROPIC_DEFAULT_OPUS_MODEL, ANTHROPIC_DEFAULT_SONNET_MODEL, ANTHROPIC_DEFAULT_FABLE_MODEL}` =
  effective values, then `ANTHROPIC_CUSTOM_MODEL_OPTION = <fable pin>`, `…_NAME = 'Fable'`,
  `…_DESCRIPTION = '<modelDisplayName(fable)> · current Fable, through the gateway'` (U+00B7 middle dot) (`:408-418`).
- `gatewayEnvBlock()` = `{...STATIC_ENV_BLOCK, ...pinEnvBlock()}` (`:428-430`).
- `envBlockFor(mode)` = `{ANTHROPIC_BASE_URL: mode === 'compat' ? 'http://api.anthropic.com' : DEFAULT_BASE_URL, ...gatewayEnvBlock()}` (`:472-474`).
  It is written into settings by `lib/commands.js:1196` (`Object.assign(settings.env, envBlockFor(effectiveMode))`, other lane).
- `ourBaseUrls()` = `[DEFAULT_BASE_URL, COMPAT_BASE_URL]` (`:476`).

### 5.5 Ownership

`ownedAliasPinValues()` (`:450-461`): per `ANTHROPIC_DEFAULT_*_MODEL` key a `Set` of: shipped pin, retired shipped pins, cached
detected pin, cached catalog pin, override, `detectedPinDefaults()[alias]`, every cached `retired[alias]` entry.
`ownedPinValues()` (`:439-448`) adds `ANTHROPIC_CUSTOM_MODEL_OPTION` → the Fable set, `_NAME` → `{'Fable'}`, `_DESCRIPTION` →
descriptions of every owned Fable pin. `stalePinUpdates(env)` (`:465-470`): keys of `pinEnvBlock()` whose current string value
differs from the expected one and is owned → `{key, from, to}`.

### 5.6 The isolated `claude` probe

`claudeVersion()` (`:216-230`): `spawn(CLAUDE_BIN, ['--version'])`, stdio `ignore/pipe/pipe`, `detached: !WIN`, `shell` only for a
Windows `.cmd`/`.bat`; inherits the full environment. stdout+stderr concatenated; resolves the trimmed text on exit 0 with
non-empty output, else null; timeout `PIN_PROBE_TIMEOUT_MS` → kill → null.

`probeClaudeAlias(alias, endpoint, timeoutMs)` (`:232-301`):
- Command: `<CLAUDE_BIN> --bare --no-session-persistence --model <alias> -p --output-format stream-json --verbose`; stdin receives
  `/model <alias>\n` then EOF.
- Environment: `process.env` minus names whose upper-case form is in {`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
  `ANTHROPIC_BASE_URL`, `ANTHROPIC_DEFAULT_FABLE_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`,
  `CLAUDE_CODE_OAUTH_TOKEN`}, plus `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_AUTOUPDATER=1`, `DISABLE_TELEMETRY=1`,
  `DISABLE_ERROR_REPORTING=1`, `ANTHROPIC_BASE_URL=<endpoint>`, `NO_PROXY` = `no_proxy` = `[NO_PROXY, no_proxy, '127.0.0.1', 'localhost']`
  (falsy dropped) joined by `,`.
- `endpoint` is a throwaway HTTP server on `127.0.0.1:0` that answers `204` to `HEAD` and `404` to everything else, no body
  (`startPinProbeServer`, `:197-206`).
- Parsing: stdout and stderr feed one buffer; split on `/\r?\n/`, keep the trailing partial line; each complete line is
  `JSON.parse`d, and an event with `type === 'system' && subtype === 'init'` yields `normalizedDetectedPin(alias, event.model)`; the
  last such value wins. If the buffer (partial line plus new chunk) exceeds 262144 characters → kill, null.
- Result: detected pin only if the child exits with code 0; error, non-zero exit or timeout → null.
- Kill (`terminateProbe`, `:185-195`): `SIGTERM`; after 250 ms, if still running, `taskkill /pid <pid> /t /f` on Windows, else
  `SIGKILL` to the process group (`-pid`), falling back to the child.
- Refused under the test runner (`NODE_TEST_CONTEXT` set and no `CODEX_GATEWAY_CLAUDE_BIN`) → null (`:212-214`).

`refreshDetectedPins({force})` (`:303-344`):
1. `cached = readDetectedPinCache()`, `version = await claudeVersion()`, `fresh = cached && now - updatedAt < TTL` (24 h default).
2. Aliases to probe: all if `force` or not fresh; else, when `version` is known, those with `detectedFor[alias] !== version`; else none.
   None → return `cached.pins || {}`.
3. Start the probe server; probe all chosen aliases in parallel; re-probe each failure once, in parallel, with `2 × PIN_PROBE_TIMEOUT_MS`.
4. For each detected alias: if the previous pin differs, push it onto `retired[alias]` (dedup, newest first, max 16); set
   `pins[alias]`, `detectedFor[alias] = version || cached.cliVersion || 'unknown'`.
5. If anything was detected, write `{cliVersion, updatedAt: now, pins, detectedFor, retired, catalogPins: cached.catalogPins || {}}`.
6. Any exception → `cached.pins || {}`; the server is closed in `finally`.

What `pin` reports: the command lives in `lib/commands.js` (UNREAD beyond `:1099` writing overrides and `:1038-1052` calling
`refreshDetectedPins` + `refreshCatalogPins` then syncing wiring); the per-alias data it can show is the `effectivePins()` record and
`pinProvenance` text above.

---

## 6. Settings wiring (`lib/settings-wiring.js`)

### 6.1 Scopes and files

| Scope name | File | Used for |
|---|---|---|
| `project` (also source `project-local`) | `cwd/.claude/settings.local.json` | the only scope new wiring is written to (`WIRING_SCOPE = 'project'`, `:15-19`) |
| `project-shared`, `legacy-project` | `cwd/.claude/settings.json` | read for precedence; migrated away from; repaired |
| `user` | `$HOME/.claude/settings.json` | read for precedence; repaired; legacy key cleanup |

`WRITABLE_SCOPE_BY_SOURCE` (`:451`): `project-local` → `project`, `project-shared` → `project` (a write for shared wiring goes to
settings.local.json), `user` → `user`.

### 6.2 Base URL resolution

`effectiveBaseUrl()` (`:38-53`): definitions in precedence order: `process.env.ANTHROPIC_BASE_URL` (source `env`) if a string; then
the `env.ANTHROPIC_BASE_URL` string of project-local, project-shared, user settings (unreadable/invalid files skipped). Returns
the first as `{source, file, value, shadowed: [rest]}` or `{value:null, source:null, file:null, shadowed:[]}`.
- `isWired()` (`:444-446`): effective value is one of `ourBaseUrls()`.
- `wiredMode()` (`:470-478`): first file-backed definition (effective then shadowed) whose value is `http://api.anthropic.com` (`compat`)
  or `DEFAULT_BASE_URL` (`default`) → `{scope, mode, source, file}`; else, if the env value is ours → `{scope:null, mode, source:'env', file:null}`; else null.
- `processEnvGatewayBypass()` (`:55-59`): env source with a foreign value while a shadowed file definition is ours.
- `isUnsupportedRemoteControlHttpsUrl(v)` (`:61-69`): parses as URL, `https:`, hostname `api.anthropic.com` (case-insensitive), port `''` or `443`.
  `unsafeRemoteControlProcessEnv()` (`:71-74`) returns the effective env definition when that holds.

### 6.3 Keys written or removed by this file

This file never writes the full env block (that is `lib/commands.js:1196`, other lane). It writes:
- pin keys and the three Fable option keys (pin sync, 6.6);
- entries copied during legacy migration (6.7);
- `hooks.SessionStart` / `hooks.UserPromptSubmit` readiness groups, on every `writeSettings` (6.4).
It removes: gateway-owned env entries (6.5), `CLAUDE_CODE_AUTO_COMPACT_WINDOW: '950000'`, the readiness groups, and empty `env`/`hooks`.
`autoCompactWindow` is written by `lib/commands.js:976-999` (other lane, UNREAD in detail).

### 6.4 `writeSettings(file, settings)` (`:107-112`) and readiness hooks

1. `assertTestSafeSettingsPath(file)` (`:88-105`): only under `NODE_TEST_CONTEXT`; throws unless the realpath (resolved through the
   nearest existing ancestor) is strictly inside `realpath(os.tmpdir())`.
2. `syncReadinessHooks(settings, ourBaseUrls().includes(settings.env?.ANTHROPIC_BASE_URL))` (`lib/readiness-hooks.js`, other lane):
   - owned hook = `{type:'command', command: 'node "' + $HOME + '/.claude/model-gateway/model-gateway.js" readiness-gate'}`
     (`lib/readiness-hooks.js:5-11`, `hooks/registry-writer.js:141-143`);
   - for `SessionStart` and `UserPromptSubmit`: remove owned hooks from every group (a group without a `hooks` array is kept; a
     group left empty is dropped); if enabled and `settings.disableAllHooks !== true`, append
     `{hooks:[{type:'command', command, timeout:35}], matcher:'startup|resume'}` for SessionStart and the same without `matcher` for
     UserPromptSubmit; an event left with no groups is deleted; `hooks` deleted when empty (`lib/readiness-hooks.js:13-46`).
   - Effect: every write re-derives the hooks from the file's own base URL, so a write that removes our URL also removes the hooks.
3. `mkdir -p` the parent; `fs.writeFileSync(file, JSON.stringify(settings, null, 2) + '\n')`: not atomic, file mode untouched.

Formatting: 2-space indent, `"key": value`, trailing newline; comments are not supported (`JSON.parse` rejects them, so a file with
comments fails to read); key order is parse order with new keys appended at the end, except integer-like keys which JS moves to the
front (section 12); numbers are re-serialized; non-ASCII is written unescaped.

`readSettingsForWrite(file)` (`:76-83`): missing file → `{}`; any other read or parse error → throws `Could not read <file>: <msg>`.

### 6.5 Ownership and removal

`ownedGatewayEnvEntries(env)` (`:116-124`): entries where (key `ANTHROPIC_BASE_URL` and the value is ours) or (the key's owned-value
set has `String(value)`) or (key in `STATIC_ENV_BLOCK`/`LEGACY_ENV_BLOCK` and `String(value) === String(static)`).
A user-typed pin, a user's own custom model option, and static keys with different values are not owned.

`removeGatewayOwnedSettings(settings)` (`:126-131`): deletes owned entries, deletes `env` if empty, and returns the pin/option keys
still present (user values preserved).

### 6.6 Wired-projects registry and pin sync

| Function | Algorithm | Citation |
|---|---|---|
| `canonicalPath(p)` / `registryKey` | `path.resolve`; realpath (`realpathSync.native`) of the nearest existing ancestor, missing segments re-appended; lower-cased on win32; on error the resolved path | `:178-199` |
| `recordedWiringProjects()` | `{projects, rewrite, exists}`; ENOENT → `exists:false`; parse error or non-array → `rewrite:true` | `:206-215` |
| `registeredProjectWiring(entry, seen)` | skip non-absolute; `path.normalize`; dedup by canonical key; missing dir → `pruned {project, reason:'directory is missing'}`; settings.local.json must parse and hold one of our URLs, else silently dropped (neither wiring nor pruned) | `:229-242` |
| `registeredProjectWiringReport({readOnly})` | evaluates every entry; unless `readOnly`, when the file exists and (`rewrite` or the surviving list differs as JSON text) rewrites the registry with the survivors | `:244-253` |
| `recordProjectWiring(dir = cwd)` | if `dir/.claude/settings.local.json` holds our URL and the dir is not registered (by key), append and write the registry (creates it) | `:317-331` |
| `pinValuesNotOwned(env, owned)` | first owned-pin key present in env whose value is not owned | `:259-263` |
| `stalePinsIn(env, expected)` | keys of `pinEnvBlock()` present in env with a different value; plus the three Fable option keys when `ANTHROPIC_DEFAULT_FABLE_MODEL` is present (adds the row to projects wired before it existed) | `:269-275` |
| `syncProjectPins(wiring, expected, owned)` | unreadable → skipped; any non-owned pin value → whole project skipped (`reason:'pin value is not gateway-owned'`); else `settings.env = {...env, ...stale}` and `writeSettings` | `:277-292` |
| `syncRegisteredProjectPins()` | runs the above over registered wirings → `{changed, pruned, skipped}` | `:294-303` |
| `registeredProjectPinDisagreements()` | read-only; every expected key whose value differs (missing included) → `{project, file, value, key, staleValue, expectedValue}`; a malformed file throws | `:305-315` |
| `reconcileRegisteredProjectWirings(target, {confirm})` | target must be ours (else throws); `conflicting` = wirings with another of our URLs; with `confirm`, remove owned entries from each (`removeOwnedProjectWiring`, logs preserved user pins to stderr) and then prune the registry | `:333-355` |

### 6.7 Repair and leftover cleanup

- `projectWiringIssues()` (`:133-156`): over project-local, project-shared, user files that exist (a malformed one throws).
  If `env.ANTHROPIC_BASE_URL` is defined → only an `unregistered` issue (project-local file, our URL, cwd not in the registry).
  Otherwise an `orphan` issue when owned entries include `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` or a pin key; message
  `leftover gateway settings: aliases pinned to <values|none>, no gateway routing`.
- `repairProjectWiring(report = console.error)` (`:158-172`): `unregistered` → `recordProjectWiring()` and report
  `model-gateway: registered wired project <cwd> for pin sync`; `orphan` → remove owned entries, `writeSettings` (also removes the
  readiness hooks), report `model-gateway: repaired <msg> in <file>; preserved user pins: <keys|none>`. Called from the readiness
  gate when the policy is `unwired` (`lib/readiness-gate.js:55`, `hooks/registry-writer.js:86-94`) and from `lib/commands.js:1131,2973`.
- `migrateLegacyProjectSettings()` (`:361-402`): legacy = `cwd/.claude/settings.json`. Skip when it is missing, is the user file
  (lexically or by realpath), or lacks our URL. `entries` = owned entries; `legacyKeys` = legacy-block keys with the legacy value.
  Copy each entry into settings.local.json only where the key is absent (written only if `entries` is non-empty); delete entries
  and legacy keys from the legacy file (deep copy via `structuredClone`), delete empty `env`, write. Returns
  `{migrated, legacyFile, localFile, keys, mode}`.
- `cleanLegacyEnvSettings()` (`:404-422`): migrate, then for user and project-local files that parse and hold our URL, delete
  `CLAUDE_CODE_AUTO_COMPACT_WINDOW` when it equals `'950000'` (string compare), drop empty `env`, write.
- `cleanLegacyGatewayModelCache()` (`:424-442`): rewrite discovery-cache ids that are gateway ids (`isGatewayModelId`: a proxy route,
  or `claude-grok-*`) to `gatewayClientModelId(id)`; write only if changed; failures → false.
- `retireWiringModeConfig()` (`:23-28`): `rm STATE/wiring.json`; true when it existed.

---

## 7. Proxy model listing (`lib/proxy-models.js`)

### 7.1 How the listing is obtained

The slice never runs a `claude-code-proxy models` CLI command. The listing is the proxy's HTTP endpoint:
`GET http://127.0.0.1:${PROXY_PORT}/v1/models`, timeout 2500 ms, and the `data` array of the JSON body is handed to
`parseProxyModelListing` (`lib/request-worker.js:1480-1481`; same call in `lib/commands.js:2025-2028`). When the fetch fails or
returns no routes, the worker keeps the last route table; with none it falls back to `STATE/models.json` (bare Codex ids), then a
built-in `DEFAULT_MODELS` list (`lib/request-worker.js:1489-1497`, other lane). Retry back-off 1 s doubling to 30 s
(`lib/request-worker.js:230-231,1474`).

### 7.2 Parsing

| Item | Rule | Citation |
|---|---|---|
| `CODEX_FAMILY_RE` | `/^gpt-/` | `:7` |
| `PROXY_DISPLAY_NAME_RE` | `/^(.+) \(([a-z0-9][a-z0-9-]*)\)$/`: `display_name` = `"<id> (<provider>)"`; provider used only if group 1 equals `row.id` | `:8,31-35` |
| fallback provider | no or mismatched suffix → `codex` if id starts `gpt-`, else unattributed (dropped) | `:34` |
| `ANTHROPIC_STYLE_ALIAS_RE` | `/^(?:claude-\|anthropic-)\|^(?:haiku\|sonnet\|opus\|fable)$/`: rows with these ids are excluded (they are Claude Code names the proxy maps onto a provider) | `:13,45` |
| `parseProxyModelListing(rows)` | non-array → `[]`; skip rows without a non-empty string `id`, excluded aliases, ids already taken; skip unattributed rows without marking them taken (a later duplicate with a provider can still win); output `[{id, provider}]` in listing order | `:40-52` |
| `modelsFileRoutes(value)` | array → unique strings matching `gpt-` → `{id, provider:'codex'}` | `:57-61` |

### 7.3 Route table and advertised ids

- Module-level `proxyRoutes: Map<id, provider>` (`:65`) and `routeIdsByAdvertisedPart: Map<advertised, id>` (`:86`), replaced
  wholesale by `setProxyModelRoutes(routes)` (`:88-98`; later duplicates overwrite the provider but keep the first position).
- `proxyModelProvider(id)` (`:149-153`): `gpt-*` → `codex` with or without a listing; else the route map; else null.
- Fable respelling (`:67-83`): Claude Code treats any id containing `claude-fable-` as its own Fable row. `renamedProxyModelPart`
  replaces every `claude-fable-` with `fable-`; `advertisedProxyModelPart(id)` returns the renamed id unless the renamed id is itself a
  listed route (then the original). Non-string input is returned as is.
- Reverse mapping: `proxyRouteIdForAdvertisedPart(part)` (`:102-105`) returns the route id behind an advertised spelling, or null when
  `part` is itself a route (the original spelling keeps routing).
- `advertisedProxyRoutes(routes)` (`:116-119`): drops a `vertex` route `vertex/<x>` when a bare `vertex` route `<x>` exists (Gemini
  listed twice); the dropped spelling still routes.
- Picker rows built by the worker: Codex rows first (+ `auto` if `CODEX_GATEWAY_LIST_DISPATCH_MODEL=1`), then Grok-backend rows from the
  Grok CLI cache, then other proxy rows; only routes whose resolved transport is `proxy` (`lib/request-worker.js:1504-1521`, other lane).
- Labels (`PROVIDER_LABELS`, `:14-24`): codex `Codex`, cursor `Cursor`, grok `Grok`, kimi `Kimi`, lmstudio `LM Studio`, local
  `Local GGUF`, muse `Muse`, opencode `OpenCode`, vertex `Vertex AI`; unknown → first letter upper-cased; empty/non-string → `Proxy`.
  `proxyModelDisplayName(id, p)` = `"<id> (<label>)"` (`:155-157`).
- Excluded ids: Anthropic-style aliases above, unattributed non-`gpt-` rows, duplicate ids; `vertex/<gemini>` duplicates are excluded
  from advertising only.
- Server-state slots: `setLocalServerState` / `setLmStudioServerState` store a shallow frozen copy (`:125-146`); null = no probe yet.

---

## 8. Local providers

### 8.1 llama-server (`lib/local-llama.js`)

| Item | Value | Citation |
|---|---|---|
| provider / route | `local`, model `local/gguf` | `:12`, comment `:3-8` |
| port | `CCP_LLAMA_PORT`; absent → not configured; must match `/^\+?\d+$/` and be 1..65535 else `{configured:true, invalid:<raw>}` | `:17-22` |
| probe | `GET http://127.0.0.1:<port>/props`, timeout 1500 ms, via injected `fetchUrl(url, {timeout})` → `{status, body}` | `:34-49` |
| window | `default_generation_settings.n_ctx`, positive safe integer, else null (router mode reports 0) | `:27-30` |
| state | `{port, configured, reachable, contextWindow, error?}`; never throws | `:32-49` |
| refresh | only when the listing has a `local` route (else state → null); when unreachable on the same port as before and the previous window was set, keep the previous window | `:55-67` |
| derived policy | `liveWindowPolicy` (section 3.3) | `lib/runtime.js:437-440` |

### 8.2 LM Studio (`lib/lmstudio.js`)

| Item | Value | Citation |
|---|---|---|
| provider / prefix | `lmstudio`, ids `lmstudio/<model key>` | `:14-15` |
| settings precedence | enabled: env → config `lmstudio.enabled` → true; base URL: env → config → `http://127.0.0.1:1234`; key: `CCP_LMSTUDIO_API_KEY` → `LM_API_TOKEN` → config `apiKey` | `:73-86` |
| config section typing | the `lmstudio` section is used only when it is an object whose `enabled` is boolean/null/absent and `baseUrl`/`apiKey` are string/null/absent; otherwise the whole section counts as `{}` | `:61-67` |
| origin validation | valid URL with host; `https:`, or `http:` to a loopback host (`[::1]`, `/^127(?:\.\d{1,3}){3}$/`, `localhost` with optional trailing dot); no user/password; pathname `''` or `/`; no `?`/`#` in href; returns `url.origin`. Failures → `{enabled:true, invalid:{setting, reason}}` with setting `CCP_LMSTUDIO_BASE_URL` or `lmstudio.baseUrl in config.json` | `:34-54,80-83` |
| probe | `GET <origin>/api/v1/models`, timeout 1500 ms, header `authorization: Bearer <key>` when set | `:125-143` |
| status handling | 401/403 → `reachable:true, authRequired:true`; other non-200 → `error: 'GET /api/v1/models answered HTTP <n>'`; body must parse with a `models` array else `error` | `:134-139` |
| catalog parse | entries with `type === 'llm'` and a non-empty string `key`; `loaded = loaded_instances.length > 0`; window = `config.context_length` of the instance whose `id === key`, else the smallest positive loaded context, else null | `:88-120` |
| state | `{enabled, baseUrl, hasApiKey, reachable, models: Map<key,{loaded, contextWindow}>, invalid?, authRequired?, error?}`; the key is never stored | `:122-143` |
| refresh | only when the listing has an `lmstudio` route; unreachable at the same base URL → keep previous `models` | `:151-161` |
| derived policy | `lmStudioModelPolicy` (section 3.3) | `lib/runtime.js:444-448` |

---

## 9. Anthropic catalog

### 9.1 `lib/anthropic-catalog.js`

| Item | Rule | Citation |
|---|---|---|
| endpoint | `new URL('/v1/models?limit=1000', ANTHROPIC_UPSTREAM)`, `http`/`https` by scheme, custom DNS `lookup` injected by the worker | `:33-36,59-61`, `lib/request-worker.js:1233` |
| headers | default `anthropic-version: 2023-06-01`, then incoming request headers whose lower-cased name is in {`authorization`, `x-api-key`, `anthropic-version`, `anthropic-beta`, `anthropic-workspace-id`, `user-agent`}, names lower-cased | `:12-18` |
| credentials gate | no `authorization` and no `x-api-key` → no fetch, cached data returned | `:77-79,100` |
| pagination | up to 10 pages; stop when `has_more` is falsy; next page sets `after_id = last_id`; a missing or repeated `last_id`, or 10 pages, throws `catalog pagination limit` | `:59-70` |
| per page | non-200 → `upstream HTTP <code>`; body over 2 MiB (string length) → `catalog too large`; 5 s deadline → `catalog timeout`; bad JSON → `invalid catalog JSON`; each page's `data` filtered by `validCatalogRows` (non-array → throws) | `:20-24,33-57` |
| TTL | 1 h (`TTL_MS`) against `updatedAt` | `:11,98` |
| concurrency | one in-flight refresh; with cached rows the stale rows are returned at once and the refresh runs in the background; with none the caller awaits | `:81-105` |
| on success | write cache, call `onRefresh` (worker forces a discovery-cache sync) | `:84-89`, `lib/request-worker.js:1233` |
| on failure | write `{...cached, error, attemptedAt}`; `updatedAt` unchanged, so the next request retries (no negative cache) | `:90-94` |
| `advertisedAnthropicRows(rows)` | each row with `id = gatewayClientModelId(id)` (for plain Claude ids this is the id itself) | `:107-109` |
| `withCachedAnthropicRows(models)` | cached Anthropic rows first, then `models` whose id is not among them | `:111-115` |
| `anthropicCatalogReport()` | `Anthropic catalog: <Ns old\|not cached>; <n> models[; <error>; serving cached Anthropic rows\|Codex/Grok only][; populates on the next Anthropic request through the gateway; serving Codex/Grok only]` | `:117-123` |

### 9.2 `lib/previous-picker-rows.js`

- Reads `additionalModelOptionsCache` (array, else `[]`) from Claude Code's global config (`:19-24`).
- For each cached row whose `value` parses with `pinVersion`, whose family is a pin alias, and which is strictly older than the
  current effective pin of that family → `{id: value, display_name: '<modelDisplayName(value)> (previous)'}` (`:30-38`).
- `withPreviousServerRows(models)` puts those rows first and drops any model with the same id (`:40-44`). `haiku` rows are never
  relabelled (no `haiku` alias).
- Final ordering of the `/v1/models` answer and discovery cache: previous rows, then cached Anthropic rows, then gateway rows
  (`lib/request-worker.js:1530,2413`).

---

## 10. Usage observability (`lib/usage-observability.js`, `docs/usage-observability.md`)

### 10.1 Enablement and transport

- Endpoint order: `CODEX_GATEWAY_USAGE_ENDPOINT` (`'0'` disables), `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_ENDPOINT`
  + `/v1/logs`, then `http://127.0.0.1:4318/v1/logs` (`:10,620-631`; doc `docs/usage-observability.md:34`).
- Only loopback: hostname (brackets stripped, lower-cased) `localhost`, `::1`, or a dotted quad starting `127` with parts ≤ 255;
  scheme `http:`/`https:`; `localhost` rewritten to `127.0.0.1`; query and fragment cleared. A non-loopback explicit endpoint gives
  null, which disables export (no fall-through) (`:597-618,623`).
- POST: `JSON.stringify(payload)`, `content-type: application/json`, `content-length`, no keep-alive agent, socket unref'd, timeout
  500 ms (`Math.max(10, …)`), errors swallowed (`:674-691,985`). Scheduled with `setImmediate` (unref'd) after the response (`:1005-1016`).
- No consent flag inside the gateway; the doc says consent is enforced by the Observability observer's outbox (`docs/usage-observability.md:34`).

### 10.2 Records

| eventName | When | Citation |
|---|---|---|
| `gateway.token.usage` | 2xx response, complete, not overflowed, with at least one of input/output/cache-read/cache-creation tokens | `:800-834` |
| `gateway.limit.signal` | non-2xx with at least one parsed limit/retry/throttle header | `:810,834` |
| `gateway.mcp.footprint` | one per `input_mcp_tools_<server>_tokens` attribute of a usage record | `:936-974,1034-1036` |
| `gateway.tool_result.usage` | one per tool_result block added since the previous request of the same identity | `:898-934,1037-1040` |

OTLP envelope (`buildOtlpLogPayload`, `:649-672`): `resourceLogs[0].resource.attributes = [service.name="codex-gateway"]`;
`scopeLogs[0].scope = {name:"eigenwise.codex-gateway.usage", version:"1"}`; one log record with `timeUnixNano` and
`observedTimeUnixNano` (ms × 1e6 as a decimal string), `eventName`, `body.stringValue = eventName`, `attributes` (null/undefined
dropped; boolean → `boolValue`; integer number → `intValue` as string; other number → `doubleValue`; else `stringValue`),
optional `traceId`/`spanId` (trace id from a valid incoming `traceparent`, span id 8 random bytes hex, `:693-699`).

Usage record attributes, in insertion order (`:835-866`): `source='codex_gateway'`, `source_event_id='gateway-usage-<uuid>'`,
`source_schema='gateway-usage-v1'`, `event.name`, `event_name`, `request_id`, `request_id_source`
(`response_header` from `request-id`/`x-request-id`/`x-codex-request-id`, else `message_id`, else `generated` =
`gateway-request-<uuid>`), `client_request_id` (`x-claude-code-request-id` or `x-request-id`), `session_id`
(`x-claude-code-session-id`), `project_name`, `agent_id`, `parent_agent_id`, `agent_role` (`executor` when agent id present, else
`orchestrator`), `model` (Anthropic backend: response model first; otherwise route model first), `requested_model`, `backend`,
`effort` (`low|medium|high|xhigh|max` only), `via`, `ticket_ref`, `status` (`ok|client_error|throttled|server_error|other|unknown`,
`:633-639`), `status_code`, `response_mode` (`json|sse`), `token_estimator='utf8_bytes_div_4_normalized'` and
`cache_attribution='prefix_order_estimate'` on success, `request_sequence`, then measurements: `duration_ms` (hrtime, float),
`response_body_bytes`, limit headers, and on success the composition, exact usage and attribution fields below.
All identifiers pass `SAFE_IDENTIFIER = /^[A-Za-z0-9][A-Za-z0-9_.:@\[\]-]{0,254}$/` or become null (`:13,42-44`).

Composition (exact bytes, `inputComposition`, `:234-260`): `request_body_bytes`, `input_system_bytes`, `input_tools_bytes`,
`input_native_tools_bytes`, `input_mcp_tools_bytes`, `input_messages_bytes`, `input_first_message_bytes`, `input_history_bytes`,
`input_tool_results_bytes`; byte count = UTF-8 length of `JSON.stringify(value)` (`:210-218`). MCP tool = name starts `mcp__`;
server = `/^mcp__(.+?)__/` group 1, lower-cased, non `[a-z0-9_]` → `_` (`:228-232`).

Exact usage (`exactUsage`, `:428-469`): `input_tokens`, `output_tokens`, `cache_read_tokens` (`cache_read_input_tokens`),
`cache_creation_tokens`, `cache_creation_5m_tokens`/`_1h_tokens` (from `cache_creation.ephemeral_*`; when absent but the total is
present, 5m = total and 1h = 0), `thinking_tokens` (first numeric of `thinking_tokens`, `reasoning_tokens`,
`output_tokens_details.thinking_tokens`, `output_tokens_details.reasoning_tokens`), `server_tool_use_count` (sum of numeric
values), `web_search_requests`, `web_fetch_requests`, `code_execution_requests`, `tool_search_requests`.

Attribution (estimates, `inputAttribution`, `:471-533`): total = input + cache read + cache creation; section weights tools,
system, first_message, history = their bytes; with usage → largest-remainder allocation of the total; without → `ceil(bytes/4)`.
Tools split native/mcp by bytes; MCP per-server split, top 20 servers by bytes then name (`localeCompare`) →
`input_mcp_tools_<server>_tokens`. Cache read is consumed in prefix order tools → system → first_message → history; remainder is
`fresh_*`. `input_tool_results_tokens = min(msgTokens, round(msgTokens × toolResultBytes / messagesBytes))`. `context_tokens` = total or null.

Largest remainder (`:262-287`): `exact = total × w / Σw`; floor; distribute the remainder by descending fraction, ties by original
index; keys in `Object.entries` order.

SSE/JSON observation (`createUsageCapture`, `:709-896`): JSON bodies over `maxResponseBytes` overflow; chunks accumulate up to the
ceiling then drop; SSE merges `message_start.message.usage` and each `message_delta.usage` with `mergeUsage` (numeric leaves of
depth ≤ 6, later values win, `:535-558`). Limit headers (`:27-40,569-595`): the twelve `anthropic-ratelimit-*` headers
(timestamps: numeric seconds below 1e12 ×1000, else `Date.parse`), `retry-after` (seconds or HTTP date → `retry_after_ms`), and the
maximum numeric `x-codex-*-used-percent` (trailing `%` stripped) as `codex_throttle_used_percent`.

### 10.3 Per-session state and bounds

| State | Bound | Citation |
|---|---|---|
| side buffer | `CODEX_GATEWAY_USAGE_MAX_RESPONSE_BYTES`, default 4 MiB, min 1024 | `:11,712,986-988` |
| context snapshots (tool-result attribution) | identity = `JSON.stringify([session, agent, agent ? null : parent])`; LRU, TTL 4 h, 500 identities (dispatch-cache env names) | `:345-389,989-1001` |
| session → project name | LRU 500; looks for `<projects>/<dir>/<sessionId>.jsonl`, first 64 KiB, regex `/"cwd"\s*:\s*"((?:\\.|[^"\\])*)"/`; repository root via `.git` dir or worktree `gitdir:` file (+ `commondir`); basename sanitised `[^A-Za-z0-9_.:@-]` → `-`, max 64; misses are not cached | `:72-162` |
| request-body high water | one file per session, no expiry; in-memory `Map` unbounded | `:177-208` |
| MCP measurements | 20 servers per record | `:17,493-498` |

Tool-result attribution (`:403-426`): `delta = contextTokens − previous`; negative → none; added blocks = multiset difference on
`<id>:<bytes>`; one added block → all of `delta`, quality `derived_exact`; several → largest remainder by bytes, quality `estimate`;
only `tool_result`/`*_tool_result` blocks are emitted, with `tool_name` from the matching `tool_use` block.

Callers: `recordRequestBodyHighWater` at `lib/request-worker.js:2570,2635,2661`; emitter created once per worker (`:1217`) and in
`lib/commands.js:47` (UNREAD usage). Commands or endpoints that display this data: none in this slice; the doc names an external
Observability observer, report and Grafana dashboard (`docs/usage-observability.md:112-124`).

---

## 11. Install resolution (`lib/runtime.js:631-771`)

`installed_plugins.json` shape expected: `{ "plugins": { "<name>@<marketplace>": [ { "installPath": "<abs dir>", "version": "<x.y.z>", "lastUpdated": "<string>", … } ] } }`.
Only `plugins`, `installPath`, `version`, `lastUpdated` are read; each returned entry gains `registry: <file>` (`:697-712`).

| Function | Algorithm | Citation |
|---|---|---|
| `parseVersionDirectory(v)` | `/^(\d+)\.(\d+)\.(\d+)$/` → `[maj, min, patch]` numbers, else null (no pre-release) | `:631-634` |
| `versionIsOlder(a, b)` | lexicographic over three numbers; equal → false | `:636-641` |
| `parseInstalledCliVersion(cli)` | version = basename of `dirname(dirname(cli))` (`…/<version>/bin/model-gateway.js`) | `:643-646` |
| `canReplaceInstalledCliPath(cur, cand)` | both absolute strings; resolve; both realpaths must exist; same lexical path → true only if realpaths equal; otherwise all four paths (lexical and real) must end `bin/model-gateway.js`; the cache root (3 levels up) must match lexically and by realpath; both versions must parse; the candidate's version directory name must equal its realpath's; true when the candidate is not older | `:648-671` |
| `pluginRegistryDirectories(env)` / `installedPluginsRegistryPaths` | section 2.1; each dir + `installed_plugins.json` | `:682-693` |
| `readInstalledPluginEntries(id)` | id with `@` matches exactly; bare name matches `name@*`; unreadable/invalid registries skipped; non-array install lists skipped; falsy entries dropped | `:697-712` |
| `newestRegisteredInstallFirst(a, b)` | split `version` on `/[^0-9]+/`, `Number` each (empty → 0), compare descending element-wise (missing → 0); tie → `String(b.lastUpdated).localeCompare(String(a.lastUpdated))` (newest first) | `:718-727` |
| `resolveRegisteredCliPath()` | entries with a non-empty string `installPath`; cli = `installPath/bin/model-gateway.js`; must exist; sort newest first; first or null | `:735-747` |
| `resolveNewestInstalledCliPath({cliPath})` | candidates = own CLI + every sibling directory of the version dir that has `bin/model-gateway.js`; fold with `canReplaceInstalledCliPath` (a later candidate replaces when not older); readdir error ignored | `:749-764` |
| `resolveStableCommandPath()` | `STATE/model-gateway.js` if it exists, else `CLI_PATH` | `:559-561` |
| `readPluginVersion()` | `version` string of `.claude-plugin/plugin.json`, else null; read at load | `:766-773` |

The hook launcher uses its own `compareVersions` (`hooks/registry-writer.js:19`, UNREAD body) with the same `lastUpdated` tie-break
(`:64-65`); the comment at `lib/runtime.js:714-717` says the two orderings are meant to agree.

---

## 12. Node-specific constructs a Go port must reproduce deliberately

| Construct | Where | Node behaviour | Go consequence |
|---|---|---|---|
| `numeric()` uses `Number(v)` | `lib/usage-observability.js:172-175` and every caller | `null`→0, `true`→1, `false`→0, `[]`→0, `[5]`→5, `''`→0, `' 3e5 '`→300000, `'0x10'`→16; `undefined`/objects → NaN → null | A usage field of `null` reports 0, not absent; `numericTree` turns arrays/booleans into numbers. Port JS `ToNumber` or decide to diverge on purpose |
| `Number()` on strings/env | `lib/runtime.js:24-26,45,268,287,320,547-548`; `lib/usage-observability.js:987-993` | trims whitespace, accepts hex/binary/octal prefixes, exponents, `Infinity`; empty → 0 | `strconv.Atoi` rejects these; `Number(env) \|\| default` treats `0` as unset |
| `Number.isInteger` / `isSafeInteger` | `lib/runtime.js:269,288,296,347,422` | `300000.0` from JSON is an integer | decode to float64 and check integrality and ≤ 2^53−1 |
| `parseInt` round-trip | `lib/pins.js:41-43` | `parseInt('5',10)` then text compare rejects leading zeros/suffixes | reproduce exactly |
| Integer-like object keys | any `JSON.parse` → `JSON.stringify` round trip (settings `env`, `hooks`, `mcpServerBytes`, `Object.fromEntries(added.map((b, i) => [i, …]))`) | keys that are canonical array indices are moved first in ascending numeric order; other keys keep insertion order | preserving order with jsontext is more faithful to the file than Node; tie-breaks in `allocateLargestRemainder` depend on `Object.entries` order |
| Key insertion order | `MODEL_WINDOW_POLICY` (`lib/runtime.js:109-239`), env blocks (`lib/runtime.js:522-527`, `lib/pins.js:420-426`), records | object literal / spread order is observable in output and in `find` | ordered slices, not Go maps |
| `{...a, ...b}` merge | `lib/settings-wiring.js:289`, `lib/pins.js:177,429,473` | existing keys keep position, new keys append | ordered map merge |
| `JSON.stringify(v, null, 2) + '\n'` | all writers | 2-space indent, `": "`, `{}`/`[]` for empty, no HTML escaping, U+2028/9 not escaped, lone surrogates as `\udXXX`, shortest round-trip numbers (`1e+21` style), `undefined` members dropped | json/v1 HTML-escapes by default; json/v2 rejects invalid UTF-8 and duplicate names by default (a user settings file may contain either); check v2 float formatting against ES `Number#toString` |
| `JSON.parse` | settings, caches | duplicate keys: last wins; integers above 2^53 lose precision; BOM not accepted | v2 needs `AllowDuplicateNames`; keep unknown values as raw JSON to avoid precision loss |
| `String.prototype.replace(string, string)` | `lib/runtime.js:492,494` | first occurrence only; `$&`, `` $` ``, `$'`, `$$` in the replacement are expanded (`$1` stays literal: a string pattern has no capture group) | `strings.Replace(s, old, new, 1)` does not expand `$`; ids with `$` would differ |
| `replaceAll(string, string)` | `lib/proxy-models.js:75` | all occurrences, same `$` expansion | `strings.ReplaceAll` |
| `localeCompare` (ICU root collation) | `lib/runtime.js:726`, `hooks/registry-writer.js:65`, `lib/usage-observability.js:496` | punctuation sorts before digits and letters (`a_b` < `a1b`), letters compare case-insensitively first and case only breaks ties (`a` < `B` < `b`) | `strings.Compare` orders `a1b` < `a_b`; MCP server tie-break and `lastUpdated` tie-break can differ; use `golang.org/x/text/collate` or accept the divergence |
| JS `\s`, `trim()` | `lib/pins.js:59-60`, `lib/lmstudio.js:25,29`, `lib/usage-observability.js:122,561` | `\s` and `trim` include U+00A0, U+FEFF, U+2028/9, U+1680, U+2000-200A, U+202F, U+205F, U+3000 | RE2 `\s` is `[\t\n\f\r ]` (no `\v`); `strings.TrimSpace` excludes U+FEFF |
| WHATWG `URL` | `lib/lmstudio.js:44-53`, `lib/settings-wiring.js:63-65`, `lib/usage-observability.js:607-614`, `lib/anthropic-catalog.js:61` | lower-cases host, normalises IPv4 (`127.1` → `127.0.0.1`), IDNA; `hostname` keeps `[::1]` brackets; `origin` drops the default port; special-scheme pathname is never empty | `net/url` does none of this; `Hostname()` strips brackets; port elision must be explicit; because `url.port` is `''` for `:443`, the `=== '443'` branch at `lib/settings-wiring.js:65` is unreachable in Node but would fire in Go |
| `Buffer.from(s).toString('base64url')` | `lib/usage-observability.js:179` | URL alphabet, no padding | `base64.RawURLEncoding` |
| String length caps | `lib/anthropic-catalog.js:46` (2 MiB), `lib/pins.js:279` (262144) | UTF-16 code units of the decoded string | Go `len()` counts bytes; differs for non-ASCII |
| `Buffer.byteLength(JSON.stringify(x))` | `lib/usage-observability.js:214` | UTF-8 bytes of JS serialization | byte counts depend on matching JSON.stringify escaping exactly (non-ASCII unescaped) |
| `Array.prototype.sort` | `lib/runtime.js:363,745`, `lib/usage-observability.js:282,495` | stable (ES2019+) | `sort.SliceStable` / `slices.SortStableFunc` |
| `Math.round` | `lib/usage-observability.js:510` | rounds .5 toward +∞ | `math.Round` rounds half away from zero (same for non-negative values here) |
| `Date.parse` | `lib/usage-observability.js:565` | ISO 8601 and implementation-defined RFC 2822 forms | `retry-after` HTTP-date needs `http.ParseTime` |
| `fs.writeFileSync(..., {mode})` | pins/registry/settings/high-water | mode only on create; respects umask | `os.OpenFile(..., perm)` same; do not chmod existing files |
| `realpathSync.native` | `lib/settings-wiring.js:189,367,372`, `lib/usage-observability.js:51` | resolves symlinks; on macOS `/var` → `/private/var` | `filepath.EvalSymlinks` |
| `path.relative(a,b) === ''` as equality | `lib/settings-wiring.js:364,372` | lexical | `filepath.Rel` |
| win32 lower-casing | `lib/settings-wiring.js:191,193` | registry keys lower-cased on Windows | |
| module-load evaluation | `lib/runtime.js:319-320,773`, `lib/usage-observability.js:23` | context windows, compact trigger, plugin version and request-body dir are fixed at first `require` | read once at process start, not per call |
| detached process groups | `lib/pins.js:219,251,192` | `detached: !WIN` → new process group; kill `-pid` | `SysProcAttr{Setpgid: true}` and `syscall.Kill(-pid, SIGKILL)` |

### 12.1 Regex inventory (all RE2-compatible: no lookbehind, lookahead, backreferences or `u`/`y`/`s` flags)

| Regex | Citation | Note |
|---|---|---|
| `/\[1m\]$/` | `lib/runtime.js:377`, `lib/pins.js:19,404` | |
| `/^claude-/` | `lib/runtime.js:377` | |
| `/-(?:ultra)?fast$/` | `lib/runtime.js:395` | |
| `/\.(?:cmd\|bat)$/i` | `lib/runtime.js:550` | |
| `/(claude\|anthropic)/i` | `lib/runtime.js:592` | |
| `/^(\d+)\.(\d+)\.(\d+)$/` | `lib/runtime.js:632` | JS `\d` is ASCII, as in RE2 |
| `/[^0-9]+/` (split) | `lib/runtime.js:719` | JS `split` with a leading separator yields `''` → 0 |
| `/[\s\x00-\x1F\x7F"'`$\;&\|<>(){}]/` | `lib/pins.js:60` | `\s` differs (Unicode in JS) |
| `/\[1m\]/gi` | `lib/pins.js:64` | global + case-insensitive, unlike `:19` |
| `/^claude-[a-z0-9][a-z0-9._-]*$/i` | `lib/pins.js:65` | |
| `` new RegExp(`(?:^\|-)${alias}(?:-\|$)`, 'i') `` | `lib/pins.js:66` | alias is a fixed literal |
| `/\r?\n/` (split) | `lib/pins.js:283` | |
| `/^gpt-/` | `lib/proxy-models.js:7` | |
| `/^(.+) \(([a-z0-9][a-z0-9-]*)\)$/` | `lib/proxy-models.js:8` | greedy `.+`; JS `.` excludes `\n`, `\r`, U+2028/9; RE2 `.` excludes only `\n` |
| `/^(?:claude-\|anthropic-)\|^(?:haiku\|sonnet\|opus\|fable)$/` | `lib/proxy-models.js:13` | |
| `/^127(?:\.\d{1,3}){3}$/` | `lib/lmstudio.js:35` | |
| `/\.$/` | `lib/lmstudio.js:36` | |
| `/[?#]/` | `lib/lmstudio.js:50` | |
| `/^\+?\d+$/` | `lib/local-llama.js:20` | |
| `/^claude-[a-z0-9._-]+$/` | `lib/anthropic-catalog.js:22` | applied to `row?.id`; `RegExp#test(undefined)` tests the string `"undefined"` (fails here) |
| `/^[A-Za-z0-9][A-Za-z0-9_.:@\[\]-]{0,254}$/` | `lib/usage-observability.js:13` | |
| `/^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,255}$/` | `lib/usage-observability.js:14` | |
| `/[^A-Za-z0-9_.:@-]/g` | `lib/usage-observability.js:111` | |
| `/"cwd"\s*:\s*"((?:\\.\|[^"\\])*)"/` | `lib/usage-observability.js:122` | JS `\s`; `\\.` excludes line terminators in JS |
| `/^mcp__(.+?)__/` | `lib/usage-observability.js:230` | lazy quantifier, supported by RE2 |
| `/[^a-z0-9_]/g` | `lib/usage-observability.js:231` | |
| `/^x-codex-.*-used-percent$/i` | `lib/usage-observability.js:588` | |
| `/%$/` | `lib/usage-observability.js:590` | |
| `/^\[\|\]$/g` | `lib/usage-observability.js:608` | |
| `/^\d{1,3}$/` | `lib/usage-observability.js:602` | |
| `/^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(?:-[0-9a-f-]+)?$/`, `/^0{32}$/`, `/^0{16}$/` | `lib/usage-observability.js:696-697` | |

`$` without the `m` flag matches only at the very end in both JS and Go RE2, so no trailing-newline difference.

---

## 13. Open questions, dead code, undetermined behaviour

1. **Load-time throw.** `CONTEXT_WINDOWS = readContextWindowSettings()` runs when `runtime.js` is first required (`lib/runtime.js:319`).
   `validateCompactAtSettings(saved?.compactAt)` throws for an unknown backend key or a non-number value, and `Object.entries(null)`
   throws a TypeError when the file holds `"compactAt": null` (the default parameter covers only `undefined`). A string value such as
   `"compactAt": "x"` iterates characters and throws `unsupported compactAt.0`. Every module that requires `runtime.js` then fails.
   Should the Go port fail fast at start (same effect) or degrade?
2. **Dead branch.** `parseCompactAtValue` accepts numeric strings (`lib/runtime.js:285-289`), but validation already rejects
   non-number values (`:296`), so the string path never runs.
3. **Incomparable pins outrank.** `detectedPinOutranksShipped` returns true when `comparePinVersions` returns null (`lib/pins.js:120`),
   so a probed dated id (5 segments) or another family's id would replace the shipped pin. Intended?
4. **Probe exit status.** A detected pin counts only when `claude -p` exits 0 (`lib/pins.js:298`), while the probe server answers 404
   to every request. Whether Claude Code exits 0 in that case cannot be determined without running it (forbidden here). UNVERIFIED.
5. **Catalog `lifecycle` field.** `usableCatalogPin` skips rows whose `lifecycle` is `retired`/`deprecated` (`lib/pins.js:136`).
   Whether Anthropic's `/v1/models` rows carry that field was not verified.
6. **Memoized reverse map.** `policyIdsByAdvertisedPart` is built once, from the route table as it is at the first call
   (`lib/runtime.js:386`). If the first call happens before a listing that contains `vertex/anthropic/fable-*` as a route of its own,
   the map keeps a stale rename. Probably harmless; unclear whether intended.
7. **No negative cache for the Anthropic catalog.** A failed refresh keeps the old `updatedAt` (`lib/anthropic-catalog.js:91`), so every
   later request with credentials starts another refresh (serialised by `inFlight` only).
8. **LM Studio auth loss.** `refreshLmStudioState` keeps previous windows only when `reachable` is false (`lib/lmstudio.js:158`); a 401/403
   is `reachable:true` with an empty `models` map, so windows (and the `[1m]` picker ids) drop during an auth failure.
9. **Unbounded state.** `requestBodyHighWater` Map (`lib/usage-observability.js:25`) never evicts; the request-body directory has no
   retention (README/SKILL confirm); `SessionProjectResolver` does not cache misses, so an unknown session rescans every
   `projects/*` directory on each request (`:147-160`).
10. **Home resolution split** (section 2.1): `STATE`, user settings and the hook command ignore `CLAUDE_CONFIG_DIR`; the discovery cache,
    transcripts and `.claude.json` honour it; only the hook launcher honours `MODEL_GATEWAY_CLAUDE_HOME`. A user with
    `CLAUDE_CONFIG_DIR` set gets user-settings repair against `~/.claude/settings.json`, which Claude Code is then not reading.
11. **Discovery-cache equality ignores `baseUrl`** (`lib/runtime.js:603-612`): a cache written for another base URL with the same rows is
    left in place.
12. **Settings repair throws on malformed JSON.** `projectWiringIssues` uses `readSettingsForWrite`, which throws on a parse error
    (`lib/settings-wiring.js:140`); `registeredProjectPinDisagreements` likewise (`:309`). Callers' handling is in other lanes
    (`hooks/registry-writer.js:89-93` catches and logs).
13. **Non-atomic writes** of user settings, `pins.json` and `wired-projects.json` (`lib/settings-wiring.js:111,203`, `lib/pins.js:358`)
    while the caches use the atomic helper. A crash mid-write can truncate a user's settings file. Keep or fix in the port?
14. **Pin sync skips the whole project** when any one owned key holds a foreign value (`lib/settings-wiring.js:285-286`), including
    a user's own `ANTHROPIC_CUSTOM_MODEL_OPTION`.
15. **Lead premise corrected:** no `claude-code-proxy models` CLI parsing exists in this slice; listings come from HTTP `/v1/models`
    (section 7.1). Whether some other lane shells out to `claude-code-proxy models` was not checked beyond a grep for `'models'`
    spawn arguments in commands.js, request-worker.js and picker-guard.js, which found none.
16. **Effective-base-URL precedence vs Claude Code.** The plugin ranks the process env above every settings file
    (`lib/settings-wiring.js:40-50`). Claude Code's own precedence between shell env and the settings `env` block was not checked
    against `claude-code-reverse-new`.
17. **`fs.realpathSync.native` fallback** in usage-observability (`:51`) exists for runtimes without `.native`; dead on current Node.
18. UNREAD and needed by the planner from other lanes: `catalog.json` schema and writer (`lib/commands.js:1572-2065`),
    `syncClaudeContextWindow` / `autoCompactWindow` ownership (`lib/commands.js:976-999`, `lib/context-window.js`), the full env-block
    write path (`lib/commands.js:1106-1210`), `hooks/registry-writer.js:19` `compareVersions`, and `fetchUrl` (request-worker).
