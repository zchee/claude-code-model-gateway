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

package usage_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/usage"
)

// TestNoRecordStillSamplesClock checks that building the primary record reads the injected clock
// exactly once even when the request yields no record: the instant is taken before the record is
// known to exist, where the source returns without a record before it reads its clock. The count
// is taken around Build or Finish alone.
func TestNoRecordStillSamplesClock(t *testing.T) {
	const usageBody = `{"usage":{"input_tokens":10,"output_tokens":2}}`
	tests := map[string]struct {
		// build sends the request through Builder.Build; otherwise through a capture's Finish.
		build    bool
		status   int
		body     string
		complete bool
	}{
		"success: Build of a success without usage": {
			build: true, status: http.StatusOK,
		},
		"error: Build of a failure without limit evidence": {
			build: true, status: http.StatusServiceUnavailable,
		},
		"success: capture of a success without usage": {
			status: http.StatusOK, body: `{"type":"message"}`, complete: true,
		},
		"error: capture of an incomplete success with usage": {
			status: http.StatusOK, body: usageBody,
		},
		"error: capture of a failure without limit evidence": {
			status: http.StatusInternalServerError, body: `{"error":{"type":"api_error"}}`, complete: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var reads atomic.Int32
			opts := newTestOptions(t)
			opts.Now = func() time.Time {
				reads.Add(1)
				return time.Unix(123, 0)
			}
			builder := usage.NewBuilder(opts)
			var got []usage.Record
			if tt.build {
				before := reads.Load()
				got = builder.Build(usage.Request{Body: decodeWire(t, `{"messages":[]}`), StatusCode: tt.status})
				if n := reads.Load() - before; n != 1 {
					t.Errorf("clock reads during Build = %d, want 1", n)
				}
			} else {
				c := usage.NewCapture(builder, usage.Request{Body: decodeWire(t, `{"messages":[]}`)}, 0, nil)
				c.SetResponse(tt.status, nil)
				c.ObserveJSON([]byte(tt.body))
				c.SetResponseComplete(tt.complete)
				before := reads.Load()
				if r := c.Finish(); r != nil {
					got = append(got, *r)
				}
				if n := reads.Load() - before; n != 1 {
					t.Errorf("clock reads during Finish = %d, want 1", n)
				}
			}
			if len(got) != 0 {
				t.Errorf("got %d records, want none: %+v", len(got), got)
			}
		})
	}
}
