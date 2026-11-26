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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/sse"
)

func TestScannerReusableInput(t *testing.T) {
	tests := map[string]struct {
		wire string
		want []sse.Frame
	}{
		"success: overwrite the read buffer between feeds": {
			"a\r\r\n\nb\n\r\nc\r",
			[]sse.Frame{{Body: "a\r", Delimiter: "\r\n\n"}, {Body: "b", Delimiter: "\n\r\n"}, {Body: "c\r"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var scanner sse.Scanner
			var buf [1]byte
			var frames []sse.Frame
			for i := range len(tt.wire) {
				buf[0] = tt.wire[i]
				frames = append(frames, scanner.Feed(buf[:])...)
			}
			buf[0] = 0
			frames = append(frames, scanner.End()...)
			if diff := gocmp.Diff(tt.want, frames); diff != "" {
				t.Fatalf("reused input (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFrameBareCR(t *testing.T) {
	tests := map[string]struct {
		body, data, rewritten string
	}{
		"success: bare CR at EOF is data":                      {"data: x\r", " x\r", "data: new"},
		"success: only the CR paired with LF is a line ending": {"id: 1\r\r\ndata:x\r\r\ndata:later\r", "x\r", "id: 1\r\ndata: new\ndata:later\r"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			frame := sse.Frame{Body: tt.body}
			if data, ok := frame.Data(); !ok || data != tt.data {
				t.Errorf("Data() = %q, %t; want %q, true", data, ok, tt.data)
			}
			if diff := gocmp.Diff(tt.rewritten, frame.WithData("new")); diff != "" {
				t.Errorf("rewrite (-want +got):\n%s", diff)
			}
		})
	}
}
