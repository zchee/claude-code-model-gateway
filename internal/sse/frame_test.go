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
	"bytes"
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/sse"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

const mixedFrames = ": 日本語\r\ndata:\t{ }\r\ndata: second\r\n\r\n: ping\n\n\r\n\ndata: [DONE]\n\r\nevent: last\r\ndata: tail"

func TestScanner(t *testing.T) {
	tests := map[string]struct {
		wire           string
		complete, tail []sse.Frame
	}{
		"success: every byte boundary": {mixedFrames, []sse.Frame{
			{Body: ": 日本語\r\ndata:\t{ }\r\ndata: second", Delimiter: "\r\n\r\n"},
			{Body: ": ping", Delimiter: "\n\n"},
			{Delimiter: "\r\n\n"},
			{Body: "data: [DONE]", Delimiter: "\n\r\n"},
		}, []sse.Frame{{Body: "event: last\r\ndata: tail"}}},
		"success: empty stream":                           {},
		"success: final delimiter is not a tail":          {"data:\n\n", []sse.Frame{{Body: "data:", Delimiter: "\n\n"}}, nil},
		"success: final lone newline belongs to the tail": {"data: x\r\n", nil, []sse.Frame{{Body: "data: x\r\n"}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for cut := range len(tt.wire) + 1 {
				var scanner sse.Scanner
				var complete []sse.Frame
				// A completed frame must be available before the next chunk or EOF.
				wantCount, end := 0, 0
				for _, frame := range tt.complete {
					end += len(frame.Body) + len(frame.Delimiter)
					if end <= cut {
						wantCount++
					}
				}
				complete = append(complete, scanner.Feed([]byte(tt.wire[:cut]))...)
				if len(complete) != wantCount {
					t.Fatalf("cut %d: expected %d frames before next chunk, got %d", cut, wantCount, len(complete))
				}
				complete = append(complete, scanner.Feed([]byte(tt.wire[cut:]))...)
				if diff := gocmp.Diff(tt.complete, complete); diff != "" {
					t.Fatalf("cut %d, completed (-want +got):\n%s", cut, diff)
				}
				var tail []sse.Frame
				tail = append(tail, scanner.End()...)
				if diff := gocmp.Diff(tt.tail, tail); diff != "" {
					t.Fatalf("cut %d, tail (-want +got):\n%s", cut, diff)
				}
				if n := len(scanner.End()); n != 0 {
					t.Fatalf("second End returned %d frames", n)
				}
			}
		})
	}
}

func TestFrameData(t *testing.T) {
	tests := map[string]struct {
		frame     sse.Frame
		data      string
		found     bool
		rewritten string
	}{
		"success: rewrite first data line with original delimiter": {sse.Frame{Body: ": 日本語\r\nevent: item\r\ndata:\t{ }\r\ndata: second", Delimiter: "\r\n\r\n"}, "\t{ }", true, ": 日本語\nevent: item\ndata: {}\ndata: second\r\n\r\n"},
		"success: unterminated rewritten tail":                     {sse.Frame{Body: "id: 7\r\ndata:  { }"}, "  { }", true, "id: 7\ndata: {}"},
		"success: data prefix is case and column sensitive":        {sse.Frame{Body: ": data:x\r\n data:x\r\nData:x", Delimiter: "\n\n"}, "", false, ": data:x\r\n data:x\r\nData:x\n\n"},
		// The relay leaves empty data unchanged; WithData only applies an explicitly requested rewrite.
		"success: WithData replaces an empty first data line": {sse.Frame{Body: "data:\r\ndata: later", Delimiter: "\r\n\n"}, "", true, "data: {}\ndata: later\r\n\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := tt.frame.Data()
			if ok != tt.found || got != tt.data {
				t.Errorf("Data() = %q, %t; want %q, %t", got, ok, tt.data, tt.found)
			}
			if diff := gocmp.Diff(tt.rewritten, tt.frame.WithData("{}")); diff != "" {
				t.Errorf("rewritten bytes (-want +got):\n%s", diff)
			}
		})
	}
}

func FuzzFrameRoundTrip(f *testing.F) {
	for _, wire := range []string{"", mixedFrames, "data:\n\n", "id: 7\r\ndata:  old", "data: bad\xff\r\n\r\n"} {
		f.Add([]byte(wire), []byte{0, 1, 4})
	}
	f.Fuzz(func(t *testing.T, wire, chunks []byte) {
		var scanner sse.Scanner
		var out bytes.Buffer
		write := func(frames []sse.Frame) {
			for _, frame := range frames {
				out.WriteString(frame.Body)
				out.WriteString(frame.Delimiter)
			}
		}
		for i, pos := 0, 0; pos < len(wire); i++ {
			n := len(wire) - pos
			if len(chunks) != 0 {
				n = min(n, 1+int(chunks[i%len(chunks)]))
			}
			write(scanner.Feed(wire[pos : pos+n]))
			pos += n
		}
		write(scanner.End())
		if !bytes.Equal(wire, out.Bytes()) {
			t.Fatalf("untouched bytes differ (-input +output):\n%s", gocmp.Diff(string(wire), out.String()))
		}
	})
}
