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

package serveworker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// drainFrame is the encoded drain message msg.
func drainFrame(t *testing.T, msg workerlink.DrainMessage) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := workerlink.EncodeDrain(&b, msg); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// peerOutcome is what the supervisor's end of the exchange saw.
type peerOutcome struct {
	// Notice is the first frame the peer read, and NoticeErr its read error, both empty when the peer read nothing.
	Notice    workerlink.DrainMessage
	NoticeErr string
	// Extra is what the worker wrote after its notice, for a peer that keeps reading until the worker's end closes.
	Extra string
}

// exchangeResult is the worker's side of one exchange: how long it took in the bubble's time and whether its error
// has one of the wanted causes.
type exchangeResult struct {
	Elapsed time.Duration
	Cause   bool
}

// runDrainExchange calls drainExchange and returns what it panicked with, if it did, so that the caller can join the
// bubble's other goroutines before it fails the case: a bubble whose main goroutine ends while they still wait aborts
// the whole test binary.
func runDrainExchange(ctx context.Context, conn net.Conn, generation workerlink.Generation) (panicked any, err error) {
	defer func() { panicked = recover() }()
	return nil, drainExchange(ctx, conn, generation)
}

// TestDrainExchangeSharesOneBudget checks the worker's side of the drain exchange in the bubble's time, over an
// in-memory pipe whose writes complete only when the other end reads them. The notice's write and the wait for the
// answer share one budget of five seconds counted from the exchange's start: a peer that reads the notice four seconds
// late and never answers ends the exchange at five seconds, where a reply timer started after the write would end it
// at nine. The first answer frame alone decides: the retirement of the worker's own generation ends the exchange at
// once whatever follows it, and any other frame fails it at once without waiting for another. A context that ends
// first, while the notice is being written or while the answer is awaited, ends the exchange with the context's
// error. The worker writes its notice once.
func TestDrainExchangeSharesOneBudget(t *testing.T) {
	// budget is the contract's five seconds, stated here independently of the package's constant.
	const budget = 5 * time.Second
	// abortAfter is beyond any outcome the contract allows, the nine seconds of a restarted reply timer included: an
	// exchange still running then has no deadline, and the case closes both ends so that it fails instead of
	// deadlocking the bubble.
	const abortAfter = 10 * time.Second
	const generation = workerlink.Generation("exchange-generation")
	notice := workerlink.DrainMessage{Type: workerlink.Draining, Generation: generation}
	retired := drainFrame(t, workerlink.DrainMessage{Type: workerlink.Retired, Generation: generation})
	wrongDirection := drainFrame(t, notice)
	otherGeneration := drainFrame(t, workerlink.DrainMessage{Type: workerlink.Retired, Generation: "another-generation"})
	malformed := []byte("not a drain frame\n")
	cutOff := retired[:len(retired)/2]
	timedOut := []error{os.ErrDeadlineExceeded, context.DeadlineExceeded}
	tests := map[string]struct {
		// readAfter is when the peer starts reading the notice; never makes it read nothing.
		readAfter time.Duration
		never     bool
		// answer is written in one write after the notice was read; nil writes none.
		answer []byte
		// closeAfterAnswer closes the peer's end once the answer was written, or after the notice without an answer.
		closeAfterAnswer bool
		// cancelAfter ends the exchange's context after this long when set.
		cancelAfter time.Duration
		// wantErr lists the causes one of which the error must have; nil wants success.
		wantErr     []error
		wantElapsed time.Duration
		wantPeer    peerOutcome
	}{
		"success: the retirement read at once ends the exchange at once, whatever follows it": {
			answer:      slices.Concat(retired, []byte("bytes after the answer frame\n")),
			wantElapsed: 0,
			wantPeer:    peerOutcome{Notice: notice},
		},
		"error: a wrong first frame fails at once although a valid one follows": {
			answer:      slices.Concat(otherGeneration, retired),
			wantErr:     []error{errUnexpectedDrainAnswer},
			wantElapsed: 0,
			wantPeer:    peerOutcome{Notice: notice},
		},
		"error: a notice read four seconds late and never answered ends at five seconds": {
			readAfter:   4 * time.Second,
			wantErr:     timedOut,
			wantElapsed: budget,
			wantPeer:    peerOutcome{Notice: notice},
		},
		// The peer reads nothing, so no notice and no later write can be observed.
		"error: a notice never read ends at five seconds": {
			never:       true,
			wantErr:     timedOut,
			wantElapsed: budget,
		},
		"error: an answer in the wrong direction fails at once": {
			answer:      wrongDirection,
			wantErr:     []error{errUnexpectedDrainAnswer},
			wantElapsed: 0,
			wantPeer:    peerOutcome{Notice: notice},
		},
		"error: an answer for another generation fails at once": {
			answer:      otherGeneration,
			wantErr:     []error{errUnexpectedDrainAnswer},
			wantElapsed: 0,
			wantPeer:    peerOutcome{Notice: notice},
		},
		"error: a malformed answer fails at once": {
			answer:      malformed,
			wantErr:     []error{workerlink.ErrDrainMessage},
			wantElapsed: 0,
			wantPeer:    peerOutcome{Notice: notice},
		},
		// The peer closes after its answer, so a later write of the worker's cannot be observed.
		"error: an answer cut off by a close fails at once": {
			answer:           cutOff,
			closeAfterAnswer: true,
			wantErr:          []error{workerlink.ErrDrainMessage},
			wantElapsed:      0,
			wantPeer:         peerOutcome{Notice: notice},
		},
		// The peer closes right after the notice, so a later write of the worker's cannot be observed.
		"error: a close without any answer fails at once with the end of file": {
			closeAfterAnswer: true,
			wantErr:          []error{io.EOF},
			wantElapsed:      0,
			wantPeer:         peerOutcome{Notice: notice},
		},
		// The context ends while the notice's write waits; the peer reads nothing, so no notice can be observed.
		"error: a context that ends while the notice is written ends the exchange with its error": {
			never:       true,
			cancelAfter: time.Second,
			wantErr:     []error{context.Canceled},
			wantElapsed: time.Second,
		},
		"error: a context that ends while the answer is awaited ends the exchange with its error": {
			cancelAfter: time.Second,
			wantErr:     []error{context.Canceled},
			wantElapsed: time.Second,
			wantPeer:    peerOutcome{Notice: notice},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				worker, peer := net.Pipe()
				defer func() { _, _ = worker.Close(), peer.Close() }()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelAfter > 0 {
					stop := time.AfterFunc(tc.cancelAfter, cancel)
					defer stop.Stop()
				}
				var aborted atomic.Bool
				abort := time.AfterFunc(abortAfter, func() {
					aborted.Store(true)
					_, _ = worker.Close(), peer.Close()
				})
				defer abort.Stop()
				seen := make(chan peerOutcome, 1)
				go func() {
					var got peerOutcome
					defer func() { seen <- got }()
					if tc.never {
						return
					}
					time.Sleep(tc.readAfter)
					r := bufio.NewReader(peer)
					msg, err := workerlink.DecodeDrain(r)
					got.Notice = msg
					if err != nil {
						got.NoticeErr = err.Error()
						return
					}
					if tc.answer != nil {
						_, _ = peer.Write(tc.answer)
					}
					if tc.closeAfterAnswer {
						_ = peer.Close()
						return
					}
					rest, _ := io.ReadAll(r)
					got.Extra = string(rest)
				}()
				start := time.Now()
				panicked, err := runDrainExchange(ctx, worker, generation)
				elapsed := time.Since(start)
				// The worker's end is closed only now, so that a write of the peer's waiting for the worker returns, and
				// the peer's reading of the rest ends. The peer is joined before any failure.
				_ = worker.Close()
				got := <-seen
				if panicked != nil {
					t.Fatalf("drainExchange panicked: %v", panicked)
				}
				if aborted.Load() {
					t.Fatalf("drainExchange was still running %v after it began, with %v; the case closed both ends", abortAfter, err)
				}
				cause := err == nil && tc.wantErr == nil
				for _, want := range tc.wantErr {
					cause = cause || errors.Is(err, want)
				}
				if diff := gocmp.Diff(exchangeResult{Elapsed: tc.wantElapsed, Cause: true}, exchangeResult{Elapsed: elapsed, Cause: cause}); diff != "" {
					t.Errorf("exchange ended with %v, want a cause among %v (-want +got):\n%s", err, tc.wantErr, diff)
				}
				if diff := gocmp.Diff(tc.wantPeer, got); diff != "" {
					t.Errorf("what the supervisor's end saw (-want +got):\n%s", diff)
				}
			})
		})
	}
}
