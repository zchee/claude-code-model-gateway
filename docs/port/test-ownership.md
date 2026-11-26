# Source test ownership

In [classification.tsv](../../test/conformance/classification.tsv), `package` is the implementation owner. For `gate=task`, join `(phase, package)` to the package row below. For `gate=phase`, join `phase` to its **phase gate** row instead: the complete case needs another task not guaranteed by its owner’s prerequisites. Package rows count task-gated cases only; a zero does not remove package ownership. Exempt cases have neither gate nor owner. For task-gated conformance cases, the gate is the later of the implementing task and launch-adapter proof (T2.0b); the conformance column counts a subset of the due cases, not additional cases. The [port plan](model-gateway-go-port.md#4-implementation-steps) defines the prerequisites and exit gates.

| Phase | Package or gate | Implementing task or exit gate | Gate for conformance cases | Due cases | Conformance cases |
|---|---|---|---|---:|---:|
| phase-0 | `internal/atomicfile` | T0.3 | T2.0b | 3 | 0 |
| phase-0 | `internal/gatewaytest` | T0.4b | T2.0b | 9 | 0 |
| phase-0 | `internal/procinfo` | T0.5 | T2.0b | 9 | 0 |
| phase-0 | **phase gate** | phase gate (§4, phase table line 590; AC-1, AC-2) | phase exit | 1 | 0 |
| phase-1 | `internal/anthropiccatalog` | T1.4 | T2.0b | 1 | 0 |
| phase-1 | `internal/catalog` | T1.12c | T2.0b | 4 | 0 |
| phase-1 | `internal/cli` | Verify that the removed command is unknown | CLI regression test | 23 | 0 |
| phase-1 | `internal/controlauth` | T1.12a | T2.0b | 2 | 0 |
| phase-1 | `internal/doctor` | Read-only hosts-block detection; assertions deferred to phase-4 | Not a conformance case | 2 | 0 |
| phase-1 | `internal/grok` | T1.9 | T2.0b | 8 | 0 |
| phase-1 | `internal/install` | T1.3 | T2.0b | 10 | 0 |
| phase-1 | `internal/lifecycle` | T1.11 | T2.0b | 9 | 0 |
| phase-1 | `internal/pins` | T1.4 | T2.0b | 2 | 0 |
| phase-1 | `internal/policy` | T1.1 | T2.0b | 15 | 0 |
| phase-1 | `internal/proxyconfig` | T1.3 | T2.0b | 2 | 0 |
| phase-1 | `internal/proxymodels` | T1.2 | T2.0b | 5 | 0 |
| phase-1 | `internal/toolschema` | T1.5b | T2.0b | 10 | 0 |
| phase-1 | `internal/upstreamstate` | T1.12a | T2.0b | 0 | 0 |
| phase-1 | `internal/usage` | T1.12d | T2.0b | 12 | 0 |
| phase-1 | `internal/wiring` | T1.10 | T2.0b | 25 | 0 |
| phase-1 | `internal/worker/dispatch` | T1.8 | T2.0b | 3 | 0 |
| phase-1 | `internal/worker/route` | T1.7 | T2.0b | 3 | 0 |
| phase-1 | `internal/worker/sentry` | T1.6 | T2.0b | 1 | 0 |
| phase-1 | **phase gate** | phase gate (§4, phase table line 591; AC-3, AC-8) | phase exit | 2 | 0 |
| phase-2 | `internal/anthropiccatalog` | T2.1 | T2.1 | 6 | 6 |
| phase-2 | `internal/catalog` | T2.1 | T2.1 | 2 | 1 |
| phase-2 | `internal/grok` | T2.7 | T2.7 | 8 | 3 |
| phase-2 | `internal/localprov` | T2.1 | T2.1 | 11 | 4 |
| phase-2 | `internal/proxymodels` | T2.1 | T2.1 | 6 | 6 |
| phase-2 | `internal/readiness` | T2.1 | T2.1 | 6 | 1 |
| phase-2 | `internal/supervisor` | T2.0 | T2.0b | 5 | 5 |
| phase-2 | `internal/telemetry` | T2.8 | T2.8 | 3 | 3 |
| phase-2 | `internal/usage` | T2.8 | T2.8 | 9 | 1 |
| phase-2 | `internal/worker` | T2.0 | T2.0b | 3 | 1 |
| phase-2 | `internal/worker/compactguard` | T2.6 | T2.6 | 2 | 2 |
| phase-2 | `internal/worker/dispatch` | T2.5 | T2.5 | 2 | 2 |
| phase-2 | `internal/worker/reply` | T2.3 | T2.3 | 1 | 0 |
| phase-2 | `internal/worker/route` | T2.5 | T2.5 | 12 | 11 |
| phase-2 | `internal/worker/sentry` | T2.5 | T2.5 | 1 | 0 |
| phase-2 | `internal/worker/sse` | T2.4 | T2.4 | 4 | 4 |
| phase-2 | **phase gate** | T2.9 | T2.9 | 130 | 127 |
| phase-3 | `internal/launch` | T3.6 | T3.6 | 13 | 0 |
| phase-3 | `internal/ownership` | T3.1 | T3.1 | 16 | 0 |
| phase-3 | `internal/shimstate` | T3.6 | T3.6 | 7 | 0 |
| phase-3 | `internal/supervisor` | T3.3 | T3.3 | 1 | 1 |
| phase-3 | `internal/supervisor/proxyrecovery` | T3.2 | T3.2 | 15 | 1 |
| phase-3 | `internal/supervisor/relay` | T3.3 | T3.3 | 30 | 6 |
| phase-3 | `internal/supervisor/workerproc` | T3.4 | T3.4 | 77 | 3 |
| phase-3 | **phase gate** | T3.7 | T3.7 | 3 | 0 |
| phase-4 | `internal/catalog` | T4.4 | T4.4 | 16 | 8 |
| phase-4 | `internal/cli/contextwindow` | T4.3 | T4.3 | 2 | 1 |
| phase-4 | `internal/cli/ensure` | T4.2 | T4.2 | 22 | 10 |
| phase-4 | `internal/cli/env` | T4.3 | T4.3 | 17 | 17 |
| phase-4 | `internal/cli/login` | T4.2 | T4.2 | 1 | 0 |
| phase-4 | `internal/cli/setup` | T4.1 | T4.1 | 13 | 0 |
| phase-4 | `internal/doctor` | T4.4 | T4.4 | 16 | 8 |
| phase-4 | `internal/pickerguard` | T4.4 | T4.4 | 3 | 0 |
| phase-4 | `internal/pins` | T4.3 | T4.3 | 10 | 7 |
| phase-4 | `internal/readiness` | T4.6 | T4.6 | 21 | 8 |
| phase-4 | `internal/release` | T4.1 | T4.1 | 24 | 0 |
| phase-4 | `internal/selfinstall` | T4.0 | T4.0 | 3 | 0 |
| phase-4 | `internal/status` | T4.2 | T4.2 | 0 | 0 |
| phase-4 | **phase gate** | T4.7 | T4.7 | 15 | 6 |
| phase-5 | `internal/selfinstall` | T5.1 | T5.1 | 8 | 1 |
| phase-5 | **phase gate** | phase gate (§4, phase table line 595; packaging acceptance: AC-7, AC-15a, AC-16; AC-13 review) | phase exit | 0 | 0 |

Total: 705 due cases (554 at task gates, 151 at phase gates).
