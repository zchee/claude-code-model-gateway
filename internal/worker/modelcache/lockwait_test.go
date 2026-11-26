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
	"runtime"
	"strings"
)

const (
	// lockWaitPolls is how many consecutive polls must see a goroutine waiting on a lock before it
	// counts as serialized, so a lock taken and released in passing is not mistaken for one.
	lockWaitPolls = 50
	// maxLockWaitPolls bounds one wait for a goroutine to return or wait on a lock.
	maxLockWaitPolls = 200_000
)

// lockWaitStates are the scheduler wait states of a goroutine acquiring a sync.Mutex or sync.RWMutex,
// as a goroutine dump prints them.
var lockWaitStates = []string{"[sync.Mutex.Lock", "[sync.RWMutex.Lock", "[sync.RWMutex.RLock"}

// goroutineDump returns the stack of every goroutine, one entry per goroutine.
func goroutineDump() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Split(string(buf[:n]), "\n\n")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitingOnLock reports whether a goroutine whose stack holds frame is waiting to acquire a lock.
func waitingOnLock(frame string) bool {
	for _, g := range goroutineDump() {
		if !strings.Contains(g, frame) {
			continue
		}
		header, _, _ := strings.Cut(g, "\n")
		for _, state := range lockWaitStates {
			if strings.Contains(header, state) {
				return true
			}
		}
	}
	return false
}

// doneOrLockWait waits until done closes, or until the goroutine whose stack holds frame has been
// seen waiting on a lock for lockWaitPolls consecutive polls. It reports whether done closed, and
// ok is false when neither happened within maxLockWaitPolls polls.
//
// A goroutine waiting on a sync.Mutex is not durably blocked in a synctest bubble, so neither
// synctest.Wait nor the bubble's clock can tell that it is serialized behind another caller. The
// scheduler's own wait state can, and polling yields instead of sleeping, so this works inside a
// bubble and outside one alike.
func doneOrLockWait(done <-chan struct{}, frame string) (returned, ok bool) {
	streak := 0
	for range maxLockWaitPolls {
		select {
		case <-done:
			return true, true
		default:
		}
		if waitingOnLock(frame) {
			if streak++; streak >= lockWaitPolls {
				return false, true
			}
		} else {
			streak = 0
		}
		runtime.Gosched()
	}
	return false, false
}

// onCurrentStack reports whether the calling goroutine's stack holds frame.
func onCurrentStack(frame string) bool {
	buf := make([]byte, 64<<10)
	return strings.Contains(string(buf[:runtime.Stack(buf, false)]), frame)
}
