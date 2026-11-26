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
	"net/http"
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func TestPassthroughEmptyCompressedBody(t *testing.T) {
	// An empty gzip, deflate or br body is a decode failure, not a clean end: the source's Node
	// streams zlib.createGunzip, createInflate and createBrotliDecompress all emit the error
	// "unexpected end of file" when they are ended with no input, measured with the venue's pinned
	// Node v24.11.1 and recorded in
	// .omc/handoffs/2026-11-26-session3/evidence/node-probes/zlib-empty-input.txt. The source then
	// marks the capture's overflow once and reports no event. An empty body holds no pending text for
	// a clean end to flush, so the overflow mark is the only observable difference from a clean end.
	tests := map[string]struct {
		encoding      string
		wantOverflows int
	}{
		"error: empty gzip SSE body is a decode failure that marks the overflow once": {
			encoding:      "gzip",
			wantOverflows: 1,
		},
		"error: empty deflate SSE body is a decode failure that marks the overflow once": {
			encoding:      "deflate",
			wantOverflows: 1,
		},
		"error: empty br SSE body is a decode failure that marks the overflow once": {
			encoding:      "br",
			wantOverflows: 1,
		},
		"success: empty uncompressed SSE body ends observation cleanly without an overflow": {},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			header := http.Header{}
			if tt.encoding != "" {
				header.Set("Content-Encoding", tt.encoding)
			}
			src := newEOFChunkBody()
			rec := newStreamingRecorder()
			usage := &usageSpy{}

			res, err := sse.RelayPassthrough(t.Context(), rec, sseResponse(src, header), sse.PassthroughOptions{Usage: usage})
			if err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil: a failed side decode does not change what the client receives", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", got)
			}
			status, _, body, _, _ := rec.snapshot()
			if status != http.StatusOK || body != "" {
				t.Errorf("client response status=%d body=%q, want 200 and an empty body", status, body)
			}
			events, chunks, totalBytes, overflows := usage.snapshot(t)
			if overflows != tt.wantOverflows {
				t.Errorf("MarkOverflow calls = %d, want %d (encoding %q)", overflows, tt.wantOverflows, tt.encoding)
			}
			if len(events) != 0 || len(chunks) != 0 || totalBytes != 0 {
				t.Errorf("usage saw events=%q chunks=%q bytes=%d, want none", events, chunks, totalBytes)
			}
		})
	}
}

func TestPassthroughRepeatedContentEncoding(t *testing.T) {
	// Node joins repeated Content-Encoding fields with ", ", and "gzip, br" names no decoder, so the
	// source observes the bytes as they arrive. The body here is plain SSE: a relay that reads only
	// the first value tries gzip on it and reports a decode failure instead of the events. A given
	// RelayHeader is already normalized (repeated fields joined), so it is used as it is and wins over
	// the raw Header, whose single "gzip" would select a decoder.
	raw := departureStartFrame + departureDeltaFrame

	tests := map[string]struct {
		header      http.Header
		relayHeader http.Header
	}{
		"success: repeated Content-Encoding values in Header are joined and select no decoder": {
			header: http.Header{
				"Content-Type":     []string{"text/event-stream"},
				"Content-Encoding": []string{"gzip", "br"},
			},
		},
		"success: the joined Content-Encoding value of a given RelayHeader is used as it is and selects no decoder": {
			header: http.Header{
				"Content-Type":     []string{"text/event-stream"},
				"Content-Encoding": []string{"gzip"},
			},
			relayHeader: http.Header{
				"Content-Type":     []string{"text/event-stream"},
				"Content-Encoding": []string{"gzip, br"},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			src := newEOFChunkBody([]byte(raw))
			rec := newStreamingRecorder()
			usage := &usageSpy{}
			resp := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      tt.header.Clone(),
				RelayHeader: tt.relayHeader.Clone(),
				Body:        src,
			}

			res, err := sse.RelayPassthrough(t.Context(), rec, resp, sse.PassthroughOptions{Usage: usage})
			if err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", got)
			}
			_, _, body, _, _ := rec.snapshot()
			if diff := gocmp.Diff(raw, body); diff != "" {
				t.Errorf("delivered bytes altered (-want +got):\n%s", diff)
			}
			events, _, totalBytes, overflows := usage.snapshot(t)
			if overflows != 0 {
				t.Errorf("MarkOverflow calls = %d, want 0: the joined encoding selects no decoder", overflows)
			}
			if diff := gocmp.Diff([]string{departureStartEvent, departureDeltaEvent}, events); diff != "" {
				t.Errorf("observed events (-want +got):\n%s", diff)
			}
			if totalBytes != int64(len(raw)) {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(raw))
			}
		})
	}
}

func TestPassthroughRepeatedContentTypeFallback(t *testing.T) {
	// Without a RelayHeader the raw Header is read the way Node parses repeated fields: Content-Type
	// keeps its FIRST value. The observable effect asserted is the observation mode of a 2xx response
	// with a usage capture: an event stream is decoded into events and counted with
	// NoteResponseBytes, while any other 2xx body is handed to ObserveChunk as it is.
	raw := departureStartFrame + departureDeltaFrame

	tests := map[string]struct {
		contentType []string
		wantEvents  []string
		wantChunks  string
		wantBytes   int64
	}{
		"success: first Content-Type application/json is not observed as an event stream although a later value is text/event-stream": {
			contentType: []string{"application/json", "text/event-stream"},
			wantChunks:  raw,
		},
		"success: first Content-Type text/event-stream is observed as an event stream although a later value is application/json": {
			contentType: []string{"text/event-stream", "application/json"},
			wantEvents:  []string{departureStartEvent, departureDeltaEvent},
			wantBytes:   int64(len(raw)),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			src := newEOFChunkBody([]byte(raw))
			rec := newStreamingRecorder()
			usage := &usageSpy{}
			resp := &upstream.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": slices.Clone(tt.contentType)},
				Body:       src,
			}

			res, err := sse.RelayPassthrough(t.Context(), rec, resp, sse.PassthroughOptions{Usage: usage})
			if err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", got)
			}
			status, _, body, _, _ := rec.snapshot()
			if status != http.StatusOK {
				t.Errorf("client status = %d, want 200", status)
			}
			if diff := gocmp.Diff(raw, body); diff != "" {
				t.Errorf("delivered bytes altered (-want +got):\n%s", diff)
			}
			events, chunks, totalBytes, overflows := usage.snapshot(t)
			if overflows != 0 {
				t.Errorf("MarkOverflow calls = %d, want 0", overflows)
			}
			if diff := gocmp.Diff(tt.wantEvents, events); diff != "" {
				t.Errorf("observed events (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantChunks, string(chunks)); diff != "" {
				t.Errorf("ObserveChunk bytes (-want +got):\n%s", diff)
			}
			if totalBytes != tt.wantBytes {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, tt.wantBytes)
			}
		})
	}
}
