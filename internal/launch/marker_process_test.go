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

package launch_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/launch"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

const (
	// markTAdmissionHelper names the child that stands in for a worker admission.
	markTAdmissionHelper = "stop-marker-admission"
	// markTFenceFile is the start fence's lock file in the state root, which the fence creates and never removes.
	markTFenceFile = "worker-start.lock"
	// markTChildLimit bounds the child's whole life; when it ends, the child's output ends with it, and every wait
	// for one of its notices returns.
	markTChildLimit = 28 * time.Second
	// markTWatchdog only aborts a publication that never returns or never reaches the fence. It is longer than the
	// child's limit, so a publication that waits on the child ends first; it is never evidence of exclusion.
	markTWatchdog = 45 * time.Second
)

// The child's modes: it checks the stop marker when the parent sends C, or at once when it has the fence.
const (
	markTCheckOnCommand = "check-on-command"
	markTCheckOnAcquire = "check-on-acquire"
)

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{markTAdmissionHelper: markTAdmit})
}

// markTWatchedContext is a caller's context whose Done channel reports its first use. The landed fence consults Done
// only while it waits after a lock attempt found another holder; it checks Err, never Done, before and after its
// attempts. So the first use of Done is the moment one real Acquire found the fence held by another holder.
type markTWatchedContext struct {
	context.Context
	once    sync.Once
	waiting func()
}

func (c *markTWatchedContext) Done() <-chan struct{} {
	c.once.Do(c.waiting)
	return c.Context.Done()
}

// markTAcquireWatched takes fence with ONE real Acquire under ctx; waiting runs once, when that Acquire waits for
// another holder.
func markTAcquireWatched(ctx context.Context, fence *lifecycle.StartFence, waiting func()) (func() error, error) {
	return fence.Acquire(&markTWatchedContext{Context: ctx, waiting: waiting})
}

// markTFenceHeld reports whether some holder has the start fence of state, by one nonblocking exclusive flock on a
// fresh open file description of the fence file: EWOULDBLOCK says it is held, and a lock it does take is given back
// at once. A fresh description is excluded also by a holder in this process.
func markTFenceHeld(state string) (bool, error) {
	path := filepath.Join(state, markTFenceFile)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(fd) }()
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return true, nil
		case err != nil:
			return false, &os.PathError{Op: "flock", Path: path, Err: err}
		}
		return false, unix.Flock(fd, unix.LOCK_UN)
	}
}

// markTAdmit stands in for a worker admission in the state root given as its first argument, as the supervisor
// admits a worker start: it takes the start fence, then looks for the stop marker. It reports "ready" and waits for
// A. It then takes the fence with one real Acquire, reports "contended" if that Acquire had to wait for another
// holder, and "held" once it has the fence. In the mode check-on-command it waits for C before it looks; in the mode
// check-on-acquire it looks at once. It reports "refused" when the marker is present and "admitted" when it is not,
// releases the fence, reports "released", and ends at the next byte or at the end of its input.
func markTAdmit(t testing.TB, args []string) {
	t.Helper()
	if len(args) != 2 || (args[1] != markTCheckOnCommand && args[1] != markTCheckOnAcquire) {
		t.Fatalf("unexpected helper arguments %q", args)
	}
	state, mode := args[0], args[1]
	fence, err := lifecycle.NewStartFence(state)
	if err != nil {
		t.Fatalf("child NewStartFence: %v", err)
	}
	notice := func(line string) {
		if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
			t.Fatal(err)
		}
	}
	command := func(want byte) {
		var b [1]byte
		if _, err := io.ReadFull(os.Stdin, b[:]); err != nil {
			t.Fatalf("waiting for the parent's %c: %v", want, err)
		}
		if b[0] != want {
			t.Fatalf("parent sent %q, want %q", b[0], want)
		}
	}
	notice("ready")
	command('A')
	release, err := markTAcquireWatched(t.Context(), fence, func() { notice("contended") })
	if err != nil {
		t.Fatalf("child Acquire: %v", err)
	}
	notice("held")
	if mode == markTCheckOnCommand {
		command('C')
	}
	present, err := lifecycle.Files{StateDir: state}.StopMarkerPresent()
	switch {
	case err != nil:
		_ = release()
		t.Fatalf("child StopMarkerPresent: %v", err)
	case present:
		notice("refused")
	default:
		notice("admitted")
	}
	if err := release(); err != nil {
		t.Fatalf("child release: %v", err)
	}
	notice("released")
	var b [1]byte
	if _, err := io.ReadFull(os.Stdin, b[:]); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// markTNotices are the lines of the child that the protocol reads; every other line goes to the transcript only.
var markTNotices = map[string]bool{"ready": true, "contended": true, "held": true, "admitted": true, "refused": true, "released": true}

// markTChild is a running stand-in admission with its notices, in the order the child printed them.
type markTChild struct {
	input *os.File
	lines chan string
	done  chan struct{}
	err   error

	mu     sync.Mutex
	output bytes.Buffer
}

func markTStart(t *testing.T, env gatewaytest.Environment, state, mode string) *markTChild {
	t.Helper()
	cmd, err := env.HelperCommand(t, markTAdmissionHelper, state, mode)
	if err != nil {
		t.Fatal(err)
	}
	in, send, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	recv, out := io.Pipe()
	c := &markTChild{input: send, lines: make(chan string, 16), done: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	runDone := make(chan struct{})
	go func() {
		c.err = env.RunFixtureWithin(cmd, markTChildLimit)
		_ = out.Close()
		_ = in.Close()
		close(runDone)
	}()
	go func() {
		defer close(c.done)
		scanner := bufio.NewScanner(recv)
		for scanner.Scan() {
			line := scanner.Text()
			c.mu.Lock()
			fmt.Fprintln(&c.output, line)
			c.mu.Unlock()
			if markTNotices[line] {
				select {
				case c.lines <- line:
				default:
				}
			}
		}
		scanErr := scanner.Err()
		_ = recv.Close()
		<-runDone
		c.err = errors.Join(c.err, scanErr)
		close(c.lines)
	}()
	t.Cleanup(func() { _ = send.Close(); <-c.done })
	return c
}

func (c *markTChild) transcript() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.output.String()
}

// receive returns the child's next notice, or "" when the child's output ended or ctx ended first. The child's
// limit bounds the wait. It does not end the test, so the publication callback may use it.
func (c *markTChild) receive(ctx context.Context) string {
	select {
	case line, ok := <-c.lines:
		if ok {
			return line
		}
	case <-ctx.Done():
	}
	return ""
}

func (c *markTChild) expect(t *testing.T, want string) {
	t.Helper()
	if got := c.receive(t.Context()); got != want {
		t.Fatalf("child notice %q, want %q; child output:\n%s", got, want, c.transcript())
	}
}

func (c *markTChild) send(command byte) error {
	_, err := c.input.Write([]byte{command})
	return err
}

// finish lets the child end after its last notice and requires that it ended cleanly.
func (c *markTChild) finish(t *testing.T) {
	t.Helper()
	if err := c.send('X'); err != nil {
		t.Fatal(err)
	}
	<-c.done
	if c.err != nil {
		t.Fatalf("stand-in admission: %v\n%s", c.err, c.transcript())
	}
}

// markTOutcome is what one fenced publication returned, or the panic it raised.
type markTOutcome struct {
	err      error
	panicked any
}

// markTRig is one fenced publication against the real fence of a temporary state root, with its stand-in admission.
// contended is closed when the controller's one real Acquire waits for another holder. atRelease records, for every
// call of the controller's release, whether the stop marker existed at that moment, before the real release ran.
type markTRig struct {
	state     string
	child     *markTChild
	contended chan struct{}
	published atomic.Int32

	mu        sync.Mutex
	atRelease []string
}

func markTNewRig(t *testing.T, mode string) *markTRig {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, ".claude", "model-gateway")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return &markTRig{state: state, child: markTStart(t, gatewaytest.NewEnv(home, nil, nil, nil), state, mode), contended: make(chan struct{})}
}

func (r *markTRig) markerAtRelease() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.atRelease
}

// publishAsync runs the publication through PublishUnderFence on its own goroutine with the real fence. The acquire
// function takes the fence with one real Acquire that closes contended when it waits for another holder, and wraps
// the release so that the marker's presence is recorded before the real release runs. publish counts itself, runs
// during, and then writes the stop marker. A panic is recovered as the outcome. The test's cleanup cancels the
// publication's context and waits for the goroutine, bounded by the watchdog; one that does not return is reported.
func (r *markTRig) publishAsync(t *testing.T, during func() error) <-chan markTOutcome {
	t.Helper()
	fence, err := lifecycle.NewStartFence(r.state)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var waited sync.Once
	acquire := func(ctx context.Context) (func() error, error) {
		release, err := markTAcquireWatched(ctx, fence, func() { waited.Do(func() { close(r.contended) }) })
		if err != nil {
			return nil, err
		}
		return func() error {
			state := "absent"
			switch present, err := (lifecycle.Files{StateDir: r.state}).StopMarkerPresent(); {
			case err != nil:
				state = "error: " + err.Error()
			case present:
				state = "present"
			}
			r.mu.Lock()
			r.atRelease = append(r.atRelease, state)
			r.mu.Unlock()
			return release()
		}, nil
	}
	publish := func() error {
		r.published.Add(1)
		if err := during(); err != nil {
			return err
		}
		return lifecycle.Files{StateDir: r.state}.Stop()
	}
	outcomes := make(chan markTOutcome, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				outcomes <- markTOutcome{panicked: p}
			}
		}()
		outcomes <- markTOutcome{err: launch.PublishUnderFence(ctx, acquire, publish)}
	}()
	t.Cleanup(func() {
		cancel()
		watchdog := time.NewTimer(markTWatchdog)
		defer watchdog.Stop()
		select {
		case <-done:
		case <-watchdog.C:
			t.Error("PublishUnderFence did not return after its context was canceled")
		}
	})
	return outcomes
}

// heldDuringPublication is the check every publication makes first: the fence must be held while it runs.
func (r *markTRig) heldDuringPublication(t *testing.T) error {
	held, err := markTFenceHeld(r.state)
	switch {
	case err != nil:
		t.Errorf("inspecting the start fence during the publication: %v", err)
		return err
	case !held:
		t.Error("the stop marker's publication ran while the start fence was free")
		return errors.New("published without the start fence")
	}
	return nil
}

func (r *markTRig) outcome(t *testing.T, outcomes <-chan markTOutcome) {
	t.Helper()
	watchdog := time.NewTimer(markTWatchdog)
	defer watchdog.Stop()
	select {
	case o := <-outcomes:
		if o.panicked != nil {
			t.Fatalf("PublishUnderFence panicked: %v", o.panicked)
		}
		if o.err != nil {
			t.Fatalf("PublishUnderFence() = %v, want nil; child output:\n%s", o.err, r.child.transcript())
		}
	case <-watchdog.C:
		t.Fatalf("PublishUnderFence did not return; child output:\n%s", r.child.transcript())
	}
	if got := r.published.Load(); got != 1 {
		t.Fatalf("the publication ran %d times, want 1", got)
	}
	if diff := gocmp.Diff([]string{"present"}, r.markerAtRelease()); diff != "" {
		t.Fatalf("the stop marker at each call of the controller's release (-want +got):\n%s", diff)
	}
	present, err := lifecycle.Files{StateDir: r.state}.StopMarkerPresent()
	if err != nil || !present {
		t.Fatalf("StopMarkerPresent() after the publication = %t, %v; want true, nil", present, err)
	}
}

// markTStandInFirst is the ordering in which the stand-in holds the fence when the controller tries to take it:
// the stand-in looks for the marker only after the controller's Acquire has found it holding the fence, and is
// admitted, because the controller cannot publish before the stand-in gives the fence up.
func markTStandInFirst(t *testing.T, r *markTRig) {
	r.child.expect(t, "ready")
	if err := r.child.send('A'); err != nil {
		t.Fatal(err)
	}
	r.child.expect(t, "held")
	if held, err := markTFenceHeld(r.state); err != nil || !held {
		t.Fatalf("after the stand-in reported held, the fence is held = %t, %v; want true, nil", held, err)
	}
	outcomes := r.publishAsync(t, func() error { return r.heldDuringPublication(t) })
	watchdog := time.NewTimer(markTWatchdog)
	defer watchdog.Stop()
	select {
	case <-r.contended:
	case o := <-outcomes:
		t.Fatalf("PublishUnderFence returned (%v, panic %v) before its Acquire waited for the stand-in's fence", o.err, o.panicked)
	case <-watchdog.C:
		t.Fatal("PublishUnderFence's Acquire never waited for the fence the stand-in holds")
	}
	// The controller's Acquire has found the stand-in's fence and waits; a publication outside the fence has run.
	if err := r.child.send('C'); err != nil {
		t.Fatal(err)
	}
	r.child.expect(t, "admitted")
	r.child.expect(t, "released")
	r.outcome(t, outcomes)
	r.child.finish(t)
}

// markTControllerFirst is the ordering in which the controller has the fence first: inside the held publication the
// stand-in's one real Acquire waits for the controller's fence, and once it has the fence the stand-in looks for the
// marker at once and refuses.
func markTControllerFirst(t *testing.T, r *markTRig) {
	r.child.expect(t, "ready")
	outcomes := r.publishAsync(t, func() error {
		if err := r.heldDuringPublication(t); err != nil {
			return err
		}
		if err := r.child.send('A'); err != nil {
			t.Errorf("starting the stand-in's attempt: %v", err)
			return err
		}
		if got := r.child.receive(t.Context()); got != "contended" {
			t.Errorf("during the publication the stand-in reported %q, want \"contended\"", got)
			return errors.New("the stand-in's Acquire did not wait for the controller's fence")
		}
		return nil
	})
	r.outcome(t, outcomes)
	r.child.expect(t, "held")
	r.child.expect(t, "refused")
	r.child.expect(t, "released")
	r.child.finish(t)
}

func TestStopMarkerFenceOrderings(t *testing.T) {
	tests := map[string]struct {
		mode string
		run  func(*testing.T, *markTRig)
	}{
		"success: a stand-in that holds the fence first is admitted before the marker is published": {
			mode: markTCheckOnCommand,
			run:  markTStandInFirst,
		},
		"success: a stand-in whose Acquire waits during the publication is refused once it has the fence": {
			mode: markTCheckOnAcquire,
			run:  markTControllerFirst,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tt.run(t, markTNewRig(t, tt.mode))
		})
	}
}
