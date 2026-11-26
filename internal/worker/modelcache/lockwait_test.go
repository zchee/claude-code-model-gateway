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

package modelcache_test

import (
	"bytes"
	"runtime"
	"testing"
)

const (
	// modelcacheFunc prefixes the package's functions as a goroutine dump names them.
	modelcacheFunc = "github.com/zchee/claude-code-model-gateway/internal/worker/modelcache."
	// mutexLockFunc is the frame of a sync.Mutex acquisition, as the pinned toolchain prints it.
	mutexLockFunc = "sync.(*Mutex).Lock"
	// mutexLockState is the scheduler wait state of a goroutine parked in a sync.Mutex acquisition.
	mutexLockState = "sync.Mutex.Lock"
	// maxLockWaitPolls bounds one wait for a goroutine to return or park on the lock. A goroutine that
	// reaches the lock parks within a few scheduler yields; each poll stops the world for one dump.
	maxLockWaitPolls = 20_000
	// maxDumpBytes bounds the goroutine dump buffer.
	maxDumpBytes = 16 << 20
)

// mutexWait names the one place a goroutine of a test parks on a lock: entry is the test's own entry
// function, which tells that goroutine apart from every other one, and blockedIn is the production
// function whose sync.Mutex acquisition it waits in.
//
// The match is tied to the synchronization the production code uses now: blockedIn must acquire a
// sync.Mutex itself, directly. When the production code changes how it serializes these callers
// (another lock, a channel, the acquisition moved into a helper), the mutexWait values and this
// matcher must be updated together with it; until then the gate fails with the last dump instead of
// passing.
type mutexWait struct {
	entry     string
	blockedIn string
}

// awaitReturnOrMutexWait waits until done closes or until the goroutine named by w is seen parked in
// its sync.Mutex acquisition, and reports whether done closed. One exact match is enough: the
// goroutine's wait state is sync.Mutex.Lock and the frame directly above blockedIn is the mutex
// acquisition itself, so no other lock and no lock taken in passing can match. A goroutine waiting on
// a sync.Mutex is not durably blocked in a synctest bubble, so neither synctest.Wait nor the bubble's
// clock can see this; the poll yields instead of sleeping and is bounded by count, which works inside
// a bubble and outside one alike. When neither happens within maxLockWaitPolls polls the test fails
// with the last dump.
func awaitReturnOrMutexWait(t *testing.T, done <-chan struct{}, w mutexWait) bool {
	t.Helper()
	buf := make([]byte, 64<<10)
	var dump []byte
	for range maxLockWaitPolls {
		select {
		case <-done:
			return true
		default:
		}
		dump, buf = goroutineDump(buf)
		if parkedOnMutex(dump, w) {
			return false
		}
		runtime.Gosched()
	}
	t.Fatalf("after %d polls the goroutine entered at %s neither returned nor waited on the sync.Mutex acquired in %s; last goroutine dump:\n%s",
		maxLockWaitPolls, w.entry, w.blockedIn, dump)
	return false
}

// goroutineDump fills buf with the stack of every goroutine and returns the dump and the buffer to
// reuse. The buffer doubles while the dump does not fit, up to maxDumpBytes; a dump still longer than
// that is returned truncated.
func goroutineDump(buf []byte) (dump, reuse []byte) {
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) || len(buf) >= maxDumpBytes {
			return buf[:n], buf
		}
		buf = make([]byte, min(2*len(buf), maxDumpBytes))
	}
}

// parkedOnMutex reports whether one goroutine in dump matches w exactly.
func parkedOnMutex(dump []byte, w mutexWait) bool {
	for g := range bytes.SplitSeq(dump, []byte("\n\n")) {
		header, stack, _ := bytes.Cut(g, []byte("\n"))
		if waitState(header) != mutexLockState {
			continue
		}
		frames := frameFuncs(stack)
		hasEntry, blocked := false, false
		for i, f := range frames {
			switch {
			case f == w.entry:
				hasEntry = true
			case f == w.blockedIn && i > 0 && frames[i-1] == mutexLockFunc:
				blocked = true
			}
		}
		if hasEntry && blocked {
			return true
		}
	}
	return false
}

// waitState returns the state of a goroutine header such as
// "goroutine 7 [sync.Mutex.Lock, synctest bubble 1]:", without the annotations after the first comma.
func waitState(header []byte) string {
	_, rest, ok := bytes.Cut(header, []byte(" ["))
	if !ok {
		return ""
	}
	end := bytes.IndexAny(rest, ",]")
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

// frameFuncs returns the function of every frame of one goroutine's stack, innermost first. A frame is
// a function line, "name(args)", followed by a tab-indented location line; inlined calls print as
// frames of their own with "(...)" arguments.
func frameFuncs(stack []byte) []string {
	var funcs []string
	for line := range bytes.Lines(stack) {
		line = bytes.TrimRight(line, "\n")
		if len(line) == 0 || line[0] == '\t' || bytes.HasPrefix(line, []byte("created by ")) {
			continue
		}
		if open := bytes.LastIndexByte(line, '('); open > 0 && line[len(line)-1] == ')' {
			funcs = append(funcs, string(line[:open]))
		}
	}
	return funcs
}
