# Package map: what is ported from where

This page lists each part of the Node gateway and the Go package that replaces it, with the phase that delivers
it. The mapping is the one in section 3.2 of `model-gateway-go-port.md`; that plan and its phase table remain the
authority. The status column is a snapshot taken on 2026-11-27 and is updated at each phase gate.

Source paths are relative to the pinned source tree. Go packages are relative to `internal/`.

## Phases

| Phase | Content | Status on 2026-11-27 |
|---|---|---|
| 0 | Foundations: test isolation, process identity, test inventory | Landed on `main` |
| 1 | Pure logic packages | Landed on `main` |
| 2 | Walking skeleton, launch adapter, worker | In progress |
| 3 | Supervisor and process layer | Not started |
| 4 | CLI and settings | Not started |
| 5 | Packaging, migration, shadow mode | Not started |
| 6 | Rehearsal, shadow soak, cutover | Not started |

## Source to Go package

"Done" means the production code is on `main`. "Partly" means the pure logic is on `main` and the rest belongs to
the later phase named in the row. "In review" means the code exists on a unit branch and has not passed its
independent review yet.

| Source (Node) | Go package | Phase | Status on 2026-11-27 |
|---|---|---|---|
| `lib/runtime.js` (paths, environment, policy, install) | `paths`, `config`, `policy`, `install` | 0, 1 | Done |
| `lib/context-window.js` | `policy` | 1 | Done |
| `lib/proxy-models.js` | `proxymodels` | 1 | Done |
| `lib/lmstudio.js`, `lib/local-llama.js`, `lib/proxy-config.js` | `localprov`, `proxyconfig` | 1, 2 | Partly: parsers done; live probes in review with the model cache |
| `lib/pins.js`, `lib/anthropic-catalog.js`, `lib/previous-picker-rows.js` | `pins`, `anthropiccatalog` | 1, 4 | Partly: logic done; the probe is phase 4 |
| `lib/tool-schema-compat.js` | `toolschema` | 1 | Done |
| `lib/codex-upstream-state.js`, `lib/control-auth.js` | `upstreamstate`, `controlauth` | 1 | Done |
| `lib/lifecycle-lock.js`, `lib/lifecycle-diagnostics.js` | `lifecycle` | 1 | Done |
| `lib/atomic-file.js` | `atomicfile` | 0 | Done |
| `lib/settings-wiring.js`, `lib/readiness-hooks.js`, `lib/recovery-policy.js`, `lib/readiness-gate.js` | `wiring`, `readiness` | 1, 4 | Partly: logic done; the rest is phase 4 |
| `lib/grok-backend.js` | `grok` | 1, 2 | Partly: translation done; the request branch is in review |
| `lib/usage-observability.js` | `usage` | 1, 2 | Partly: records done; capture and emitter in review |
| `lib/request-worker.js`: routing, context sentry, dispatch | `worker/route`, `worker/sentry`, `worker/dispatch`, `sse` (frame scanner) | 1 | Done |
| `lib/request-worker.js`: server; `lib/commands.js`: `serve-shim`, `serve-worker` | `worker`, `cli/serveshim`, `cli/serveworker`, `supervisor` (skeleton) | 2 | In review |
| `lib/request-worker.js`: upstream requests | `worker/upstream` | 2 | Reviewed; a follow-up on TLS error text is in review |
| `lib/request-worker.js`: buffered replies | `worker/reply` | 2 | In review |
| `lib/request-worker.js`: stream relays | `worker/sse` | 2 | In review |
| `lib/request-worker.js`: compaction guard | `worker/compactguard` | 2 | In review |
| `lib/request-worker.js`: proxy request assembly | `worker/proxyreq` | 2 | Reviewed; follow-up tests pending |
| `lib/request-worker.js`: model cache and `/v1/models` | `worker/modelcache` | 2 | In review |
| `lib/request-worker.js`: route telemetry and the request-route log | `telemetry` | 2 | In review |
| `lib/request-worker.js`: route and terminal response facts | `worker/outcome` | 2 | In review |
| Worker assembly (new) | `worker/compose` | 2 | Not started: follows the unit reviews |
| `lib/commands.js`: supervisor | `supervisor`, `supervisor/relay`, `supervisor/workerproc` | 3 | Not started |
| `lib/process-supervision.js` | `procinfo`, `ownership`, `supervisor/proxyrecovery`, `launch` | 0, 3 | Partly: `procinfo` done |
| `lib/shim-state.js` | `shimstate` | 3 | Not started |
| `lib/windows-detached.js` | `launch` (Windows stub) | 3 | Not started |
| `lib/picker-guard.js` | `pickerguard` | 4 | Not started |
| `lib/github-release.js`, `lib/release-verification.js` | `release` | 4 | Not started |
| rest of `lib/commands.js` (one command each) | `cli`, `cli/<command>`, `catalog`, `doctor`, `status` | 0, 3, 4 | Partly: registration, argument scan and `catalog` done |
| `hooks/registry-writer.js` | `selfinstall` | 4, 5 | Not started |
| `lib/remote-control.js` | Not ported. Only a read-only check for the hosts block that the Node version manages remains, in `doctor` and in the migration check | 4, 5 | Not started |
| New | `jsonx`, `venue`, `gatewaytest` | 0 | Done |
| New | `migrate`, `shadow` | 5 | Not started |
| New, not Go | `plugin/`, `.claude-plugin/marketplace.json`, `test/conformance/` | 2, 5 | Partly: conformance tests and the hosted evidence export exist; the plugin is phase 5 |
