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

//go:build process && unix

package worker_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
)

// staleSocket leaves a socket file at path that no process listens on, as a worker that died without closing does.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}

// reclaimRound releases every starter at once against the same stale socket and returns the listeners they obtained.
func reclaimRound(path string, starters int) []*net.UnixListener {
	start := make(chan struct{})
	results := make([]net.Listener, starters)
	var wg sync.WaitGroup
	for i := range starters {
		wg.Go(func() {
			<-start
			// A refusal is an acceptable answer for every starter but one; only listeners are judged.
			results[i], _ = worker.ListenUnix(path)
		})
	}
	close(start)
	wg.Wait()
	var owners []*net.UnixListener
	for _, ln := range results {
		if ln == nil {
			continue
		}
		owners = append(owners, ln.(*net.UnixListener))
	}
	return owners
}

// reachedBy reports whether a connection to path arrives at ln. The dial completes into the listen queue of whatever
// socket the path names, so the owner accepts at once; the deadline only bounds a listener the path no longer names.
func reachedBy(ctx context.Context, path string, ln *net.UnixListener) (bool, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	if err := ln.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return false, err
	}
	accepted, err := ln.Accept()
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return false, nil
		}
		return false, err
	}
	return true, accepted.Close()
}

// TestStaleSocketReclaimByConcurrentStarters starts several workers' socket binding at the same moment against one
// stale socket file. Probing, unlinking and binding are separate steps, so a starter that found the file stale can
// unlink the socket another starter has just bound. Whatever the interleaving, exactly one starter may end up owning
// the path, and the path must lead to that owner's listener; every other starter refuses.
func TestStaleSocketReclaimByConcurrentStarters(t *testing.T) {
	tests := map[string]struct {
		starters int
		rounds   int
		// processes runs each starter as its own process, as separate workers are; otherwise as goroutines.
		processes bool
	}{
		"success: two starters reclaim one stale socket":            {starters: 2, rounds: 2000},
		"success: eight starters reclaim one stale socket":          {starters: 8, rounds: 500},
		"success: two starter processes reclaim one stale socket":   {starters: 2, rounds: 30, processes: true},
		"success: eight starter processes reclaim one stale socket": {starters: 8, rounds: 15, processes: true},
	}
	// A subtest's descriptive name can by itself exceed sockaddr_un.sun_path.
	dir, err := os.MkdirTemp("", "mg-reclaim-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "gw.sock")
			want := reclaimVerdict{Owners: 1, OwnerReached: true}
			for round := range tc.rounds {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				staleSocket(t, path)
				var got reclaimVerdict
				if tc.processes {
					got, err = processRound(t, env, path, tc.starters)
				} else {
					got, err = goroutineRound(t.Context(), path, tc.starters)
				}
				if err != nil {
					t.Fatalf("round %d of %d: %v", round+1, tc.rounds, err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatalf("round %d of %d with %d starters (-want +got):\n%s%s", round+1, tc.rounds, tc.starters, diff, got)
				}
			}
		})
	}
}

// goroutineRound runs one round with the starters as goroutines of this process.
func goroutineRound(ctx context.Context, path string, starters int) (reclaimVerdict, error) {
	owners := reclaimRound(path, starters)
	defer func() {
		for _, ln := range owners {
			_ = ln.Close()
		}
	}()
	got := reclaimVerdict{Owners: len(owners)}
	if len(owners) != 1 {
		return got, nil
	}
	reached, err := reachedBy(ctx, path, owners[0])
	got.OwnerReached = reached
	return got, err
}

// reclaimStarterHelper names the starter of a process round among the package's fixture helpers.
const reclaimStarterHelper = "reclaim-starter"

// TestGatewayFixtureHelper dispatches the fixture helpers this package launches through gatewaytest.
func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{reclaimStarterHelper: reclaimStarter})
}

// reclaimStarter is one starter of a process round, a helper process that runs this test binary again with the
// socket path as its argument. It waits on the shared start barrier, binds the path, reports whether it owns a
// listener together with its process ID, and while it owns one answers every connection with that process ID, so
// the test can tell whose socket the path leads to.
func reclaimStarter(t testing.TB, args []string) {
	if len(args) != 1 {
		t.Fatalf("reclaim starter needs the socket path, got %d arguments", len(args))
	}
	// gatewaytest passes the working directory first; the barrier is the descriptor after it.
	barrier := os.NewFile(4, "start barrier")
	if barrier == nil {
		t.Fatal("missing start barrier")
	}
	if _, err := fmt.Println("ready"); err != nil {
		t.Fatal(err)
	}
	// Every starter holds the read end; the test closes the only write end to release them together.
	_, _ = io.Copy(io.Discard, barrier)
	pid := os.Getpid()
	ln, err := worker.ListenUnix(args[0])
	if err != nil {
		fmt.Printf("refused %d\n", pid)
	} else {
		fmt.Printf("owner %d\n", pid)
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = fmt.Fprintf(conn, "%d\n", pid)
				_ = conn.Close()
			}
		}()
	}
	// Stay alive until the test has judged the round; exit without closing, so no starter unlinks the path.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// startInOwnDirectory runs the package's reclaim starter on name in a working directory of its own and returns that
// directory, the starter's result line, "owner PID" or "refused PID", and join. The starter lives until join, so an
// owner keeps answering connections on name with its process ID. join ends the starter's stdin, reads its remaining
// output and waits for it. If the starter or the scan fails, join reports the output read so far. The test calls join
// before it ends. A test that ends without calling it still has the starter stopped and waited for, but its exit status is
// discarded then: the command runs under the test's context, which is cancelled before cleanup and can kill it.
func startInOwnDirectory(t *testing.T, env gatewaytest.Environment, name string) (dir, result string, join func() error) {
	t.Helper()
	cmd, err := env.HelperCommand(t, reclaimStarterHelper, name)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture runner launches from the sandbox root and the starter then enters this directory.
	dir = cmd.Dir
	release, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = release.Close(), hold.Close() }()
	cmd.ExtraFiles = []*os.File{release}
	stop, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW := io.Pipe()
	cmd.Stdout = outW
	done := make(chan error, 1)
	go func() {
		err := env.RunFixtureWithin(cmd, 20*time.Second)
		_ = outW.CloseWithError(err)
		done <- err
	}()
	lines := bufio.NewScanner(outR)
	join = sync.OnceValue(func() error {
		_ = stop.Close()
		// The output pipe has no buffer: a failing starter blocks on its failure text until it is read.
		var rest []string
		for lines.Scan() {
			rest = append(rest, lines.Text())
		}
		_ = outR.Close() // Release a blocked writer if the scanner stopped before EOF.
		err := <-done
		if scanErr := lines.Err(); scanErr != nil && !errors.Is(err, scanErr) {
			err = errors.Join(err, scanErr)
		}
		if err != nil {
			return fmt.Errorf("the starter on %q failed: %w; its output after the result: %q", name, err, rest)
		}
		return nil
	})
	t.Cleanup(func() { _ = join() })
	if !lines.Scan() || lines.Text() != "ready" {
		t.Fatal(errors.Join(errors.New("the starter did not reach its barrier: "+lines.Text()), lines.Err()))
	}
	// The starter waits for end of file on the barrier; the test holds its only write end.
	_ = hold.Close()
	if !lines.Scan() {
		t.Fatal(errors.Join(errors.New("the starter gave no result"), lines.Err()))
	}
	return dir, lines.Text(), join
}

// processRound runs one round with every starter as its own helper process, released together by closing a shared
// pipe. Each starter runs under gatewaytest's scrubbed environment in its own sandbox directory.
func processRound(t *testing.T, env gatewaytest.Environment, path string, starters int) (reclaimVerdict, error) {
	t.Helper()
	release, hold, err := os.Pipe()
	if err != nil {
		return reclaimVerdict{}, err
	}
	defer func() { _, _ = release.Close(), hold.Close() }()
	type starter struct {
		stop  io.WriteCloser
		out   *io.PipeReader
		lines *bufio.Scanner
		done  chan error
	}
	var started []starter
	defer func() {
		for _, s := range started {
			_ = s.stop.Close()
		}
		for _, s := range started {
			<-s.done
			_ = s.out.Close()
		}
	}()
	for range starters {
		cmd, err := env.HelperCommand(t, reclaimStarterHelper, path)
		if err != nil {
			return reclaimVerdict{}, err
		}
		cmd.ExtraFiles = []*os.File{release}
		stop, err := cmd.StdinPipe()
		if err != nil {
			return reclaimVerdict{}, err
		}
		outR, outW := io.Pipe()
		cmd.Stdout = outW
		s := starter{stop: stop, out: outR, lines: bufio.NewScanner(outR), done: make(chan error, 1)}
		go func() {
			err := env.RunFixtureWithin(cmd, 20*time.Second)
			_ = outW.CloseWithError(err)
			s.done <- err
		}()
		started = append(started, s)
	}
	for i, s := range started {
		if !s.lines.Scan() || s.lines.Text() != "ready" {
			return reclaimVerdict{}, errors.Join(fmt.Errorf("starter %d did not reach the barrier: %q", i, s.lines.Text()), s.lines.Err())
		}
	}
	_ = release.Close()
	_ = hold.Close()
	var owners []string
	for i, s := range started {
		if !s.lines.Scan() {
			return reclaimVerdict{}, errors.Join(fmt.Errorf("starter %d gave no result", i), s.lines.Err())
		}
		result, pid, ok := strings.Cut(s.lines.Text(), " ")
		if !ok || (result != "owner" && result != "refused") {
			return reclaimVerdict{}, fmt.Errorf("starter %d gave an unknown result %q", i, s.lines.Text())
		}
		if result == "owner" {
			owners = append(owners, pid)
		}
	}
	got := reclaimVerdict{Owners: len(owners)}
	if len(owners) != 1 {
		return got, nil
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", path)
	if err != nil {
		// The path names nothing although a starter believes it owns it.
		return got, nil
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return got, err
	}
	answer := bufio.NewScanner(conn)
	got.OwnerReached = answer.Scan() && answer.Text() == owners[0]
	return got, nil
}

// reclaimVerdict is the state of the path after every starter of one round has returned.
type reclaimVerdict struct {
	Owners       int
	OwnerReached bool
}

// String explains a verdict that is not the expected single reachable owner.
func (v reclaimVerdict) String() string {
	switch {
	case v.Owners > 1:
		return fmt.Sprintf("%d starters each believe they own the path; at most one of their sockets is still named by it, and closing any of them unlinks the path", v.Owners)
	case v.Owners == 0:
		return "no starter reclaimed the stale socket"
	case !v.OwnerReached:
		return "the only owner's listener is not the socket the path leads to"
	}
	return ""
}
