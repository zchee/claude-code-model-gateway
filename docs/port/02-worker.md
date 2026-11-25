model: claude-opus-5-5

# model-gateway request worker: behaviour inventory for a Go port

Source: `/Users/zchee/src/github.com/Eigenwise/eigenwise-toolshed/plugins/model-gateway`, branch `dev`, commit `a8501f251755` (written against `4ffb2f672`; moved to this commit on 2026-11-25, see `07-repin.md`).
All citations are `path:line` relative to the plugin root. Nothing was executed; everything below comes from reading the source.

Slice read in full: `lib/request-worker.js` (1-2734), `lib/tool-schema-compat.js` (1-388), `lib/context-window.js` (1-133),
`lib/grok-backend.js` (1-477), `lib/codex-upstream-state.js` (1-101), `lib/shim-state.js` (1-78).

Files outside the slice, opened only to resolve calls made from it (read ranges stated so nobody mistakes them for full coverage):

| file | lines read | why |
|---|---|---|
| `lib/runtime.js` | 1-60, 104-534, 579-684, 764-780 | constants, `MODEL_WINDOW_POLICY`, `resolveGatewayModelPolicy`, `effectiveSentryPolicy`, discovery cache writer |
| `lib/pins.js` | 1-56, 476 | `proxyRouteFromId`, `ourBaseUrls` |
| `lib/proxy-models.js` | 1-176 (full) | proxy listing parse, route table, provider labels |
| `lib/anthropic-catalog.js` | 1-125 (full) | `/v1/models` Anthropic rows |
| `lib/control-auth.js` | 1-56 (full) | `/drain` authentication |
| `lib/previous-picker-rows.js` | 1-46 (full) | "(previous)" relabelling |
| `lib/atomic-file.js` | 1-65 (full) | every state-file write |
| `lib/usage-observability.js` | 10-26, 177-209, 597-632, 976-1063 | emitter surface and request-body high-water writer only |
| `lib/process-supervision.js` | 25-60 | `fetchUrl` |
| `lib/remote-control.js` | 8, 23-34, 270-302 | `detectHostsCompat` |
| `lib/commands.js` | 2484-2492, 2610-2660 (plus grep hits) | how the supervisor forks the worker |
| `lib/lmstudio.js`, `lib/local-llama.js` | constant lines only (`lmstudio.js:14-15`, `local-llama.js:12`) | provider names |
| `README.md` | line 74 (grep hit) | the "byte-identical" claim |

UNREAD (cited only by name): `runtime.js` 60-104, 534-579, 684-764; `pins.js` beyond line 56 (`effectivePins`, `modelDisplayName`,
`pinVersion`, `comparePinVersions` bodies beyond 35-52); `usage-observability.js` internals (`createUsageCapture`, OTLP log payloads,
tool-result tracker); `lmstudio.js`/`local-llama.js` refresh functions; `settings-wiring.js` (`wiredMode`, `effectiveBaseUrl`);
`commands.js` outside the ranges above (supervisor public listener, drain timeout use); `README.md` except line 74.

---

## 1. HTTP surface

The worker is one `http.createServer(handleRequest)` handler shared by up to three listeners (section 10). `handleRequest` is
`lib/request-worker.js:2361-2678`. Dispatch is strictly in this order; the first match wins.

| # | match | condition / auth | response | cite |
|---|---|---|---|---|
| 0 | every request | `pathOnly = req.url.split('?')[0]`; query string is ignored for matching but kept when forwarding | - | request-worker.js:2362 |
| 1 | `POST /drain` | `authenticatedControlRequest(req, token, [req.socket.localPort], compatListener)` must pass (row 1a) | fail: `403`, no headers, empty body. ok: `202`, `content-type: application/json`, body `{"ok":true,"draining":true,"activeRequests":<n>}`; after the response `finish`, `setImmediate(beginDrain)` | request-worker.js:2364-2374 |
| 1a | /drain auth | `compatibilityListener` must be false (refused when `port80Bound && localPort === COMPAT_PORT`); method `POST`; no `Origin` header; `Host` exactly `127.0.0.1:<localPort>`; `content-type` exactly `application/json`; header `x-model-gateway-control-token` matches `/^[a-f0-9]{64}$/` and `crypto.timingSafeEqual` with the token file | - | control-auth.js:41-54, request-worker.js:2365 |
| 2 | any path except `/healthz` while `draining` | none | `503`, `content-type: application/json`, body `{"type":"error","error":{"type":"api_error","message":"model-gateway shim is restarting; retry this request shortly"}}` (no content-length) | request-worker.js:2376-2379 |
| 3 | not `/healthz` | `trackInFlight(res)` increments `activeRequests`; decremented on res `finish` or `close` | - | request-worker.js:2381, 1247-1260 |
| 4 | `/healthz`, any method | none | `200`, `content-type: application/json`, body shape below | request-worker.js:2383-2407 |
| 5 | `GET /v1/models` | none (credentials are only read from request headers for the Anthropic catalog fetch) | `200`, `content-type: application/json`, body `{"data":[...],"has_more":false}` (section 6) | request-worker.js:2409-2414 |
| 6 | `HEAD <any path>` | none | `200`, no headers, empty body | request-worker.js:2416 |
| 7 | everything else (any method, any path, including `POST /v1/models`, `GET /v1/models/<id>`, `GET /drain`, `/v1/messages`, `/v1/messages/count_tokens`, `/api/...`) | body is buffered fully, then routed (section 2) | per backend (sections 3-4) | request-worker.js:2420-2677 |

Not present in the worker:

- No `upgrade` event listener (grep for `upgrade` in request-worker.js finds only the proxy-side WebSocket rejection strings at
  request-worker.js:1654, 1663). What Node does with a client `Upgrade:` request when no listener exists was not verified here; a Go
  port must decide explicitly.
- No Host or Origin check on any path other than `/drain`; no CORS headers; no request-body size limit (section 4.6).
- No 404 path: an unmatched path is forwarded to `ANTHROPIC_UPSTREAM`.
- `/drain` over the Unix socket: `req.socket.localPort` is not a TCP port on a Unix-socket connection, so the `Number.isInteger(port)
  && port > 0` test in control-auth.js:49 would fail and drain is refused there. This is a reading of Node's socket API, not verified.

### 1.1 `/healthz` body (request-worker.js:2384-2406)

```json
{
  "ok": true,
  "version": "<PLUGIN_VERSION from .claude-plugin/plugin.json>",
  "models": <modelCache.data.length>,
  "catalog": "proxy" | "models.json" | "fallback",
  "served": { "models": n, "codex": n, "grok": n, "anthropic": n, "<other provider>": n },
  "draining": bool,
  "activeRequests": n,
  "compat": { "hostsDetected": bool, "hostsLine": string|null, "port80Bound": bool, "reason": string|null },
  "compaction": { "streamGuard": bool, "retries": n, "maxBufferedBytes": n },
  "usage": { "enabled": bool, "endpoint": string|null, "maxResponseBytes": n, "settingsLevelBaseUrl": bool },
  "codexReadiness": { ... }
}
```

- `served` keys other than the four seeds are added by `counters[provider] = (counters[provider] || 0) + 1` (request-worker.js:2551).
- `codexReadiness` = `healthzCodexReadiness(latestProxyAndAuthCheck(), health)` (request-worker.js:2406, 116-137):
  - no completed check yet: `{ready:false, state:"checking", message:"Codex readiness is still being checked (proxy /v1/models and codex auth status); ask /healthz again in a few seconds.", checks:null, upstreamBlocked, upstreamUnavailable, checkedAt:null, ageMs:null, stale:true}` (request-worker.js:117-129).
  - otherwise `{ready, state, message, checks, upstreamBlocked, upstreamUnavailable, checkedAt:<ISO>, ageMs, stale: ageMs >= 15000}` (request-worker.js:130-136, 165-174).
  - `checks = {proxyBinary, proxyModels, codexAuth, shimRunning, servingVersion, installedVersion, servingVersionMatches}` (request-worker.js:142-150). `shimRunning` is `health.ok` of the worker's own health object, so it is always true here.
  - `state` precedence: `binary-missing` > `proxy-down` > `auth-missing` > `shim-down` > `serving-version-mismatch` > `upstream-blocked` > `upstream-unavailable` > `ready` (request-worker.js:63-72). The message for non-ready states comes from `codexReadinessMessage` (runtime.js:585; message table at runtime.js 530-575 UNREAD).
  - `upstreamBlocked`/`upstreamUnavailable` are the parsed state files or `null` (codex-upstream-state.js:28-32, 69-73); read synchronously on every `/healthz`.
- The check itself (`sharedProxyAndAuthCheck`, request-worker.js:102-114): returns the last result immediately and starts at most one new run when the last is older than 15000 ms (`CODEX_CHECK_FRESH_MS`, request-worker.js:98). A run = `fs.existsSync(PROXY_BIN)`, then in parallel `GET http://127.0.0.1:<PROXY_PORT>/v1/models` (timeout 2000, ok iff status 200) and `execFile(PROXY_BIN, ['codex','auth','status'], timeout 15000)` matching `/account/i` on stdout+stderr (request-worker.js:49-61, 80-88).
- The worker's health object has no `proxyRecovery` or `supervisorVersion` field, while `shim-state.js:68` (`shimNeedsRestart`) requires `health.proxyRecovery === true`; the supervisor presumably answers the public `/healthz` (commands.js UNREAD). See section 14.

---

## 2. Routing decision tree for inference

Input: the buffered body `raw` (`null` when empty) and `pathOnly` (request-worker.js:2428-2431). Body parsing happens only when
`raw && pathOnly.startsWith('/v1/messages')` (request-worker.js:2435, 2596). That prefix also matches `/v1/messages/count_tokens`
and any other `/v1/messages...` path.

```
if raw && path startsWith "/v1/messages":
    parsed = JSON.parse(raw)                         # throw -> catch -> step C (request-worker.js:2594)
    route  = proxyRouteFromId(parsed.model)          # step A
    if route != null: -> PROXY branch (returns)      # request-worker.js:2444-2593
if raw && path startsWith "/v1/messages":
    parsed = JSON.parse(raw)  (parsed a second time)  # request-worker.js:2598
    if typeof parsed.model == string && startsWith("claude-grok-"): -> GROK branch   # step B, 2599-2645
-> ANTHROPIC passthrough                             # step C, 2648-2676
```

### 2.1 Step A: `proxyRouteFromId(id)` (pins.js:17-28)

| order | test | result |
|---|---|---|
| 1 | `id` not a string | `null` |
| 2 | `bare = id` with a trailing `[1m]` removed | - |
| 3 | `bare === "claude-codex-auto"` (`DISPATCH_MODEL_ID`, runtime.js:30) | `{provider:"codex", base:"auto"}` (dispatch, section 7) |
| 4 | `bare` starts with `claude-codex-` (`LEGACY_CODEX_PREFIX`, runtime.js:29) | rest matches `/^gpt-/` -> `{provider:"codex", base:rest}`; else `null` |
| 5 | `bare` does not start with `claude-` | `null` |
| 6 | `policy = resolveGatewayModelPolicy(bare)`; `policy.transport === "proxy"` | `{provider: policy.backend, base: policy.backendId}`; else `null` |

`resolveGatewayModelPolicy(id)` (runtime.js:478-497):

1. `backendId = gatewayBackendModelId(id)` (runtime.js:376-380): strip `[1m]`, strip leading `claude-`; if `proxyModelProvider(part)` is
   non-null (part matches `/^gpt-/`, or is a key in the last proxy listing, proxy-models.js:149-153) return `part`; else map an advertised
   alias back (`proxyRouteIdForAdvertisedPart`, proxy-models.js:102-105: the `claude-fable-` -> `fable-` rename, proxy-models.js:67-83),
   else a table id whose advertised spelling differs (runtime.js:384-390), else `part`.
2. `policyId` = `backendId` with `-fast`/`-ultrafast` removed only when it starts with `gpt-` (runtime.js:395, 480).
3. First hit wins: table row `MODEL_WINDOW_POLICY[policyId]` (not `default`); `default` row if `gpt-`; `proxyProviderPolicy(provider)` if the
   listing names a provider; the `anthropic` row if `id` starts `claude-` and not `claude-codex-`; else `null` (runtime.js:482-488).
4. `transport` = explicit `policy.transport`, else `anthropic`/`grok` when backend is one of those, else `proxy` (runtime.js:473-476).
5. Returns `{...policy, backendId, pickerAlias, transport}` (runtime.js:490-496). `backendId` keeps a `-fast` suffix; only the policy is shared.

### 2.2 Worked classification (derived from the code above)

| client `model` | branch | upstream model id | cite |
|---|---|---|---|
| `claude-gpt-5.6-sol[1m]` | proxy, provider `codex` | `gpt-5.6-sol` | runtime.js:119-127 |
| `claude-gpt-5.6-sol-fast` | proxy, `codex` | `gpt-5.6-sol-fast` (policy of `gpt-5.6-sol`) | runtime.js:395, 480 |
| `claude-gpt-9` (not in table) | proxy, `codex` | `gpt-9` (row `default`, window 920000) | runtime.js:110-118, 483 |
| `claude-codex-gpt-5.6-sol` (legacy) | proxy, `codex` | `gpt-5.6-sol` | pins.js:21-23 |
| `claude-codex-auto[1m]` | proxy dispatch | marker model (section 7) | pins.js:20 |
| `claude-codex-foo` (non-gpt) | Anthropic passthrough | unchanged | pins.js:23 then not `claude-grok-` |
| `claude-grok-4.7`, `-4.6`, `-4.5` | Grok backend | `grok-4.7` ... | runtime.js:164-213 (no `transport` -> `grok`) |
| `claude-grok-4.7-fast`, `claude-grok-4.7-build-fast` | proxy, provider `grok` | `grok-4.7-fast` / `grok-4.7-build-fast` | runtime.js:176-195 (`transport:'proxy'`) |
| `claude-grok-<x>` listed by the proxy with a provider | proxy, that provider | `grok-<x>` | runtime.js:484, 453-468 |
| `claude-grok-<x>` not listed, not in table | Grok backend | `grokModelFromPicker(x)` -> matching cached id or `grok-<x>` | grok-backend.js:419-423 |
| `claude-muse-spark-1.3` etc. | proxy, `muse` | `muse-spark-1.3` | runtime.js:218-228 |
| `claude-<id>` where the proxy lists `<id>` (lmstudio, local, vertex, kimi, ...) | proxy, listed provider | `<id>` | runtime.js:453-468 |
| `claude-opus-5-5[1m]`, `claude-<anything else>` | Anthropic passthrough | unchanged bytes | runtime.js:485-486 |
| `gpt-5.6-sol` (no `claude-` prefix) | Anthropic passthrough | unchanged | pins.js:25 |
| missing / non-string `model`, non-JSON body, non-`/v1/messages` path | Anthropic passthrough | unchanged | request-worker.js:2594, 2646 |

Unknown ids are never refused by the gateway; they go to `api.anthropic.com`, which answers for itself.

`[1m]` alias handling: stripped by `proxyRouteFromId` (pins.js:19) and by the Grok branch (`replace(/\[1m\]$/, '')`, request-worker.js:2601).
The upstream sees the bare backend id; the client's original string (with `claude-` and `[1m]`) is kept as `advertisedModel` and written
back into responses (section 3.4).

### 2.3 Branch preconditions after classification

| branch | refusal checks, in order | cite |
|---|---|---|
| proxy | (1) `thread` refusal, only on exact `/v1/messages`; (2) dispatch marker resolution for `base === "auto"`; (3) tool schema compatibility, only provider `codex`; (4) context sentry, only on exact `/v1/messages` | request-worker.js:2446-2460, 2466-2508, 2516-2541, 2565-2568 |
| Grok | (1) `thread` refusal, only on exact `/v1/messages`; (2) context sentry on **every** `/v1/messages*` path (no exact-path guard) | request-worker.js:2605-2619, 2631-2634 |
| Anthropic | `assertAnthropicPassthroughSentryIsDisabled(requestedModel)` throws if an `anthropic`-backend policy has `sentry !== 'none'` (cannot happen with the current table; a throw here is inside the `end` handler, see section 14) | request-worker.js:260-265, 2648 |

`thread` refusal (request-worker.js:241-253): if the payload has an own `thread` property and it is not a plain object with
`type === "create"`, answer `400` `content-type: application/json` with:

```json
{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}
```

`count_tokens` consequence (read from code): on the proxy branch it gets deferred-tool hoisting, schema adaptation and plan-tool removal,
and is forwarded to the proxy at the same path; thread refusal, sentry, usage capture and the compaction guard are skipped because they
test `pathOnly === '/v1/messages'`. On the Grok branch, `forwardGrok` ignores the path and always POSTs a full translated inference to
`GROK_ENDPOINT` (request-worker.js:2256-2263), so a `count_tokens` call becomes a billed generation answered with a Messages-shaped body,
not `{"input_tokens":n}`; it can also be refused by the sentry.

---

## 3. Request transforms per backend

### 3.1 Summary

| aspect | Anthropic passthrough | proxy (claude-code-proxy on 127.0.0.1:PROXY_PORT) | Grok backend |
|---|---|---|---|
| target | `new URL(req.url, ANTHROPIC_UPSTREAM)` (default `https://api.anthropic.com`, env `CODEX_GATEWAY_ANTHROPIC_UPSTREAM`), path + query kept | `new URL(req.url, "http://127.0.0.1:" + PROXY_PORT)` (default 18765, env `CODEX_GATEWAY_PROXY_PORT`), path + query kept | `GROK_ENDPOINT` (default `https://cli-chat-proxy.grok.com/v1/responses`, env `CODEX_GATEWAY_GROK_ENDPOINT`); client path ignored |
| method | client method | client method | always `POST` |
| body | **original bytes** `raw` | `JSON.stringify(parsed)` after rewrites | `JSON.stringify(translateRequest(payload, model))` |
| headers | client headers minus drop set | client headers minus drop set minus auth set | only `grokHeaders(...)` + `content-length`; no client header is forwarded |
| cite | request-worker.js:2676, 1822-1841, 2240; runtime.js:34 | request-worker.js:2511-2592; runtime.js:26 | request-worker.js:2244-2263; runtime.js:31; grok-backend.js:8 |

### 3.2 Header handling in `forward()` (request-worker.js:1827-1839)

| set | headers | applies to |
|---|---|---|
| always removed | `host`, `connection`, `content-length`, `keep-alive`, `x-model-gateway-control-token` (`CONTROL_HEADER`, control-auth.js:8), `traceparent`, `tracestate`, `baggage` (`TRACE_HEADERS`, runtime.js:42) | Anthropic and proxy |
| also removed | `authorization`, `proxy-authorization`, `x-api-key`, `cookie` (`AUTH_HEADERS`, runtime.js:43) | proxy only (request-worker.js:2591) |
| set | `content-length = Buffer.byteLength(body)` when `body != null` | both |
| kept | everything else, including `anthropic-version`, `anthropic-beta`, `accept-encoding`, `x-claude-code-session-id`, `x-claude-code-agent-id`, `x-claude-code-parent-agent-id`, `user-agent`, `x-stainless-*` | both |
| added | nothing (no auth, no trace header, no beta header) | both |
| `Host` on the wire | Node derives it from the target URL (not set by the code) | both |
| DNS override | `lookup = anthropicBypass.lookup` only when `compatState.hostsDetected && url.hostname === "api.anthropic.com"` (section 10.2) | Anthropic |

Byte identity: the README says "Anthropic requests remain byte-identical" (README.md:74). The **body** is forwarded as the buffered bytes
(request-worker.js:2431, 2676, 2240; the comment at 2418-2419 says prompt caching keys on them). The **headers** are not identical:
`traceparent`/`tracestate`/`baggage` and the hop-by-hop set are stripped even on passthrough, and `content-length` is recomputed. When
`raw` is `null` (empty body) the code calls `clientReq.pipe(upReq)` on a request whose `end` already fired (request-worker.js:2241); this
relies on Node's pipe-after-end behaviour (not verified here). A Go port should send an empty body directly.

### 3.3 Proxy body rewrites, in order (request-worker.js:2511-2569)

| # | rewrite | detail | cite |
|---|---|---|---|
| 1 | model id | `parsed.model = dispatchRoute ? dispatchRoute.model : requestedBase` (bare backend id) | request-worker.js:2511 |
| 2 | effort from dispatch | if the dispatch route has `effort`: `parsed.output_config = {...output_config, effort}` | request-worker.js:2512-2514 |
| 3 | deferred tools | `resolveCodexDeferredTools(parsed)` for **every** proxy provider (name notwithstanding); algorithm below | request-worker.js:2515, 1543-1610 |
| 4 | JSON Schema compatibility copy | only `provider === "codex"`: `parsed.tools = adaptCodexToolSchemas(parsed.tools)` (section 9.1); failure -> 400 | request-worker.js:2516-2541 |
| 5 | plan tools | unless `CODEX_GATEWAY_KEEP_PLAN_TOOLS === "1"`, remove tools named `EnterPlanMode` / `ExitPlanMode` (all proxy providers) | request-worker.js:2547-2550, 224 |
| 6 | serialize | `forwardedBody = JSON.stringify(parsed)` | request-worker.js:2569 |

There is no thinking/effort mapping in the worker other than row 2: `output_config.effort` passes through unchanged to the proxy;
`thinking`, `max_tokens`, `metadata`, `stream`, `system` are untouched. Key order of the re-serialized body is the parse order of the
client JSON (JS object semantics); nothing in this slice depends on it, but a Go port that decodes into `map[string]any` and re-encodes
with `encoding/json` would sort keys. Whether claude-code-proxy or the Codex prompt cache cares is UNVERIFIED; use an order-preserving
representation (e.g. `jsontext` token streaming or an ordered object type) to stay equivalent.

`resolveCodexDeferredTools(request)` (request-worker.js:1577-1610):

```
referenced = {}; addedDefs = ordered map name->definition; addedRefs = {}
for each message with array content:
    content' = content minus blocks of type "tool_addition":
        tool.type == "tool_definition" && definition.name is string -> addedDefs[name] = definition
        tool.type == "tool_reference"  && tool.name is string       -> addedRefs += name
    if content' empty and content was non-empty: drop the message entirely
    else content' = renderToolReferences(content'):
        block.type == "tool_reference" (tool_name string) -> referenced += name; replace with {"type":"text","text":"Tool reference: <name>"}
        block.type == "tool_result" with array content     -> recurse into its content
if request.tools not array and addedDefs empty: return
tools = request.tools or []
loads(t) = t.name in referenced or t.name in addedDefs or (t.name in addedRefs and t has own "input_schema")
request.tools = for each t:
    non-object      -> keep
    t.defer_loading === true and !loads(t) -> drop
    no own defer_loading -> keep as is
    else -> copy without defer_loading
append every addedDefs entry whose name is not already present, copied without defer_loading
```

### 3.4 Response-side model id rewrite (proxy only)

`rewriteResponseModel(value, advertisedModel, backendModel)` (request-worker.js:1612-1623) walks every object and array recursively and
replaces any `model` key whose string value starts with `gpt-` or equals `backendModel` (= `parsed.model`) with `advertisedModel` (the
client's original id, e.g. `claude-gpt-5.6-sol[1m]` or `claude-codex-auto`). Applied to every SSE event (request-worker.js:1684), every
buffered JSON body via `rewriteCodexJson` (request-worker.js:1634-1648), including error bodies (request-worker.js:1905, 1966, 1998, 2016).
`rewriteCodexJson` returns the original buffer untouched when it is not JSON; otherwise it always re-serializes.

Plan-tool filtering on responses (`filterPlanTools = !keepPlanTools`, request-worker.js:2591):
- JSON: drop `content[]` blocks `{type:"tool_use", name in PLAN_TOOLS}`; if any were dropped and no `tool_use` remains and
  `stop_reason === "tool_use"`, set `stop_reason = "end_turn"` (request-worker.js:1638-1646).
- SSE: on `content_block_start` of a plan tool, remember its `index` and drop that event and every later event with that index; shift
  later indices down by the count dropped; rewrite `message_delta.delta.stop_reason` `tool_use` -> `end_turn` when something was dropped
  and no other `tool_use` block was kept (request-worker.js:1686-1700).

### 3.5 Grok request translation

See section 8.2.

---

## 4. Response handling

### 4.1 `forward()` response branches (request-worker.js:1840-2220), evaluated in this order

`resHeaders` = upstream headers minus `transfer-encoding`, `connection`, `keep-alive` (request-worker.js:1841-1842). "proxy" below means
`proxyProvider` is set (proxy branch); "advertised" means `advertisedModel` is set, which only happens on the proxy branch.

| # | condition | behaviour | cite |
|---|---|---|---|
| R1 | compaction retry already wrote the status line (`compactGuard.headWritten`) and the new attempt is not 2xx SSE | buffer body; on end/error/aborted: compaction telemetry `upstream_error`; if proxy and 413 and sentry on -> `noteGenuineOverflow`; codex -> `noteCodexUpstreamRejection`; write one in-band SSE `error` frame (auth-failure body for codex 401/auth-403, else `api_error` with `upstreamErrorMessage`) and end | request-worker.js:1849-1883 |
| R2 | proxy and status 403 | buffer. If body is `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`: retry the whole `forward()` after 250 ms up to `WEBSOCKET_UPGRADE_RETRIES` (2) times; exhausted -> `503` with the upstream headers and the transient body (4.4). Otherwise: codex rejection evidence, codex auth mapping, model rewrite, pass status 403 | request-worker.js:1884-1924, 1650-1666, 280-287 |
| R3 | always next | `usageCapture.setResponse(status, headers)`; compaction and non-2xx -> compaction telemetry `upstream_error` | request-worker.js:1925-1934 |
| R4 | proxy and 413 and `SENTRY_ENABLED` | buffer; on end: `normalizeGenuineContextOverflow` (section 5.4) then model rewrite; `413` with upstream headers and new `content-length`. error/aborted before end -> `502` early-end body | request-worker.js:1943-1973 |
| R5 | proxy and status >= 400 and != 413 | buffer; on end: model rewrite; codex `answerEmptyCodexCompletion` (4.3); codex rejection evidence; `answerLegacyContextOverflow` (section 5.4); else `answerUpstreamFailure` (codex auth mapping, else upstream body and status as is). error/aborted -> `502` early-end body | request-worker.js:1976-2006, 1414-1421 |
| R6 | advertised and status >= 400 (only reachable for a proxy 413 with the sentry disabled) | buffer, model rewrite, pass through | request-worker.js:2007-2022 |
| R7 | advertised and 2xx | proxy success path, 4.2 | request-worker.js:2023-2174 |
| R8 | everything else (all Anthropic responses; proxy 1xx/3xx) | `writeHead(status, resHeaders)` then `upRes.pipe(clientRes)`; bytes untouched, `content-encoding` untouched | request-worker.js:2175-2219 |

R8 observation (does not alter bytes): for 2xx `text/event-stream`, an SSE observer decodes `br`/`deflate`/`gzip` on a side branch
(`zlib`), feeds parsed events to the usage capture (and to the sentry only when proxy), stops observing after 4 MiB of unparsed pending
text (`usageEmitter.maxResponseBytes`), and calls `usageCapture.finish()` only after both the client `finish` and the observer end
(request-worker.js:2178-2208, 1724-1794). Non-SSE 2xx: `usageCapture.observeChunk(chunk)`; non-2xx: byte counting only
(request-worker.js:2209-2213). Heartbeats are added in R8 only when proxy (request-worker.js:2179), so Anthropic streams never get
`: ping` frames.

Upstream request errors (connect failure, timeout) -> if headers were sent, destroy the client socket; else `502`
`content-type: application/json` `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream <origin> failed: <e.message>"}}`
(request-worker.js:2222-2239). The timeout is `upReq.setTimeout(3600000)`, a socket-inactivity timeout, which destroys with
`Error('upstream timeout')` (request-worker.js:2221).

### 4.2 Proxy 2xx (R7)

- `content-length` is deleted from the relayed headers (request-worker.js:2024).
- SSE (`content-type` contains `text/event-stream`):
  - provider `lmstudio` and no compaction head written: `peekFirstSseEvent(upRes, 10000ms)` reads until the first complete frame with a
    `data:` line (comment-only frames skipped), puts the bytes back with `unshift`, and resumes. If that event is `type:"error"` with
    llama overflow counts -> destroy upstream, answer synthetic `413` (section 5.4). Upstream end/error/aborted before any event ->
    `502` early-end body. Timeout or non-overflow -> relay normally (request-worker.js:2129-2145, 488-546; env
    `CODEX_GATEWAY_LMSTUDIO_FIRST_EVENT_WAIT_MS`, request-worker.js:276-279).
  - `relayStream` (request-worker.js:2025-2128): write status + headers (unless a compaction attempt already did), arm the heartbeat,
    parse frames with `createCodexSseTransformer` (4.5). Per event: LM Studio usage correction, structure tracking
    (`noteCompactEvent`), cumulative usage merge, sentry record, usage capture (request-worker.js:2031-2039).
  - Non-compaction end: flush transformer; if the generation finished, record the merged usage with `ordinaryTurnCompleted = !isCompaction`;
    if the model finished but no `message_stop` arrived, write `event: message_stop\ndata: {"type":"message_stop"}\n\n`
    (request-worker.js:339, 569-574); telemetry status `upstream_error` unless deliverable; `end()` (request-worker.js:2059-2067).
    Upstream `error`/`aborted`, or `close` before `complete` -> `clientRes.destroy()` (request-worker.js:2068-2078).
  - Compaction (guarded) path: section 4.7.
- Non-SSE: buffer; LM Studio usage correction (re-serializes); `usageCapture.observeJson`; if the body is `type:"message"` without `error`,
  record sentry usage with `ordinaryTurnCompleted = stop_reason in {end_turn, stop_sequence, tool_use} && !isCompaction`; model rewrite and
  plan filter; telemetry `upstream_error` unless completed; status as upstream with new `content-length` (request-worker.js:2147-2173).

### 4.3 Synthetic and rewritten bodies, verbatim

| trigger | status | body | cite |
|---|---|---|---|
| codex 503 with error detail exactly `Codex completed without producing output` | `200` (content-type JSON or `text/event-stream`, content-length set) | non-stream: `{"id":"msg_gateway_<uuid without dashes>","type":"message","role":"assistant","model":<advertisedModel>,"content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`; stream: `message_start` (same message, `stop_reason:null`), `message_delta` `{"delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}`, `message_stop`, each as `event: <type>\ndata: <json>\n\n`. Stream-ness from the request body's `stream === true` | request-worker.js:580-605 |
| codex 401, or 403 whose detail matches `/\b(?:not authenticated\|unauthenticated\|authentication (?:is )?required\|authentication failed)\b/i` | upstream status, `content-type: application/json` | `{"type":"error","error":{"type":"authentication_error","message":"<codexReadinessMessage('auth-missing')> Upstream detail: <detail with 'claude-code-proxy codex auth login' replaced by 'the model-gateway login command above', or 'HTTP <status>'>"}}` | request-worker.js:607-620 |
| WebSocket upgrade rejected after retries | `503`, upstream 403 headers + new content-length | `{"type":"error","error":{"type":"api_error","message":"model-gateway: Codex WebSocket upgrade was temporarily rejected after <n> attempts; retry the request."}}` | request-worker.js:1658-1666, 1916-1921 |
| buffered upstream body broke before end | `502` `content-type: application/json` (if headers not sent) | `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}` | request-worker.js:1950-1954, 1983-1987, 2139-2143 |
| in-band error after the compaction status line was sent | (SSE frame) | `event: error\ndata: {"type":"error","error":{"type":<type>,"message":<msg>}}\n\n`, msg = `model-gateway: upstream returned <status>: <detail>` or `model-gateway: upstream returned <status> with no readable error body` | request-worker.js:548-567 |
| compaction stream ended without a finished generation after retries | (SSE frame) | `api_error`, `model-gateway: the <ProviderLabel> compaction stream ended without a completed generation after <n> attempt(s); the summary above is incomplete` | request-worker.js:2118-2122 |
| tool schema refusal | `400` JSON | `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: Codex tool schema compatibility refused tool <name> at <pointer>; reason=<code>. Nothing was forwarded or rerouted."}}` | request-worker.js:2518-2540 |
| dispatch without exactly one usable marker | `400` JSON | section 7 | request-worker.js:2487-2507 |
| context overflow (sentry or normalized) | `413` | section 5.5 | request-worker.js:1344-1448 |

`upstreamErrorDetail(body)` = `error.message` or top-level `message` when a non-empty string, else `null` (or the trimmed plain text when
asked) (request-worker.js:553-560).

### 4.4 Codex upstream state side effects

| event | effect | cite |
|---|---|---|
| route telemetry `finish` with `route.backend === "codex"`, status ok and 2xx | delete `codex-upstream-blocked.json` and `codex-upstream-unavailable.json` (sync `rmSync`) | request-worker.js:1159-1162 |
| same, status >= 500 or override `upstream_error`/`upstream_aborted` | write `codex-upstream-unavailable.json` (unless a block is active), TTL 30 s | request-worker.js:1163-1165; codex-upstream-state.js:75-84 |
| codex 401/403/429 with an `x-openai-*` / `openai-processing-ms` header or `/\bopenai\b/i` in the body | write `codex-upstream-blocked.json`; only 429 expires (`retry-after` seconds or HTTP date, else `anthropic-ratelimit-unified-reset` epoch seconds, else 60 s) | request-worker.js:176-191; codex-upstream-state.js:34-63 |
| client abort (`req aborted` or `res close`) | telemetry cancelled -> no state change | request-worker.js:1152-1156, 2421, 2427 |

Note `res.once('close')` cancels telemetry (request-worker.js:2427), and `close` also fires after a normal response; `finish` normally
runs first so the state is written, but any `finish` call made after `close` is dropped.

### 4.5 SSE framing in the proxy transformer (request-worker.js:1668-1722)

- Decode chunks with a UTF-8 `StringDecoder`; split on the first `/\r?\n\r?\n/`; keep the separator as matched.
- Frame without a `data:` line, with an empty data, `[DONE]`, or unparsable JSON -> written through unchanged.
- Only the **first** `data:` line is parsed (a frame with JSON split over several `data:` lines fails to parse and passes unchanged).
- Parsed frames are rebuilt: the data line becomes `data: <JSON.stringify(event)>`, lines joined with `\n` (a CRLF frame loses its inner
  `\r`), followed by the original separator. `end()` flushes a trailing partial frame with no separator.
- No decompression on this path: if the proxy compressed (client `accept-encoding` is forwarded), the transformer would see binary.
  Whether the proxy ever compresses is UNVERIFIED (section 14).

### 4.6 Limits and timeouts

| item | value | cite |
|---|---|---|
| request body size | **no limit**; buffered twice (`chunks` and `routeContexts.requestChunks`) | request-worker.js:2428-2429, 1127-1128 |
| server `requestTimeout` / `headersTimeout` / `keepAliveTimeout` | 0 (off) / 120000 ms / 75000 ms | request-worker.js:2682-2684 |
| upstream socket inactivity | 3600000 ms (Anthropic, proxy, Grok) | request-worker.js:2221, 2350 |
| compaction buffer | 16 MiB (`CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES`), then degrade to live streaming | request-worker.js:314-317, 2045-2051 |
| SSE observer pending text (R8) | `usageEmitter.maxResponseBytes`, default 4 MiB (`CODEX_GATEWAY_USAGE_MAX_RESPONSE_BYTES`, min 1024) | request-worker.js:2187; usage-observability.js:11, 986-988 |
| LM Studio first-event wait | 10000 ms | request-worker.js:276-279 |
| heartbeat | 20000 ms (`CODEX_GATEWAY_SSE_HEARTBEAT_S`, seconds; 0 disables) | request-worker.js:268-271 |
| WS-upgrade retries / delay | 2 / 250 ms (`CODEX_GATEWAY_WS_UPGRADE_RETRIES`, `..._RETRY_DELAY_MS`) | request-worker.js:280-287 |
| compaction retries / delay | 2 / 250 ms (`CODEX_GATEWAY_COMPACT_STREAM_RETRIES`, `..._RETRY_DELAY_MS`) | request-worker.js:306-313 |
| Anthropic catalog | 5000 ms per page, 2 MiB per page, 10 pages | anthropic-catalog.js:46, 53, 62 |
| proxy `/v1/models` fetch | 2500 ms (catalog), 2000 ms (readiness) | request-worker.js:1480, 58 |
| `codex auth status` | 15000 ms exec timeout | request-worker.js:51 |
| route span POST | 500 ms | runtime.js:41; request-worker.js:1007 |

### 4.7 Abort, back-pressure, keep-alive

- Client -> upstream: **no code found** that aborts the upstream request when the client disconnects. `req aborted` and `res close` only
  cancel telemetry (request-worker.js:2421, 2427); `clientReq.pipe(upReq)` never carries the body because `body` is always the buffered
  `raw` or a string. An abandoned stream keeps the upstream generation (and the Codex billing) running until it ends or idles for an hour.
  The heartbeat timer stops on client `close` (request-worker.js:1818).
- Upstream -> client: `error`, `aborted`, or `close` before `complete` destroy the client response (request-worker.js:2068-2078,
  2214-2218, 2323-2327).
- Back-pressure: only R8 uses `pipe` (back-pressure honoured). Every transformed or buffered path calls `clientRes.write` and ignores its
  return value (request-worker.js:2042-2053, 2303), so Node buffers without bound when the client reads slowly.
- Heartbeat (`keepSseAlive`, request-worker.js:1796-1820): after the head, a timer re-armed on each upstream `data` writes `: ping\n\n`
  after 20 s of upstream silence; stops on upstream end/error/aborted or client close. Applied to proxy streams only
  (request-worker.js:2028, 2179).

### 4.8 Compaction stream guard (request-worker.js:2081-2127, 289-342, 622-701)

Enabled by `CODEX_GATEWAY_COMPACT_STREAM_GUARD !== "0"`, for proxy requests on exact `/v1/messages` whose payload is a compaction
request (`stream === true` and system text contains `You are a helpful AI assistant tasked with summarizing conversations.`)
(request-worker.js:292, 386-389, 2586-2588). Guard object `{attempts:0, headWritten:false, observedUsage}`.

| step | rule |
|---|---|
| buffering | every transformed frame goes to `attempt.chunks`; over 16 MiB -> log, flush buffered, set `degraded`, stream live from then on |
| per-event tracking | `message_stop` -> `terminal`; `message_delta` with `stop_reason` in `{end_turn, stop_sequence}` -> `endOfGenerationStop`; content block start/delta/stop after that -> `contentAfterStop`; `openBlocks` +/-; non-blank `text_delta` -> `hasVisibleText`; `error` event -> `sawError`, `errorCode`, `fatal` if type in the fatal set |
| finished | `terminal` OR (`endOfGenerationStop` and no `contentAfterStop` and `openBlocks === 0`) |
| deliverable | finished and no `sawError` |
| at upstream end/error/aborted | if client gone: stop. If not deliverable, not fatal, not degraded, and `attempts < 2`: merge usage, reset capture, `attempts++`, destroy upstream, re-run `forward()` after 250 ms (status line already sent, so R1 handles a non-2xx retry) |
| else | outcome = `upstream_error` (non-2xx) / `completed` or `empty_summary` (deliverable, by visible text) / `aborted` (override `upstream_aborted`) / `unknown_error` / `incomplete`; telemetry; flush buffered chunks; write synthesized `message_stop` if deliverable without terminal; write the "ended without a completed generation" error frame if not deliverable and no upstream error frame was seen; end |
| fatal error types | `invalid_request_error`, `authentication_error`, `permission_error`, `not_found_error`, `request_too_large`, `rate_limit_error`, `billing_error` (request-worker.js:320-328) |

---

## 5. Context sentry state machine (synthetic 413)

Purpose: Claude Code never learns a gateway row's real window (runtime.js:254-259), so the gateway refuses the next turn of a session
whose previous turn crossed the compaction trigger with a 413 `request_too_large`, which makes Claude Code compact.

### 5.1 Switches and keys

| item | definition | cite |
|---|---|---|
| global switch | `SENTRY_ENABLED = CODEX_GATEWAY_SENTRY !== "0"` | request-worker.js:255 |
| session key | `JSON.stringify([x-claude-code-session-id, x-claude-code-agent-id or null])`; `null` (no sentry) when the session header is absent/empty or the sentry is off. No `safeMetadataId` validation here (unlike dispatch) | request-worker.js:1299-1302, 1262-1265, 1174-1177 |
| model key | `codexContextWindowModelId(model)`: backend id, `-fast`/`-ultrafast` removed only for `gpt-` ids, `default` when empty | runtime.js:400-404 |
| policy | `effectiveSentryPolicy(resolveGatewayModelPolicy(model))`; `null` (sentry inert) unless `policy.sentry === "synthetic-413"` | request-worker.js:256-258; runtime.js:358-365 |
| session state | `sentrySessions: Map<sessionKey, Map<modelKey, {usage:number, fired:bool}>>`, created lazily on first touch, never evicted | request-worker.js:1222, 1318-1332 |
| model state | `sentryModels: Map<modelKey, {backendWindow, compactTrigger, source, observedCeiling}>`; recreated (learning discarded) when the policy's `backendWindow` changes, e.g. llama-server restarted with another `n_ctx` | request-worker.js:1223, 1304-1316 |

### 5.2 Trigger (`effectiveSentryPolicy`, runtime.js:331-365)

Candidates; the lowest `compactTrigger` wins and its `source` is kept:

| source | value | when |
|---|---|---|
| `derived` | `backendWindow - headroom`; headroom = `policy.compactHeadroom` if a positive safe integer else 40000 | always (throws if `backendWindow <= headroom` or not finite, runtime.js:350-356) |
| `compact-at` | `min(compactAt, cap ?? Infinity)` | `compactAt.<backend>` saved in `~/.claude/model-gateway/context-window.json`; when set, `env` and `cap` are not candidates |
| `env` | `CODEX_GATEWAY_COMPACT_TRIGGER` if finite > 0 | no compact-at |
| `cap` | `cap - 85000` (`2*40000 + 5000`) | integer window cap for the backend (saved, or `CODEX_GATEWAY_CONTEXT_WINDOW` for codex) |

Defaults: `DEFAULT_CONTEXT_WINDOWS = {claude:'full', codex:272000, grok:'full'}` (runtime.js:247), so with nothing saved a Codex row's
trigger is `272000 - 85000 = 187000` (`cap`), not `920012 - 40000`. Grok rows default to `500000 - 40000 = 460000` (`derived`). The
settings are read once when runtime.js loads (`CONTEXT_WINDOWS`, runtime.js:319); the worker does not see a change until it restarts.
Live-window rows (local llama-server, LM Studio): headroom `min(40000, floor(window/3))`; `sentry: 'none'` when window < 32768
(runtime.js:414-435). The startup log line per table row is `model-gateway: sentry policy id=<id> backendWindow=<w> sentry=<...> effectiveTrigger=<t> source=<s>` (request-worker.js:1450-1459).

### 5.3 State table (per session key x model key)

`T` = current `compactTrigger` of the model state. `u` = input tokens of an observed usage = `input_tokens + cache_read_input_tokens +
cache_creation_input_tokens` (finite values only) (request-worker.js:344-347). Low watermark `L = T - min(40000, 0.25*T)`
(request-worker.js:358-361).

| state | meaning | new non-compaction request on exact `/v1/messages` (proxy) or any `/v1/messages*` (Grok) | compaction request | usage observation with `u > 0` | genuine upstream overflow |
|---|---|---|---|---|---|
| ABSENT | no entry | entry created `{usage:0, fired:false}` -> ARMED_LOW, forwarded | forwarded (exempt before lookup) | entry created, `usage=u` -> ARMED_LOW/HIGH | entry created, `fired=true` -> FIRED |
| ARMED_LOW | `!fired && usage <= T` | forwarded | forwarded | `usage=u` -> ARMED_LOW or ARMED_HIGH | `fired=true` -> FIRED |
| ARMED_HIGH | `!fired && usage > T` | **synthetic 413**, `fired=true` -> FIRED; nothing is sent upstream | forwarded | `usage=u` -> ARMED_LOW or ARMED_HIGH | `fired=true` -> FIRED |
| FIRED | `fired` | forwarded (the latch fires once per crossing) | forwarded | `usage=u`; rearm (-> ARMED_*) iff `u < L` or (`ordinaryTurnCompleted` and `u > T`) | stays FIRED |

Notes on the table:

- Evaluation order on a request (`fireContextSentry`, request-worker.js:1360-1377): compaction exemption first (by re-parsing the
  **original** buffered client bytes from `routeContexts`), then `crossedContextSentry` (policy null -> pass; no session -> pass;
  `fired` -> pass; `usage <= T` -> pass) (request-worker.js:1351-1358).
- `usage <= 0` observations are ignored (request-worker.js:1339); only events of `type === "message_delta"` count
  (request-worker.js:1335).
- Where observations come from:

| path | per-event record (`ordinaryTurnCompleted=false`) | end-of-response record | cite |
|---|---|---|---|
| proxy stream, normal | every event (gate `filterPlanTools \|\| (SENTRY_ENABLED && sessionId)`; filterPlanTools is true by default) | if deliverable: merged usage (message_start usage + every later usage, later fields override) with `ordinaryTurnCompleted = !isCompaction` | request-worker.js:2036-2037, 2062-2063, 353-356 |
| proxy stream, compaction guard | every event | none | request-worker.js:2037, 2081-2127 |
| proxy JSON | - | if `type:"message"`: `usage`, `ordinaryTurnCompleted` = stop_reason in `{end_turn, stop_sequence, tool_use}` and not compaction | request-worker.js:2160-2166 |
| proxy in R8 | dead code: the R8 sentry observer needs `proxyProvider` and a 2xx SSE response, but a proxy 2xx always takes R7, so it never runs | - | request-worker.js:2176-2185 |
| Grok stream | each frame with `response.usage` (converted, cache fields absent) | if `response.completed` seen and no error: last usage, `ordinaryTurnCompleted = !isCompaction` | request-worker.js:363-378, 2319-2320 |
| Grok JSON | - | `usage` with `ordinaryTurnCompleted = status === "completed" && !isCompaction` | request-worker.js:2335-2337 |
| Anthropic | never | never | request-worker.js:2676 (`sessionId=null`) |

### 5.4 Genuine overflow handling and ceiling learning

`noteGenuineOverflow(sessionKey, model, {learnCeiling=true})` (request-worker.js:1382-1396): set `fired=true`; if the model has a policy,
`learnCeiling`, `source === "derived"` and the session's last `usage > headroom`: `observedCeiling = min(previous, usage)`,
`compactTrigger = max(1, observedCeiling - headroom)`. The learned trigger is per model key and shared by every session. Returns the
session's last usage.

| upstream signal (proxy only) | detection | action | cite |
|---|---|---|---|
| HTTP 413 with `error.type === "request_too_large"` and string message | `parsedRequestTooLarge` | learn ceiling; keep upstream wording; if the message has no `N tokens > M tokens`, append ` (<usage or limit+1> tokens > <codexContextWindow(model)> tokens)`; append the gateway suffix; add `error.overflow`; 413 with upstream headers | request-worker.js:1400-1412, 426-438 |
| HTTP 413 other shape | - | passed through after model rewrite (`normalize` returns the body unchanged) | request-worker.js:1402 |
| HTTP 4xx/5xx != 413: llama-server `error.type === "exceed_context_size_error"` (`n_prompt_tokens`, `n_ctx`), or LM Studio `api_error` whose message contains `exceed_context_size_error` (embedded JSON after the first `{`, else wording `request (N tokens) exceeds the available context size (M tokens)`) | `llamaContextOverflow` | `answerContextOverflow` with engine counts; `learnCeiling = !limitTokens` | request-worker.js:440-465, 1425-1448 |
| HTTP 4xx/5xx != 413 whose body text matches `/context window\|context length\|input exceeds\|prompt token count\|too many tokens/i` | regex | `answerContextOverflow(null, ...)`: tokens = previous usage | request-worker.js:1427 |
| LM Studio HTTP 200 stream whose first data event is `type:"error"` with llama counts | `peekFirstSseEvent` | same as the row above, before any status line is written | request-worker.js:2129-2136 |

`answerContextOverflow` (request-worker.js:1432-1448): status `413`, headers `content-type: application/json`, `content-length`,
`x-model-gateway-upstream-status: <original status>`. Message `Input exceeds the model context window; compact and retry.` plus
` (<tokens or limit+1> tokens > <limit> tokens)` when counts exist or the sentry is on.

LM Studio usage correction (`exclusiveLmStudioUsage`, request-worker.js:467-486): when `input_tokens >= cache_read_input_tokens > 0`,
`input_tokens -= cache_read_input_tokens`, applied to the event object before it is re-serialized (stream) and to buffered JSON
(re-serialized). Applied only for provider `lmstudio` (`LMSTUDIO_PROVIDER`, lmstudio.js:14).

### 5.5 413 body shape

```json
{"type":"error","error":{
  "type":"request_too_large",
  "message":"<lead> (<N> tokens > <M> tokens) [model-gateway: phase <phase>, refused by <refused_by>; request <bytes> bytes, <N> tokens from <tokens_from>|tokens UNVERIFIED, limit <M> tokens; recovery: <text>]",
  "overflow":{"phase":"first_turn|continuation|compaction|UNVERIFIED","refused_by":"gateway_sentry|upstream","request_bytes":n,"tokens":n|null,"tokens_from":"previous_turn_usage|upstream"|null,"limit_tokens":n}
}}
```

- Sentry lead: `Prompt is too long for the <ProviderLabel> context window; compact and retry.` (request-worker.js:1368-1370); provider
  labels from proxy-models.js:14-29 (`codex`->`Codex`, `grok`->`Grok`, `lmstudio`->`LM Studio`, `local`->`Local GGUF`, unknown ->
  capitalized, empty -> `Proxy`).
- The `(N tokens > M tokens)` part must stay first; Claude Code parses it (request-worker.js:418).
- `phase` (request-worker.js:400-404): `compaction` if compaction request; `UNVERIFIED` if `messages` not an array; `continuation` if any
  message has `role:"assistant"`; else `first_turn`. Recovery texts verbatim at request-worker.js:393-398.
- `request_bytes`: original client bytes for the sentry 413, the re-serialized forwarded body for upstream overflows
  (request-worker.js:1366 vs 1406, 1437).
- The sentry 413 has only `content-type` and `content-length` headers (request-worker.js:1371-1374).

---

## 6. Catalog and discovery

### 6.1 In-memory catalog `modelCache` (request-worker.js:1211-1214, 1477-1526)

| step | behaviour | cite |
|---|---|---|
| initial | `{source:"fallback", data: gatewayModel(id) for DEFAULT_MODELS (gpt-6.1-sol, gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna, gpt-6-astra) + "auto" if CODEX_GATEWAY_LIST_DISPATCH_MODEL=1}` | request-worker.js:226-228, 1211-1214; runtime.js:39 |
| fetch | `GET http://127.0.0.1:<PROXY_PORT>/v1/models` (timeout 2500); status 200 -> `parseProxyModelListing(body.data)` | request-worker.js:1480-1481 |
| listing parse | skip non-string/empty ids, ids matching `/^(?:claude-\|anthropic-)\|^(?:haiku\|sonnet\|opus\|fable)$/`, repeats; provider from `display_name` `"<id> (<provider>)"`, else `codex` for `/^gpt-/`, else skip | proxy-models.js:13, 31-52 |
| non-empty listing | `setProxyModelRoutes(routes)`, `source="proxy"`, reset retry delay | request-worker.js:1483, 1490-1492 |
| empty/failed listing and no previous routes | `models.json` (`~/.claude/model-gateway/models.json`, list of `gpt-*` ids) -> `source="models.json"`, else DEFAULT_MODELS -> `source="fallback"`; failed listing with previous routes keeps them and the previous source | request-worker.js:1493-1497, 237-239; proxy-models.js:57-61 |
| retry | on an empty listing: one unref'd timer, 1000 ms doubling to 30000 ms | request-worker.js:1465-1475, 230-231 |
| live windows | `refreshLocalServerState`, `refreshLmStudioState` (bodies UNREAD) | request-worker.js:1502 |
| rows | `[codex rows (+auto)] ++ [Grok backend rows] ++ [other proxy rows]` | request-worker.js:1507-1522 |
| proxy rows | `advertisedProxyRoutes(routes)` (drops `vertex/<id>` when bare `<id>` is also vertex) filtered to `transport === "proxy"` | request-worker.js:1504; proxy-models.js:116-119 |
| Grok rows | `~/.grok/models_cache.json` (or `CODEX_GATEWAY_GROK_HOME`) via `grokModelsFromCache`, else `GROK_MODELS`; kept only if `transport === "grok"` (so a cached id absent from `MODEL_WINDOW_POLICY` is not advertised) | request-worker.js:1505-1516, 233-235 |
| persist | only when `source === "proxy"`: `syncDiscoveryCache()` | request-worker.js:1524-1525 |
| schedule | at start (with sentry-policy log) and `setInterval(refreshModels, 60000)` (env `CODEX_GATEWAY_MODELS_REFRESH_MS`), unref'd | request-worker.js:1535-1538, 266-267 |

Row shape `gatewayModel(id, backend='codex')` (request-worker.js:703-712); returns `null` (dropped) when the policy's backend differs:

```json
{"id":"<gatewayClientModelId(id)>","display_name":"<name>","type":"model","max_input_tokens":<gatewayAdvertisedWindow(id) || codexContextWindow(id)>}
```

- `id`: picker alias (`claude-<id>[1m]`) when the advertised window > 200000, else `claude-<id>` (runtime.js:506-512). `auto` ->
  `claude-codex-auto`, display `Sidequest Dispatch (Codex)`.
- display: codex `id.replace(/^gpt-/,'GPT-').replace(/\[1m\]$/,'') + ' (Codex)'`; Grok `id.replace(/^grok-/,'Grok ').replace(/-/g,' ')`;
  others `<id> (<ProviderLabel>)` (request-worker.js:216-220; proxy-models.js:155-157).
- `max_input_tokens`: `min(advertisedWindow, cap)` when a cap is set (runtime.js:499-504).

### 6.2 `GET /v1/models` response (request-worker.js:2409-2414)

`data = withPreviousServerRows([...advertisedAnthropicRows(await anthropicModels(req.headers)), ...modelCache.data])`. There is no
de-duplication between Anthropic rows and gateway rows here.

Anthropic catalog (`createAnthropicCatalog`, anthropic-catalog.js:81-105):

| item | behaviour |
|---|---|
| cache file | `~/.claude/model-gateway/anthropic-models.json`, `{"data":[rows],"updatedAt":ms,"error":null}`; on failure `{...previous, "error":"<msg>","attemptedAt":ms}` (updatedAt unchanged); pretty JSON, mode 0600, atomic (anthropic-catalog.js:10, 72-75, 86-92) |
| validity | rows with `id` matching `/^claude-[a-z0-9._-]+$/` and string `display_name`; other fields kept as sent (anthropic-catalog.js:20-24) |
| TTL | 1 h from `updatedAt` (anthropic-catalog.js:11, 98) |
| credentials | from the incoming request: only `authorization`, `x-api-key`, `anthropic-version`, `anthropic-beta`, `anthropic-workspace-id`, `user-agent`; `anthropic-version: 2023-06-01` default; no fetch without `authorization` or `x-api-key` (anthropic-catalog.js:12-18, 77-79, 100) |
| fetch | `GET <ANTHROPIC_UPSTREAM>/v1/models?limit=1000`, follow `last_id` as `after_id` while `has_more`, max 10 pages, 5 s and 2 MiB per page, non-200 = failure (anthropic-catalog.js:33-70) |
| DNS | always via the hosts-bypass `lookup` (passed unconditionally, request-worker.js:1233), unlike `forward()` which uses it only in compat mode |
| concurrency | single in-flight refresh; stale cached data is returned immediately if non-empty, else the caller awaits the refresh (anthropic-catalog.js:82, 101-103) |
| triggers | every `GET /v1/models`, and every Anthropic passthrough request (`void anthropicModels(req.headers)`, request-worker.js:2650); each call reads the cache file synchronously |
| on success | `onRefresh` -> `syncDiscoveryCache(true)` (request-worker.js:1233) |
| id rewrite | `advertisedAnthropicRows` maps `id` through `gatewayClientModelId` (anthropic-catalog.js:107-109); for `anthropic` rows this yields the same `claude-...` id |

"(previous)" relabelling (`withPreviousServerRows`, previous-picker-rows.js:40-44): reads `additionalModelOptionsCache` from
`$CLAUDE_CONFIG_DIR/.claude.json` or `~/.claude.json` (read-only); for each cached row whose `value` parses as a pin version older than the
current pin of its family (`effectivePins`, UNREAD), prepend `{id:value, display_name:"<modelDisplayName(value)> (previous)"}` and drop
gateway rows with that id. This is the "relabelling of older Fable rows" path (previous-picker-rows.js:8-12).

### 6.3 Claude Code discovery cache (`syncGatewayDiscoveryCache`, runtime.js:589-629)

| item | behaviour |
|---|---|
| input | `withPreviousServerRows(withCachedAnthropicRows(modelCache.data))` (request-worker.js:1530); `withCachedAnthropicRows` puts cached Anthropic rows first and drops gateway rows with the same id (anthropic-catalog.js:111-115) |
| base URL | `effectiveBaseUrl().value` (settings-wiring.js, UNREAD); skipped (`rc-compatibility`) when it is `http://api.anthropic.com`, skipped (`gateway-not-wired`) unless it equals `http://127.0.0.1:<PUBLIC_SHIM_PORT>` |
| filter | only rows whose `id` matches `/(claude\|anthropic)/i`; each becomes `{id: gatewayClientModelId(id), display_name?}` |
| file | `$CLAUDE_CONFIG_DIR/cache/gateway-models.json` (default `~/.claude/cache/gateway-models.json`, runtime.js:552): `{"baseUrl":"...","fetchedAt":ms,"models":[{"id","display_name"}]}`, pretty JSON, mode 0600, atomic, then `chmod 0600` |
| skip-if-same | unless `force`, not rewritten when ids and display names are equal in order |
| errors | logged `model-gateway: could not update discovery cache (<code or message>)` (request-worker.js:1531-1533) |

---

## 7. Dispatch (`claude-codex-auto`) and the session route cache

### 7.1 Marker and resolution (request-worker.js:714, 834-879, 2466-2508)

Marker regex (global): `\[(sidequest-route) model=([a-z0-9][a-z0-9.-]{0,63})(?: effort=(low|medium|high|xhigh|max))?(?: ticket=([A-Za-z][A-Za-z0-9_-]{0,63}))?\]`.
Only `role:"user"` messages count: a string `content`, or `type:"text"` blocks of array content. `tool_result` blocks and other roles are
ignored (request-worker.js:854-875).

Identity (`dispatchRequestIdentity`, request-worker.js:816-832):
- `sessionId` = header `x-claude-code-session-id` if `safeMetadataId` accepts it, else `metadata.user_id` parsed as JSON `.session_id`,
  else the text after the last `_session_` in `metadata.user_id` (request-worker.js:804-814). `safeMetadataId`: 1-128 chars,
  `/^[a-zA-Z0-9][a-zA-Z0-9._:/\[\]-]*$/` (request-worker.js:969-972). No session -> identity `null`.
- `key = JSON.stringify([sessionId, agentId])`, except `null` when a parent-agent header is present without an agent id.
- `parentKey = JSON.stringify([sessionId, parentAgentId])` when both agent headers are present and differ.

| markers in request | resolution | `via` |
|---|---|---|
| exactly 1 | use it; `cache.set(key, route)` (no-op when key null) | `dispatch` |
| 0 | `cache.get(key)` | `dispatch-cached` |
| 0, own key missed | `cache.get(parentKey)`; on hit copy to own key | `dispatch-inherited` |
| >= 2, or 0 with no cache hit | `400` `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: dispatch model requires exactly one [sidequest-route model=... effort=... ticket=...] marker in the conversation; redispatch the ticket"}}`; route log `via:"dispatch-unbound"`; telemetry override `invalid_route` | - |

After resolution: `parsed.model = route.model` (any id the regex allows; not checked against the catalog), `effort` overrides
`output_config.effort`; the request goes to the proxy with provider `codex` (schema adaptation applies). `ticket` becomes the usage
record's `ticketRef` except on `dispatch-inherited` (request-worker.js:2582). The route-span `via` attribute allows only
`direct|dispatch|dispatch-cached` (request-worker.js:1021, 1051), so `dispatch-inherited` is exported as null.

### 7.2 `DispatchSessionRouteCache` (request-worker.js:724-802)

| item | behaviour |
|---|---|
| file | `~/.claude/model-gateway/dispatch-routes.json` (env `CODEX_GATEWAY_DISPATCH_CACHE_PATH`, runtime.js:38) |
| format | `{"version":1,"routes":[["<key>",{"model":"...","effort":"..."|null,"ticket":"..."|null,"lastUsedAt":ms}], ...]}` + `\n`, compact JSON, mode 0600, atomic (request-worker.js:788-794) |
| TTL | sliding 4 h since `lastUsedAt` (`CODEX_GATEWAY_DISPATCH_CACHE_TTL_MS`) (request-worker.js:715-718) |
| capacity | 500 keys (`CODEX_GATEWAY_DISPATCH_CACHE_MAX_SESSIONS`); LRU by Map insertion order (`get` deletes and re-inserts), oldest evicted first (request-worker.js:719-722, 781-786) |
| get | null key or miss -> null; expired -> delete, persist, null; hit -> refresh `lastUsedAt`, move to newest, **persist** (a file write per cached dispatch request) |
| set | validates route (`validDispatchRoute`, request-worker.js:797-802), prune, insert, prune, persist |
| load | at construction; skips invalid entries; prune; persist; any error ignored (request-worker.js:766-779) |
| write errors | ignored silently (request-worker.js:793) |

---

## 8. Grok backend

### 8.1 Endpoint, credentials, headers

| item | behaviour | cite |
|---|---|---|
| endpoint | `POST https://cli-chat-proxy.grok.com/v1/responses` (override `CODEX_GATEWAY_GROK_ENDPOINT`); http or https by URL scheme; shared keep-alive agents | grok-backend.js:8; runtime.js:31; request-worker.js:2258-2263 |
| auth file | `~/.grok/auth.json` (dir override `CODEX_GATEWAY_GROK_HOME`); first top-level key starting `https://auth.x.ai::` whose value is an object; access token `value.key` (non-empty string required) | grok-backend.js:18-50 |
| expiry | `expires_at` ISO string, or number (seconds if < 1e11, else ms); refresh when unparseable or within 5 min | grok-backend.js:52-64 |
| refresh | needs `refresh_token` and `oidc_client_id`; token endpoint from `<oidc_issuer or https://auth.x.ai>/.well-known/openid-configuration` `token_endpoint`, fallback `https://auth.x.ai/oauth/token` (or `<issuer>/oauth/token`); `POST application/x-www-form-urlencoded` `grant_type=refresh_token&refresh_token=..&client_id=..`; 2xx with `access_token` required; updates `key`, optional rotated `refresh_token`, `expires_at = now + expires_in*1000` (ms); rewrites the whole auth file pretty-printed, mode 0600, atomic | grok-backend.js:70-113 |
| refresh limits | no request timeout in `requestJson`; no lock or single-flight, so concurrent requests refresh concurrently | grok-backend.js:393-408, 115-119 |
| errors | any token error -> client `401` `{"type":"error","error":{"type":"authentication_error","message":"model-gateway: <msg>"}}`, telemetry override `authentication_error`; the `<msg>` texts are listed verbatim after this table | grok-backend.js:43-105; request-worker.js:2246-2254 |
| headers | `authorization: Bearer <token>`, `content-type: application/json`, `user-agent: xai-grok-build/<v>`, `x-grok-client-version: <v>`, `x-grok-client-identifier: xai-grok-cli`, `x-grok-model-override: <model>`, `content-length`; `<v>` from `~/.grok/version.json` `version` or `client_version` matching `/^\d+\.\d+\.\d+/`, else `1.0.60` | grok-backend.js:121-139, 11; request-worker.js:2262 |
| model id | `grokModelFromPicker(pickerId, cachedIds or GROK_MODELS ids)`: compare with dots flattened to dashes, so legacy `claude-grok-4-5` resolves to `grok-4.5`; no match -> `grok-<pickerId>` | grok-backend.js:413-423; request-worker.js:2601-2603 |
| model list | `GROK_MODELS` = grok-4.7, grok-4.6, grok-4.5 (context 500000, reasoning true); cache file `models_cache.json` accepted as array, `{models:[...]}`, `{data:[...]}`, or `{models:{...}}`; entries string or object (`info` sub-object honoured), id `/^grok-[a-z0-9.-]+$/i`, `context_window`, `supports_reasoning_effort` | grok-backend.js:12-16, 425-456 |

Token error messages, verbatim (the backticks around `grok` are part of each message):

```text
Grok CLI auth is missing. Run `grok` and log in again.          (grok-backend.js:43)
Grok CLI auth is invalid. Run `grok` and log in again.          (grok-backend.js:47)
Grok CLI auth cannot refresh. Run `grok` and log in again.      (grok-backend.js:87)
Grok CLI auth refresh failed. Run `grok` and log in again.      (grok-backend.js:102, 105)
```

### 8.2 Request translation `translateRequest(payload, model)` (grok-backend.js:141-223)

```
request = { model, input: translateInput(payload), max_output_tokens: payload.max_tokens, stream: payload.stream === true }
tools'  = translateTools(payload.tools); if non-empty: request.tools = tools'
if output_config.effort is string and model is in static GROK_MODELS with reasoning: request.reasoning = { effort }  (value passed raw)

translateInput:
  system (string, or text blocks joined "\n") non-empty -> {role:"developer", content:[{type:"input_text", text}]}
  for each message: role = "assistant" if assistant else "user"
    string / text block -> assistant: {type:"output_text"} ; else {type:"input_text"}
    tool_use    -> flush text, then item {type:"function_call", call_id: id, name, arguments: JSON.stringify(input || {})}
    tool_result -> flush text, then item {type:"function_call_output", call_id: tool_use_id, output: concatenated text blocks}
    everything else (image, document, thinking, redacted_thinking, server tool blocks) -> dropped
    text items are grouped into {role, content:[...]} runs between function items

translateTools:
  type starts "web_search", or name "web_search" without input_schema -> {type:"web_search"}
  no name -> dropped
  else {type:"function", name, description (omitted if empty), parameters: input_schema || {type:"object", properties:{}}}
```

Not translated: `temperature`, `top_p`, `stop_sequences`, `tool_choice`, `thinking`, `metadata`, `cache_control`. No tool-schema
compatibility copy, deferred-tool hoisting or plan-tool removal on this branch.

### 8.3 Response translation

Non-streaming (`translateResponse`, grok-backend.js:249-279; request-worker.js:2330-2343): parse body (invalid JSON -> `null`);
`output[]` items: `message` -> each `output_text` part -> `{type:"text"}`; `web_search_call` -> `server_tool_use` +
`web_search_tool_result` (sources with string `url` -> `{type:"web_search_result", title (or url), url}`); `function_call` ->
`{type:"tool_use", id: call_id, name, input: JSON.parse(arguments) or {}}`. Envelope `{id: response.id or "msg_grok_<ms>", type:"message",
role:"assistant", model: advertisedModel, content, stop_reason: "tool_use" if any tool_use else "end_turn", stop_sequence:null, usage}`.
`usage = {input_tokens, output_tokens, output_tokens_details:{reasoning_tokens}}` (grok-backend.js:225-231), no cache fields. Status is
the upstream status; headers are upstream headers minus hop-by-hop and `content-length`, with `content-type: application/json` and a new
`content-length`.

Streaming (`createGrokSseTransformer`, grok-backend.js:285-391; frame loop request-worker.js:2299-2328): head = upstream headers minus
hop-by-hop and `content-length`, `content-type: text/event-stream`. Frames split on `\r?\n\r?\n`; first `data:` line, trimmed; `[DONE]`
and invalid JSON skipped.

| Responses event | emitted Anthropic events |
|---|---|
| `response.created` | `message_start` once (`model = advertisedModel`, usage from response) |
| `response.output_text.delta` | start; text block keyed by `item_id`/`output_index` (closing the previous active block); `content_block_delta` `text_delta` |
| `response.output_item.added` with `function_call` | start; `content_block_start` `tool_use {id: call_id, name, input:{}}` |
| `response.function_call_arguments.delta` | `content_block_delta` `input_json_delta` |
| `response.function_call_arguments.done` | full `arguments` as one `input_json_delta` only if no delta was seen |
| `response.output_item.done` `web_search_call` | `server_tool_use` block start+stop, `web_search_tool_result` block start+stop |
| `response.output_item.done` other | `content_block_stop` for that item |
| `response.completed` | stop all open blocks; `message_delta {stop_reason: tool_use|end_turn}` with usage; `message_stop` |
| `error`, `response.failed` | `event: error` frame `api_error` `model-gateway: Grok upstream failed: <message>`; the stream is not stopped |
| upstream end | if started and not stopped: `message_delta end_turn` + `message_stop` (also after an error frame or `response.incomplete`) |

Each emitted frame is `event: <type>\ndata: <json>\n\n`. No heartbeat on Grok streams.

### 8.4 Errors

| upstream | client | cite |
|---|---|---|
| 426 | `426` `{"type":"error","error":{"type":"api_error","message":"model-gateway: Grok CLI version header is outdated. Update the grok CLI, then retry."}}` | request-worker.js:2272-2282 |
| other non-2xx | same status, `{"type":"error","error":{"type":"authentication_error" if 401 else "api_error","message":"model-gateway: <error.message, or error string, or 'Grok upstream returned <status>'>"}}` | request-worker.js:2283-2297 |
| request error / 1 h idle | `502` `model-gateway: Grok upstream failed: <msg>` (`api_error`), or destroy when headers were sent | request-worker.js:2350-2357 |
| stream break | destroy client | request-worker.js:2323-2327 |

The Grok branch has no 413 normalization and no context-overflow detection; an overflow reaches the client as the generic error above.

---

## 9. `tool-schema-compat.js` and `context-window.js`

### 9.1 `adaptCodexToolSchemas(tools)` (tool-schema-compat.js:356-380)

Called only for provider `codex`, after deferred-tool hoisting (request-worker.js:2517). Rewrites Unicode property escapes
(`\p{..}`/`\P{..}`) that Codex rejects; everything else in a schema is left as is.

```
if tools is not an array: return tools
for i, tool: if tool is a plain object with own "input_schema":
    cls[i] = classifySchema(tool.input_schema, "/tools/<i>/input_schema")
        on ToolSchemaCompatibilityError: set toolName (tool.name or "<unnamed tool>"), rethrow; any other error rethrows unchanged
if no cls[i].changed: return the SAME array (identity)
return tools.map: unchanged -> same object; changed -> {...tool, input_schema: structuredClone(schema) with each replacement set at its
    pointer (pointer minus the "/tools/<i>" prefix, then minus its first segment)}

classifySchema(schema, root):
    p = findAffectedPattern(schema, root)   # DFS over all objects and arrays, cycle-safe; skips the values of const, default, enum,
                                            # examples, description; hit = an object whose "pattern" string or any patternProperties key
                                            # contains a \p / \P escape (well-formed or malformed)
    if p is null: return {changed:false}    # fast path: the strict walk below never runs for schemas without \p
    if schema is not a plain object: error(root, "invalid-schema")
    visitSchema(schema, root); return {changed: replacements non-empty, replacements}
```

`visitSchema(schema, ptr)` (tool-schema-compat.js:273-311); `true`/`false` accepted; a non-object is `invalid-schema-container`. Per
keyword, first matching rule:

| keyword | rule / reason code |
|---|---|
| `$schema` | must be `https://json-schema.org/draft/2020-12/schema` or the same with `#`, else `unsupported-dialect` |
| `$vocabulary` | `custom-vocabulary` |
| `$id`, `$ref`, `$dynamicRef`, `$recursiveRef`, `$anchor`, `$dynamicAnchor`, `$recursiveAnchor` | `reference-or-anchor-keyword` |
| `$defs`, `definitions`, `not`, `if`, `then`, `else`, `oneOf`, `contains`, `contentSchema` | `forbidden-schema-applicator` |
| `pattern` | non-string or no `\p` -> skip; else `validateRegexGrammar`, record replacement if any |
| `patternProperties` | non-object: `invalid-schema-container` if it contains an affected pattern; a key with `\p` -> `affected-pattern-properties-key`; recurse into values |
| `properties`, `dependentSchemas` | map of schemas (non-object: as above) |
| `additionalProperties`, `items`, `propertyNames`, `unevaluatedProperties`, `unevaluatedItems` | single schema |
| `prefixItems`, `allOf`, `anyOf` | array of schemas (non-array: `invalid-schema-container` if affected inside) |
| `const`, `default`, `enum`, `examples`, `description`, and the known scalar keywords (`$comment`, `title`, `format`, `type`, `required`, `minimum`, ... tool-schema-compat.js:24-30) | skip |
| anything else | `unknown-schema-container` if an affected pattern is inside it, else skip |

`validateRegexGrammar(pattern, ptr)` (tool-schema-compat.js:75-247), a scanner over UTF-16 code units:

| construct | rule / reason code |
|---|---|
| `\` at end | `invalid-regex` (malformed) |
| `\p{..}`/`\P{..}` outside a class | `property-outside-negated-character-class`; malformed (`\p` without `{`, empty or unclosed braces) `malformed-unicode-property` |
| `\1`-`\9`, `\k` | `backreference-or-unsupported-escape` |
| `\0`, `\xHH`, `\uHHHH`, `\u{1-6 hex}`, `\cA-Z`, `\d\D\s\S\w\W\b\B\f\n\r\t\v`, escaped `^$\.*+?()[]{}\|/-` | accepted; malformed hex/unicode/control -> `invalid-regex` |
| any other escape | `unsupported-regex-escape` |
| group `(?:`, `(?=`, `(?<=` (positive) or `(?!`, `(?<!` (negative) | accepted; context pushed, popped on `)` |
| any other `(` (capturing, named `(?<name>`, flags) | `capture-or-unsupported-group` |
| class `[...]` without property escapes | accepted unchanged; unclosed -> `invalid-regex` |
| class with property escapes, not negated | `positive-character-class` |
| negated class with property escapes and `-`, `&&` or `--` | `range-or-set-character-class` |
| negated class with property escapes inside an open negative lookaround | `negative-regex-context` |
| negated class whose content is only property escapes | `empty-negated-character-class` |
| after the scan | `new RegExp(pattern, 'u')` must compile, else `invalid-regex` |

Replacement: inside each accepted negated class, every `\p{..}`/`\P{..}` atom is deleted; all other bytes stay. Example: `[^\p{Cc}\t]`
-> `[^\t]`. This widens what the pattern accepts (the excluded property is no longer excluded); README.md:74 calls it a provider hint
only. Error object: `ToolSchemaCompatibilityError{toolName, pointer (RFC 6901, ~0/~1 escaped), reasonCode}`, message
`Codex schema compatibility refused <tool> at <pointer> (<code>)` (tool-schema-compat.js:32-51). The worker maps any thrown error to the
400 in section 4.3, using `schema-classifier-failure` and `/tools` for non-compat errors (request-worker.js:2519-2522).

Go port risk: the final validity oracle is the ECMAScript regex engine with the `u` flag. Go's `regexp` (RE2) differs in syntax
(lookarounds, `\u{..}`, `\cX`, property names), so it cannot stand in; a port needs an ECMAScript-compatible validator or must accept a
behaviour change. Indexing is by UTF-16 code unit in JS; every token compared is ASCII, so a byte scan in Go gives the same tokens.

### 9.2 `context-window.js`: CLI-side, not on the request path

`request-worker.js` does not import it (imports at request-worker.js:14-43). It parses and persists the `context-window` command's
settings; the worker consumes the result only through `runtime.js` reading `context-window.json` once at load (runtime.js:244, 307-319).

| function | behaviour | cite |
|---|---|---|
| `contextWindowUpdate(option, raw)` | `--claude|--codex|--grok <tokens|full>` -> `{backend, field:'window', value}`; `--codex-compact-at|--grok-compact-at <tokens|cap>` -> `{backend, field:'compactAt', value|null}`; errors verbatim at context-window.js:13, 16, 22, 24, 27 | context-window.js:11-30 |
| ranges | window: `full` or integer `CONTEXT_WINDOW_MIN[backend]`..1000000 (claude 100000, codex/grok 185000); compact-at: positive safe integer | runtime.js:260-270, 285-289 |
| `savedContextWindows`, `saveContextWindowUpdate` | build/patch the saved object `{claude?, codex?, grok?, compactAt?:{codex?, grok?}}`; `cap` deletes a compact-at | context-window.js:32-52 |
| `contextWindowReport` / `gatewayWindowNote` / `gatewayWindowLine` / `claudeWindowLine` | human-readable lines; Codex adds the 272k double-billing note | context-window.js:54-97; runtime.js:367-372 |
| `syncClaudeContextWindow(files, {owned, next})` | per settings file: set or delete `autoCompactWindow` only when the current value is absent, equal to the previously gateway-owned value, or already `next`; returns `{changed:[], skipped:[]}` | context-window.js:99-129 |

### 9.3 `shim-state.js` and `codex-upstream-state.js`

`shim-state.js` is CLI-side: `probeShimState` combines the owner of `PUBLIC_SHIM_PORT` with `GET 127.0.0.1:<SHIM_PORT>/healthz` into
`running-ours | running-foreign | starting | stopped` (shim-state.js:28-42); `shimNeedsRestart` is true unless `health.proxyRecovery ===
true` and the serving semver is not older than installed (shim-state.js:67-72). `codex-upstream-state.js` is the state-file store used in
section 4.4: `codex-upstream-blocked.json` `{state:"upstream-blocked", observedAt, statusCode, evidence, expiresAt?, expiresAtMs?}` and
`codex-upstream-unavailable.json` `{state:"upstream-unavailable", observedAt, observedAtMs, expiresAt, statusCode}` with a 30 s TTL
(codex-upstream-state.js:8-12, 53-84).

---

## 10. Networking details

### 10.1 Listeners (request-worker.js:2680-2726)

| listener | address | notes | cite |
|---|---|---|---|
| main | `127.0.0.1:SHIM_PORT`; `SHIM_PORT = CODEX_GATEWAY_WORKER_PORT || CODEX_GATEWAY_PORT || 18764` | the supervisor forks the worker with `CODEX_GATEWAY_WORKER_PORT=<configured or "0">`, so a supervised worker listens on an ephemeral port and reports it over IPC `{type:"listening", port}`; the supervisor waits 5 s for it | request-worker.js:2688-2694; runtime.js:24-25; commands.js:2487-2488, 2612-2638 |
| Unix socket / named pipe | `CODEX_GATEWAY_SOCKET_PATH` or `~/.claude/model-gateway/gateway.sock` (Windows `\\.\pipe\model-gateway`) | always bound by the worker (the supervisor does not bind it). On `EADDRINUSE`: probe-connect; only `ECONNREFUSED` plus `lstat().isSocket()` unlinks and re-listens; bind failure is logged and the listener dropped | request-worker.js:1180-1206, 2696-2701; runtime.js:47-49 |
| RC compatibility | `<hosts IP>:COMPAT_PORT` (default 80, `CODEX_GATEWAY_COMPAT_PORT`) | only when the hosts file maps `api.anthropic.com` to `127.0.0.1` or `::1` (`detectHostsCompat`, remote-control.js:8, 23-34, 295-299) and `CODEX_GATEWAY_WORKER_PORT` is unset; failures recorded in `compatState.reason` | request-worker.js:2710-2726 |

All three use the same handler and timeouts (`requestTimeout 0`, `headersTimeout 120000`, `keepAliveTimeout 75000`). No IPv6 or
wildcard listener; no TLS listener.

### 10.2 Hosts-bypass resolver (`createHostsBypassResolver`, request-worker.js:881-931)

- `resolve(hostname)`: one cache slot (not keyed by hostname), TTL 5 min. `dns.promises.resolve4` first (first address, family 4), then
  `resolve6`; success refreshes the slot; both failing returns the stale slot if any, else `null`.
- `lookup(hostname, options, cb)` (Node `lookup` signature): `options.all === true` -> `cb(null, [{address, family}])`; else
  `cb(null, address, family)`; unresolved -> `cb(Error("model-gateway: could not resolve <host> via DNS to bypass the hosts compatibility entry"))`.
  It never falls back to the OS resolver, which would read the hosts file and loop back into the shim.
- Used for `forward()` only when `compatState.hostsDetected && hostname === "api.anthropic.com"` (request-worker.js:1837-1839); used for
  every Anthropic catalog fetch (request-worker.js:1232-1233). Not used for the proxy or Grok.
- Go: a custom `net.Dialer` `DialContext` that resolves A then AAAA with a resolver that skips `/etc/hosts` (e.g. `miekg/dns` against the
  system nameservers), then dials the IP, while TLS keeps `ServerName = api.anthropic.com`.

### 10.3 Agents, TLS, proxy env

| item | behaviour | cite |
|---|---|---|
| upstream agents | one `http.Agent` and one `https.Agent`, `keepAlive: true`, `maxSockets: 64` per host, shared by Anthropic, proxy and Grok traffic | request-worker.js:1540-1541, 1833, 2263 |
| other clients | `fetchUrl` (proxy `/v1/models`, follows redirects) uses Node's global agent; route spans `agent:false` with `socket.unref()`; catalog uses `https.get` with the global agent and the bypass `lookup`; Grok token refresh uses `requestJson` with the global agent | process-supervision.js:25-42; request-worker.js:997-1005; anthropic-catalog.js:36; grok-backend.js:393-408 |
| TLS | Node defaults; no custom CA, client certificate or `rejectUnauthorized` override in the slice | - |
| `HTTP(S)_PROXY` / `NO_PROXY` | not read anywhere in the slice. Go's `http.DefaultTransport` uses `ProxyFromEnvironment`; set `Proxy: nil` to match, or decide deliberately | - |

### 10.4 Drain and shutdown (request-worker.js:1230, 1235-1260, 2364-2374)

`POST /drain` sets `draining`; after the 202 is flushed, `beginDrain` closes every server and calls `closeIdleConnections()`; the process
exits when all servers report closed (or at once with none). When `activeRequests` reaches 0 while draining, `closeAllConnections()`.
During drain every non-`/healthz` request gets the 503 in section 1. The worker has no drain timeout of its own (the supervisor's
`CODEX_GATEWAY_DRAIN_TIMEOUT_MS`, default 30000, is at commands.js:2490; its use is UNREAD). `process.once('disconnect')` (IPC parent
gone) -> `process.exit(0)` (request-worker.js:1210). No signal handlers in the worker.

---

## 11. Logs and telemetry

### 11.1 Request-route JSONL (request-worker.js:1267-1295)

File `~/.claude/model-gateway/logs/request-routes.jsonl` (`CODEX_GATEWAY_REQUEST_LOG_PATH`); on unless `CODEX_GATEWAY_REQUEST_LOG=0`
(runtime.js:35-36). One `fs.appendFileSync` per request (mode 0600 on creation) after a synchronous `mkdirs()` of `STATE`, `LOGS`,
`BIN_DIR` (runtime.js:775-777). Write errors -> stderr `model-gateway: could not write request route log: <code>`.

| field | presence | value |
|---|---|---|
| `at` | always | ISO-8601 |
| `backend` | always | provider (`codex`, `grok`, `muse`, `lmstudio`, ...) or `anthropic` |
| `model` | always | effective model string or `null` (Anthropic: client id) |
| `path` | always | `pathOnly` |
| `via` | if set | `dispatch`, `dispatch-cached`, `dispatch-inherited`, `dispatch-unbound` (proxy dispatch), `direct` (Grok only); absent for proxy direct and Anthropic |
| `effort` | if set | effective effort (proxy) / requested effort (Grok); Anthropic passes none |
| `sessionId` | if known | dispatch identity session, else raw `x-claude-code-session-id` header (unvalidated) |
| `agentId`, `parentAgentId`, `sessionSource` (`header`/`metadata`), `markersLength`, `inheritedFromAgentId` | dispatch requests only | - |

Written for: proxy requests (before the sentry check, so a sentry-refused request is logged), unbound dispatch, Grok, Anthropic
(request-worker.js:2504, 2553, 2621, 2651). Not written for thread refusals or schema refusals.

### 11.2 Route spans (OTLP/HTTP JSON traces) (request-worker.js:933-1172)

- Enabled only when `CLAUDE_CODE_PROPAGATE_TRACEPARENT=1` (runtime.js:40). Endpoint: `CODEX_GATEWAY_TELEMETRY_ENDPOINT` (`0` disables),
  else `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, else `OTEL_EXPORTER_OTLP_ENDPOINT` + `/v1/traces`, else `http://127.0.0.1:4318/v1/traces`;
  must be loopback (`localhost` rewritten to `127.0.0.1`, `::1`, `127.x.x.x`) and http/https; query and fragment removed
  (request-worker.js:933-957).
- Incoming `traceparent` `^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(-[0-9a-f-]+)?$`; version `ff`, version `00` with a
  suffix, or all-zero ids rejected (request-worker.js:959-967). Trace id inherited or random 16 bytes; span id random 8 bytes; route id UUID.
  The trace headers are still stripped before forwarding, so nothing propagates upstream.
- One span per request at `finish`: `name:"codex_gateway.route"`, `kind:2`, start/end unix nanos, `parentSpanId`/`flags` when linked,
  `status.code` 1 when `ok` else 2, one event duplicating the attributes. Envelope `resourceSpans[0].resource.attributes = service.name:
  model-gateway`, scope `eigenwise.codex-gateway` (request-worker.js:984-1010, 1064-1078).
- Attributes (request-worker.js:1033-1062): `source=codex-gateway`, `source_event_id`, `source_schema="1"`, `event_name`, `route_id`,
  `trace_id`, `span_id`, `parent_span_id`, `trace_linked`, `session_id` (validated header), `selected_model`, `effective_model`, `backend`
  (`/^[a-z][a-z0-9-]{0,31}$/`), `effort`, `fallback`, `via`, `status` (`ok|client_error|server_error|upstream_error|<override>`),
  `status_code`, `compaction_outcome`, `upstream_status_code`, `compaction_terminal_code`, `compaction_error_code`,
  `compaction_{input,output,cache_read,cache_creation}_tokens`, `overflow_<field>` for each overflow detail, `duration_ms`. OTLP values:
  bool, `intValue` as string for integers, `doubleValue`, `stringValue`; null values dropped.
- Overrides used: `thread_unsupported`, `invalid_route`, `invalid_tool_schema`, `authentication_error`, `upstream_error`, `upstream_aborted`.
- POST: `agent:false`, socket unref'd, 500 ms timeout, all errors ignored.

### 11.3 Usage emitter (other lane; call sites only)

`createGatewayUsageEmitter()` is created once (request-worker.js:1217). It is **on by default**: endpoint
`CODEX_GATEWAY_USAGE_ENDPOINT` (`0` disables), else `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, else `OTEL_EXPORTER_OTLP_ENDPOINT` + `/v1/logs`,
else `http://127.0.0.1:4318/v1/logs`, loopback only (usage-observability.js:10, 597-631, 976-1045). Capture is started for proxy
`/v1/messages` when the emitter or route spans are enabled, for Grok when the emitter is enabled (any `/v1/messages*`), for Anthropic
`/v1/messages` with a parsed payload (request-worker.js:2571-2585, 2636-2643, 2662-2675). Methods used: `setResponse`,
`noteResponseBytes`, `observeEvent`, `observeJson`, `observeChunk`, `markOverflow`, `snapshot`, `resetUsage`, `setResponseComplete`,
`finish`. `recordRequestBodyHighWater(sessionId, bytes)` writes `~/.claude/model-gateway/request-body/<base64url(session)>.json`
`{"value":bytes,"observed_at":ISO}` (non-atomic `writeFileSync`, 0600) when a session's body size exceeds its previous maximum
(usage-observability.js:177-208; request-worker.js:2570, 2635, 2661).

### 11.4 Files this slice writes

| file | writer | when |
|---|---|---|
| `~/.claude/model-gateway/control-token` | `ensureControlToken` (`wx`, 0600, 32 random bytes hex) | worker start, if absent (request-worker.js:1209; control-auth.js:23-33) |
| `~/.claude/model-gateway/gateway.sock` | listener; stale one unlinked | start |
| `logs/request-routes.jsonl` | 11.1 | per routed request |
| `dispatch-routes.json` | 7.2 | dispatch set/get/expiry |
| `anthropic-models.json` | 6.2 | catalog refresh |
| `$CLAUDE_CONFIG_DIR/cache/gateway-models.json` | 6.3 | proxy-sourced refresh, Anthropic refresh |
| `codex-upstream-blocked.json`, `codex-upstream-unavailable.json` | 4.4 (create and delete) | Codex responses |
| `request-body/<b64>.json` | 11.3 | larger request body per session |
| `~/.grok/auth.json` | Grok refresh (atomic, 0600) | token expiring |
| stdout/stderr | `console.log`/`console.error`, redirected to `logs/shim.log` by the supervisor (commands.js:2618-2620) | throughout |

All atomic writes use `writeFileAtomically`: temp `<target>.<pid>.<12 hex>.tmp`, `rename` with retries on `EPERM/EACCES/EBUSY` after
20/60/140/300 ms (blocking `Atomics.wait`), temp removed in `finally`, then sibling `.tmp` files older than 1 h are swept
(atomic-file.js:7-59).

---

## 12. In-memory state and concurrency

| structure | scope | lifetime / bound / eviction | cite |
|---|---|---|---|
| `routeContexts: WeakMap<req, {requestChunks, routeTelemetry}>` | module | GC with the request | request-worker.js:1115, 1170 |
| `modelCache` | worker | replaced on each refresh | request-worker.js:1211, 1507 |
| `counters` | worker | monotonic, unbounded keys by provider | request-worker.js:1215 |
| `dispatchRoutes` | worker | 500 keys, 4 h sliding | 7.2 |
| `sentrySessions` | worker | **unbounded, never evicted** | request-worker.js:1222 |
| `sentryModels` | worker | one per model key; reset on window change | request-worker.js:1223, 1311 |
| `compatState`, `servers`, `draining`, `activeRequests` | worker | process | request-worker.js:1227-1231 |
| shared readiness check `{latest, inFlight}` | worker | 15 s freshness, single flight | request-worker.js:102-114 |
| bypass resolver cache | worker | one slot, 5 min | request-worker.js:893 |
| catalog `inFlight` | worker | single flight | anthropic-catalog.js:82 |
| listing retry timer/delay | worker | 1 s..30 s | request-worker.js:1465-1466 |
| `proxyRoutes`, `routeIdsByAdvertisedPart`, `localServer`, `lmStudioServer` | module (proxy-models.js) | replaced each refresh | proxy-models.js:65, 86, 125, 138 |
| `policyIdsByAdvertisedPart` | module (runtime.js) | built once lazily | runtime.js:382-389 |
| `CONTEXT_WINDOWS` | module (runtime.js) | read once at load | runtime.js:319 |
| `requestBodyHighWater` | module (usage-observability.js) | unbounded per session | usage-observability.js:25 |

Timers: refresh interval 60 s (unref), listing retry (unref), per-stream heartbeat 20 s, LM Studio peek 10 s, WS retry 250 ms, compaction
retry 250 ms, upstream idle 1 h, telemetry 500 ms, catalog 5 s per page. All mutation happens on Node's single event loop; a Go port needs
locks or a single-owner goroutine for the sentry maps, the dispatch cache (including its write-on-read persistence), the catalog snapshot
(an atomic pointer), the proxy route table, and single-flight groups for the readiness check, catalog refresh and Grok token refresh.

---

## 13. Node-specific constructs and their Go counterparts

| Node construct | where | Go port must reproduce |
|---|---|---|
| `StringDecoder('utf8')` before frame splitting | request-worker.js:508, 1669, 1725, 2301 | buffer partial UTF-8 sequences across chunks (or split frames on bytes, since `\r\n\r\n` is ASCII) |
| `upRes.unshift()` after a peek | request-worker.js:507-546 | `bufio.Reader` peek or `io.MultiReader(bytes.NewReader(peeked), body)` |
| `upRes.pipe(clientRes)` | request-worker.js:2219 | `io.Copy` with `http.Flusher.Flush()` after each write for SSE (Go's `ResponseWriter` buffers) |
| `res.write` ignoring back-pressure | request-worker.js:2042-2053 | blocking writes plus flush (back-pressure comes for free) |
| zlib side-branch decode for observation | request-worker.js:1765-1794 | tee the body into `gzip`/`flate`/`brotli` readers; forward raw bytes. Go's Transport auto-decompresses gzip unless the request sets `Accept-Encoding` itself; keep the client's header so bytes stay untouched |
| hop-by-hop header removal | request-worker.js:1828, 1842, 2266 | `httputil.ReverseProxy` removes a wider set; replicate this exact list instead |
| `http.Agent {keepAlive, maxSockets:64}` | request-worker.js:1540-1541 | `http.Transport{MaxConnsPerHost:64, IdleConnTimeout:...}`; `Proxy: nil` |
| custom `lookup` | request-worker.js:890-931, 1838 | custom `DialContext` (10.2) |
| `server.requestTimeout/headersTimeout/keepAliveTimeout` | request-worker.js:2682-2684 | `ReadHeaderTimeout 120s`, `IdleTimeout 75s`, no `ReadTimeout`/`WriteTimeout` |
| `req.setTimeout(3600000)` idle | request-worker.js:2221, 2350 | per-read idle deadline on the upstream connection, not a total timeout |
| client abort | request-worker.js:2421, 2427 | in Go the upstream request inherits `r.Context()` and is cancelled on disconnect by default; that differs from the Node code (section 14) |
| `server.close`, `closeIdleConnections`, `closeAllConnections` | request-worker.js:1235-1260 | `http.Server.Shutdown(ctx)`, then `Close()` |
| Unix socket bind with stale-socket removal; Windows named pipe | request-worker.js:1183-1206 | `net.Listen("unix")` plus the same probe; named pipe needs `github.com/Microsoft/go-winio` |
| `process.send({type:'listening'})`, `process.once('disconnect')` | request-worker.js:2692, 1210 | the supervisor/worker split is a Node fork-with-IPC pattern; in Go one process can own the listener, or use an inherited pipe/fd |
| `execFile` with timeout | request-worker.js:51 | `exec.CommandContext` with a 15 s deadline |
| `timer.unref()`, `socket.unref()` | request-worker.js:1473, 1538, 1005 | not needed in Go (no event-loop liveness) |
| `JSON.parse`/`JSON.stringify` key order | request-worker.js:2569, 1647, 1701 | order-preserving JSON (section 3.3) |
| `structuredClone` | tool-schema-compat.js:351, 374 | deep copy of the decoded schema |
| `new RegExp(p, 'u')` as validator | tool-schema-compat.js:232-236 | an ECMAScript regex validator (section 9.1) |
| `crypto.timingSafeEqual`, `randomUUID`, `randomBytes` | control-auth.js:53; request-worker.js:582, 1084-1087 | `subtle.ConstantTimeCompare`, `crypto/rand` |
| `Atomics.wait` sleep | atomic-file.js:24 | `time.Sleep` |
| `AbortController` / `AbortSignal` | not used anywhere in the six slice files (grep finds no hit); cancellation is only `destroy()` | decide context-cancellation semantics explicitly (section 14 item 2) |
| synchronous fs on the request path (`appendFileSync`, `rmSync`, `readFileSync` of catalog/state files) | request-worker.js:1291; codex-upstream-state.js; anthropic-catalog.js:28 | plain file I/O in the handler goroutine is fine in Go; keep the write ordering |

---

## 14. Open questions, dead code, surprises

| # | item | evidence |
|---|---|---|
| 1 | `count_tokens` with a `claude-grok-*` model is sent to the Grok Responses API as a full generation and can be sentry-refused; probably unintended | request-worker.js:2596-2644 (no exact-path guard), 2256-2263 |
| 2 | No upstream cancellation when the client disconnects; an abandoned Codex/Anthropic stream runs until it ends or idles 1 h. A Go port inherits cancellation by default; decide which behaviour to keep | request-worker.js:2421, 2427, 2221 |
| 3 | `sentrySessions` grows without bound for the worker's lifetime (one entry per session x agent x model) | request-worker.js:1222, 1318-1332 |
| 4 | `dispatch-inherited` is not in `ROUTE_VIAS`, so route spans export `via=null` for it | request-worker.js:1021, 1051, 2483 |
| 5 | `assertAnthropicPassthroughSentryIsDisabled` throws inside the `req end` callback; unreachable with today's table, but a throw there is uncaught and would end the worker | request-worker.js:260-265, 2648 |
| 6 | Proxy 2xx bodies are parsed without decompression while the client's `accept-encoding` is forwarded; whether claude-code-proxy ever compresses is UNVERIFIED | request-worker.js:1827-1828, 2054-2058 |
| 7 | The worker's `/healthz` has no `proxyRecovery` or `supervisorVersion`, which `shim-state.js` and `servingShimVersion` expect; the supervisor's own `/healthz` (UNREAD) presumably adds them | request-worker.js:2384-2404; shim-state.js:14-16, 67-72 |
| 8 | Default Codex compaction trigger is 187000 (272000 cap - 85000), far below the 920012 window; intended per the billing note, worth confirming for the port | runtime.js:247-264, 331-334 |
| 9 | `context-window.json` is read once per process; the worker needs a restart to apply a new cap. Whether the CLI restarts it is UNREAD | runtime.js:319 |
| 10 | Each `forward()` retry registers another `clientRes.once('finish')` usage-finish listener; whether `usageCapture.finish()` is idempotent is UNREAD | request-worker.js:1824-1825, 1912-1914, 2094-2096 |
| 11 | Anthropic catalog: after the 1 h TTL a failed refresh leaves `updatedAt` unchanged, so every following `/v1/models` or Anthropic request starts a new fetch (single-flight only) | anthropic-catalog.js:90-101; request-worker.js:2650 |
| 12 | Grok token refresh has no timeout and no single-flight; concurrent refreshes race on `auth.json` (shared with the Grok CLI) | grok-backend.js:83-119, 393-408 |
| 13 | Grok stream after an `error`/`response.failed` event still ends with a synthesized `end_turn` + `message_stop`; `response.incomplete` is treated as a normal end | grok-backend.js:384-389 |
| 14 | The Grok branch never forwards client headers (no `anthropic-beta`, no session headers) and has no 413 normalization | request-worker.js:2262, 2283-2297 |
| 15 | Request bodies are buffered twice with no size limit | request-worker.js:1128, 2429 |
| 16 | `HEAD` on any path answers 200 locally, including `/v1/messages` | request-worker.js:2416 |
| 17 | The dispatch marker's model is forwarded to the proxy as provider `codex` without checking the catalog or provider | request-worker.js:2511, 797-802 |
| 18 | Grok model ids present in `~/.grok/models_cache.json` but absent from `MODEL_WINDOW_POLICY` are routable through `claude-grok-<id>` but never advertised | request-worker.js:233-235, 1514-1516 |
| 19 | `/drain` is unreachable over the Unix socket (no TCP `localPort`); reading of Node's API, not verified | control-auth.js:49; request-worker.js:2365 |
| 20 | README.md:74 "Anthropic requests remain byte-identical" holds for the body only; trace and hop-by-hop headers are stripped on passthrough | request-worker.js:1828 |
| 21 | Behaviour of a client `Upgrade:` request with no `upgrade` listener was not determined from the code | grep result in section 1 |
| 22 | `fireContextSentry` re-parses the full original body on every proxy and Grok request (a third parse), only to test the compaction exemption | request-worker.js:1362 |
| 23 | Dead code: the sentry observer inside the R8 passthrough branch (and the R8 heartbeat for proxy) can only run for a proxy 2xx SSE, which always takes R7; R8 sees proxy responses only for 1xx/3xx | request-worker.js:2023, 2176-2185 |

---

Inventory written 2026-11-25 21:42:54 JST from source commit 4ffb2f672 (read-only; no plugin code executed). Citations were moved to commit a8501f251755 on 2026-11-25 (`07-repin.md`).
