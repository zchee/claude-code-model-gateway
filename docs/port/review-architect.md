model: claude-gpt-6.2-astra-fast[1m]

# Architect review of model-gateway-go-port.v1.md

Persisted by the lead at 2026-11-25 22:07:17 JST from the reviewer's team message (the reviewer's role is read-only and it did not write files).
Reviewed snapshot: .omc/drafts/model-gateway-go-port.v1.md. Source citations are plugin-relative at 4ffb2f672f0c.

## Verdict
SOUND WITH CHANGES; do not execute v1. Keep option A for the stable public listener, the existing drain/control wire
contract and process-crash isolation, not for a supposed at-most-once guarantee. Cutover and shadow are critical blockers.

## Ranked defects and fixes

1. CRITICAL — Cross-process takeover is not reentrant. Node setup and stop each acquire the directory lock; the reentrancy
   capability is process-local; stop writes the maintenance marker before proving ownership (commands.js:2936,2951-2960;
   lifecycle-lock.js:65-93,96-107; commands.js:473-486). Invoking `node <launcher> stop` inside locked Go setup waits on
   itself; invoking outside creates a restart window and changes a previously running home to intentionally stopped. An
   existing launcher can already be the Go trampoline or resolve another install. Rule 5's regex recognizes a path shape,
   not the owner: process-supervision.js:510-542 separately proves physical install/marketplace; 570-596 distinguish
   unknown, unowned, foreign; rule 5 omits unowned. Fix: quiesce the old publisher and legacy mutators first, pre-stage and
   verify the new binary, use one fenced migration owner and a non-relocking stop operation on a verified legacy
   listener/birth/install/role; revalidate before signaling, preserve pre-transition intent, reject ambiguity, make
   repeat/resume/rollback idempotent. Test running, stopped, unrelated marketplace, already-Go, simultaneous setup and
   failure between steps. Do not invent a `command` disguising Go as Node.
2. CRITICAL — State-dir override is not shadow isolation. Recovery's healthy branch is non-owning only while healthy: on an
   empty port it can start its own proxy (INV-03 §4.6 rules 13-14); a foreign unhealthy listener differs from a vacant
   port. The source also uses HOME for settings/launchers/registry, cwd for project settings, the config dir for discovery,
   a separate proxy config and Grok home for writable auth; RC may bind the hosts entry's address/port (INV-04 §2;
   INV-02 §§6,8; commands.js:2728-2778; hooks/registry-writer.js:132-154). Plan lines 69-72 and 423 cannot both hold for a
   default-port live proxy. Fix: prefer a dedicated proxy with isolated state/config and ephemeral ports; if the user
   chooses sharing, add an explicit externally-managed read-only lifecycle mode (no spawn/adoption/signal/upgrade/auth
   refresh against shared state even on failure), no RC listener, separate HOME/config/cache/socket/project/registry/auth
   and a preflight writable-path/port matrix. Simulate live proxy disappearance in isolated fixtures. AC-10 cannot classify
   counterfactual production 5xx without an oracle: incident attribution from logs plus deterministic isolated replay; do
   not mirror paid prompts.
3. CRITICAL — Migration has two writers and an insufficient rollback. The old SessionStart hook unconditionally rewrites
   BOTH launchers even before checking breadcrumb future schema (hooks/hooks.json:4-10; registry-writer.js:145-155,
   192-201). `update.js` ignores argv and runs `setup --preserve-wiring` (114-122); generic pass-through breaks updates.
   Restoring only model-gateway.js leaves binary-form hooks, updater, breadcrumb, registry choice and potentially a swapped
   proxy/version or stop intent pointing at the wrong installation (plan:272-275,424). Fix: stage old/new assets with
   verification first; disable the old registry writer and account for cached/active sessions, enable one plugin identity,
   back up owned fields and restore conditional on unchanged values, no user edits clobbered. Exercise restart of a fresh
   host on both forward migration and rollback; test both launchers' exit/stdin/signal behavior plus a missing binary.
   `doctor` cannot prove there is no legacy hook in every unregistered project.
4. MAJOR — Process-model premise contradicted by the source's own test: test/shim-request.test.js:84-104 requires resend to
   a replacement worker; 107-131 forbids resend to the same current worker. Delivery is latched at TCP connect, not body
   read or provider commitment; missing port / no delivery retries; loss before headers while that worker remains proceeds
   to a bounded wait and a non-retryable 503; a change of generation permits resend; a committed response aborts the
   connection (commands.js:2353-2401,2420-2450). Fix: specify the worker-generation, response-commit and client-disconnect
   state machine, the transport retry/redirect policy, and count actual provider calls. Pick parity or explicitly choose a
   stricter never-replay-after-possible-dispatch rule and document the availability cost. Go's buffered request plus an
   idempotency header can make a pooled request replayable (Go 1.27.4 net/http/request.go:1561-1573,
   transport.go:844-878); Node uses http.request, not automatic redirect following (request-worker.js:1822-1841).
5. MAJOR — PID compatibility and instance scope are different problems. lifecycle-lock.js:30-41 compares local
   host/pid/raw `startedAt` exactly; LC_ALL=C fixes locale, not TZ or day padding. Darwin gopsutil derives epoch
   milliseconds (process_darwin.go:88-94), Linux combines boot seconds and ticks (process_linux.go:1108). Exact legacy
   `ps` output may be achievable for the same timezone/format and OS; require a native ps-vs-Go fixture on both OSes,
   single-digit days and TZ mismatch; use the actual legacy probe for mixed-mode locks and fail closed on incomparable
   evidence, or quiesce old writers. For a readable Go guardian the old owner parser rejects non-JS argv regardless of
   timestamp; `recordedGatewayPid` can retire the .pid pair (process-supervision.js:149-185,510-514,581-588);
   bidirectional lock exclusion is needed during overlap, bidirectional ownership is neither achieved nor desirable. A
   shared exe directory also fails to distinguish different state roots/ports/roles or launch via $GOBIN versus the
   stable copied bin. Fix: record stable logical install, instance/state root and generation plus role and original
   executable file identity at spawn, separate caller location from service identity; recheck listener and process birth.
   Do not sweep `.old-*` still in use. Linux reads /proc/<pid>/exe; Darwin uses proc_pidpath (gopsutil
   process_linux.go:666-673; process_darwin.go:369-379); rename/unlink/replace/symlink require a two-OS probe, not a
   basename wildcard. Test the same binary running two independent instances.
6. MAJOR — Ordinary stdin is not proof of supervision. Node watches IPC disconnect and optionally sends the listener
   report; no IPC is valid (request-worker.js:1208-1211,2688-2694). The harness sets stdin `ignore`, Node IPC in slot 3 and
   parses stdout (test/support.js:790-829). An unconditional EOF watcher makes a direct worker with /dev/null exit
   immediately, or lets a terminal user control its lifetime. Fix: select supervised mode by an explicit inherited
   descriptor contract; in standalone mode neither IPC nor stdin is mandatory. Bound report size/deadline, validate port
   and child generation, own a unique non-inherited parent write end, close duplicates in all paths, define malformed
   report and cleanup. Crash/SIGKILL closes the final writer but EOF requires live scheduling and no leaked write ends; a
   suspended worker can survive, and the external proxy is not covered. Keep legacy birth evidence until proven stale,
   allow proven orphan remediation; test a stopped/unresponsive child, supervisor killed before/after report, fd
   inheritance and independent proxy adoption. Cmd.ExtraFiles is not supported on Windows (Go os/exec/exec.go:238-243);
   compile-only is fine, runtime needs an explicit alternative.
7. MAJOR — Oracle and phase gates cannot run as specified. The harness probes 200 after stdout; worker drain must return
   202 using the fixture token; supervisor fixture shutdown is delivered by IPC; teardown waits for actual child close and
   recorded decimal PIDs (support.js:105-120,158-175,193-202,292-330,790-829). A spawned wrapper's PID differs from
   supervisor.pid and SIGKILL cannot be forwarded (gateway-drain.test.js:359,415-422); ignored stdin fails a direct worker
   first. `require(CLI)` can silently test Node code; fixture generation is fine, code under assertion is not
   (catalog-refresh.test.js:13,28-30). compact-stream:84, context-sentry-lifecycle:61, thread-negotiation:80,
   request-worker:67, gateway-usage:119, gateway-telemetry:54 and tool-search:110 all start serve-shim, not the phase-2
   worker. AC-11's 82 are not 82 fully black-box tests: INV-05 §3.1 gives 69+6+5 against 82 gross, before further
   exclusions. Fix: prefer adapting only harness launch/control to spawn Go directly for process-identity suites while
   preserving assertions; permit a forwarding wrapper purely for CLI/HTTP cases. Track each assertion by test name, process
   under test, isolated invocation and positive evidence that Go ran; no green Node-only assertions, no fallback by
   relabeling red tests. The early spike must include a standalone worker, a real supervisor shutdown and a PID assertion;
   split phase-2 direct cases from post-supervisor ones. Preserve test/isolate-home.js; scrub new MODEL_GATEWAY_* variables
   not cleared by source support.js:659-731; make the safety rails a dependency of T0.5 before any actual run.
8. MAJOR — A single JSON policy cannot serve both files and wire. Node parses `raw.toString()` and uses JSON.stringify on
   the transformed branch (request-worker.js:2431-2441,2569; INV-02 §3.3). AllowDuplicateNames permits duplicates; it does
   not resolve them. Fix: validate exactly one complete document through EOF, even after the model was found; collapse
   decoded names at every nesting level (last value, first insertion slot), enumerate array-index keys as JS does,
   distinguish array/scalar/null, coerce numbers with JS semantics on the wire (9007199254740993 -> 9007199254740992,
   1e400 -> null); replace ill-formed input UTF-8 as Node does; preserve escaped lone surrogates where required instead of
   losing them in Go strings. Retaining invalid raw strings with both jsontext options can emit invalid bytes (Go
   jsontext/options.go:102-111). Preserve original bytes only on passthrough; file output can keep lexemes and unknown keys
   under X12 separately. The response rewrite is recursive and conditional; malformed input is returned unchanged as a
   whole, which is impossible after emitting a speculative prefix (request-worker.js:1612-1623,1634-1647). Golden tests
   need input bytes + branch + output; JS stringify goldens for the wire, a separate golden for lexical settings. Atomic
   replace on a settings symlink may replace the link rather than the target, and rename alone cannot prevent concurrent
   lost edits (settings-wiring.js:88-112; renameio tempfile.go:192-205). Define a symlink/mode/owned-field concurrency
   policy with refuse/retry and test it.
9. MAJOR — X6 and the SSE dependency alter billing, usage and sentry behavior. Cancellation must propagate across both
   public->worker and worker->provider; Go outbound requests do not implicitly inherit the incoming context. Do not
   rearm or latch from partial/aborted usage or retry after disconnect. Buffering a summary has no completion sink once the
   client is gone: Node checks `destroyed || !writable` before issuing a compaction retry (request-worker.js:2042-2053,
   2059-2077,2081-2095). Default to strict parity, or to explicit cancel + no retry; a bounded independent context only if
   a named consumer benefits. go-sse@v0.12.0/event.go:23-38,51-68,82-109 is range-compatible but its return type is not
   literally iter.Seq2; default max event 64 KiB; it joins data lines and drops comments and a partial EOF frame. Node
   preserves raw frames, transforms the first data line, emits the EOF tail, with a 4 MiB observation budget
   (request-worker.js:1668-1724,1729-1761); the peek joins lines and replays the original bytes (490-507). Keep exact raw
   framing where needed; use the library only on suitable translation/observer paths with explicit limits and
   non-interfering failure. Test large events, comments, CRLF, multiple data lines, partial EOF before and after headers,
   direct and relayed disconnect, usage high-water, no detached retries.
10. MAJOR — The implementation graph needs executable gates. T3.5 requires end-to-end ensure/setup but they land in
    T4.1/T4.2; T4.1 claims self-install before T5.1 implements it; T6.2 uses a rollback not rehearsed in a running
    multi-install fixture; T1.9 needs an identity seam before T3.1; T2.7 needs T1.5; T2.6/T2.8 require T2.4 despite the
    section 9 parallel schedule. No named owner of the worker/route classifier (AC-3; INV-02 §2) or of the shared snapshot
    and synchronization contract (INV-02 §12): plan a separate classifier and an atomic model/alias snapshot, define race
    tests. T2.3 owns R1-R8, which also appear in T2.4-T2.8: split transport/DNS, reply dispatch, SSE/compaction and
    telemetry. Split T3.3 into relay, worker lifecycle and RC listener; bind each public CLI command to the root before its
    tests. T3.4's guardian-first stop differs from INV-03 §4.5 (shim first): keep the safer ordering but list, test and log
    the difference. Test the implemented guard for subprocesses, external config and symlinks, not just settings:
    size/mtime cannot prove AC-2 byte equality and an after-the-fact tripwire cannot prevent a signal (plan:286-289,434).
    Test migration with a real legacy helper/owned process in a sandbox, not a pid-shaped JSON file. go.mod:1 says
    github.com/zchee/claude-code-model-gateway, not plan:9's claude-code-model-gateway; root main.go:15 is only
    `package main`. Resolve the import / go install path and bootstrap before CI; no silent user-file deletion.

## Antithesis and trade-offs
The best steelman is B: today's immutable listener + drain solves process replacement, whereas B can atomically swap router
generations, drain old handlers and keep the exact client response writer for recoverable goroutine/upstream failures,
using a conservative possibly-submitted bit; this avoids an entire TCP hop and one full extra body buffer. B cannot write a
503 if the whole daemon is killed; C passing only the LISTEN socket likewise cannot answer an already accepted connection.
A socket design that preserves an independent connection owner can answer before headers, but then needs
accepted-connection/response-commit coordination (and TLS/H2 constraints), not just listener passing. Durable provider
idempotency could address billing but is not shown supported or equivalent wire behavior. B defeats the exclusivity claim,
not A's independent restart/availability argument. Keep A, make retry semantics a separate decision.
Tensions: parity versus avoiding duplicate billed work; file fidelity versus JS wire normalisation; prompt throughput versus
proven owner; easy coexistence versus single-writer safety. Choose explicit boundaries and test both outcomes rather than
treating topology as proof.

## Deliberate differences (X1-X12) against the principles
- X1 CHANGE: atomic regular-file updates yes; preserve symlink/metadata/concurrent edits, keep exclusive token creation,
  refuse an unsupported replace (P2).
- X2 DROP as the parity default; opt-in strict CLI if requested (P1).
- X3 KEEP scoped to the supported host: exact unquoted absolute command and args ["readiness-gate"]; preserve
  differently-argued invocations and unrelated groups, disableAllHooks, the SessionStart matcher, UserPromptSubmit without
  onFailure:block; distinguish an unspawnable hook from a runnable gate reporting a real failure (INV-06 §3;
  readiness-hooks.js:5-45).
- X4 KEEP with the paired controller change and explicit legacy mismatch/rollback; preserve the loopback-only
  Host/Origin/type/expected-PID gates (commands.js:2715-2720,2783-2787).
- X5 CHANGE: adopt only a previously supervised process with matching instance, role, record and live listener/birth;
  health alone never confers kill authority; no adoption in external mode (P2).
- X6 CHANGE per finding 9 (P1/P4).
- X7 KEEP with the explicit interaction between the 5 s report window, the 5 s backoff and the 81 x 50 ms retry window,
  plus crash-loop tests; count consecutive quick failures, not planned drains (commands.js:2360-2365,2487-2490,2612-2617,
  2642-2654).
- X8 DROP the unconditional deletion and the orphan-proof claim (P2/P4); retain proven cleanup until a justified retirement.
- X9 CHANGE: the newer version was explicitly sent to recovery to preserve a reload notice
  (readiness-fast-path.test.js:73-87); keep the fast path only for matching instance/PID, no stop marker, healthy
  supervisor, required protocol/proxyRecovery; emit the equivalent notice without forced recovery; unknown/incompatible
  never fast-paths; no Codex auth requirement (readiness-gate.js:19-36; shim-state.js:67-75).
- X10 CHANGE: one cooperative 25 s budget is useful but does not preempt blocking stdin, filesystem, children or detach.
  Include stdin, lock, child and cleanup; discriminate unable-to-run/indeterminate from established unhealthy; explicit
  0/1/2 + stderr contract for both events; external host watchdog 35 s, no onFailure:block (registry-writer.js:72-110;
  INV-06 §3).
- X11 CHANGE: recover from malformed config in read-only serving and report plainly; never rewrite the original or
  silently use cost-sensitive defaults without owner sign-off; specify every command's result and the doctor diagnostic
  (INV-04 §13.1).
- X12 KEEP for settings only; test the lexical exception separately, not as a blanket wire contract (P1/P3).

## Dependencies
Keep gopsutil behind `procinfo`; Darwin net.Connections enumerates via lsof (net_unix.go:17-54), not native port
filtering: bound it and distinguish absent/denied/unknown/multiple listeners. renameio/maybe on Windows uses os.WriteFile
(maybe/maybe_windows.go:7-20): the compile-only stub must reject writes rather than silently violate X1; file Sync+Rename
is not a complete directory-durability or metadata policy. miekg/dns is reasonable for DNS-only A/AAAA; Go's Resolver is
not always hosts-first, but a normal lookup may consult hosts (Go net/dnsclient_unix.go:573-602); account for system
nameservers, fallback and timeout. Standard compress/gzip and compress/zlib cover those codecs, only Brotli is necessarily
external (Node createInflate at request-worker.js:1765-1769); defer klauspost until measured. No need to reverse the
rejected flock, tidwall or tableflip. Test-only regexp2 is not proof of V8 semantics: keep the pinned real JS/Ajv as a
differential oracle for targeted cases in the already Node-based job (plan:223,364). Pin the optional updater only if it is
actually distributed.

## Questions for the lead before execution
What is the retry promise on generation change? Who fences old hooks and owns rollback, and is simultaneous legacy operation
supported? Dedicated or explicitly unmanaged shadow proxy? Are the newer-compatible notice, cancelled usage and
malformed-cap fallback acceptable? Which tests really exercise Go, what is the module/distribution path, and what artifact
bootstraps a clean plugin install? Never infer that an unregistered project is migrated because a registry scan is empty.
