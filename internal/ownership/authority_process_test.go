// Copyright 2026 The claude-code-model-gateway Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build process && (linux || darwin)

package ownership_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// fixtureLifetime is the longest a fixture runner lets a child live.
const fixtureLifetime = 30 * time.Second

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		// hold binds a loopback listener, reports "pid port" on the report descriptor and stays alive until its
		// standard input ends. Its arguments only shape its command line.
		"hold": func(t testing.TB, _ []string) {
			var cfg net.ListenConfig
			ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			// The first output tells the parent that the runner's output copier, started after the process handle
			// was set, is running.
			if _, err := io.WriteString(os.Stdout, "started\n"); err != nil {
				t.Fatal(err)
			}
			// The launcher reserves fd 3 for the working directory; fd 4 is the report.
			if _, err := unix.Write(4, fmt.Appendf(nil, "%d %d\n", os.Getpid(), ln.Addr().(*net.TCPAddr).Port)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
				t.Fatal(err)
			}
		},
	})
}

// lockedBuffer collects a child's output while the runner may still be writing it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startedWriter is the child's output. exec starts the goroutine that copies the child's output into it only
// after it has set Cmd.Process, so its first Write is the point from which the test may use that handle. The
// child's input stays the original file, so that waiting for a terminated child never waits for a copier.
type startedWriter struct {
	out     *lockedBuffer
	once    sync.Once
	started chan struct{}
}

func (w *startedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.out.Write(p)
}

// child is a fixture process started through gatewaytest, with the handle its runner started it with.
type child struct {
	pid     int
	port    uint16
	argv    []string
	process *os.Process
	stdin   *os.File
	reports *bufio.Reader
	report  *os.File
	// join waits for the runner, which reaps the child; it never ends the child itself.
	join   func() error
	output *lockedBuffer
	// first is the first line a helper other than hold reported.
	first string
}

// startChild starts a helper child and waits for its first report. Cleanup ends its standard input and joins its
// runner.
func startChild(t *testing.T, helper string, args ...string) *child {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	cmd, err := env.HelperCommand(t, helper, args...)
	if err != nil {
		t.Fatal(err)
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	report, reportOut, err := os.Pipe()
	if err != nil {
		_, _ = stdinR.Close(), stdinW.Close()
		t.Fatal(err)
	}
	c := &child{output: new(lockedBuffer), argv: slices.Clone(cmd.Args), stdin: stdinW, report: report, reports: bufio.NewReader(report)}
	out := &startedWriter{out: c.output, started: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, out, out
	cmd.ExtraFiles = []*os.File{reportOut}
	done := make(chan error, 1)
	go func() {
		err := env.RunFixtureWithin(cmd, fixtureLifetime)
		_ = reportOut.Close()
		done <- err
	}()
	c.join = sync.OnceValue(func() error { return <-done })
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = c.join()
		_ = stdinR.Close()
		_ = report.Close()
	})
	line, err := c.next(fixtureLifetime)
	if err != nil {
		_ = stdinW.Close()
		t.Fatalf("fixture child did not report: %v; run: %v; output: %s", err, c.join(), c.output)
	}
	select {
	case <-out.started:
		c.process = cmd.Process
	case <-time.After(fixtureLifetime):
		t.Fatal("the runner did not start copying the child's output")
	}
	if helper != "hold" {
		c.first = line
		return c
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("fixture child reported %q", line)
	}
	pid, perr := strconv.Atoi(fields[0])
	port, qerr := strconv.ParseUint(fields[1], 10, 16)
	if perr != nil || qerr != nil || pid != c.process.Pid || port == 0 {
		t.Fatalf("fixture child reported %q; its handle is for pid %d", line, c.process.Pid)
	}
	c.pid, c.port = pid, uint16(port)
	return c
}

// next reads the child's next report line within limit.
func (c *child) next(limit time.Duration) (string, error) {
	if err := c.report.SetReadDeadline(time.Now().Add(limit)); err != nil {
		return "", err
	}
	line, err := c.reports.ReadString('\n')
	return strings.TrimSuffix(line, "\n"), err
}

// terminatedBy joins the runner and reports whether the child died of sig. A child that ended because its input
// ended exits normally and does not count.
func (c *child) terminatedBy(t *testing.T, sig syscall.Signal) bool {
	t.Helper()
	joined := make(chan error, 1)
	go func() { joined <- c.join() }()
	var err error
	select {
	case err = <-joined:
	case <-time.After(fixtureLifetime):
		t.Fatal("the fixture runner did not return")
	}
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == sig
}

// probe is the native probe with a budget that suits a busy venue.
func probe() procinfo.Probe {
	return procinfo.New(func(key string) (string, bool) { return "30000", key == "CODEX_GATEWAY_PROBE_TIMEOUT_MS" })
}

// fixtureDispatch is the prefix gatewaytest puts between a helper child's executable and its arguments.
var fixtureDispatch = regexp.MustCompile(`^--gatewaytest-helper=`)

// productionShape rewrites a helper child's command line into the shape the gateway's CLI is started with: the
// executable, then the command, then its operands. Only the fixture's dispatch prefix is removed; every other
// command line is returned unchanged.
func productionShape(argv []string) []string {
	if len(argv) < 4 || argv[1] != "-test.run=^TestGatewayFixtureHelper$" || argv[2] != "--" || !fixtureDispatch.MatchString(argv[3]) {
		return argv
	}
	return append([]string{argv[0]}, argv[4:]...)
}

// fixtureProbe is the native probe with the helper children's command lines adapted at the probe boundary, so
// that the authority sees them as the production CLI's.
type fixtureProbe struct{ procinfo.Probe }

func (p fixtureProbe) Identity(ctx context.Context, pid int) (procinfo.Identity, error) {
	id, err := p.Probe.Identity(ctx, pid)
	id.Argv = productionShape(id.Argv)
	return id, err
}

func (p fixtureProbe) Table(ctx context.Context) (map[int]procinfo.Identity, error) {
	table, err := p.Probe.Table(ctx)
	for pid, row := range table {
		row.Argv = productionShape(row.Argv)
		table[pid] = row
	}
	return table, err
}

// launchImage is the identifier a publisher records at launch: the executable every fixture child runs.
func launchImage(t testing.TB) procinfo.Image {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(exe, &st); err != nil {
		t.Fatal(err)
	}
	return procinfo.Image{Path: exe, Device: uint64(st.Dev), Inode: st.Ino}
}

// liveRecord samples a fixture child and returns the record its publisher writes in root.
func liveRecord(t *testing.T, ctx context.Context, root, role string, c *child) (procinfo.Record, procinfo.Identity) {
	t.Helper()
	id, err := (fixtureProbe{probe()}).Identity(ctx, c.pid)
	if err != nil {
		t.Fatalf("identity of fixture child %d: %v; output: %s", c.pid, err, c.output)
	}
	r := recordFor(root, id, role, c.port)
	r.Image = launchImage(t)
	return r, id
}

// signals delivers real signals through the handle of a child the test started, never by number, and nothing
// once the caller's context has ended.
type signals struct {
	t          *testing.T
	mu         sync.Mutex
	children   []*child
	sent       []sent
	suppressed int
}

func (s *signals) deliver(ctx context.Context, id procinfo.Identity, sig os.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		s.suppressed++
		return err
	}
	i := slices.IndexFunc(s.children, func(c *child) bool { return c.pid == id.PID })
	if i < 0 {
		s.t.Errorf("a signal %v was asked for process %d, which this test did not start", sig, id.PID)
		return syscall.EPERM
	}
	s.sent = append(s.sent, sent{id.PID, sig})
	return s.children[i].process.Signal(sig)
}

func (s *signals) record() []sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

func realAuthority(root string, s *signals, legacy func([]string) (string, string, bool)) ownership.Authority {
	return ownership.Authority{
		Records:   ownership.Store{Root: root},
		Processes: fixtureProbe{probe()},
		Signal:    s.deliver,
		Wait:      timerWait,
		Legacy:    legacy,
	}
}

// TestInstanceRecordAuthority decides with two live instances of the same executable that authority comes only
// from the record in the acting instance's own state root: the other instance's process, a stale or partial
// record and a missing record grant nothing, and a stop ends exactly the recorded process.
func TestInstanceRecordAuthority(t *testing.T) {
	live, cancel := context.WithTimeout(t.Context(), fixtureLifetime*2/3)
	defer cancel()
	rootA, rootB := stateRoot(t), stateRoot(t)
	a, b := startChild(t, "hold", "serve-worker"), startChild(t, "hold", "serve-worker")
	recA, _ := liveRecord(t, live, rootA, roleWorker, a)
	recB, idB := liveRecord(t, live, rootB, roleWorker, b)
	seed(t, rootA, workerName, recA)
	seed(t, rootB, workerName, recB)
	sig := &signals{t: t, children: []*child{a, b}}
	authA, authB := realAuthority(rootA, sig, noLegacy), realAuthority(rootB, sig, noLegacy)
	// inRootA is the other instance's record as if it had been written in root A.
	inRootA := recB
	inRootA.StateRoot = rootA

	refusals := map[string]struct {
		prepare func(t *testing.T) (name string, expected procinfo.Record)
		wantErr error
		// record is "kept" when the stored record must survive, "removed" when it names a lifetime that has ended and
		// is retired without a signal, and empty when no record of that name exists in root A.
		record string
	}{
		"error: the other instance's record is not in this state root": {
			prepare: func(*testing.T) (string, procinfo.Record) { return workerName + "-other", recB },
			wantErr: ownership.ErrNotOwned,
		},
		"error: a missing record grants nothing": {
			prepare: func(*testing.T) (string, procinfo.Record) { return "missing", inRootA },
			wantErr: ownership.ErrNotOwned,
		},
		"error: a partial record without a start token grants nothing": {
			prepare: func(t *testing.T) (string, procinfo.Record) {
				r := inRootA
				r.StartToken = ""
				seed(t, rootA, "partial", r)
				return "partial", r
			},
			wantErr: ownership.ErrNotOwned,
			record:  "kept",
		},
		"success: a stale record naming the other instance's number is retired without a signal": {
			prepare: func(t *testing.T) (string, procinfo.Record) {
				r := inRootA
				r.StartToken = "stale-start-token"
				seed(t, rootA, "stale", r)
				return "stale", r
			},
			record: "removed",
		},
	}
	for name, tt := range refusals {
		t.Run(name, func(t *testing.T) {
			recordName, expected := tt.prepare(t)
			err := stop(t, authA, live, recordName, expected)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Stop(%q) = %v; want %v", recordName, err, tt.wantErr)
			}
			if got := sig.record(); len(got) != 0 {
				t.Fatalf("signals were sent: %v", got)
			}
			if tt.record != "" {
				if s := readRaw(t, rootA, recordName); s.HasJSON != (tt.record == "kept") {
					t.Errorf("record %q present = %v; want it %s", recordName, s.HasJSON, tt.record)
				}
			}
		})
	}
	t.Run("success: each instance resolves only the listener its own record names", func(t *testing.T) {
		tests := map[string]struct {
			auth   ownership.Authority
			port   uint16
			want   owner
			reason string
		}{
			"success: instance A's listener is A's":   {auth: authA, port: a.port, want: owner{ownership.Ours, a.pid}},
			"error: instance B's listener is not A's": {auth: authA, port: b.port, want: owner{ownership.Unknown, b.pid}, reason: unidentifiedReason(b.port, b.pid)},
			"error: instance A's listener is not B's": {auth: authB, port: a.port, want: owner{ownership.Unknown, a.pid}, reason: unidentifiedReason(a.port, a.pid)},
			"success: instance B's listener is B's":   {auth: authB, port: b.port, want: owner{ownership.Ours, b.pid}},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				got, err := resolve(t, tt.auth, live, workerName, tt.port)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if diff := gocmp.Diff(tt.want, owner{got.Class, got.PID}); diff != "" {
					t.Errorf("owner (-want +got):\n%s", diff)
				}
				if tt.reason != "" && got.Reason != tt.reason {
					t.Errorf("refusal text %q; want %q", got.Reason, tt.reason)
				}
			})
		}
	})
	t.Run("error: a stop whose context has ended sends nothing", func(t *testing.T) {
		ended, end := context.WithCancel(live)
		end()
		_ = stop(t, authA, ended, workerName, recA)
		if got := sig.record(); len(got) != 0 {
			t.Errorf("signals were delivered with an ended context: %v", got)
		}
		if _, err := probe().Identity(live, a.pid); err != nil {
			t.Errorf("instance A is no longer identifiable after a stop with an ended context: %v", err)
		}
	})
	t.Run("success: a stop ends exactly the process its own record names", func(t *testing.T) {
		if err := stop(t, authA, live, workerName, recA); err != nil {
			t.Fatalf("Stop: %v; output: %s", err, a.output)
		}
		if diff := gocmp.Diff([]sent{{a.pid, syscall.SIGTERM}}, sig.record()); diff != "" {
			t.Errorf("signals (-want +got):\n%s", diff)
		}
		if !a.terminatedBy(t, syscall.SIGTERM) {
			t.Error("the recorded process did not die of the terminate")
		}
		if s := readRaw(t, rootA, workerName); s.HasJSON || s.HasMirror {
			t.Errorf("the stopped process's record files are still stored: %+v", s)
		}
		if s := readRaw(t, rootB, workerName); gocmp.Diff(recB, s.decoded(t)) != "" || s.Mirror != strconv.Itoa(recB.PID) {
			t.Errorf("the other instance's record changed: %+v", s)
		}
		after, err := probe().Identity(live, b.pid)
		if err != nil {
			t.Fatalf("the other instance is no longer identifiable: %v", err)
		}
		if after.StartToken != idB.StartToken || !after.Birth.Equal(idB.Birth) {
			t.Errorf("the other instance changed: token %q birth %v; want %q %v", after.StartToken, after.Birth, idB.StartToken, idB.Birth)
		}
	})
	if err := live.Err(); err != nil {
		t.Errorf("the live checks did not finish within their deadline: %v", err)
	}
}

// lstart is the shape of a legacy start time, as ps prints lstart in the C locale.
var lstart = regexp.MustCompile(`^[A-Z][a-z]{2} [A-Z][a-z]{2} [ 0-9][0-9] [0-9]{2}:[0-9]{2}:[0-9]{2} [0-9]{4}$`)

const (
	// The install roots the probe fixtures' Node-shaped command lines name, with non-ASCII bytes in the path: this
	// install, and the same marketplace's install under a physically unrelated cache, as the source's foreign fixture
	// is (test/gateway-ps-width.test.js:69).
	fixtureThisInstall  = "/tmp/模型/.claude/plugins/cache/example/model-gateway/9.9.9"
	fixtureOtherInstall = "/tmp/模型-foreign-install/.claude/plugins/cache/example/model-gateway/9.9.9"
)

// probeFixtures are four live processes with long command lines: a worker this root records, the Node gateway of
// another install root, the Node gateway of this install, and a process that is no gateway at all.
type probeFixtures struct {
	root                          string
	own, foreign, legacy, unknown *child
}

func (f probeFixtures) all() map[string]*child {
	return map[string]*child{"own": f.own, "foreign": f.foreign, "legacy": f.legacy, "unknown": f.unknown}
}

func startProbeFixtures(t *testing.T, ctx context.Context) probeFixtures {
	t.Helper()
	long := func(label string, command ...string) []string {
		return append(command,
			"--gw-ps-width-padding="+strings.Repeat("p", 160),
			"--gw-ps-width-sentinel="+label+"-"+strconv.FormatInt(time.Now().UnixNano(), 16))
	}
	f := probeFixtures{root: stateRoot(t)}
	f.own = startChild(t, "hold", long("own", "serve-worker")...)
	f.foreign = startChild(t, "hold", long("foreign", fixtureOtherInstall+"/bin/model-gateway.js", "serve")...)
	f.legacy = startChild(t, "hold", long("legacy", fixtureThisInstall+"/bin/model-gateway.js", "serve")...)
	f.unknown = startChild(t, "hold", long("unknown", "/tmp/模型/not-a-gateway.js")...)
	own, _ := liveRecord(t, ctx, f.root, roleWorker, f.own)
	seed(t, f.root, "own", own)
	proxy, _ := liveRecord(t, ctx, f.root, roleProxy, f.unknown)
	seed(t, f.root, "proxy", proxy)
	return f
}

// checkProbeEvidence decides, under whatever terminal width and locale the caller set, that the native probe
// returns complete command lines, start times, parents and table rows, and that ownership follows the records and
// the Node install roots.
func checkProbeEvidence(t *testing.T, ctx context.Context, f probeFixtures) {
	t.Helper()
	environBefore := os.Environ()
	p := probe()
	children := f.all()
	samples := map[string]procinfo.Identity{}
	for name, c := range children {
		id, err := p.Identity(ctx, c.pid)
		if err != nil {
			t.Fatalf("identity of the %s fixture: %v", name, err)
		}
		samples[name] = id
	}
	table, err := p.Table(ctx)
	if err != nil {
		t.Fatalf("process table: %v", err)
	}
	checks := map[string]func(t *testing.T){
		"success: full command lines": func(t *testing.T) {
			for name, c := range children {
				if diff := gocmp.Diff(c.argv, samples[name].Argv); diff != "" {
					t.Errorf("%s fixture argv (-want +got):\n%s", name, diff)
				}
			}
		},
		"success: process samples": func(t *testing.T) {
			for name, c := range children {
				id := samples[name]
				if id.PID != c.pid || id.ParentPID != os.Getpid() {
					t.Errorf("%s fixture sample pid %d parent %d; want %d and this test process %d", name, id.PID, id.ParentPID, c.pid, os.Getpid())
				}
				start := procinfo.LegacyStart(id.Birth)
				if !lstart.MatchString(start) {
					t.Errorf("%s fixture legacy start %q is not in the C locale's lstart form", name, start)
				}
				if want := kernelBirth(t, c.pid); !id.Birth.Truncate(time.Second).Equal(want) {
					t.Errorf("%s fixture birth %v (%q); the kernel's own record says %v", name, id.Birth, start, want)
				}
			}
		},
		"success: the table holds exactly the fixtures as this process's children and agrees with the samples": func(t *testing.T) {
			var census []int
			for pid, row := range table {
				if row.ParentPID == os.Getpid() {
					census = append(census, pid)
				}
			}
			var want []int
			for _, c := range children {
				want = append(want, c.pid)
			}
			slices.Sort(census)
			slices.Sort(want)
			if diff := gocmp.Diff(want, census); diff != "" {
				t.Errorf("children of this test process in the table (-want +got):\n%s", diff)
			}
			for name, c := range children {
				row, ok := table[c.pid]
				if !ok {
					t.Errorf("%s fixture %d is missing from the process table", name, c.pid)
					continue
				}
				id := samples[name]
				if diff := gocmp.Diff([]any{id.ParentPID, id.Argv, id.Birth.Unix()}, []any{row.ParentPID, row.Argv, row.Birth.Unix()}); diff != "" {
					t.Errorf("%s fixture table row and sample disagree (-sample +row):\n%s", name, diff)
				}
			}
		},
		"success: each fixture descends from this test process in the table": func(t *testing.T) {
			for name, c := range children {
				seen := map[int]bool{}
				for pid := c.pid; pid != os.Getpid(); {
					row, ok := table[pid]
					if !ok || seen[pid] || row.ParentPID <= 0 {
						t.Errorf("%s fixture's ancestry breaks at %d before reaching this test process", name, pid)
						break
					}
					seen[pid] = true
					pid = row.ParentPID
				}
			}
		},
		"success: ownership follows the records and the install roots": func(t *testing.T) {
			sig := &signals{t: t}
			legacy := noPanic(t, "NodeGateway", func() func([]string) (string, string, bool) { return ownership.NodeGateway(fixtureThisInstall, nil) })
			auth := realAuthority(f.root, sig, legacy)
			unknownRefusal := unidentifiedReason(f.unknown.port, f.unknown.pid)
			tests := map[string]struct {
				record string
				c      *child
				want   owner
				reason string
				legacy bool
				image  bool
			}{
				"success: the recorded worker is ours": {record: "own", c: f.own, want: owner{ownership.Ours, f.own.pid}},
				"error: the Node gateway of another install root is foreign": {
					record: "own", c: f.foreign, want: owner{ownership.Foreign, f.foreign.pid},
					reason: foreignReason(f.foreign.port, f.foreign.pid, fixtureOtherInstall, fixtureThisInstall), image: true,
				},
				"success: the Node gateway of this install is legacy":   {record: "own", c: f.legacy, want: owner{ownership.LegacyNode, f.legacy.pid}, legacy: true},
				"error: an unrecorded process stays unknown":            {record: "unknown", c: f.unknown, want: owner{ownership.Unknown, f.unknown.pid}, reason: unknownRefusal},
				"error: another fixture's record does not make it ours": {record: "own", c: f.unknown, want: owner{ownership.Unknown, f.unknown.pid}, reason: unknownRefusal},
				"error: a proxy record for a process that does not run the proxy binary is refused": {
					record: "proxy", c: f.unknown, want: owner{ownership.Unknown, f.unknown.pid}, reason: unknownRefusal,
				},
			}
			for name, tt := range tests {
				t.Run(name, func(t *testing.T) {
					got, err := resolve(t, auth, ctx, tt.record, tt.c.port)
					if err != nil {
						t.Fatalf("Resolve: %v", err)
					}
					if diff := gocmp.Diff(tt.want, owner{got.Class, got.PID}); diff != "" {
						t.Errorf("owner (-want +got):\n%s", diff)
					}
					switch {
					case tt.legacy && got.Reason != legacyReason(tt.c.port, tt.c.pid):
						t.Errorf("refusal text %q; want %q", got.Reason, legacyReason(tt.c.port, tt.c.pid))
					case tt.reason != "" && got.Reason != tt.reason:
						t.Errorf("refusal text %q; want %q", got.Reason, tt.reason)
					}
					if want := samples["foreign"].Image.Path; tt.image && got.Image != want {
						t.Errorf("observed image %q; want the holder's executable %q", got.Image, want)
					}
				})
			}
			if n := len(sig.record()); n != 0 {
				t.Errorf("resolving sent %d signals", n)
			}
		},
		"success: the probes leave the environment unchanged": func(t *testing.T) {
			if diff := gocmp.Diff(environBefore, os.Environ()); diff != "" {
				t.Errorf("environment (-before +after):\n%s", diff)
			}
		},
	}
	for name, check := range checks {
		t.Run(name, check)
	}
}

// probeBudget is each probe case's own allowance for starting its fixtures and probing them; it is shorter than a
// fixture's lifetime, so the children outlive every read.
const probeBudget = fixtureLifetime * 2 / 3

// TestProbeEvidenceAtTerminalWidth decides that the terminal width of the caller changes nothing the probe or the
// ownership decision sees. Every width starts its own fixtures under its own budget.
func TestProbeEvidenceAtTerminalWidth(t *testing.T) {
	widths := map[string]string{
		"success: COLUMNS=80":    "80",
		"success: COLUMNS=117":   "117",
		"success: COLUMNS=128":   "128",
		"success: COLUMNS=133":   "133",
		"success: COLUMNS=200":   "200",
		"success: COLUMNS=unset": "",
	}
	for name, width := range widths {
		t.Run(name, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			if width == "" {
				if err := os.Unsetenv("COLUMNS"); err != nil {
					t.Fatal(err)
				}
			}
			live, cancel := context.WithTimeout(t.Context(), probeBudget)
			defer cancel()
			checkProbeEvidence(t, live, startProbeFixtures(t, live))
			if err := live.Err(); err != nil {
				t.Errorf("the checks at this width did not finish within their budget: %v", err)
			}
		})
	}
}

// TestProbeEvidenceUnderLocale decides that a French locale in the caller's environment changes nothing the probe or
// the ownership decision sees, and that probing does not change the caller's environment.
func TestProbeEvidenceUnderLocale(t *testing.T) {
	for _, key := range []string{"LANG", "LC_ALL", "LC_TIME"} {
		t.Setenv(key, "fr_FR.UTF-8")
	}
	t.Setenv("MODEL_GATEWAY_LOCALE_SENTINEL", "keep-me")
	live, cancel := context.WithTimeout(t.Context(), probeBudget)
	defer cancel()
	checkProbeEvidence(t, live, startProbeFixtures(t, live))
	if got := os.Getenv("LC_ALL"); got != "fr_FR.UTF-8" {
		t.Errorf("LC_ALL is %q after probing; the caller's locale must be left alone", got)
	}
	if err := live.Err(); err != nil {
		t.Errorf("the live checks did not finish within their budget: %v", err)
	}
}
