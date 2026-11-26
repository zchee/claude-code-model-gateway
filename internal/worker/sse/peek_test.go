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

package sse_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

// trackedChunkBody delivers scripted chunks, optional trailing error, and tracks Close count and concurrent reads.
type trackedChunkBody struct {
	mu           sync.Mutex
	chunks       [][]byte
	trailingErr  error
	closes       atomic.Int32
	inFlight     atomic.Int32
	overlapReads atomic.Int32
}

func (b *trackedChunkBody) Read(p []byte) (int, error) {
	if b.inFlight.Add(1) > 1 {
		b.overlapReads.Add(1)
	}
	defer b.inFlight.Add(-1)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closes.Load() > 0 {
		return 0, io.ErrClosedPipe
	}
	if len(b.chunks) == 0 {
		if b.trailingErr != nil {
			return 0, b.trailingErr
		}
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	if n < len(b.chunks[0]) {
		b.chunks[0] = b.chunks[0][n:]
	} else {
		b.chunks = b.chunks[1:]
	}
	return n, nil
}

func (b *trackedChunkBody) Close() error {
	b.closes.Add(1)
	return nil
}

// singleReaderPipeBody wraps an io.PipeReader and detects any concurrent Read calls or duplicate/premature Close.
type singleReaderPipeBody struct {
	pr           *io.PipeReader
	closes       atomic.Int32
	inFlight     atomic.Int32
	overlapReads atomic.Int32
}

func (b *singleReaderPipeBody) Read(p []byte) (int, error) {
	if b.inFlight.Add(1) > 1 {
		b.overlapReads.Add(1)
	}
	defer b.inFlight.Add(-1)
	return b.pr.Read(p)
}

func (b *singleReaderPipeBody) Close() error {
	b.closes.Add(1)
	return b.pr.Close()
}

// gatedReadBody blocks Read until Close is called and then waits on allowReadReturn
// before letting Read exit so tests can prove join-before-return rather than eventual exit.
type gatedReadBody struct {
	readEntered     chan struct{}
	readEnteredOnce sync.Once
	closeCalled     chan struct{}
	closeOnce       sync.Once
	allowReadReturn chan struct{}
	allowOnce       sync.Once
	closes          atomic.Int32
	readReturned    atomic.Bool
	inFlight        atomic.Int32
	overlapReads    atomic.Int32
}

func newGatedReadBody() *gatedReadBody {
	return &gatedReadBody{
		readEntered:     make(chan struct{}),
		closeCalled:     make(chan struct{}),
		allowReadReturn: make(chan struct{}),
	}
}

func (b *gatedReadBody) Read(_ []byte) (int, error) {
	if b.inFlight.Add(1) > 1 {
		b.overlapReads.Add(1)
	}
	defer b.inFlight.Add(-1)
	b.readEnteredOnce.Do(func() { close(b.readEntered) })
	<-b.closeCalled
	<-b.allowReadReturn
	b.readReturned.Store(true)
	return 0, io.ErrClosedPipe
}

func (b *gatedReadBody) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closeCalled) })
	return nil
}

func (b *gatedReadBody) allowReturn() {
	b.allowOnce.Do(func() { close(b.allowReadReturn) })
}

func TestPeekFirstEvent(t *testing.T) {
	overflowFrame := lmStudioOverflowFrame(6017, 4096)
	wordedFrame := lmStudioWordedOverflowFrame(6017, 4096)
	multilineOverflow := "event: error\ndata: {\"type\":\"error\",\ndata: \"error\":{\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":6017,\"n_ctx\":4096}}\n\n"
	markerOnlyOverflow := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"exceed_context_size_error\"}}\n\n"
	nonErrorOverflowShaped := "event: message_start\ndata: {\"type\":\"message_start\",\"error\":{\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":6017,\"n_ctx\":4096}}\n\n"
	malformedThenOverflow := "event: message_start\ndata: {broken-json}\n\n" + overflowFrame
	otherError := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"model crashed\"}}\n\n"
	ordinary := lmStudioOrdinaryStream(19, 0)
	partial := "event: message_start\ndata: {\"type\":\"mess"

	tests := map[string]struct {
		chunks           []string
		trailingErr      error
		wantReplay       string
		wantNilBody      bool
		wantOverflow     *sentry.Counts
		wantEarlyEnd     bool
		wantClosedBefore bool
	}{
		"success: embedded JSON overflow closes body and returns counts with nil Body": {
			chunks:           []string{overflowFrame},
			wantNilBody:      true,
			wantOverflow:     &sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))},
			wantClosedBefore: true,
		},
		"success: worded engine overflow closes body and returns counts with nil Body": {
			chunks:           []string{wordedFrame},
			wantNilBody:      true,
			wantOverflow:     &sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))},
			wantClosedBefore: true,
		},
		"success: multiline data first error event normalizes to overflow": {
			chunks:           []string{multilineOverflow},
			wantNilBody:      true,
			wantOverflow:     &sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))},
			wantClosedBefore: true,
		},
		"success: overflow marker without usable counts returns non-nil zero Counts and nil Body": {
			chunks:           []string{markerOnlyOverflow},
			wantNilBody:      true,
			wantOverflow:     &sentry.Counts{},
			wantClosedBefore: true,
		},
		"success: comment frame and split writes before overflow": {
			chunks:           []string{": ping\n\n" + overflowFrame[:40], overflowFrame[40:]},
			wantNilBody:      true,
			wantOverflow:     &sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))},
			wantClosedBefore: true,
		},
		"success: ordinary first event replays exact prefix and unread tail": {
			chunks:     []string{ordinary[:30], ordinary[30:90], ordinary[90:]},
			wantReplay: ordinary,
		},
		"success: overflow-shaped fields on a non-error event do not match and replay verbatim": {
			chunks:     []string{nonErrorOverflowShaped},
			wantReplay: nonErrorOverflowShaped,
		},
		"error: malformed first data frame followed by overflow does not skip and replays verbatim": {
			chunks:     []string{malformedThenOverflow},
			wantReplay: malformedThenOverflow,
		},
		"error: non-overflow error event replays verbatim": {
			chunks:     []string{otherError},
			wantReplay: otherError,
		},
		"error: clean EOF on empty stream is not an error and returns empty replay Body": {
			chunks:     nil,
			wantReplay: "",
		},
		"error: clean EOF on partial frame is not an error and returns partial replay Body": {
			chunks:     []string{partial},
			wantReplay: partial,
		},
		"error: abnormal read error before first frame closes body and reports EarlyEndOverride": {
			chunks:           []string{": hi\n"},
			trailingErr:      io.ErrUnexpectedEOF,
			wantNilBody:      true,
			wantEarlyEnd:     true,
			wantClosedBefore: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			chunks := make([][]byte, len(tt.chunks))
			for i, c := range tt.chunks {
				chunks[i] = []byte(c)
			}
			src := &trackedChunkBody{chunks: chunks, trailingErr: tt.trailingErr}

			res, err := sse.PeekFirstEvent(t.Context(), src, 5*time.Second)
			if err != nil {
				t.Fatalf("PeekFirstEvent() error = %v, want nil", err)
			}
			if tt.wantNilBody {
				if res.Body != nil {
					_ = res.Body.Close()
					t.Fatalf("PeekResult.Body = non-nil, want nil")
				}
				if tt.wantClosedBefore {
					if got := src.closes.Load(); got != 1 {
						t.Errorf("underlying body close count = %d when PeekFirstEvent returned nil Body, want 1", got)
					}
				}
			} else {
				if res.Body == nil {
					t.Fatal("PeekResult.Body = nil, want non-nil replay body")
				}
				if got := src.closes.Load(); got != 0 {
					t.Errorf("underlying body close count = %d before replay Body was read or closed, want 0", got)
				}
				gotBytes, readErr := io.ReadAll(res.Body)
				closeErr := res.Body.Close()
				if readErr != nil {
					t.Fatalf("ReadAll(replay): %v", readErr)
				}
				if closeErr != nil {
					t.Fatalf("Close(replay): %v", closeErr)
				}
				if got := src.closes.Load(); got != 1 {
					t.Errorf("underlying body close count after Close(replay) = %d, want 1", got)
				}
				if diff := gocmp.Diff(tt.wantReplay, string(gotBytes)); diff != "" {
					t.Errorf("replayed bytes mismatch (-want +got):\n%s", diff)
				}
			}

			if tt.wantOverflow == nil {
				if res.Overflow != nil {
					t.Errorf("PeekResult.Overflow = %+v, want nil", res.Overflow)
				}
			} else {
				if res.Overflow == nil {
					t.Fatalf("PeekResult.Overflow = nil, want non-nil %+v", *tt.wantOverflow)
				}
				if diff := gocmp.Diff(*tt.wantOverflow, *res.Overflow); diff != "" {
					t.Errorf("PeekResult.Overflow (-want +got):\n%s", diff)
				}
			}

			if tt.wantEarlyEnd {
				if res.EarlyEndOverride != "upstream_error" && res.EarlyEndOverride != "upstream_aborted" {
					t.Errorf("EarlyEndOverride = %q, want upstream_error or upstream_aborted", res.EarlyEndOverride)
				}
			} else if res.EarlyEndOverride != "" {
				t.Errorf("EarlyEndOverride = %q, want empty", res.EarlyEndOverride)
			}

			if got := src.overlapReads.Load(); got != 0 {
				t.Errorf("detected %d overlapping concurrent Read calls on source body", got)
			}
		})
	}

	t.Run("success: wait timeout transfers single reader to replay Body without closing or racing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			src := &singleReaderPipeBody{pr: pr}

			type peekOut struct {
				res sse.PeekResult
				err error
			}
			done := make(chan peekOut, 1)
			go func() {
				res, err := sse.PeekFirstEvent(t.Context(), src, 50*time.Millisecond)
				done <- peekOut{res: res, err: err}
			}()
			joined := false
			defer func() {
				if !joined {
					_ = pw.Close()
					_ = pr.Close()
					out := <-done
					if out.res.Body != nil {
						_ = out.res.Body.Close()
					}
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				_ = pw.Close()
				t.Fatalf("PeekFirstEvent returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			// Send partial first frame, then stall until the 50ms wait boundary.
			if _, err := io.WriteString(pw, ordinary[:30]); err != nil {
				t.Fatalf("write prefix: %v", err)
			}
			synctest.Wait()

			time.Sleep(49 * time.Millisecond)
			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				_ = pw.Close()
				t.Fatalf("PeekFirstEvent timed out early at 49ms: res=%+v err=%v", out.res, out.err)
			default:
			}

			time.Sleep(1 * time.Millisecond)
			synctest.Wait()

			// Non-blocking receive at the 50ms boundary so synctest cannot advance fake time past it.
			var out peekOut
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("PeekFirstEvent did not return at the 50ms wait boundary")
			}
			if out.err != nil {
				_ = pw.Close()
				_ = pr.Close()
				t.Fatalf("PeekFirstEvent error = %v, want nil on timeout", out.err)
			}
			if out.res.Body == nil || out.res.Overflow != nil || out.res.EarlyEndOverride != "" {
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				_ = pw.Close()
				_ = pr.Close()
				t.Fatalf("unexpected PeekResult on timeout: %+v", out.res)
			}
			if got := src.closes.Load(); got != 0 {
				_ = out.res.Body.Close()
				_ = pw.Close()
				t.Fatalf("timeout closed the usable upstream body (closes=%d, want 0)", got)
			}

			// Start reading the replay Body first while the source pipe is still silent, wait for
			// quiescence, and assert no second reader races the in-flight source Read before writing the tail.
			type readOut struct {
				data []byte
				err  error
			}
			readCh := make(chan readOut, 1)
			readJoined := false
			defer func() {
				if !readJoined {
					_ = pw.Close()
					_ = pr.Close()
					_ = out.res.Body.Close()
					<-readCh
				}
			}()
			go func() {
				data, err := io.ReadAll(out.res.Body)
				readCh <- readOut{data: data, err: err}
			}()
			synctest.Wait()

			if overlaps := src.overlapReads.Load(); overlaps != 0 {
				t.Fatalf("detected %d overlapping concurrent Read calls while source was silent after timeout", overlaps)
			}
			select {
			case r := <-readCh:
				readJoined = true
				t.Fatalf("ReadAll(replay) returned before source EOF: data=%q err=%v", string(r.data), r.err)
			default:
			}

			// Now release the stalled remainder and tail to complete the stream.
			writeErrCh := make(chan error, 1)
			writeJoined := false
			defer func() {
				if !writeJoined {
					_ = pw.Close()
					_ = pr.Close()
					<-writeErrCh
				}
			}()
			go func() {
				_, err := io.WriteString(pw, ordinary[30:])
				_ = pw.Close()
				writeErrCh <- err
			}()
			synctest.Wait()

			var writeErr error
			select {
			case writeErr = <-writeErrCh:
				writeJoined = true
			default:
				t.Fatal("tail writer did not complete while replay reader was active")
			}
			if writeErr != nil {
				t.Fatalf("writing remainder after timeout: %v", writeErr)
			}

			var r readOut
			select {
			case r = <-readCh:
				readJoined = true
			default:
				t.Fatal("ReadAll(replay) did not complete after source EOF")
			}
			closeErr := out.res.Body.Close()
			if r.err != nil {
				t.Fatalf("ReadAll(replay): %v", r.err)
			}
			if closeErr != nil {
				t.Fatalf("Close(replay): %v", closeErr)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("underlying body close count after Close(replay) = %d, want 1", got)
			}
			if diff := gocmp.Diff(ordinary, string(r.data)); diff != "" {
				t.Errorf("replayed stream after timeout (-want +got):\n%s", diff)
			}
			if overlaps := src.overlapReads.Load(); overlaps != 0 {
				t.Errorf("detected %d overlapping concurrent Read calls after timeout", overlaps)
			}
		})
	})

	t.Run("success: closing replay Body after timeout unblocks and joins stalled reader", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			src := newGatedReadBody()

			type peekOut struct {
				res sse.PeekResult
				err error
			}
			done := make(chan peekOut, 1)
			go func() {
				res, err := sse.PeekFirstEvent(t.Context(), src, 50*time.Millisecond)
				done <- peekOut{res: res, err: err}
			}()
			peekJoined := false
			var closeDone chan error
			closeJoined := true
			defer func() {
				src.allowReturn()
				if !peekJoined {
					_ = src.Close()
					out := <-done
					if out.res.Body != nil {
						_ = out.res.Body.Close()
					}
				}
				if !closeJoined {
					_ = src.Close()
					<-closeDone
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				peekJoined = true
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				t.Fatalf("PeekFirstEvent returned early: res=%+v err=%v", out.res, out.err)
			default:
			}
			select {
			case <-src.readEntered:
			default:
				t.Fatal("PeekFirstEvent did not start reading source body")
			}

			time.Sleep(49 * time.Millisecond)
			synctest.Wait()
			select {
			case out := <-done:
				peekJoined = true
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				t.Fatalf("PeekFirstEvent timed out early at 49ms: res=%+v err=%v", out.res, out.err)
			default:
			}

			time.Sleep(1 * time.Millisecond)
			synctest.Wait()

			var out peekOut
			select {
			case out = <-done:
				peekJoined = true
			default:
				t.Fatal("PeekFirstEvent did not return at the 50ms wait boundary")
			}
			if out.err != nil || out.res.Body == nil {
				t.Fatalf("expected replay body on timeout, got res=%+v err=%v", out.res, out.err)
			}
			if got := src.closes.Load(); got != 0 {
				t.Fatalf("timeout closed the underlying source body (closes=%d, want 0)", got)
			}

			// Start Close on the replay Body while the stalled reader is gated inside Read.
			closeDone = make(chan error, 1)
			closeJoined = false
			go func() {
				closeDone <- out.res.Body.Close()
			}()
			synctest.Wait()

			if got := src.closes.Load(); got != 1 {
				t.Fatalf("underlying body close count during Close(replay) = %d, want 1", got)
			}
			select {
			case err := <-closeDone:
				closeJoined = true
				t.Fatalf("Close(replay) returned before joining the stalled reader (err=%v, readReturned=%t)", err, src.readReturned.Load())
			default:
			}

			// Release the reader's Read return and verify Close now completes immediately.
			src.allowReturn()
			synctest.Wait()
			select {
			case err := <-closeDone:
				closeJoined = true
				if err != nil {
					t.Fatalf("Close(replay): %v", err)
				}
			default:
				t.Fatal("Close(replay) did not return after stalled reader exited")
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("underlying body close count after Close(replay) = %d, want 1", got)
			}
			if !src.readReturned.Load() {
				t.Errorf("underlying Read did not exit before Close(replay) returned")
			}
			if overlaps := src.overlapReads.Load(); overlaps != 0 {
				t.Errorf("detected %d overlapping concurrent Read calls", overlaps)
			}
		})
	})

	t.Run("error: context cancellation while peeking closes body and joins reader", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			src := newGatedReadBody()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			type peekOut struct {
				res sse.PeekResult
				err error
			}
			done := make(chan peekOut, 1)
			go func() {
				res, err := sse.PeekFirstEvent(ctx, src, 5*time.Second)
				done <- peekOut{res: res, err: err}
			}()
			joined := false
			defer func() {
				cancel()
				src.allowReturn()
				if !joined {
					_ = src.Close()
					out := <-done
					if out.res.Body != nil {
						_ = out.res.Body.Close()
					}
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				t.Fatalf("PeekFirstEvent returned early: res=%+v err=%v", out.res, out.err)
			default:
			}
			select {
			case <-src.readEntered:
			default:
				t.Fatal("PeekFirstEvent did not start reading source body")
			}

			// Cancel ctx while the reader is gated inside Read: PeekFirstEvent must close src
			// and block until the reader exits rather than returning immediately.
			cancel()
			synctest.Wait()

			if got := src.closes.Load(); got != 1 {
				t.Fatalf("underlying body close count on cancellation = %d, want 1", got)
			}
			select {
			case out := <-done:
				joined = true
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				t.Fatalf("PeekFirstEvent returned before joining its reader goroutine (res=%+v err=%v readReturned=%t)", out.res, out.err, src.readReturned.Load())
			default:
			}

			// Release the reader's Read return and verify PeekFirstEvent returns immediately with ctx.Err().
			src.allowReturn()
			synctest.Wait()
			var out peekOut
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("PeekFirstEvent did not return after reader exited on cancellation")
			}
			if !errors.Is(out.err, context.Canceled) {
				t.Fatalf("PeekFirstEvent error = %v, want context.Canceled", out.err)
			}
			if out.res.Body != nil {
				_ = out.res.Body.Close()
				t.Fatalf("PeekResult.Body = non-nil on cancellation, want nil")
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("underlying body close count after cancellation return = %d, want 1", got)
			}
			if !src.readReturned.Load() {
				t.Errorf("underlying Read did not exit before PeekFirstEvent returned")
			}
			if overlaps := src.overlapReads.Load(); overlaps != 0 {
				t.Errorf("detected %d overlapping concurrent Read calls", overlaps)
			}
		})
	})
}
