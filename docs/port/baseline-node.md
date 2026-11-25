# Pinned Node gateway baseline

Three metrics for comparison with the Go port: healthy prompt-hook latency,
client time to first byte through the supervisor, and idle resident memory.
These measurements describe the pinned runtime, not the performance of a real
provider. Re-measure both runtimes in the same job on the same runner when
applying the Go performance gate; do not use historical numbers as its threshold.

The [raw JSON Lines file](baseline-node-samples/measurements.jsonl) contains every
warmup, timed sample, RSS reading, load average, launch environment, and probe event.
It was decoded from the single checksum-verified run frame. UTC timestamps were
read with `date` in the venue; durations and offsets use a monotonic clock.

## Machine and commands

| Field | Observed value |
| --- | --- |
| Source commit | a8501f25175572071a0d085d34346dcf67e49b23 |
| Root tree | 22be5dae8859c7307906f938fa364b7ffadf006c |
| Plugin tree | aaf4f26b527c2ff08dbc47707a5f69e6ec2cf40a |
| Plugin | 0.56.3 |
| CPU | Intel(R) Xeon(R) Platinum 8481C CPU @ 2.70GHz |
| Logical CPUs | 44 |
| Exposed cores | 22 |
| CPUs available to Node | 44 |
| Memory (bytes) | 185,697,378,304 |
| Kernel | Linux 6.12.118+deb13-cloud-amd64 #1 SMP PREEMPT_DYNAMIC Debian 6.12.118-1 (2026-11-20) x86_64 |
| Node | v24.11.1 |
| npm | 11.6.2 |
| Ajv | 8.20.0 |
| Run started (UTC) | 2026-11-25T20:03:54.138257481Z |
| Run ended (UTC) | 2026-11-25T20:07:33.145159348Z |
| Compressed frame SHA-256 | a8aeef35b357cc2519019aa388f24dc34de9fabaeab8f303d735edc119be44f9 |
| Raw JSON Lines SHA-256 | e184e5dc52d045a5c8562c4013aac996b7fd7b2f8b78cc99219c42f1858935e0 |

```bash
VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run bench
```

```bash
VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote status
```

Inside the venue (`$WORK` is the private scratch directory and `$PLUGIN` is the
plugin in its verified source export):

| Purpose | Command |
| --- | --- |
| Make target | `make -- bench` |
| Make recipe | `./hack/venue-check && ./hack/bench` |
| pin | `$WORK/hack/pinned-source` |
| install | `npm --prefix "$PLUGIN" ci --ignore-scripts --no-audit --no-fund` |
| harness | `node --input-type=commonjs - "$PLUGIN" "$WORK"` |
| worker | `node "$PLUGIN/bin/model-gateway.js" serve-worker` |
| supervisor | `node "$PLUGIN/bin/model-gateway.js" serve-shim` |
| readiness | `node "$HOME/.claude/model-gateway/model-gateway.js" readiness-gate` |
| date | `date -u +%Y-%m-%dT%H:%M:%S.%NZ` |
| kernel | `uname -srvm` |
| CPU | `lscpu -p=SOCKET,CORE` |
| listeners | `ss -H -ltn "sport = :$PORT"` |

The launch environment was cleared with `env -i`; the `run`, `environment`, and
`probe-environment` records list the exact environment allowlists (private scratch
prefix replaced by `$WORK`). The isolated install also sets `npm_config_cache`
to the private `$WORK/npm-cache`. Supervisor and direct worker get a throwaway home,
an empty hosts fixture, ephemeral loopback ports and a private Unix socket. The
child worker inherits the supervisor's environment. The hook points both port
selectors and `ANTHROPIC_BASE_URL` at the healthy supervisor. No real `claude`
resolves on the venue PATH; the CLI override points at a nonexistent file. The
readiness fixture writes only the pinned plugin's installed registry, launcher,
and supervisor identity into its sandbox. The default usage emitter is left on
and pointed at an ephemeral fake collector, which received 603
POSTs. All other gateway settings are default. The proxy-recovery monitor sees a
healthy loopback fake proxy during idle.

## Performance

- **Readiness:** p95 over 50 quiet, successful installed
  `UserPromptSubmit` hook invocations per series, measured immediately before
  spawning the Node launcher until its process and stdio close. This hook is the
  per-prompt path, unlike the session-start hook.
- **Time to first response byte (TTFB):** p99 over 200 serial
  requests per series. Start immediately before creating the client request on
  a fresh loopback TCP connection; stop on the first socket data, including the
  status line (not the first SSE event). Each valid 1,048,576-byte
  JSON request goes through the supervisor and worker to the fake Anthropic
  provider. The fake sends a fixed 2,000-event response of
  257,982 body bytes with no artificial delay, and the client
  drains and verifies the entire body before starting the next request. This
  isolates upload/relay latency rather than measuring the provider's think
  time or a warmed client pool. The gateway's agent defaults are unchanged.
- **Idle RSS:** supervisor plus its worker's Linux `VmRSS`, measured after at
  least 60 seconds with no benchmark traffic following the
  latency tests. This is warm idle, not cold-start memory; it counts shared
  resident pages in both processes and excludes the client and fake providers.

Each of the 3 series started a fresh supervisor, ran serially under
one venue lock, and discarded 1 warmup per timed workload. Other
lanes may have been using the host: the lock excludes other venue runs, not all
host activity. Report the spread rather than choose a favorable run. The load
table gives short-, medium-, and long-window averages, in that order, at each
measurement interval's start and end.

| Series | Metric | Samples | Min (ms) | Median (ms) | p95 or p99 (ms) | Max (ms) |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Readiness | 50 | 60.768 | 61.818 | 62.543 | 64.598 |
| 1 | TTFB | 200 | 12.899 | 13.551 | 17.354 | 22.198 |
| 2 | Readiness | 50 | 61.112 | 62.228 | 63.013 | 63.301 |
| 2 | TTFB | 200 | 12.692 | 13.361 | 18.007 | 19.199 |
| 3 | Readiness | 50 | 61.249 | 62.152 | 63.013 | 65.426 |
| 3 | TTFB | 200 | 12.557 | 13.283 | 17.537 | 18.621 |

Percentiles use nearest rank (ceiling of fraction times sample count, with the
rank starting at one). Warmup samples are flagged and excluded from summaries.

| Series | Idle (s) | Supervisor RSS (KiB) | Worker RSS (KiB) | Sum (KiB) |
| --- | --- | --- | --- | --- |
| 1 | 60.000 | 110352 | 189452 | 299804 |
| 2 | 60.000 | 109508 | 192700 | 302208 |
| 3 | 60.000 | 111424 | 142312 | 253736 |

| Statistic | Across-series median | Across-series min–max |
| --- | --- | --- |
| Readiness p95 (ms) | 63.013 | 62.543–63.013 |
| TTFB p99 (ms) | 17.537 | 17.354–18.007 |
| Idle RSS sum (KiB) | 299804 | 253736–302208 |

### Load at measurement boundaries

| Series | Metric | Start UTC | Start load | End UTC | End load |
| --- | --- | --- | --- | --- | --- |
| 1 | readiness | 2026-11-25T20:03:54.320915186Z | 0.00/0.31/0.90 | 2026-11-25T20:03:57.479776831Z | 0.08/0.33/0.90 |
| 1 | ttfb | 2026-11-25T20:03:57.480992152Z | 0.08/0.33/0.90 | 2026-11-25T20:04:00.748925299Z | 0.08/0.32/0.90 |
| 1 | rss | 2026-11-25T20:04:00.750787999Z | 0.08/0.32/0.90 | 2026-11-25T20:05:00.753398981Z | 0.14/0.29/0.85 |
| 2 | readiness | 2026-11-25T20:05:00.957180655Z | 0.14/0.29/0.85 | 2026-11-25T20:05:04.133203739Z | 0.14/0.29/0.85 |
| 2 | ttfb | 2026-11-25T20:05:04.134621693Z | 0.14/0.29/0.85 | 2026-11-25T20:05:07.303538054Z | 0.13/0.29/0.84 |
| 2 | rss | 2026-11-25T20:05:07.305465086Z | 0.13/0.29/0.84 | 2026-11-25T20:06:07.307827463Z | 0.05/0.23/0.79 |
| 3 | readiness | 2026-11-25T20:06:07.512463549Z | 0.05/0.23/0.79 | 2026-11-25T20:06:10.691168584Z | 0.12/0.24/0.79 |
| 3 | ttfb | 2026-11-25T20:06:10.692873278Z | 0.12/0.24/0.79 | 2026-11-25T20:06:13.837730868Z | 0.12/0.24/0.79 |
| 3 | rss | 2026-11-25T20:06:13.839706406Z | 0.12/0.24/0.79 | 2026-11-25T20:07:13.842565613Z | 0.10/0.21/0.74 |

## Client disconnects

The direct path calls `serve-worker`; the relayed path calls `serve-shim`, which
forks a worker. Both paths exercise native Anthropic passthrough and Codex routing
to the fake proxy.

- **Before upload end:** the client sends a partial body with its full
  `Content-Length`, waits 100 ms and closes.
- **Headers only:** the provider flushes its SSE headers but withholds the body.
  The client disconnects after a 100-ms settle interval from
  that signal, without waiting for downstream headers that Node may buffer.
- **Mid-stream:** the provider sends its first event; the client waits until it
  sees that event before disconnecting.

After either response-stage disconnect, the fake provider attempts to send
16,777,216 SSE body bytes including any initial event but excluding
the terminal event, respecting socket back-pressure. It writes in
65,536-byte blocks and observes for 1,500 ms. Bytes offered to `write` are not an
acknowledgement from the gateway; an open connection can be pooled even after a
response completes. The raw events record provider request arrival, receipt of
the whole request body, response finish or close, TCP close, and client data and
close. Event offsets below are milliseconds relative to client disconnect.
Pending write age is how long the last write had been waiting for `drain` at the
end of the window. A dash means the event was not observed within the window,
not that it never occurs. Forced fixture cleanup after the observation window
is not counted as a provider cancellation.

| Path | Model | Disconnect stage | Provider requests | Provider request bytes | Provider response bytes offered | Provider headers (ms) | Provider finish (ms) | Provider TCP close (ms) | Pending write age (ms) | Observed (ms) | Client status / wire bytes | Client close (ms) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| direct | `claude-sonnet-4-6` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1501.732 | — / 0 | +0.113 |
| direct | `claude-sonnet-4-6` | headers-only | 1 | 118 | 4980736 | -100.209 | — | — | 1453.662 | 1502.123 | — / 0 | +0.060 |
| direct | `claude-sonnet-4-6` | mid-stream | 1 | 118 | 4980982 | -0.941 | — | — | 1452.490 | 1500.361 | 200 / 417 | +0.066 |
| direct | `claude-gpt-5.6-sol` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1500.645 | — / 0 | +0.054 |
| direct | `claude-gpt-5.6-sol` | headers-only | 1 | 112 | 16777216 | -100.204 | +28.840 | — | — | 1500.842 | — / 0 | +0.056 |
| direct | `claude-gpt-5.6-sol` | mid-stream | 1 | 112 | 16777216 | -0.451 | +30.145 | — | — | 1501.751 | 200 / 417 | +0.067 |
| relayed | `claude-sonnet-4-6` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1500.699 | — / 0 | +0.058 |
| relayed | `claude-sonnet-4-6` | headers-only | 1 | 118 | 8192000 | -100.218 | — | — | 1280.524 | 1500.390 | — / 0 | +0.054 |
| relayed | `claude-sonnet-4-6` | mid-stream | 1 | 118 | 10354934 | -0.887 | — | — | 1448.604 | 1500.811 | 200 / 388 | +0.054 |
| relayed | `claude-gpt-5.6-sol` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1500.792 | — / 0 | +0.056 |
| relayed | `claude-gpt-5.6-sol` | headers-only | 1 | 112 | 16777216 | -100.193 | +39.448 | — | — | 1500.987 | — / 0 | +0.057 |
| relayed | `claude-gpt-5.6-sol` | mid-stream | 1 | 112 | 16777216 | -0.666 | +38.378 | — | — | 1501.944 | 200 / 388 | +0.049 |

Neither path sent a provider request on an incomplete upload. After an upload
completed, neither path cancelled the provider on downstream disconnect. The
Anthropic response stalled under back-pressure in both response stages on both
paths; Codex's response finished and its TCP connection stayed open during the
observation window. In the worker, native passthrough uses `upRes.pipe(clientRes)`,
while the Codex SSE transformer consumes `data` events and does not gate them on
`clientRes.write()` returning true. None of the headers-only clients received a
status line, even though the provider had flushed headers. The probes do not
establish when a stalled response will eventually expire.

## Upgrade request

The raw client sends `GET /healthz` with `Connection: Upgrade, close` and
`Upgrade: fixture` to each path, then records the response and peer close.
The raw file preserves the status line, headers and response body separately.

| Path | Status line | Upgrade header | Wire bytes | Connect to close (ms) |
| --- | --- | --- | --- | --- |
| direct | `HTTP/1.1 200 OK` | — | 1176 | 1.297 |
| relayed | `HTTP/1.1 200 OK` | — | 1249 | 2.798 |

## Cleanup

Each pinned gateway child and its worker exited, and the harness checked that
none of the ephemeral gateway or fake provider ports still had a listener. The
benchmark and remote wrappers removed their private working directories. The
[post-run venue status](baseline-node-samples/venue-status.txt) shows that the
attestation remained present, the standard gateway ports had no listeners, the
account's gateway state was absent, and the venue lock was free. The real CLI
pin probe was not run.
