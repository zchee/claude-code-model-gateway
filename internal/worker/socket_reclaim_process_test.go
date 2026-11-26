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
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

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
	if path := os.Getenv(reclaimStarterKey); path != "" {
		// This process is one starter of a process round, not the test itself.
		reclaimStarter(t, path)
		return
	}
	tests := map[string]struct {
		starters int
		rounds   int
		// processes runs each starter as its own process, as separate workers are; otherwise as goroutines.
		processes bool
	}{
		"success: two starters reclaim one stale socket":            {starters: 2, rounds: 2000},
		"success: eight starters reclaim one stale socket":          {starters: 8, rounds: 500},
		"success: two starter processes reclaim one stale socket":   {starters: 2, rounds: 150, processes: true},
		"success: eight starter processes reclaim one stale socket": {starters: 8, rounds: 60, processes: true},
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
					got, err = processRound(t, path, tc.starters)
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

// reclaimStarterKey names the socket path a starter process binds; it is set only for the starters of a process round.
const reclaimStarterKey = "RECLAIM_STARTER_PATH"

// reclaimStarter is one starter process of a process round, which runs this test binary again. It waits on the shared
// start barrier, binds the path, and while it owns a listener answers every connection with its process ID, so the
// test can tell whose socket the path leads to.
func reclaimStarter(t *testing.T, path string) {
	t.Helper()
	barrier := os.NewFile(3, "start barrier")
	if barrier == nil {
		t.Fatal("missing start barrier")
	}
	if _, err := fmt.Println("ready"); err != nil {
		t.Fatal(err)
	}
	// Every starter holds the read end; the test closes the only write end to release them together.
	_, _ = io.Copy(io.Discard, barrier)
	ln, err := worker.ListenUnix(path)
	if err != nil {
		fmt.Println("refused")
	} else {
		fmt.Println("owner")
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = fmt.Fprintf(conn, "%d\n", os.Getpid())
				_ = conn.Close()
			}
		}()
	}
	// Stay alive until the test has judged the round; exit without closing, so no starter unlinks the path.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// processRound runs one round with every starter as its own process, released together by closing a shared pipe.
func processRound(t *testing.T, path string, starters int) (reclaimVerdict, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		return reclaimVerdict{}, err
	}
	release, hold, err := os.Pipe()
	if err != nil {
		return reclaimVerdict{}, err
	}
	defer func() { _, _ = release.Close(), hold.Close() }()
	type starter struct {
		cmd   *exec.Cmd
		stop  io.WriteCloser
		lines *bufio.Scanner
	}
	var started []starter
	defer func() {
		for _, s := range started {
			_ = s.stop.Close()
		}
		for _, s := range started {
			_ = s.cmd.Wait()
		}
	}()
	for range starters {
		cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestStaleSocketReclaimByConcurrentStarters$")
		cmd.Env = append(os.Environ(), reclaimStarterKey+"="+path)
		cmd.ExtraFiles = []*os.File{release}
		stop, err := cmd.StdinPipe()
		if err != nil {
			return reclaimVerdict{}, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return reclaimVerdict{}, err
		}
		if err := cmd.Start(); err != nil {
			return reclaimVerdict{}, err
		}
		started = append(started, starter{cmd: cmd, stop: stop, lines: bufio.NewScanner(out)})
	}
	for _, s := range started {
		if !s.lines.Scan() || s.lines.Text() != "ready" {
			return reclaimVerdict{}, fmt.Errorf("starter %d did not reach the barrier: %q", s.cmd.Process.Pid, s.lines.Text())
		}
	}
	_ = release.Close()
	_ = hold.Close()
	var owners []int
	for _, s := range started {
		if !s.lines.Scan() {
			return reclaimVerdict{}, fmt.Errorf("starter %d gave no result", s.cmd.Process.Pid)
		}
		if s.lines.Text() == "owner" {
			owners = append(owners, s.cmd.Process.Pid)
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
	got.OwnerReached = answer.Scan() && answer.Text() == strconv.Itoa(owners[0])
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
