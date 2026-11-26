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

package sse

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	sseframe "github.com/zchee/claude-code-model-gateway/internal/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// readBufferSize bounds one upstream read.
const readBufferSize = 32 << 10

// overrideAborted labels a response whose upstream body ended before it was complete.
const overrideAborted = "upstream_aborted"

var (
	errNilBody          = errors.New("sse: nil upstream body")
	errReplayBodyClosed = errors.New("sse: read on closed replay body")
)

// readResult is the outcome of one upstream read: the filled part of the request buffer.
type readResult struct {
	data []byte
	err  error
}

// asyncReader owns an upstream body and reads it on one goroutine, one read per request,
// so a caller can wait for data, a timer and cancellation together. The body is read by
// nothing else while the goroutine lives, and close joins the goroutine.
type asyncReader struct {
	body    io.ReadCloser
	reqs    chan []byte
	results chan readResult
	quit    chan struct{}
	exited  chan struct{}
	closing atomic.Bool

	// pending and finished belong to the single consumer: a read is in flight, or a read
	// returned an error and the goroutine has exited.
	pending  bool
	finished bool
}

func newAsyncReader(body io.ReadCloser) *asyncReader {
	r := &asyncReader{
		body:    body,
		reqs:    make(chan []byte, 1),
		results: make(chan readResult),
		quit:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *asyncReader) run() {
	defer close(r.exited)
	for {
		var buf []byte
		select {
		case buf = <-r.reqs:
		case <-r.quit:
			return
		}
		n, err := r.body.Read(buf)
		select {
		case r.results <- readResult{data: buf[:n], err: err}:
		case <-r.quit:
			return
		}
		if err != nil {
			return
		}
	}
}

// request starts one read into buf unless a read is in flight or the body has ended.
// The caller must not touch buf until the matching result arrives.
func (r *asyncReader) request(buf []byte) {
	if r.pending || r.finished {
		return
	}
	r.reqs <- buf
	r.pending = true
}

// got records a result received from r.results.
func (r *asyncReader) got(res readResult) {
	r.pending = false
	if res.err != nil {
		r.finished = true
	}
}

// close closes the body, which unblocks a read in flight, and waits for the reader goroutine
// to exit. Only the first call closes the body; every call returns after the goroutine exits.
func (r *asyncReader) close() error {
	if !r.closing.CompareAndSwap(false, true) {
		<-r.exited
		return nil
	}
	err := r.body.Close()
	close(r.quit)
	<-r.exited
	return err
}

// peekOutcome is what the first-event peek decided.
type peekOutcome struct {
	// prefix holds every byte read so far, to be replayed before anything else.
	prefix []byte
	// termErr is the error that ended the body during the peek, io.EOF included.
	termErr error
	// overflow is set when the first data event is an engine context overflow.
	overflow *sentry.Counts
	// earlyEnd is the route status override when the body broke before its first data event.
	earlyEnd string
}

// peekFirst reads r up to the first complete SSE frame that carries data, the wait deadline,
// or the end of the body. On an overflow or an abnormal end it closes r; otherwise r stays open
// and may still have the read that was in flight when the wait ran out. A service context that
// has ended closes r and returns its error: on entry (whatever the wait), before each read, and
// over a read error, since closing the transport on shutdown also fails the read in flight.
func peekFirst(ctx context.Context, r *asyncReader, wait time.Duration) (peekOutcome, error) {
	if err := ctx.Err(); err != nil {
		_ = r.close()
		return peekOutcome{}, err
	}
	if wait == 0 {
		wait = DefaultFirstEventWait
	}
	if wait < 0 {
		return peekOutcome{}, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	var scanner sseframe.Scanner
	var prefix []byte
	buf := make([]byte, readBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			_ = r.close()
			return peekOutcome{}, err
		}
		r.request(buf)
		select {
		case res := <-r.results:
			r.got(res)
			prefix = append(prefix, res.data...)
			if event, decided := firstDataEvent(scanner.Feed(res.data)); decided {
				if counts, ok := engineOverflow(event); ok {
					_ = r.close()
					return peekOutcome{overflow: &counts}, nil
				}
				return peekOutcome{prefix: prefix, termErr: res.err}, nil
			}
			switch {
			case errors.Is(res.err, io.EOF):
				return peekOutcome{prefix: prefix, termErr: res.err}, nil
			case res.err != nil:
				_ = r.close()
				if err := ctx.Err(); err != nil {
					return peekOutcome{}, err
				}
				return peekOutcome{earlyEnd: overrideAborted}, nil
			}
		case <-timer.C:
			return peekOutcome{prefix: prefix}, nil
		case <-ctx.Done():
			_ = r.close()
			return peekOutcome{}, ctx.Err()
		}
	}
}

// firstDataEvent decides on the first frame whose data lines are not all empty. All of the
// frame's data lines are joined; a frame whose data is not JSON decides with an absent event.
func firstDataEvent(frames []sseframe.Frame) (jsonx.WireValue, bool) {
	for _, f := range frames {
		var data []string
		for line := range lines(f.Body) {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, jsTrimStart(value))
			}
		}
		joined := strings.Join(data, "\n")
		if joined == "" {
			continue
		}
		event, err := jsonx.DecodeWire([]byte(joined))
		if err != nil {
			return jsonx.WireValue{}, true
		}
		return event, true
	}
	return jsonx.WireValue{}, false
}

// engineOverflow recognizes a first stream event that is an error event carrying an engine
// context overflow.
func engineOverflow(event jsonx.WireValue) (sentry.Counts, bool) {
	if !isText(member(event, "type"), "error") {
		return sentry.Counts{}, false
	}
	return sentry.EngineOverflow(member(event, "error"))
}

// replayBody replays a peeked prefix and then the rest of the body it owns.
type replayBody struct {
	r      *asyncReader
	prefix []byte
	err    error
}

// Read returns the peeked bytes first, then the result of the read that was in flight when
// the peek stopped, then reads the body directly while the reader goroutine is idle.
func (b *replayBody) Read(p []byte) (int, error) {
	for {
		if len(b.prefix) > 0 {
			n := copy(p, b.prefix)
			b.prefix = b.prefix[n:]
			return n, nil
		}
		if b.err != nil {
			return 0, b.err
		}
		if !b.r.pending {
			return b.r.body.Read(p)
		}
		select {
		case res := <-b.r.results:
			b.r.got(res)
			b.prefix, b.err = res.data, res.err
		case <-b.r.exited:
			b.r.pending = false
			b.err = errReplayBodyClosed
		}
	}
}

// Close closes the owned body and waits for a read in flight to return.
func (b *replayBody) Close() error { return b.r.close() }

// PeekFirstEvent inspects body up to its first complete SSE data event or wait deadline.
// The ctx parameter is the service-owned lifetime context, not the client request context.
// PeekFirstEvent takes ownership of body on entry. The body passed in must allow Close
// concurrently with Read, and Close must unblock a Read in flight. A zero wait uses
// DefaultFirstEventWait; a negative wait times out immediately.
//
// A clean EOF before a first data event is not an error and returns a non-nil replay Body.
// An abnormal upstream read error returns a nil Body, a non-empty EarlyEndOverride, and a nil error.
// On wait expiration, PeekFirstEvent does not close the usable stream and does not leave a goroutine
// racing a second reader; the single reader transfers to the returned Body whose Close unblocks and
// joins it. On ctx cancellation, PeekFirstEvent closes body, joins its reader goroutine, and returns
// ctx.Err(); so does a ctx that has already ended on entry (whatever the wait), and a ctx that has
// ended by the time a read error arrives, which then reports no EarlyEndOverride.
func PeekFirstEvent(ctx context.Context, body io.ReadCloser, wait time.Duration) (PeekResult, error) {
	if body == nil {
		return PeekResult{}, errNilBody
	}
	r := newAsyncReader(body)
	peeked, err := peekFirst(ctx, r, wait)
	switch {
	case err != nil:
		return PeekResult{}, err
	case peeked.overflow != nil:
		return PeekResult{Overflow: peeked.overflow}, nil
	case peeked.earlyEnd != "":
		return PeekResult{EarlyEndOverride: peeked.earlyEnd}, nil
	}
	return PeekResult{Body: &replayBody{r: r, prefix: peeked.prefix, err: peeked.termErr}}, nil
}
