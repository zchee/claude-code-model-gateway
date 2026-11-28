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

//go:build linux && process

package procinfo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	rand "math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/shirou/gopsutil/v4/common"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// startedInput is a fixture child's standard input. exec starts the goroutine that reads it only after the process
// has started, so its first Read marks the point after which this process no longer reads the descriptors it handed
// to the child, and may close them. Input ends when release is closed.
type startedInput struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (in *startedInput) Read([]byte) (int, error) {
	in.once.Do(func() { close(in.started) })
	<-in.release
	return 0, io.EOF
}

// heldBy starts a fixture child through gatewaytest that holds files as its descriptors 5 and up, and returns its
// process number once the child has reported it and the start of the child is complete in this process: from then on
// the caller may close its own copies of files, and the child keeps holding them. The child lives until cleanup.
func heldBy(t *testing.T, files ...*os.File) int {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	cmd, err := env.HelperCommand(t, "hold")
	if err != nil {
		t.Fatal(err)
	}
	report, reportOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	in := &startedInput{started: make(chan struct{}), release: make(chan struct{})}
	// The output is read only after the runner has returned, so that the read never races the runner's copier.
	output := new(bytes.Buffer)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, output, output
	// The launcher puts the working directory at descriptor 3; the report is 4.
	cmd.ExtraFiles = append([]*os.File{reportOut}, files...)
	done := make(chan error, 1)
	go func() {
		err := env.RunFixtureWithin(cmd, fixtureLifetime)
		_ = reportOut.Close()
		done <- err
	}()
	finish := sync.OnceValue(func() error { close(in.release); return <-done })
	t.Cleanup(func() { _ = finish(); _ = report.Close() })
	if err := report.SetReadDeadline(time.Now().Add(fixtureLifetime)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(report).ReadString('\n')
	if err != nil {
		t.Fatalf("fixture child did not report its pid: %v; run: %v; output: %s", err, finish(), output)
	}
	pid, err := strconv.Atoi(strings.TrimSuffix(line, "\n"))
	if err != nil || pid <= 0 {
		t.Fatalf("fixture child reported %q: %v; run: %v; output: %s", line, err, finish(), output)
	}
	select {
	case <-in.started:
	case <-time.After(fixtureLifetime):
		t.Fatalf("the runner did not start feeding the child's input; run: %v; output: %s", finish(), output)
	}
	return pid
}

// sameNumberPair listens on a random IPv4 loopback address and on the IPv6 loopback with one port number. A number
// already taken on the IPv6 loopback is given up for another; any other refusal there means the host lacks the IPv6
// loopback, which this test needs.
func sameNumberPair(t *testing.T, host string) (v4, v6 *net.TCPListener) {
	t.Helper()
	var lc net.ListenConfig
	for range 32 {
		v4 = listenerOn(t, lc, host, 0)
		port := v4.Addr().(*net.TCPAddr).Port
		l, err := lc.Listen(t.Context(), "tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			_ = v4.Close()
			continue
		}
		if err != nil {
			_ = v4.Close()
			t.Fatalf("test infrastructure: cannot listen on the IPv6 loopback [::1]:%d, which this test needs: %v", port, err)
		}
		t.Cleanup(func() { _, _ = v4.Close(), l.Close() })
		return v4, l.(*net.TCPListener)
	}
	t.Fatal("test infrastructure: no port number was free on both loopbacks in 32 tries")
	return nil, nil
}

// listenerInode returns the inode of the socket behind l, which is what names its row in the TCP tables.
func listenerInode(t *testing.T, l *net.TCPListener) uint64 {
	t.Helper()
	raw, err := l.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	var statErr error
	if err := raw.Control(func(fd uintptr) { statErr = unix.Fstat(int(fd), &st) }); err != nil {
		t.Fatal(err)
	}
	if statErr != nil {
		t.Fatal(statErr)
	}
	return st.Ino
}

// stagedProc returns a process table root that holds, of this host, only the TCP table rows of the sockets inodes
// and the descriptor tables of pids: the live tables' header lines and those rows are copied, and each pid's
// descriptor table is a link to its live one. No other socket of the host, on any port number, is in it. Each socket
// must show in exactly one row of the copies; the tables are read again when one shows twice or not at all, since a
// table that changes while it is read can repeat or drop a row.
func stagedProc(t *testing.T, inodes []uint64, pids ...int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	var tables map[string]string
	var seen map[uint64]int
	exactlyOnce := func() bool {
		return len(seen) == len(inodes) && !slices.ContainsFunc(inodes, func(i uint64) bool { return seen[i] != 1 })
	}
	for range 8 {
		tables, seen = map[string]string{}, map[uint64]int{}
		for _, table := range []string{"tcp", "tcp6"} {
			data, err := os.ReadFile(filepath.Join("/proc/net", table))
			if table == "tcp6" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatalf("test infrastructure: cannot read the live TCP table to stage it: %v", err)
			}
			header, body, _ := strings.Cut(string(data), "\n")
			var b strings.Builder
			b.WriteString(header + "\n")
			for line := range strings.SplitSeq(body, "\n") {
				f := strings.Fields(line)
				if len(f) < 10 {
					continue
				}
				if inode, err := strconv.ParseUint(f[9], 10, 64); err == nil && slices.Contains(inodes, inode) {
					seen[inode]++
					b.WriteString(line + "\n")
				}
			}
			tables[table] = b.String()
		}
		if exactlyOnce() {
			break
		}
	}
	if !exactlyOnce() {
		t.Fatalf("test infrastructure: in 8 reads of the live TCP tables the staged sockets %v did not each show in exactly one row: %v", inodes, seen)
	}
	for table, content := range tables {
		putFile(t, filepath.Join(root, "net", table), content)
	}
	for _, pid := range pids {
		name := strconv.Itoa(pid)
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("/proc", name, "fd"), filepath.Join(root, name, "fd")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// listeningRow is the part of a listening row of the TCP tables a case decides.
type listeningRow struct {
	Table   string
	Local   string
	Holders []int
}

func listeningRows(ev portEvidence) []listeningRow {
	var rows []listeningRow
	for _, r := range ev.Rows {
		if strings.HasPrefix(r.State, "0A") {
			rows = append(rows, listeningRow{r.Table, r.Local, r.Holders})
		}
	}
	return rows
}

// stagedCase is what a case of TestListenerHolderCharacterisation opened: the port, the lookup's answer today, the
// listening rows the staged root must show, and the sockets and processes the staged root holds.
type stagedCase struct {
	port   uint16
	want   Listener
	rows   []listeningRow
	inodes []uint64
	pids   []int
}

// TestListenerHolderCharacterisation pins what the Linux listener lookup answers today when the port number of a
// listener this test opened shows more than one listening socket or more than one holder. Each case stands for one
// suspected cause of the venue failures in which the lookup could not name the single holder of a test's own port:
//   - "another process" stands for a foreign listener that got the same port number on another address or family:
//     the lookup matches rows by port number alone, so the foreign socket's holder is counted with the test's own;
//   - "this process" stands for the same collision when every matching socket has one holder;
//   - "inherited" stands for one socket seen in two processes, as a child of a parallel test holds its parent's
//     descriptors between fork and exec.
//
// The cases describe what the lookup returns today. It decides by port number alone, as the original gateway's
// lookup does. They are not a statement of what is desirable: a lookup that answers differently for one of these
// cases needs a ruling first, not a change of this test to match.
//
// The lookup runs against a staged process table root that holds only the rows of the sockets a case opened and the
// descriptor tables of their holders, so that another listener of this host on the same number cannot change its
// answer; a binding of a random IPv4 loopback address and the IPv6 loopback does not keep the number from other
// addresses. The rows and descriptor links are the live ones, so the case still decides what the lookup makes of
// real kernel rows. The lookup's answer is asserted in every run. The cases also assert that the evidence the failure
// messages print names the same rows and holders, read from the same root, when that evidence was collected whole
// within its budget, and that it says it is partial when it was not.
func TestListenerHolderCharacterisation(t *testing.T) {
	self := os.Getpid()
	tests := map[string]struct {
		stage func(t *testing.T, host string) stagedCase
	}{
		"success: a same-number listener on the IPv6 loopback held by another process is ambiguous": {
			stage: func(t *testing.T, host string) stagedCase {
				v4, v6 := sameNumberPair(t, host)
				inodes := []uint64{listenerInode(t, v4), listenerInode(t, v6)}
				f, err := v6.File()
				if err != nil {
					t.Fatal(err)
				}
				// Closing v6 does not close this duplicate, so a failed start of the child must not leave it open.
				t.Cleanup(func() { _ = f.Close() })
				child := heldBy(t, f)
				// From here only the child holds the IPv6 socket.
				if err := errors.Join(f.Close(), v6.Close()); err != nil {
					t.Fatal(err)
				}
				port := uint16(v4.Addr().(*net.TCPAddr).Port)
				return stagedCase{port, Listener{State: ListenerAmbiguous}, []listeningRow{
					{"tcp", netip.AddrPortFrom(netip.MustParseAddr(host), port).String(), []int{self}},
					{"tcp6", netip.AddrPortFrom(netip.IPv6Loopback(), port).String(), []int{child}},
				}, inodes, []int{self, child}}
			},
		},
		"success: a same-number listener on the IPv6 loopback held by this process is identified": {
			stage: func(t *testing.T, host string) stagedCase {
				v4, v6 := sameNumberPair(t, host)
				port := uint16(v4.Addr().(*net.TCPAddr).Port)
				return stagedCase{port, Listener{ListenerIdentified, self}, []listeningRow{
					{"tcp", netip.AddrPortFrom(netip.MustParseAddr(host), port).String(), []int{self}},
					{"tcp6", netip.AddrPortFrom(netip.IPv6Loopback(), port).String(), []int{self}},
				}, []uint64{listenerInode(t, v4), listenerInode(t, v6)}, []int{self}}
			},
		},
		"success: one listening socket inherited by a child is ambiguous": {
			stage: func(t *testing.T, host string) stagedCase {
				v4 := listenerOn(t, net.ListenConfig{}, host, 0)
				t.Cleanup(func() { _ = v4.Close() })
				f, err := v4.File()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				child := heldBy(t, f)
				port := uint16(v4.Addr().(*net.TCPAddr).Port)
				return stagedCase{port, Listener{State: ListenerAmbiguous}, []listeningRow{
					{"tcp", netip.AddrPortFrom(netip.MustParseAddr(host), port).String(), slices.Sorted(slices.Values([]int{self, child}))},
				}, []uint64{listenerInode(t, v4)}, []int{self, child}}
			},
		},
	}
	// The staged scan is small, but a busy host can stall single reads of the proc filesystem past the default budget.
	p := New(func(key string) (string, bool) { return "30000", key == "CODEX_GATEWAY_PROBE_TIMEOUT_MS" })
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			host := netip.AddrFrom4([4]byte{127, byte(1 + rand.IntN(254)), byte(rand.Uint32()), byte(1 + rand.IntN(254))}).String()
			s := tt.stage(t, host)
			root := stagedProc(t, s.inodes, s.pids...)
			got := p.Listener(context.WithValue(t.Context(), common.EnvKey, common.EnvMap{common.HostProcEnvKey: root}), s.port)
			evCtx, evCancel := context.WithTimeout(t.Context(), evidenceBudget)
			defer evCancel()
			ev := readPortEvidenceAt(evCtx, root, s.port, nil)
			if got != s.want {
				t.Errorf("Listener(%d) on the staged root = %+v; want %+v%s", s.port, got, s.want, ev)
			}
			// The evidence is read independently of the lookup, under its own budget: its rows are compared only when
			// it is complete, and otherwise it must say that it is partial.
			switch text := ev.String(); {
			case ev.incomplete() != "":
				if !strings.Contains(text, "\n  the evidence is partial (") {
					t.Errorf("evidence of port %d is incomplete but does not say so:%s", s.port, text)
				}
			default:
				if diff := gocmp.Diff(s.rows, listeningRows(ev)); diff != "" {
					t.Errorf("listening rows of port %d in the staged root (-want +got):\n%s%s", s.port, diff, text)
				}
			}
		})
	}
}
