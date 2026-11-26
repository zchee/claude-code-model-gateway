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

// trackedChunkBody delivers scripted chunks, optional trailing error, and tracks Close and concurrent reads.
type trackedChunkBody struct {
	mu           sync.Mutex
	chunks       [][]byte
	trailingErr  error
	closed       atomic.Bool
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
	if b.closed.Load() {
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
	b.closed.Store(true)
	return nil
}

// singleReaderPipeBody wraps an io.PipeReader and detects any concurrent Read calls or premature Close.
type singleReaderPipeBody struct {
	pr           *io.PipeReader
	closed       atomic.Bool
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
	b.closed.Store(true)
	return b.pr.Close()
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
				if tt.wantClosedBefore && !src.closed.Load() {
					t.Errorf("underlying body was not closed when PeekFirstEvent returned nil Body")
				}
			} else {
				if res.Body == nil {
					t.Fatal("PeekResult.Body = nil, want non-nil replay body")
				}
				gotBytes, readErr := io.ReadAll(res.Body)
				closeErr := res.Body.Close()
				if readErr != nil {
					t.Fatalf("ReadAll(replay): %v", readErr)
				}
				if closeErr != nil {
					t.Fatalf("Close(replay): %v", closeErr)
				}
				if !src.closed.Load() {
					t.Errorf("closing replay Body did not close underlying body")
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

			synctest.Wait()
			select {
			case out := <-done:
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				_ = pw.Close()
				t.Fatalf("PeekFirstEvent returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			// Send partial first frame, then stall past wait.
			if _, err := io.WriteString(pw, ordinary[:30]); err != nil {
				t.Fatalf("write prefix: %v", err)
			}
			synctest.Wait()

			time.Sleep(50 * time.Millisecond)
			synctest.Wait()

			out := <-done
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
			if src.closed.Load() {
				_ = out.res.Body.Close()
				_ = pw.Close()
				t.Fatal("timeout closed the usable upstream body")
			}

			// Deliver the stalled remainder and tail while reading from replay Body concurrently.
			writeErrCh := make(chan error, 1)
			go func() {
				_, err := io.WriteString(pw, ordinary[30:])
				_ = pw.Close()
				writeErrCh <- err
			}()

			got, readErr := io.ReadAll(out.res.Body)
			closeErr := out.res.Body.Close()
			if writeErr := <-writeErrCh; writeErr != nil {
				t.Fatalf("writing remainder after timeout: %v", writeErr)
			}
			if readErr != nil {
				t.Fatalf("ReadAll(replay): %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Close(replay): %v", closeErr)
			}
			if !src.closed.Load() {
				t.Errorf("closing replay Body did not close underlying body")
			}
			if diff := gocmp.Diff(ordinary, string(got)); diff != "" {
				t.Errorf("replayed stream after timeout (-want +got):\n%s", diff)
			}
			if overlaps := src.overlapReads.Load(); overlaps != 0 {
				t.Errorf("detected %d overlapping concurrent Read calls after timeout", overlaps)
			}
		})
	})

	t.Run("success: closing replay Body after timeout unblocks and joins stalled reader", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()
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

			synctest.Wait()
			select {
			case out := <-done:
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				t.Fatalf("PeekFirstEvent returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			time.Sleep(50 * time.Millisecond)
			synctest.Wait()

			out := <-done
			if out.err != nil || out.res.Body == nil {
				t.Fatalf("expected replay body on timeout, got res=%+v err=%v", out.res, out.err)
			}
			if err := out.res.Body.Close(); err != nil {
				t.Fatalf("Close(replay): %v", err)
			}
			if !src.closed.Load() {
				t.Errorf("Close(replay) did not close underlying body")
			}
			// Returning from synctest.Test verifies no reader goroutine was left running.
		})
	})

	t.Run("error: context cancellation while peeking closes body and joins reader", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()
			src := &singleReaderPipeBody{pr: pr}
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

			synctest.Wait()
			select {
			case out := <-done:
				if out.res.Body != nil {
					_ = out.res.Body.Close()
				}
				t.Fatalf("PeekFirstEvent returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			cancel()
			synctest.Wait()

			out := <-done
			if !errors.Is(out.err, context.Canceled) {
				t.Fatalf("PeekFirstEvent error = %v, want context.Canceled", out.err)
			}
			if out.res.Body != nil {
				_ = out.res.Body.Close()
				t.Fatalf("PeekResult.Body = non-nil on cancellation, want nil")
			}
			if !src.closed.Load() {
				t.Errorf("cancellation did not close underlying body")
			}
			// Returning from synctest.Test verifies the reader goroutine exited before PeekFirstEvent returned.
		})
	})
}
