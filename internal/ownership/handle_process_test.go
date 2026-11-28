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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/ownership"
)

const (
	// handleTHeldArg selects the held child's entry in this test binary; the argument after it is its lifetime.
	handleTHeldArg = "--ownership-held-child"
	// handleTHeldLifetime is how long the held child lets itself live before it exits on its own.
	handleTHeldLifetime = 30 * time.Second
	// handleTObserveBound bounds one look for the child's exit; it proves nothing about what reached the child.
	handleTObserveBound = 40 * time.Second
	// handleTHeldExitLifetime is the status a held child exits with when its own lifetime ran out.
	handleTHeldExitLifetime = 4
)

// TestHandleHeldChild is the entry of the held child that handleTStartHeld starts; in an ordinary test run it returns
// at once. The child enters its working directory through descriptor 3, prints its process number on its standard
// output, and ends itself: normally when its standard input ends, or with status 4 when the lifetime given on its
// command line has passed. It exits through os.Exit, so it never returns to the test runner. Nothing else ever ends
// it, unless it ignored both.
func TestHandleHeldChild(t *testing.T) {
	i := slices.Index(os.Args, handleTHeldArg)
	if i < 0 || i+1 >= len(os.Args) {
		return
	}
	lifetime, err := time.ParseDuration(os.Args[i+1])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "held child: lifetime:", err)
		os.Exit(3)
	}
	dir := os.NewFile(3, "held child working directory")
	if err := errors.Join(dir.Chdir(), dir.Close()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "held child: enter the working directory:", err)
		os.Exit(3)
	}
	time.AfterFunc(lifetime, func() { os.Exit(handleTHeldExitLifetime) })
	if _, err := fmt.Fprintf(os.Stdout, "%d\n", os.Getpid()); err != nil {
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// handleTHeld is a child that only its row reaps, and only last.
type handleTHeld struct {
	cmd    *exec.Cmd
	stdin  *os.File
	out    *os.File
	reader *bufio.Reader
	output strings.Builder
	exited bool
}

// handleTStartHeld starts the held child with plain exec.Command: no context, no WaitDelay and nothing else that could
// kill it or wait for it. The child gets exactly the environment gatewaytest.NewEnv builds and nothing inherited, and
// a private working directory under that home, entered through descriptor 3. Its standard output and error are one
// pipe the row reads, so the row can see the child's exit as that pipe's end without reaping it. The program is
// always os.Executable(), this test binary. The caller must end every path through handleTFinish.
//
// gatewaytest's fixture runner is not used: it kills the child at the end of its lifetime and waits for it at once,
// and that wait frees the number, which another process may then take while the row still opens or signals it.
func handleTStartHeld(t *testing.T) *handleTHeld {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workdir, err := os.MkdirTemp(env["HOME"], "held-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(workdir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_, _ = stdinR.Close(), stdinW.Close()
		t.Fatal(err)
	}
	//nolint:noctx // A context would kill the child from another goroutine, beside the reap; the child ends itself.
	cmd := exec.Command(exe, "-test.run=^TestHandleHeldChild$", "--", handleTHeldArg, handleTHeldLifetime.String())
	cmd.Dir = workdir
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	slices.Sort(cmd.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, outW, outW
	cmd.ExtraFiles = []*os.File{dir}
	err = cmd.Start()
	_, _ = stdinR.Close(), outW.Close()
	if err != nil {
		_, _ = stdinW.Close(), outR.Close()
		t.Fatal(err)
	}
	return &handleTHeld{cmd: cmd, stdin: stdinW, out: outR, reader: bufio.NewReader(outR)}
}

// pid reads the number the child printed and checks it is the process this row started.
func (c *handleTHeld) pid(t *testing.T) int {
	t.Helper()
	if err := c.out.SetReadDeadline(time.Now().Add(handleTObserveBound)); err != nil {
		t.Fatal(err)
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("held child did not report its number: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSuffix(line, "\n"))
	if err != nil || pid != c.cmd.Process.Pid {
		t.Fatalf("held child reported %q; the row started %d", line, c.cmd.Process.Pid)
	}
	return pid
}

// observeExit reads the child's output until its end, which comes only when the child has exited, within one bound.
// It does not reap: until Wait the number stays the child's.
func (c *handleTHeld) observeExit() bool {
	if c.exited {
		return true
	}
	if err := c.out.SetReadDeadline(time.Now().Add(handleTObserveBound)); err != nil {
		return false
	}
	_, err := io.Copy(&c.output, c.reader)
	c.exited = err == nil
	return c.exited
}

// handleTEnded says how a reaped child ended: "normal" after its input ended, "SIGTERM" when the terminate reached it,
// "lifetime" when it ended itself at its own lifetime, and otherwise the status or the signal's text.
func handleTEnded(err error) string {
	if err == nil {
		return "normal"
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok {
			switch {
			case status.Signaled() && status.Signal() == syscall.SIGTERM:
				return "SIGTERM"
			case status.Signaled():
				return "signal " + status.Signal().String()
			case status.ExitStatus() == handleTHeldExitLifetime:
				return "lifetime"
			}
			return "status " + strconv.Itoa(status.ExitStatus())
		}
	}
	return "unexpected: " + err.Error()
}

// handleTLiveRow is one live row: the handle operations it makes on its held child, how the child is expected to end
// ("signal" keeps its input open so that only the terminate can end it; "EOF" ends its input), and the results.
type handleTLiveRow struct {
	ops, want []string
	ending    string
	exit      string
	// refusedOps, refusedWant and refusedExit replace the above where the fixture finds pidfd_open refused (EPERM)
	// and no handle can be opened.
	refusedOps, refusedWant []string
	refusedExit             string
	// trace is the guard's trace the row expects, by the backend the fixture found.
	trace map[string][]string
}

// TestProcessHandle decides the handle on a real child this row started: the terminate reaches it as SIGTERM through
// the backend the fixture finds on it, and a closed handle sends it nothing. Every exit of the row, failures and
// panics included, runs one deferred sequence in one goroutine and in this order: the row's handle operations end;
// the child's exit is observed without reaping it; the handle is closed; the guard stops forwarding and its trace is
// checked; only then is the child reaped. Until that reap the number is the child's, so every call the guard forwarded
// reached that child alone. Nothing but the child itself ends it, except a last-resort kill of a child that ignored its
// input's end and its own lifetime, made after the guard stopped and before the reap.
func TestProcessHandle(t *testing.T) {
	tests := map[string]handleTLiveRow{
		"success: terminate reaches the held child with SIGTERM through the backend the fixture found": {
			ops:         []string{"open", "terminate"},
			want:        []string{"nil", "nil"},
			ending:      "signal",
			exit:        "SIGTERM",
			refusedOps:  []string{"open"},
			refusedWant: []string{"os.ErrPermission"},
			refusedExit: "normal",
			trace:       handleTTerminateTrace,
		},
		"error: a closed handle sends nothing to the held child": {
			ops:         []string{"open", "close", "terminate", "close"},
			want:        []string{"nil", "nil", "os.ErrClosed", "nil"},
			ending:      "EOF",
			exit:        "normal",
			refusedOps:  []string{"open"},
			refusedWant: []string{"os.ErrPermission"},
			refusedExit: "normal",
			trace:       handleTOpenTrace,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if !handleTRunLive(t, tt) {
				return
			}
			t.Log("fixture expired: the held child ended by its own lifetime before the handle acted; trying one fresh child")
			if handleTRunLive(t, tt) {
				t.Error("fixture expired twice: both held children ended by their own lifetime before the handle acted, so this row has no live evidence; no product result was compared")
			}
		})
	}
}

// handleTRawClose closes h and keeps what came back untouched: its error, or the value of a panic. Nothing is asked of
// either until the child has been reaped, since a faulty error's own methods may panic.
func handleTRawClose(h *ownership.Handle) (panicked any, err error) {
	defer func() { panicked = recover() }()
	return nil, h.Close()
}

// handleTClassify classifies a result after the reap. A panic in the classification, from a faulty error's own
// methods, becomes the result's text instead of ending the test binary; only the panic's type is named.
func handleTClassify(err error, panicked any) (kind string) {
	if panicked != nil {
		return fmt.Sprintf("panicked with a %T", panicked)
	}
	defer func() {
		if p := recover(); p != nil {
			kind = fmt.Sprintf("classifying the error panicked with a %T", p)
		}
	}()
	return handleTMatch(err)
}

// handleTDone is one handle operation a live row made: which, whether the handle was open when it was made, and its
// raw result.
type handleTDone struct {
	op   string
	open bool
	err  error
}

// handleTJudgeExpired judges the results of a row whose held child ended by its own lifetime, after the reap. An
// expiry explains only a process that is gone; it cannot close a handle or fail a release. So:
//
//   - an open that answered as the row wants, or answered os.ErrProcessDone, is left undecided; anything else fails;
//   - a terminate on an open handle that answered nil is left undecided when the trace holds the delivery its backend
//     requires for it (it raced the child's own exit), and fails when it does not; os.ErrProcessDone is left
//     undecided; any other error, os.ErrClosed among them, fails;
//   - a terminate on a closed handle owes os.ErrClosed and no delivery, expired or not;
//   - a close, and the sequence's final close, owe nil;
//   - a successful delivery through any backend beyond one per terminate on the open handle fails: a closed handle
//     that signals by number is a delivery even where the backend is a pidfd.
//
// required is the delivery the backend requires; deliveries are the successful deliveries of every backend the guard
// can forward. It returns the failures. A row with none is undecided, and the caller may try one fresh child.
func handleTJudgeExpired(done []handleTDone, want []string, finalClose string, trace []string, required string, deliveries []string) []string {
	var failures []string
	requiredCount, anyCount := 0, 0
	for _, call := range trace {
		if call == required {
			requiredCount++
		}
		if slices.Contains(deliveries, call) {
			anyCount++
		}
	}
	openTerminates := 0
	for i, d := range done {
		kind := handleTClassify(d.err, nil)
		switch {
		case d.op == "terminate" && d.open:
			openTerminates++
			switch kind {
			case "nil":
				if requiredCount < openTerminates {
					failures = append(failures, fmt.Sprintf("a terminate on the open handle answered nil but the guard traced no %q for it", required))
				}
			case "os.ErrProcessDone":
			default:
				failures = append(failures, fmt.Sprintf("a terminate on the open handle answered %s, which an expiry does not explain", kind))
			}
		case d.op == "terminate":
			if kind != "os.ErrClosed" {
				failures = append(failures, fmt.Sprintf("a terminate on the closed handle answered %s, not os.ErrClosed", kind))
			}
		case kind == want[i]:
		case d.op == "open" && kind == "os.ErrProcessDone":
		default:
			failures = append(failures, fmt.Sprintf("%s answered %s, not %s", d.op, kind, want[i]))
		}
	}
	if anyCount > openTerminates {
		failures = append(failures, fmt.Sprintf("the guard traced %d deliveries through any backend for %d terminates on the open handle", anyCount, openTerminates))
	}
	if finalClose != "nil" && finalClose != "no handle" {
		failures = append(failures, fmt.Sprintf("the final close answered %s, not nil", finalClose))
	}
	return failures
}

// handleTRunLive runs one live row on a fresh held child and ends it, on every path out of it, through one deferred
// sequence in this goroutine:
//
//  1. the row's handle operations have ended, their errors kept raw;
//  2. the child's exit is observed without reaping it;
//  3. the handle is closed, its result kept raw;
//  4. the guard stops forwarding and its trace is taken; a child that ignored both its ends is killed only now;
//  5. the child's input is closed and the child is reaped.
//
// Steps 4 and 5 are deferred inside the sequence itself, so they run even when an earlier step panics, and step 5 even
// when step 4 does. Every result is classified only after step 5. When the preflight failed, its one report stands and
// nothing else is compared. When the child ended by its own lifetime, handleTJudgeExpired first fails every result the
// expiry does not explain; only a row it leaves undecided reports expired, and the caller may then try one fresh
// child.
func handleTRunLive(t *testing.T, tt handleTLiveRow) (expired bool) {
	c := handleTStartHeld(t)
	var (
		h          *ownership.Handle
		g          *ownership.HandleTGuard
		backend    string
		preflight  bool
		terminated bool
		done       []handleTDone
		handleOpen bool
	)
	ops, want, exit := tt.ops, tt.want, tt.exit
	defer func() {
		var (
			exited     bool
			closeErr   error
			closePanic any
			trace      []string
			waitErr    error
		)
		func() {
			defer func() {
				defer func() {
					// Step 5: only now is the child reaped. Its input is closed first, so a child still running ends
					// itself.
					_ = c.stdin.Close()
					waitErr = c.cmd.Wait()
					_ = c.out.Close()
				}()
				// Step 4: the guard stops forwarding; from here on nothing it is asked reaches a process.
				if g != nil {
					trace = g.Stop()
				}
				// A child that ignored its input's end and its own lifetime is killed here, after the guard stopped
				// and before the reap, in this goroutine.
				if !exited {
					_ = c.cmd.Process.Kill()
					_ = c.observeExit()
				}
			}()
			// Step 2: the exit, observed without reaping. A row that keeps the input open looks first with it open,
			// so that only the terminate can have ended the child.
			if tt.ending != "signal" || !terminated || backend == "EPERM" {
				_ = c.stdin.Close()
			}
			exited = c.observeExit()
			if !exited {
				_ = c.stdin.Close()
				exited = c.observeExit()
			}
			// Step 3: the handle's Close, while the number is still the child's.
			if h != nil {
				closePanic, closeErr = handleTRawClose(h)
			}
		}()
		// Everything below runs after the reap and reaches no process.
		ended := handleTEnded(waitErr)
		if g != nil {
			t.Logf("the guard traced, backend %q: %q", backend, trace)
		}
		switch {
		case !preflight:
			return
		case ended == "lifetime":
			// Every result the expiry does not explain fails here, before any retry is allowed.
			closed := "no handle"
			if h != nil {
				closed = handleTClassify(closeErr, closePanic)
			}
			failures := handleTJudgeExpired(done, want, closed, slices.Clone(trace), handleTDelivery[backend], handleTDeliveries)
			for _, f := range failures {
				t.Errorf("the held child ended by its own lifetime, backend %q, and %s", backend, f)
			}
			expired = len(failures) == 0
			return
		}
		if !exited {
			t.Errorf("the held child had not exited within its bounds and was killed before the reap; output: %s", c.output.String())
		}
		got := make([]string, 0, len(done)+2)
		for _, d := range done {
			got = append(got, handleTClassify(d.err, nil))
		}
		closed, wantClose := "no handle", "no handle"
		if h != nil {
			closed = handleTClassify(closeErr, closePanic)
		}
		if backend != "EPERM" {
			wantClose = "nil"
		}
		got = append(got, "close: "+closed, "exit: "+ended)
		if diff := gocmp.Diff(append(slices.Clone(want), "close: "+wantClose, "exit: "+exit), got); diff != "" {
			t.Errorf("results of %v, backend %q (-want +got):\n%s\nchild output: %s", ops, backend, diff, c.output.String())
		}
		if diff := gocmp.Diff(tt.trace[backend], trace); diff != "" {
			t.Errorf("the guard's trace, backend %q (-want +got):\n%s", backend, diff)
		}
	}()
	pid := c.pid(t)
	var err error
	backend, err = ownership.HandleTFixture(pid)
	if err != nil {
		t.Fatalf("live evidence unavailable on this host (a fixture capability, not a handle failure): %v", err)
	}
	if backend == "EPERM" {
		ops, want, exit = tt.refusedOps, tt.refusedWant, tt.refusedExit
	}
	g = ownership.HandleTHoldChild(t, pid)
	preflight = true
operations:
	for _, s := range ops {
		switch s {
		case "open":
			var err error
			h, err = handleTOpenRaw(t, pid)
			done = append(done, handleTDone{op: s, err: err})
			handleOpen = h != nil
			if h != nil && err != nil {
				t.Fatalf("OpenHandle(%d) returned a handle beside an error", pid)
			}
			if err != nil {
				// An open that failed ends the row's operations; the verdict after the reap decides it.
				break operations
			}
		case "terminate":
			if h == nil {
				t.Fatalf("no handle to terminate through after %d operations", len(done))
			}
			err := noPanic(t, "Handle.Terminate", h.Terminate)
			terminated = terminated || err == nil
			done = append(done, handleTDone{op: s, open: handleOpen, err: err})
		case "close":
			if h == nil {
				t.Fatalf("no handle to close after %d operations", len(done))
			}
			done = append(done, handleTDone{op: s, open: handleOpen, err: noPanic(t, "Handle.Close", h.Close)})
			handleOpen = false
		}
	}
	return false
}
