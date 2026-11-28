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
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// evidenceBudget bounds one collection of evidence. It is far shorter than a fixture child's lifetime, so that the
// children a failure message describes are still alive while it is read, and a failing test is not held up by it.
const evidenceBudget = 5 * time.Second

const (
	// evidenceBatch is how many entries of one directory the descriptor scan reads at a time, and how many table rows
	// are parsed between two checks of the collection's context.
	evidenceBatch = 256
	// The most rows, holders of one row, denied processes and problems a failure message prints; the rest are
	// counted as omitted.
	shownRows     = 32
	shownHolders  = 16
	shownPIDs     = 32
	shownProblems = 16
)

// tcpStates names the kernel's hexadecimal TCP states of /proc/net/tcp.
var tcpStates = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2", "06": "TIME_WAIT",
	"07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING", "0C": "NEW_SYN_RECV",
}

// portRow is one row of a kernel TCP table whose local port is the port in question, in any state.
type portRow struct {
	Table   string
	Local   string
	Inode   uint64
	State   string
	Holders []int
}

// holderFacts is what the proc filesystem tells about one process without its arguments: its command name and its
// parent. A field that could not be read holds the reason instead.
type holderFacts struct {
	Command       string
	Parent        int
	ParentCommand string
}

// portEvidence is what a process table root shows for one port at one moment: every row of both TCP tables on that
// port number, every process holding a descriptor for one of their sockets, and the descriptor tables that could not
// be listed, so that a holder hidden in one of them is visible as a gap. Everything in it is read when it is
// collected, under the collection's context; rendering it reads nothing.
type portEvidence struct {
	Port uint16
	Self int
	Rows []portRow
	// Facts are the command names and parents read for this test process, the holders and, when a listening row has
	// no visible holder, the first shownPIDs denied processes.
	Facts map[int]holderFacts
	// Unlisted counts the descriptor tables and descriptors that could not be read, by reason.
	Unlisted map[string]int
	// Unexpected counts the descriptor tables and descriptors that failed for a reason other than a lack of
	// permission or a process or descriptor that went away, which the lookup itself skips; each makes the evidence
	// partial.
	Unexpected int
	// Denied are the processes whose descriptor table could not be listed for lack of permission.
	Denied []int
	// Unread names the TCP tables that could not be read: the rows they hold were not observed.
	Unread []string
	// Partial says why the collection stopped before it had read everything, or is empty when it did not.
	Partial string
	// Problems are reads that failed and tables or rows that could not be decoded; each makes the evidence partial.
	Problems []string
	// finished, when set, is told of each read just after it has returned.
	finished func(op string)
}

// readPortEvidence reads the evidence for port from /proc while parent lasts, and within evidenceBudget.
func readPortEvidence(parent context.Context, port uint16) portEvidence {
	ctx, cancel := context.WithTimeout(parent, evidenceBudget)
	defer cancel()
	return readPortEvidenceAt(ctx, "/proc", port, nil)
}

// readPortEvidenceAt reads the evidence for port from the process table root while ctx lasts, telling finished, when
// it is set, of each read just after it has returned. ctx is checked before each open, directory read, link read and file
// read, and between batches of parsed rows; a read already in progress when ctx ends is not interrupted, so the bound
// is that no new read starts after ctx has ended. It never fails: a read that fails, or a collection cut short, is
// recorded in the evidence, since the evidence is only ever wanted for a failure message or beside a result. The
// holders' command names and parents are read from /proc whatever the root, since a staged root holds only the rows
// and descriptor links the lookup reads.
func readPortEvidenceAt(ctx context.Context, root string, port uint16, finished func(op string)) portEvidence {
	ev := portEvidence{Port: port, Self: os.Getpid(), Facts: map[int]holderFacts{}, Unlisted: map[string]int{}, finished: finished}
	for _, table := range []string{"tcp", "tcp6"} {
		path := filepath.Join(root, "net", table)
		if !ev.before(ctx, "read "+path) {
			return ev
		}
		data, err := os.ReadFile(path)
		ev.after("read " + path)
		if table == "tcp6" && errors.Is(err, os.ErrNotExist) {
			// The lookup treats a kernel without IPv6 as one without IPv6 rows.
			continue
		}
		if err != nil {
			ev.Unread = append(ev.Unread, table)
			ev.Problems = append(ev.Problems, fmt.Sprintf("read %s: %v", path, err))
			continue
		}
		header, body, ok := strings.Cut(string(data), "\n")
		if !ok || !strings.Contains(header, "local_addr") {
			// The lookup refuses a table without its header as inconclusive.
			ev.Problems = append(ev.Problems, fmt.Sprintf("%s has no header line naming the local address: %q", path, header))
			continue
		}
		lines := 0
		for line := range strings.SplitSeq(body, "\n") {
			if lines%evidenceBatch == 0 && ctx.Err() != nil {
				ev.Partial = fmt.Sprintf("collection stopped after %d rows of %s, since %v; no read started after that", lines, path, ctx.Err())
				return ev
			}
			lines++
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			if len(f) < 10 {
				ev.Problems = append(ev.Problems, fmt.Sprintf("short row of %s: %q", table, line))
				continue
			}
			local, rowPort, ok := decodeLocal(f[1])
			if !ok {
				ev.Problems = append(ev.Problems, fmt.Sprintf("undecodable local address in %s: %q", table, f[1]))
				continue
			}
			if rowPort != port {
				continue
			}
			inode, err := strconv.ParseUint(f[9], 10, 64)
			if err != nil {
				ev.Problems = append(ev.Problems, fmt.Sprintf("undecodable inode in %s: %q", table, f[9]))
			}
			state := f[3]
			if name, ok := tcpStates[state]; ok {
				state += " " + name
			}
			ev.Rows = append(ev.Rows, portRow{Table: table, Local: local, Inode: inode, State: state})
		}
	}
	targets := make(map[string][]int, len(ev.Rows))
	for i, row := range ev.Rows {
		if row.Inode != 0 {
			name := fmt.Sprintf("socket:[%d]", row.Inode)
			targets[name] = append(targets[name], i)
		}
	}
	if len(targets) > 0 && !ev.scanHolders(ctx, root, targets) {
		return ev
	}
	ev.collectFacts(ctx)
	return ev
}

// before reports whether a new read op may start under ctx. Once ctx has ended it records why the collection
// stopped, and refuses this and every later read.
func (ev *portEvidence) before(ctx context.Context, op string) bool {
	if err := ctx.Err(); err != nil {
		if ev.Partial == "" {
			ev.Partial = fmt.Sprintf("collection stopped before %s, since %v; no read started after that", op, err)
		}
		return false
	}
	return true
}

// after tells finished, when it is set, that the read op has returned.
func (ev *portEvidence) after(op string) {
	if ev.finished != nil {
		ev.finished(op)
	}
}

// decodeLocal turns the local address field of a TCP table row into text. The kernel prints each 32-bit word of the
// address in host byte order.
func decodeLocal(field string) (string, uint16, bool) {
	hexAddr, hexPort, ok := strings.Cut(field, ":")
	raw, err := hex.DecodeString(hexAddr)
	port, perr := strconv.ParseUint(hexPort, 16, 16)
	if !ok || err != nil || perr != nil || len(raw) != 4 && len(raw) != 16 {
		return "", 0, false
	}
	b := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(b[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr, uint16(port)).String(), uint16(port), true
}

// unlisted counts a descriptor table or a descriptor that could not be read, by reason. A lack of permission and a
// process or descriptor that went away are skipped by the lookup too; any other reason is also counted as unexpected.
func (ev *portEvidence) unlisted(what string, err error) {
	switch {
	case errors.Is(err, os.ErrPermission):
		ev.Unlisted[what+": permission denied"]++
		return
	case errors.Is(err, os.ErrNotExist):
		ev.Unlisted[what+": gone before it was read"]++
		return
	}
	ev.Unexpected++
	// The kernel's reason without the path, so that one reason of many processes is counted once.
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		ev.Unlisted[what+": "+pe.Op+": "+pe.Err.Error()]++
		return
	}
	ev.Unlisted[what+": "+err.Error()]++
}

// readBatches calls each with the entries of dir, evidenceBatch at a time, until the entries end or each returns
// false, checking ctx before the open and before each directory read. It returns whether the collection stopped and
// the error of opening or reading dir.
func (ev *portEvidence) readBatches(ctx context.Context, dir string, each func([]os.DirEntry) bool) (stopped bool, err error) {
	if !ev.before(ctx, "open "+dir) {
		return true, nil
	}
	f, err := os.Open(dir)
	ev.after("open " + dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	for {
		if !ev.before(ctx, "read directory "+dir) {
			return true, nil
		}
		ents, err := f.ReadDir(evidenceBatch)
		ev.after("read directory " + dir)
		if len(ents) > 0 && !each(ents) {
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// scanHolders finds every process under root with a descriptor for one of the sockets in targets while ctx lasts,
// checking it before each link read. It reports whether the scan finished; a scan cut short is marked partial.
func (ev *portEvidence) scanHolders(ctx context.Context, root string, targets map[string][]int) bool {
	scanned := 0
	stopped, err := ev.readBatches(ctx, root, func(ents []os.DirEntry) bool {
		for _, ent := range ents {
			pid, err := strconv.Atoi(ent.Name())
			if err != nil || pid <= 0 {
				continue
			}
			scanned++
			fdPath := filepath.Join(root, ent.Name(), "fd")
			stopped, err := ev.readBatches(ctx, fdPath, func(fds []os.DirEntry) bool {
				for _, fd := range fds {
					link := filepath.Join(fdPath, fd.Name())
					if !ev.before(ctx, "readlink "+link) {
						return false
					}
					target, err := os.Readlink(link)
					ev.after("readlink " + link)
					if err != nil {
						ev.unlisted("descriptor", err)
						continue
					}
					for _, i := range targets[target] {
						ev.Rows[i].Holders = append(ev.Rows[i].Holders, pid)
					}
				}
				return true
			})
			if err != nil {
				ev.unlisted("descriptor table", err)
				if errors.Is(err, os.ErrPermission) {
					ev.Denied = append(ev.Denied, pid)
				}
			}
			if stopped {
				return false
			}
		}
		return true
	})
	if err != nil {
		ev.Problems = append(ev.Problems, fmt.Sprintf("list %s: %v", root, err))
	}
	for i := range ev.Rows {
		ev.Rows[i].Holders = slices.Compact(slices.Sorted(slices.Values(ev.Rows[i].Holders)))
	}
	if stopped {
		ev.Partial += fmt.Sprintf("; %d process entries of %s were scanned, and holders of the others are missing", scanned, root)
	}
	return !stopped
}

// collectFacts reads the command names and parents of this test process and of every holder, and of the first
// shownPIDs denied processes when a listening row has no visible holder, since only then can a denied table hide the
// missing holder. It stops when ctx ends, before the next read.
func (ev *portEvidence) collectFacts(ctx context.Context) {
	pids := []int{ev.Self}
	for _, r := range ev.Rows {
		pids = append(pids, r.Holders...)
	}
	if ev.holderless() {
		pids = append(pids, ev.Denied[:min(len(ev.Denied), shownPIDs)]...)
	}
	for _, pid := range pids {
		if _, done := ev.Facts[pid]; done {
			continue
		}
		facts, ok := ev.processFacts(ctx, pid)
		if !ok {
			return
		}
		ev.Facts[pid] = facts
	}
}

// holderless reports whether a listening row has no visible holder.
func (ev *portEvidence) holderless() bool {
	return slices.ContainsFunc(ev.Rows, func(r portRow) bool { return strings.HasPrefix(r.State, "0A") && len(r.Holders) == 0 })
}

// incomplete says why the evidence cannot stand for everything the root held, or is empty when it can.
func (ev portEvidence) incomplete() string {
	var why []string
	if len(ev.Unread) > 0 {
		why = append(why, "unread table "+strings.Join(ev.Unread, ", "))
	}
	if n := len(ev.Problems); n > 0 {
		why = append(why, fmt.Sprintf("%d read or decoding problems", n))
	}
	if ev.Unexpected > 0 {
		why = append(why, fmt.Sprintf("%d unexpected descriptor errors", ev.Unexpected))
	}
	if ev.Partial != "" {
		why = append(why, "collection cut short by its context")
	}
	return strings.Join(why, "; ")
}

// processFacts reads the command name and the parent of pid, and the parent's command name, checking ctx before each
// read. It reports false when ctx ended first. It never reads the arguments of any process.
func (ev *portEvidence) processFacts(ctx context.Context, pid int) (holderFacts, bool) {
	command, ok := ev.commandName(ctx, pid)
	if !ok {
		return holderFacts{}, false
	}
	facts := holderFacts{Command: command, Parent: -1}
	path := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	if !ev.before(ctx, "read "+path) {
		return holderFacts{}, false
	}
	stat, err := os.ReadFile(path)
	ev.after("read " + path)
	if err != nil {
		facts.ParentCommand = "stat unreadable: " + err.Error()
		return facts, true
	}
	ppid, _, err := statFields(pid, stat)
	if err != nil {
		facts.ParentCommand = "stat undecodable"
		return facts, true
	}
	parentCommand, ok := ev.commandName(ctx, ppid)
	if !ok {
		return holderFacts{}, false
	}
	facts.Parent, facts.ParentCommand = ppid, parentCommand
	return facts, true
}

// commandName reads the command name of pid once ctx allows the read; it reports false when ctx ended first.
func (ev *portEvidence) commandName(ctx context.Context, pid int) (string, bool) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "comm")
	if !ev.before(ctx, "read "+path) {
		return "", false
	}
	comm, err := os.ReadFile(path)
	ev.after("read " + path)
	if err != nil {
		return "comm unreadable: " + err.Error(), true
	}
	return strings.TrimSuffix(string(comm), "\n"), true
}

// classified is the lookup's own classification of the evidence's listening rows.
func (ev portEvidence) classified() Listener {
	var rows []socketEvidence
	for _, r := range ev.Rows {
		if strings.HasPrefix(r.State, "0A") {
			rows = append(rows, socketEvidence{port: ev.Port, listening: true, owners: r.Holders})
		}
	}
	return classifyListener(rows, nil, ev.Port)
}

// String renders the evidence for a failure message from what was collected, reading nothing: at most shownRows
// rows, shownHolders holders a row, shownPIDs denied processes, shownProblems problems and shownProblems reasons a
// descriptor could not be read, with a count of what was left out. Partial evidence gets no classification.
func (ev portEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nproc evidence for port %d, read after the lookup returned, so it can miss what the lookup saw; this test process is pid %d (%s):", ev.Port, ev.Self, ev.command(ev.Self))
	incomplete := ev.incomplete()
	if incomplete != "" {
		fmt.Fprintf(&b, "\n  the evidence is partial (%s)", incomplete)
	}
	if ev.Partial != "" {
		b.WriteString("\n  " + ev.Partial)
	}
	if len(ev.Rows) == 0 {
		b.WriteString("\n  no row observed on this port")
		if incomplete != "" {
			b.WriteString(" in what could be read; this is not evidence of absence")
		}
	}
	for _, r := range ev.Rows[:min(len(ev.Rows), shownRows)] {
		fmt.Fprintf(&b, "\n  %-4s %s inode %d state %s holders:", r.Table, r.Local, r.Inode, r.State)
		if len(r.Holders) == 0 {
			b.WriteString(" none visible")
			if ev.Partial != "" {
				b.WriteString(" in the processes scanned")
			}
		}
		for _, pid := range r.Holders[:min(len(r.Holders), shownHolders)] {
			b.WriteString(" " + ev.holder(pid))
		}
		if n := len(r.Holders) - shownHolders; n > 0 {
			fmt.Fprintf(&b, " and %d more holders omitted", n)
		}
	}
	if n := len(ev.Rows) - shownRows; n > 0 {
		fmt.Fprintf(&b, "\n  %d more rows omitted", n)
	}
	if len(ev.Unlisted) > 0 {
		b.WriteString("\n  not read:")
		keys := slices.Sorted(maps.Keys(ev.Unlisted))
		for _, k := range keys[:min(len(keys), shownProblems)] {
			fmt.Fprintf(&b, " %s x%d;", k, ev.Unlisted[k])
		}
		if n := len(keys) - shownProblems; n > 0 {
			fmt.Fprintf(&b, " and %d more reasons omitted", n)
		}
	}
	if ev.holderless() {
		b.WriteString("\n  a listening row has no visible holder; processes whose descriptor table was denied:")
		if len(ev.Denied) == 0 {
			b.WriteString(" none")
		}
		for _, pid := range ev.Denied[:min(len(ev.Denied), shownPIDs)] {
			b.WriteString(" " + ev.holder(pid))
		}
		if n := len(ev.Denied) - shownPIDs; n > 0 {
			fmt.Fprintf(&b, " and %d more omitted", n)
		}
	}
	for _, p := range ev.Problems[:min(len(ev.Problems), shownProblems)] {
		b.WriteString("\n  problem: " + p)
	}
	if n := len(ev.Problems) - shownProblems; n > 0 {
		fmt.Fprintf(&b, "\n  %d more problems omitted", n)
	}
	if incomplete != "" {
		fmt.Fprintf(&b, "\n  the lookup's classification of these listening rows: not derived, since the evidence is partial (%s)", incomplete)
	} else {
		fmt.Fprintf(&b, "\n  the lookup's classification of these listening rows: %+v", ev.classified())
	}
	return b.String()
}

// command is the collected command name of pid, or says that it was not read.
func (ev portEvidence) command(pid int) string {
	if f, ok := ev.Facts[pid]; ok {
		return f.Command
	}
	return "command not read"
}

// relation is how pid relates to this test process by the collected facts: ", this test process", ", a child of
// this test process", or empty. Fixture children run this test's own executable, so only the parent tells them from
// a parallel test of another package.
func (ev portEvidence) relation(pid int) string {
	switch f, ok := ev.Facts[pid]; {
	case pid == ev.Self:
		return ", this test process"
	case ok && f.Parent == ev.Self:
		return ", a child of this test process"
	}
	return ""
}

// holder renders one process with its collected command name and parent and its relation to this test process.
func (ev portEvidence) holder(pid int) string {
	f, ok := ev.Facts[pid]
	if !ok {
		return fmt.Sprintf("%d (command and parent not read%s)", pid, ev.relation(pid))
	}
	return fmt.Sprintf("%d (command %q, parent %d %q%s)", pid, f.Command, f.Parent, f.ParentCommand, ev.relation(pid))
}

// TestPortEvidenceIncomplete decides that evidence which could not be read whole says so and yields no
// classification: a table that could not be read or has no header, a row that could not be decoded, an unexpected
// descriptor error, and a context that had ended or ends between two reads, after which no read starts; and that the
// rendered text stays within its limits with a count of what was left out.
func TestPortEvidenceIncomplete(t *testing.T) {
	const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	const row = "   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242 1\n"
	// tables returns a root whose TCP table is content.
	tables := func(t *testing.T, content string) string {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "net"), 0o700); err != nil {
			t.Fatal(err)
		}
		putFile(t, filepath.Join(root, "net", "tcp"), content)
		return root
	}
	// holding returns a root whose TCP table has row and whose process 1 has descriptor 3; link makes it a link to
	// the row's socket, and otherwise it is a plain file, whose link cannot be read.
	holding := func(t *testing.T, link bool) string {
		root := tables(t, header+row)
		fd := filepath.Join(root, "1", "fd")
		if err := os.MkdirAll(fd, 0o700); err != nil {
			t.Fatal(err)
		}
		if !link {
			putFile(t, filepath.Join(fd, "3"), "")
			return root
		}
		if err := os.Symlink("socket:[4242]", filepath.Join(fd, "3")); err != nil {
			t.Fatal(err)
		}
		return root
	}
	budget := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), evidenceBudget)
		t.Cleanup(cancel)
		return ctx
	}
	tests := map[string]struct {
		evidence func(t *testing.T) portEvidence
		want     []string
		absent   []string
	}{
		"error: an unreadable table yields no observed row and no classification": {
			evidence: func(t *testing.T) portEvidence { return readPortEvidenceAt(budget(t), t.TempDir(), 8080, nil) },
			want: []string{
				"the evidence is partial (unread table tcp",
				"no row observed on this port in what could be read; this is not evidence of absence",
				"problem: read ",
				"classification of these listening rows: not derived",
			},
			absent: []string{"{State:"},
		},
		"error: a table without its header yields no classification": {
			evidence: func(t *testing.T) portEvidence { return readPortEvidenceAt(budget(t), tables(t, ""), 8080, nil) },
			want: []string{
				"the evidence is partial (1 read or decoding problems)",
				"has no header line naming the local address",
				"classification of these listening rows: not derived",
			},
			absent: []string{"{State:"},
		},
		"error: an undecodable row yields no classification": {
			evidence: func(t *testing.T) portEvidence {
				return readPortEvidenceAt(budget(t), tables(t, header+"   0: short row\n"), 8080, nil)
			},
			want: []string{
				"the evidence is partial (1 read or decoding problems)",
				"no row observed on this port in what could be read; this is not evidence of absence",
				"problem: short row of tcp",
				"classification of these listening rows: not derived",
			},
			absent: []string{"{State:"},
		},
		"error: an unexpected descriptor error yields no classification": {
			evidence: func(t *testing.T) portEvidence { return readPortEvidenceAt(budget(t), holding(t, false), 8080, nil) },
			want: []string{
				"the evidence is partial (1 unexpected descriptor errors)",
				"not read: descriptor: readlink: invalid argument x1;",
				"classification of these listening rows: not derived",
			},
			absent: []string{"{State:"},
		},
		"error: a context that had ended starts no read": {
			evidence: func(t *testing.T) portEvidence {
				ctx, cancel := context.WithDeadline(t.Context(), time.Now())
				t.Cleanup(cancel)
				var finished []string
				ev := readPortEvidenceAt(ctx, tables(t, header+row), 8080, func(op string) { finished = append(finished, op) })
				if len(finished) != 0 {
					t.Errorf("reads ran after the context had ended: %q", finished)
				}
				return ev
			},
			want: []string{
				"the evidence is partial (collection cut short by its context)",
				"collection stopped before read ",
				"since context deadline exceeded; no read started after that",
				"this test process is pid " + strconv.Itoa(os.Getpid()) + " (command not read)",
				"no row observed on this port in what could be read; this is not evidence of absence",
				"classification of these listening rows: not derived",
			},
			absent: []string{"inode 4242", "{State:"},
		},
		"error: a context that ends between two reads starts no further read": {
			evidence: func(t *testing.T) portEvidence {
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				root := holding(t, true)
				fd := filepath.Join(root, "1", "fd")
				// The context ends once the fourth read, the directory read of the root, has returned; the open of
				// process 1's descriptor table that would follow it must be refused.
				var finished []string
				ev := readPortEvidenceAt(ctx, root, 8080, func(op string) {
					finished = append(finished, op)
					if len(finished) == 4 {
						cancel()
					}
				})
				want := []string{"read " + filepath.Join(root, "net", "tcp"), "read " + filepath.Join(root, "net", "tcp6"), "open " + root, "read directory " + root}
				if diff := gocmp.Diff(want, finished); diff != "" {
					t.Errorf("reads that ran (-want +got):\n%s", diff)
				}
				if !strings.Contains(ev.Partial, "collection stopped before open "+fd+", since context canceled") {
					t.Errorf("partial marker %q does not name the refused read", ev.Partial)
				}
				return ev
			},
			want: []string{
				"the evidence is partial (collection cut short by its context)",
				"; 1 process entries of ",
				"tcp  127.0.0.1:8080 inode 4242 state 0A LISTEN holders: none visible in the processes scanned",
				"classification of these listening rows: not derived",
			},
			absent: []string{"{State:"},
		},
		"success: the rendering is capped and counts what it left out": {
			evidence: func(*testing.T) portEvidence {
				ev := portEvidence{Port: 8080, Self: os.Getpid(), Facts: map[int]holderFacts{}, Unlisted: map[string]int{}}
				for i := range shownRows + 8 {
					ev.Rows = append(ev.Rows, portRow{Table: "tcp", Local: "127.0.0.1:8080", Inode: uint64(i + 1), State: "0A LISTEN"})
				}
				// The process numbers lie above the kernel's largest, so none of them is this test process.
				for pid := range shownHolders + 4 {
					ev.Rows[0].Holders = append(ev.Rows[0].Holders, pid+5000100)
				}
				for pid := range shownPIDs + 8 {
					ev.Denied = append(ev.Denied, pid+5001000)
				}
				for i := range shownProblems + 4 {
					ev.Problems = append(ev.Problems, "problem "+strconv.Itoa(i))
				}
				ev.unlisted("descriptor table", &os.PathError{Op: "open", Path: "/proc/77/fd", Err: errors.New("i/o fault")})
				ev.unlisted("descriptor table", &os.PathError{Op: "open", Path: "/proc/78/fd", Err: errors.New("i/o fault")})
				for i := range shownProblems + 3 {
					ev.Unlisted[fmt.Sprintf("reason %02d", i)]++
				}
				return ev
			},
			want: []string{
				"and 4 more holders omitted",
				"8 more rows omitted",
				"5001031 (command and parent not read) and 8 more omitted",
				"4 more problems omitted",
				"not read: descriptor table: open: i/o fault x2;",
				"and 4 more reasons omitted",
			},
			absent: []string{"inode 33 ", "5001032 (", "problem 16", "/proc/77/fd", "reason 15"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			text := tt.evidence(t).String()
			for _, s := range tt.want {
				if !strings.Contains(text, s) {
					t.Errorf("evidence does not say %q:%s", s, text)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(text, s) {
					t.Errorf("evidence says %q, which it must leave out:%s", s, text)
				}
			}
		})
	}
}
