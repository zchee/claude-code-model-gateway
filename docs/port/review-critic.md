model: claude-gpt-6.2-astra-fast[1m]

# Critic review of model-gateway-go-port.v1.md

Persisted by the lead at 2026-11-25 22:11:44 JST from the reviewer's team message (the reviewer's role is read-only and it did not write files).
Reviewed snapshot: .omc/drafts/model-gateway-go-port.v1.md. Sources: git objects at 4ffb2f672f0c, INV-01 … INV-06, target worktree.

## Verdict: REVISE
Do not authorize Phase 0 or cutover as written. The three-process design is defensible; the ownership transition, the
purported oracle and the acceptance evidence are not. Citation sample: 20/25 support the exact proposition (all cited
places exist). Acceptance criteria: 6/13 operationally concrete (AC-1, 3, 4, 5, 9, 13).

## Critical
1. Takeover (design rule 6) conflicts with the drop-in lock and stop semantics. Node setup and stop both take the same lock
   (commands.js:2936,2951-55; lifecycle-lock.js:35-40,65-92); the Go parent's re-entrant context cannot extend to the Node
   child. Holding the lock times out; releasing it permits a competing ensure. A successful stop CREATES
   intentionally-stopped.json, which preserve-wiring then treats as pre-existing intent (commands.js:473,485). Finding a
   launcher does not distinguish legacy JS from the Go trampoline on a repeated setup. The old plugin's SessionStart
   unconditionally rewrites both launcher files (hooks/registry-writer.js:151-156,192-201); T6.2 restores only one, not the
   migrated hook args, update launcher, prior plugin selection or settings. Required: a non-nested takeover protocol with a
   single owner at each transition; persist pre-cutover run/stop intent and owned bytes; identify both installations and
   quiesce the old writer; test active / previously stopped, foreign listener, repeated setup, concurrent hook, failure at
   each write/restart, and reversible compare-and-restore. An inert PID fixture proves none of this.
2. T2.9 is not a worker-only phase gate. Seven named files call serve-shim before T3.3: compact-stream:84,
   context-sentry-lifecycle:61, gateway-telemetry:54, thread-negotiation:80, tool-search:110, request-worker:67,
   gateway-usage:119. anthropic-catalog:99,105,252 needs ensure/pin from phase 4. T3.5 similarly certifies setup/ensure
   before T4.1/T4.2. A trampoline spawns a new Go child: shim-upload-abort:197 requires health.supervisorPid === child.pid;
   gateway-drain:415 signals the parent. test/support.js:792-799 has ignored stdin and an IPC fd; an unconditional stdin-EOF
   worker exits before readiness, and inheriting stdio does not supply the Go fd-3 port-report protocol. Keeping JS exports
   lets Node-only tests pass without Go. Required: isolate direct versus supervised launch mode; a harness adapter that
   reports the real Go PID and preserves stdio/teardown; classify individual expanded test names; prove representative
   direct, supervised, signal, registry and CLI cases; distribute gates by first available product behavior; before the
   implementation gate agree the minimum Go-exercising tests per surface and a fallback with a replacement AC-11, budget,
   owner and independent expected results. K4 says "before phase 2" but decides after it.
3. Isolation (sections 1.4, 3.5, 10.1): "refuses to bind" does not prevent connecting to or controlling a live listener,
   signaling a process, writing a registered project's settings, or contacting a production provider from a child missing
   the env. The guard disappears with its env trigger. Size+mtime is not byte equality, prevention, a filesystem boundary or
   the saved copy section 10.1 promises. MODEL_GATEWAY_STATE_DIR does not move the discovery cache, user settings, cwd
   project files, breadcrumb, hosts or provider home (INV-01 §1.1; INV-04 §2; runtime.js:15-16,548). T6.1 assumes a healthy
   probe of the live proxy means never manipulating it; INV-03 §4.6 rule 14 may spawn when that port becomes free.
   Required: separate user/container/VM for Node and Go tests and baselines, restricted fs/process/network access, isolated
   cwd/config/provider homes and caches; fail closed before effects; test stripped-env descendants and symlinked paths. For
   the soak use a separate proxy or an enforced read-only external-proxy mode with no spawn/kill/adoption. Rehearse rollback
   entirely in fixtures first.

## Major
4. Acceptance criteria with false positives or wrong expectations. AC-6 conflates "no signal" with "refusal on every
   command" (a healthy foreign proxy is reusable; disabled / intentional-stop ensure may succeed). AC-7 requires a stopped
   shim and doctor exit 0 (commands.js:1517: non-ready -> 1). AC-12 requires one JSON object although stop/disable produce no
   stdout (commands.js:577-586,227-238,3043-44). AC-8: `go test -list` lists names, not runs or passes; an X row can waive
   all behavior; `hack/parity-check` has no producing task. AC-10 needs a hypothetical Node status; request-routes.jsonl has
   neither status nor token counts (INV-02 §11.1). At threshold equality the sentry passes; it refuses the NEXT eligible turn
   after usage > trigger. Required: an outcome matrix by state and command; countable executed case ids with no unexpected
   skips; sanitized test JSON/TAP; instrument redacted usage/spans; deterministic fixtures, no replay of billed prompts.
5. Deliberate differences against named source tests: X4 fails rc-compat-mode:886,900-902 and shim-upload-abort:198-203;
   X3 breaks wiring-mode:898; X8 invalidates gateway-process-isolation:1326-79 and owned-worker-cleanup:48,135,266; X9
   reverses readiness-fast-path:76-87; X10 changes readiness-gate:169's child-kill observable. X11 conflates malformed JSON
   (runtime.js:277-279 catches syntax/read errors) with invalid compactAt (287-307, tested at context-window-setting:296).
   Required: every deviation gets a named paired old/new test, owner, exception reason and independent approval; never
   classify known failures as exclusions only after tests run.
6. Section 3.4 rule 8: update.js does not forward arguments; it inserts `setup --preserve-wiring`
   (hooks/registry-writer.js:114-122). "same-install = executable directory" and the stable bin do not define POST /restart's
   optional candidate, monotonic sibling version, symlink/foreign rejection, or selection among plugin cache versions
   (INV-03 §3.3; gateway-drain:460,541,562). X8 deletes evidence for already-live legacy orphans before demonstrating their
   exit. Required: role/ownership proofs and a versioned artifact mapping across mixed binaries; retain unmatched records;
   test standalone worker, old/upgraded siblings and interrupted switches.
7. T0.1 cannot be reproduced. Target go.mod:1 and origin are github.com/zchee/claude-code-model-gateway, not plan line 9. The
   boilerplate path does not exist. Root main.go:15 is `package main` with no main; adding cmd/model-gateway alone leaves
   `go build ./...` unable to link an unchanged root. README/main/.claude are untracked; do not delete or absorb them without
   consent. Local source HEAD is a8501f2517, not the pin: README/runtime/skill/context-window test changed; consumers must use
   the Git object, not live file line numbers. T0.1 requires fmt/lint/test and tripwire before T0.4 supplies it; T0.6 needs a
   Node-capable baseline runner with a ready fixture.
8. Hidden dependencies: T1.2 consumes T1.1 policy or must be restricted to parsers; T1.6 catalog needs policy/models/readiness
   contracts while CLI catalog is T4.4; T1.8 consumes paths/config, pins and install decisions; T1.9 stale lock needs identity
   (T3.1). No unequivocal owner for serve-shim/serve-worker command registration, worker/route classification and catch-all,
   local live probes, the standalone worker compatibility listener (request-worker.js:2703-13), attachment of all HTTP
   branches, or the detector for K8. T2.6/T2.8 need T2.4 despite section 9; T4.4 needs T4.3; T4.1 needs selfinstall (T5.1),
   which needs T4.1. T5.6 must review the actual plugin wrapper; T5.5 must follow tested differences. Five workers on one
   branch plus module-wide autoformat can touch each other's edits. Required: split deliverables, publish package interfaces
   and owners, give each command an integration owner, specify worktree/merge ownership and clean-checkout inputs, then sort.
9. Phase 5: optional release + go install with no exact package path, bootstrap or versioning fallback; plugin/ has no defined
   marketplace entry and silently exits when the binary is absent (INV-06 §§1,4,5,7; PATH after user PATH). A go install
   need not include release ldflags. No cold install or plugin host test across new install, disable and upgrade. Decide
   mandatory distribution, binary origin and verification, platforms, marketplace install, PATH collision, version lookup;
   smoke-test with no Node and no pre-existing stable binary. No T6.3 changes to the external gateway-switch repository
   without separate authority.
10. Section 3.3 / T1.4 call jsonschema/v6 + regexp2 an ECMAScript oracle and test-only, but the product validates every
    rewritten regex with `new RegExp(pattern,'u')` (tool-schema-compat.js:232-236). Assign runtime validation, pin independent
    Node/Ajv differential fixtures, and verify byte-preserving SSE separately from parsed observation. X1: atomic rename alone
    does not merge concurrent settings updates or preserve symlink/metadata semantics.

## Citation sample (25)
Hits (20): commands.js:2935-3041 (17 commands); INV-06 §6; INV-04 §2.3; INV-01 §4.2; INV-03 §4.6 rule 11;
process-supervision.js:510-514 (parses a legacy path, not proof of this installation); INV-02 §2.2 (15 rows); INV-04 §3.5;
INV-01 §3.6 (20 sections); INV-05 §6.2 (13 classes); request-worker.js:1210; commands.js:2420-2441;
process-supervision.js:981-991; lifecycle-lock.js:88-94; registry-writer.js:106; request-worker.js:1827-39;
remote-control.js:219; package.json:13; registry-writer.js:19-28; runtime.js:244-256.
Misses (5): INV-05 §3.1 says nine whole files plus two mixed, not eleven fully black-box; INV-01 §1/§3.2 warn quiet stdout may
be empty; INV-04 §13.1 is about invalid compactAt, not malformed JSON generally; commands.js:2633-39 validates a port and
clears the timer but does not establish the five-second timeout; INV-04 §1.2 lists names only, not parsing rules.
Numbers: pinned production JS is 13,056 lines; 57 *.test.js = 21,677 lines / 693 static test() sites; all 65 test-tree files =
23,539 lines. All 30 lib lengths in section 3.2 match. 77 invariant table rows (not 77 atomic assertions); 69+7+6 = 82 static
sites before subtracting two unit-only cases. 28764, 50/200 samples, 12 bodies, 1 s / 3 d / 5% budgets, the X7 backoff and
0.57.0 are proposals, not upstream measurements.

## Acceptance criteria audit
AC-1 yes; AC-2 no (size/mtime is not byte identity; the trigger can vanish); AC-3 yes (add an HTTP end-to-end route); AC-4 yes
(identify all 12 inputs); AC-5 yes (require abort versus fabricated success); AC-6 no (state-dependent outcomes; missing CLIs
at the gate); AC-7 no (ambiguous; stopped + doctor 0 contradiction); AC-8 no; AC-9 yes once baselined (define machine, timing
origin, noise, raw distributions); AC-10 no; AC-11 no (wrong denominator); AC-12 no; AC-13 yes. Missing criteria: no second
billed inference before versus after a worker accepted the body; two-runtime migration and rollback; real cold plugin install;
isolated shared-proxy failure; per-difference evidence.

## Coverage gaps
INV-01 §§0-2 (usage, unknown command, log routing need explicit cobra cases); updater argv; stable-command advice with no
installation; env-only wiring. INV-02: unmatched any-method fallback, count_tokens, malformed JSON precedence and the
standalone compatibility listener need a named owner; live llama/LM Studio probes and cross-package snapshot integration are
not clearly assigned; §§12-14 specify serialized map update order, not just race freedom. INV-03: versioned worker
replacement, duplicate listener and retention of legacy orphan evidence. INV-04 §1.2: names only; inherited credentials,
proxy/OTLP/trace disablement, registry overrides, names from bin/hooks. A state manifest of every file with a cross-runtime
shape/writer test is missing. INV-05 §4 matrix has no author. INV-06: no cold-install/upgrade host acceptance. `version`,
`completions`, `self update`, the state override, doctor host-schema validation are extensions, not parity; give them
independent tests.

## Section 9 gaps
Agent definitions are untracked, so a clean-checkout verifier may lack them; missing worktree/branch owner, integration
permission, verify-before-rebase/hash rules, grok routing probe and exact fallback, explicit user-only phase 6, verifier
evidence storage, shutdown acknowledgements, commit message rules. An agent identity line is not test evidence.

## Vague phrases to replace
"same arguments", "stable Node launcher exists", "matches", "wait for the port", "every settings file", "preserved and
named", "documented refusal", "non-older sibling", "about 200", "enough tests", "one file", "one extra review pass", "three
riskiest", "black-box parts", "three backends", "12 fixture bodies" (five specified), "full restart", "one compaction", "zero
5xx that the Node gateway would not have produced", "no unexplained exit", "rollback rehearsed", "optional", "marketplace
name chosen by the user", "doctor checks … accepted", "protocol bugs", "relay overhead … total request time".

## Contradictions
Rule 6 handling two producers while old hooks still write; stop-marker handling between rules; section 1.3 excluding source
tests versus section 4 "port all"; section 3.5 size/mtime versus AC-2 bytes versus section 10 backup; section 9 phase-wide
parallelism versus the task graph; OD-3's claim that dropping X8 adds Node to the hook path; "performance unchanged" versus new
cancellation/IPC/JSON; no consumer-safe rollback for the new hook string; X3/X4/X8/X9 versus an unchanged suite.

## Additional failure scenarios the pre-mortem misses
Both launchers overwritten again by the old SessionStart hook; a user running before cutover becomes intentionally stopped;
outage or restart while the shadow points at the production proxy; a slow or aborted relay creates a second billable upstream
request; a changed binary or socket after an interrupted install leaves no rollback.
