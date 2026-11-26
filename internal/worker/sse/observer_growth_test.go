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
	"strings"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

func TestEventObserverLongFrameInSmallChunks(t *testing.T) {
	tests := map[string]struct {
		frameBytes int
		chunkBytes int
		budget     time.Duration
	}{
		// Work per Write that grows with the pending frame makes this input quadratic: about 3.7
		// million one-byte writes over an average of 1.75 MiB pending text is several TiB of copying,
		// more than a minute at any memory bandwidth, while work proportional to the chunk finishes
		// in a few seconds even under the race detector.
		"success: a frame just under the default budget fed one byte at a time is observed in linear time": {
			frameBytes: sse.DefaultMaxObserveBytes - 512<<10,
			chunkBytes: 1,
			budget:     20 * time.Second,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			head := `data: {"type":"message_delta","pad":"`
			tail := `"}`
			pad := strings.Repeat("x", tt.frameBytes-len(head)-len(tail))
			payload := head[len("data: "):] + pad + tail
			frame := []byte(head + pad + tail + "\n\n")

			var events []string
			var overflows int
			o := sse.NewEventObserver(0, func(event jsonx.WireValue) {
				events = append(events, mustEncodeWire(t, event))
			}, func() { overflows++ })

			start := time.Now()
			writes := 0
			for off := 0; off < len(frame); off += tt.chunkBytes {
				o.Write(frame[off:min(off+tt.chunkBytes, len(frame))])
				writes++
				if writes%65536 == 0 {
					if elapsed := time.Since(start); elapsed > tt.budget {
						t.Fatalf("observer spent %v on %d of %d bytes (%d writes of %d bytes); the cost of a write grows with the pending frame",
							elapsed, off, len(frame), writes, tt.chunkBytes)
					}
				}
			}
			o.End()
			elapsed := time.Since(start)
			t.Logf("observed a %d-byte frame in %d writes of %d bytes in %v", len(frame), writes, tt.chunkBytes, elapsed)
			if elapsed > tt.budget {
				t.Errorf("observer spent %v, want at most %v", elapsed, tt.budget)
			}

			if overflows != 0 {
				t.Errorf("onOverflow calls = %d, want 0 for a frame under the budget", overflows)
			}
			if len(events) != 1 {
				t.Fatalf("observed %d events, want 1", len(events))
			}
			if events[0] != payload {
				t.Errorf("observed event of %d bytes does not match the %d-byte payload (prefix %q)", len(events[0]), len(payload), events[0][:min(len(events[0]), 64)])
			}
		})
	}
}
