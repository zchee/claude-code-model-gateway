# Source test ownership

Each due case in [classification.tsv](../../test/conformance/classification.tsv) joins to exactly one implementing task by `(phase, package)`. The task owns the full case at that phase gate, including integration with earlier dependencies. Exempt cases have no owner and are not counted. Task scopes are defined in the [port plan](model-gateway-go-port.md#4-implementation-steps).

| Phase | Package | Implementing task | Due cases |
|---|---|---|---:|
| phase-0 | `internal/atomicfile` | T0.3 | 3 |
| phase-0 | `internal/gatewaytest` | T0.4b | 10 |
| phase-0 | `internal/procinfo` | T0.5 | 9 |
| phase-1 | `internal/anthropiccatalog` | T1.4 | 1 |
| phase-1 | `internal/catalog` | T1.12c | 4 |
| phase-1 | `internal/controlauth` | T1.12a | 2 |
| phase-1 | `internal/grok` | T1.9 | 8 |
| phase-1 | `internal/install` | T1.3 | 10 |
| phase-1 | `internal/lifecycle` | T1.11 | 9 |
| phase-1 | `internal/pins` | T1.4 | 2 |
| phase-1 | `internal/policy` | T1.1 | 12 |
| phase-1 | `internal/proxyconfig` | T1.3 | 2 |
| phase-1 | `internal/proxymodels` | T1.2 | 10 |
| phase-1 | `internal/rccompat` | T1.12b | 7 |
| phase-1 | `internal/toolschema` | T1.5b | 10 |
| phase-1 | `internal/upstreamstate` | T1.12a | 3 |
| phase-1 | `internal/usage` | T1.12d | 13 |
| phase-1 | `internal/wiring` | T1.10 | 24 |
| phase-1 | `internal/worker/dispatch` | T1.8 | 3 |
| phase-1 | `internal/worker/route` | T1.7 | 1 |
| phase-1 | `internal/worker/sentry` | T1.6 | 1 |
| phase-2 | `internal/anthropiccatalog` | T2.1 | 8 |
| phase-2 | `internal/catalog` | T2.1 | 2 |
| phase-2 | `internal/grok` | T2.7 | 10 |
| phase-2 | `internal/localprov` | T2.1 | 15 |
| phase-2 | `internal/proxymodels` | T2.1 | 6 |
| phase-2 | `internal/readiness` | T2.1 | 6 |
| phase-2 | `internal/supervisor` | T2.0 | 5 |
| phase-2 | `internal/telemetry` | T2.8 | 11 |
| phase-2 | `internal/usage` | T2.8 | 13 |
| phase-2 | `internal/worker` | T2.0 | 6 |
| phase-2 | `internal/worker/compactguard` | T2.6 | 19 |
| phase-2 | `internal/worker/dispatch` | T2.5 | 9 |
| phase-2 | `internal/worker/reply` | T2.3 | 24 |
| phase-2 | `internal/worker/route` | T2.5 | 17 |
| phase-2 | `internal/worker/sentry` | T2.5 | 36 |
| phase-2 | `internal/worker/sse` | T2.4 | 21 |
| phase-2 | `internal/worker/upstream` | T2.2 | 6 |
| phase-3 | `internal/launch` | T3.6 | 10 |
| phase-3 | `internal/ownership` | T3.1 | 21 |
| phase-3 | `internal/shimstate` | T3.6 | 7 |
| phase-3 | `internal/supervisor` | T3.3 | 1 |
| phase-3 | `internal/supervisor/proxyrecovery` | T3.2 | 15 |
| phase-3 | `internal/supervisor/rclistener` | T3.5 | 5 |
| phase-3 | `internal/supervisor/relay` | T3.3 | 31 |
| phase-3 | `internal/supervisor/workerproc` | T3.4 | 77 |
| phase-4 | `internal/catalog` | T4.4 | 19 |
| phase-4 | `internal/cli/contextwindow` | T4.3 | 2 |
| phase-4 | `internal/cli/ensure` | T4.2 | 27 |
| phase-4 | `internal/cli/env` | T4.3 | 17 |
| phase-4 | `internal/cli/login` | T4.2 | 1 |
| phase-4 | `internal/cli/setup` | T4.1 | 12 |
| phase-4 | `internal/doctor` | T4.4 | 20 |
| phase-4 | `internal/pickerguard` | T4.4 | 3 |
| phase-4 | `internal/pins` | T4.3 | 12 |
| phase-4 | `internal/rccompat` | T4.5 | 8 |
| phase-4 | `internal/readiness` | T4.6 | 22 |
| phase-4 | `internal/release` | T4.1 | 24 |
| phase-4 | `internal/selfinstall` | T4.0 | 3 |
| phase-4 | `internal/status` | T4.2 | 2 |
| phase-5 | `internal/selfinstall` | T5.1 | 8 |

Total: 705 due cases.
