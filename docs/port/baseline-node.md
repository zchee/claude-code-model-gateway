# Pinned Node gateway baseline

Three metrics for comparison with the Go port: healthy prompt-hook latency,
client time to first byte through the supervisor, and idle resident memory.
These measurements describe the pinned runtime, not the performance of a real
provider. Re-measure both runtimes in the same job on the same runner when
applying the Go performance gate, using this same fixture for both runtimes.
Do not use historical numbers as its threshold.

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
| Run started (UTC) | 2026-11-25T20:33:11.461299924Z |
| Run ended (UTC) | 2026-11-25T20:36:56.769137729Z |
| Compressed frame SHA-256 (recorded at capture) | 1d2d2b1994df73c01676934c16c3ab61bcbd78028af5fde5408132966912bc8a |
| Raw JSON Lines SHA-256 (verifiable) | 0302b95c4e24deb34837666db3ec1e92535f3aeaddc4447567800b790def2873 |

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
prefix replaced by `$WORK`, also the process marker value). The source export
additionally prepends its private `env` shim to `PATH` so Git inherits the marker.
The isolated install sets `npm_config_cache`
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
  a fresh loopback TCP connection; stop when the first socket `data` callback is
  delivered in user space, including the status line (not kernel arrival or the
  first SSE event). The client, fake providers, and fake collector share one Node
  event loop. The gateway supervisor and worker run in separate processes. Each
  valid 1,048,576-byte JSON request goes through the supervisor and worker to the
  fake Anthropic provider. The fake sends a fixed 2,000-event response of
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
table gives short-, medium-, and long-window averages at each interval's
start and end.

| Series | Metric | Samples | Min (ms) | Median (ms) | p95 or p99 (ms) | Max (ms) |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Readiness | 50 | 61.125 | 61.839 | 62.613 | 63.000 |
| 1 | TTFB | 200 | 12.819 | 13.553 | 17.876 | 20.043 |
| 2 | Readiness | 50 | 61.041 | 61.878 | 62.747 | 64.517 |
| 2 | TTFB | 200 | 12.716 | 13.231 | 16.815 | 18.239 |
| 3 | Readiness | 50 | 60.964 | 62.203 | 62.927 | 62.978 |
| 3 | TTFB | 200 | 12.430 | 13.425 | 17.565 | 18.009 |

The 95th and 99th percentiles use nearest rank (ceiling
of fraction times sample count, with the rank starting at one). Within-series
latency medians are the mean of the two middle samples; the across-series median
is the middle series value. Warmups are excluded from summaries.

| Series | Idle (s) | Supervisor RSS (KiB) | Worker RSS (KiB) | Sum (KiB) |
| --- | --- | --- | --- | --- |
| 1 | 60.001 | 111032 | 186052 | 297084 |
| 2 | 60.000 | 111244 | 190340 | 301584 |
| 3 | 60.000 | 111940 | 152484 | 264424 |

| Statistic | Across-series median | Across-series min–max |
| --- | --- | --- |
| Readiness p95 (ms) | 62.747 | 62.613–62.927 |
| TTFB p99 (ms) | 17.565 | 16.815–17.876 |
| Idle RSS sum (KiB) | 297084 | 264424–301584 |

### Load at measurement boundaries

| Series | Metric | Start UTC | Start load | End UTC | End load |
| --- | --- | --- | --- | --- | --- |
| 1 | readiness | 2026-11-25T20:33:11.645053149Z | 0.52/0.30/0.30 | 2026-11-25T20:33:14.804231132Z | 0.64/0.33/0.31 |
| 1 | ttfb | 2026-11-25T20:33:14.805417826Z | 0.64/0.33/0.31 | 2026-11-25T20:33:17.996227660Z | 0.67/0.34/0.32 |
| 1 | rss | 2026-11-25T20:33:17.997972877Z | 0.67/0.34/0.32 | 2026-11-25T20:34:18.000971949Z | 0.24/0.27/0.29 |
| 2 | readiness | 2026-11-25T20:34:18.204494452Z | 0.24/0.27/0.29 | 2026-11-25T20:34:21.365332128Z | 0.24/0.27/0.29 |
| 2 | ttfb | 2026-11-25T20:34:21.366576280Z | 0.24/0.27/0.29 | 2026-11-25T20:34:24.508725404Z | 0.30/0.29/0.30 |
| 2 | rss | 2026-11-25T20:34:24.510451317Z | 0.30/0.29/0.30 | 2026-11-25T20:35:24.512619443Z | 0.11/0.23/0.27 |
| 3 | readiness | 2026-11-25T20:35:24.715546398Z | 0.11/0.23/0.27 | 2026-11-25T20:35:27.889948100Z | 0.18/0.24/0.28 |
| 3 | ttfb | 2026-11-25T20:35:27.891292739Z | 0.18/0.24/0.28 | 2026-11-25T20:35:31.064973829Z | 0.18/0.24/0.28 |
| 3 | rss | 2026-11-25T20:35:31.067166084Z | 0.18/0.24/0.28 | 2026-11-25T20:36:31.069947532Z | 0.06/0.20/0.26 |

## Client disconnects

The direct path calls `serve-worker`; the relayed path calls `serve-shim`, which
forks a worker. Both paths exercise native Anthropic passthrough and Codex routing
to the fake proxy.

- **Before upload end:** the client sends a partial body with its full
  `Content-Length`, waits 100 ms and closes.
- **Awaiting headers:** the provider signals that it received the full request
  body but has not sent any response headers. The client disconnects, and the
  provider waits 100 ms after client close before attempting
  its headers and tail. The raw events record the header send or skip.
- **Headers only:** the provider flushes its SSE headers but withholds the body.
  The client disconnects after a 100-ms settle interval from
  that signal, without waiting for downstream headers that Node may buffer.
- **Mid-stream:** the provider sends its first event; the client waits until it
  sees that event before disconnecting.

After each response-stage disconnect, the fake provider attempts to send
16,777,216 SSE body bytes including any initial event but excluding
the terminal event, respecting socket back-pressure. It writes in
65,536-byte blocks. The target observation window is
1,500 ms after disconnect; the actual duration is in each row.
Bytes offered to `write` are not an acknowledgement from the gateway; an open
connection can be pooled even after a response completes. The raw events record
provider request arrival, receipt of the whole request body, response finish
or close, TCP close, and client data and close. Event offsets below are relative
to client disconnect. Pending write age is how long the last write had been
waiting for `drain` at the end of the window. A dash means the event was not
observed within the window. Forced fixture cleanup after the observation window
is not counted as a provider cancellation.

| Path | Model | Disconnect stage | Provider requests | Provider request bytes | Provider response bytes offered | Provider headers (ms) | Provider finish (ms) | Provider TCP close (ms) | Pending write age (ms) | Observed (ms) | Client status / wire bytes | Client close (ms) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| direct | `claude-sonnet-4-6` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1499.845 | — / 0 | +0.096 |
| direct | `claude-sonnet-4-6` | awaiting-headers | 1 | 118 | 7536640 | +100.371 | — | — | 1310.507 | 1500.576 | — / 0 | +0.070 |
| direct | `claude-sonnet-4-6` | headers-only | 1 | 118 | 4980736 | -100.246 | — | — | 1451.830 | 1499.624 | — / 0 | +0.073 |
| direct | `claude-sonnet-4-6` | mid-stream | 1 | 118 | 5177590 | -0.858 | — | — | 1454.772 | 1499.835 | 200 / 417 | +0.064 |
| direct | `claude-gpt-5.6-sol` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1500.647 | — / 0 | +0.060 |
| direct | `claude-gpt-5.6-sol` | awaiting-headers | 1 | 112 | 16777216 | +100.294 | +129.892 | — | — | 1500.355 | — / 0 | +0.063 |
| direct | `claude-gpt-5.6-sol` | headers-only | 1 | 112 | 16777216 | -100.206 | +29.681 | — | — | 1500.295 | — / 0 | +0.060 |
| direct | `claude-gpt-5.6-sol` | mid-stream | 1 | 112 | 16777216 | -0.350 | +29.855 | — | — | 1500.416 | 200 / 417 | +0.051 |
| relayed | `claude-sonnet-4-6` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1499.723 | — / 0 | +0.060 |
| relayed | `claude-sonnet-4-6` | awaiting-headers | 1 | 118 | 13500416 | +100.303 | — | — | 1301.495 | 1500.439 | — / 0 | +0.076 |
| relayed | `claude-sonnet-4-6` | headers-only | 1 | 118 | 12779520 | -100.199 | — | — | 1403.649 | 1499.643 | — / 0 | +0.060 |
| relayed | `claude-sonnet-4-6` | mid-stream | 1 | 118 | 15532278 | -0.942 | — | — | 1402.406 | 1499.199 | 200 / 388 | +0.066 |
| relayed | `claude-gpt-5.6-sol` | before-upload-end | 0 | 0 | 0 | — | — | — | — | 1500.626 | — / 0 | +0.050 |
| relayed | `claude-gpt-5.6-sol` | awaiting-headers | 1 | 112 | 16777216 | +100.294 | +140.228 | — | — | 1499.689 | — / 0 | +0.055 |
| relayed | `claude-gpt-5.6-sol` | headers-only | 1 | 112 | 16777216 | -100.207 | +33.779 | — | — | 1499.807 | — / 0 | +0.057 |
| relayed | `claude-gpt-5.6-sol` | mid-stream | 1 | 112 | 16777216 | -0.548 | +37.105 | — | — | 1499.662 | 200 / 388 | +0.047 |

No provider request was observed in the incomplete-upload cases. After a
complete upload, no provider TCP close was observed on either path during the
recorded windows. The Anthropic responses stalled under back-pressure at each
sampled response stage. Codex sent the full response tail in each case before
the window ended. In the worker, native passthrough uses `upRes.pipe(clientRes)`,
while the Codex SSE transformer consumes `data` events without gating them on
`clientRes.write()` returning true. Clients received no status line in the
awaiting-headers or headers-only cases. In the headers-only case the provider
had already flushed its headers before the client disconnected. These probes
do not establish when a stalled response will eventually expire.

## Upgrade request

The raw client sends `GET /healthz` with `Connection: Upgrade, close` and
`Upgrade: fixture` to each path, then records the response and peer close.
The raw file preserves the full wire response as Base64, including its
chunk-framed body, and extracts the headers at the first delimiter only.

| Path | Status line | Upgrade header | Wire bytes | Connect to close (ms) |
| --- | --- | --- | --- | --- |
| direct | `HTTP/1.1 200 OK` | — | 1177 | 1.573 |
| relayed | `HTTP/1.1 200 OK` | — | 1250 | 2.311 |

## Cleanup

The measurement run verified that each pinned gateway child and worker exited
and that no ephemeral fixture port still had a listener. The measurements and
workload are unchanged by the later containment change.

The harness now runs pin export, dependency installation, and the Node job under
a Linux child subreaper. Detached orphans are adopted by that supervisor. The
supervisor signals only its own children and accepts `waitpid` returning
`ECHILD`, not a process listing or an environment marker, as proof of cleanup.
It fails on a cleanup deadline or when a normally successful run required forced
termination. The subreaper writes diagnostics to a private file, so containment
can prove emptiness or fail within its budget without waiting for the client
to read. Output is delivered afterward; as for other venue targets, an unread
connection can delay delivery and release of the venue lock. The wrapper removes
its private scratch (home, sockets, and files); the remote runner retains its
separate staging copy on failure.

The [cleanup controls](baseline-node-samples/cleanup-controls.txt) include
interruptions during the first supervisor's detached worker identity probe,
with an unmarked descendant in a separate process group. One control also
filled the harness's stderr pipe and verified containment without reading it. The
[post-run venue status](baseline-node-samples/venue-status.txt) links the
latest control's venue completion time and run identifier to the immediate
status check: attestation present, no gateway state, no standard listeners,
and lock free. The real CLI pin probe was not run.
