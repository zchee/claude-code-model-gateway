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

// Package sse splits event streams without normalizing their wire bytes.
package sse

import (
	"bytes"
	"strings"
)

// Frame contains the bytes preceding a blank line and the matched blank-line delimiter.
// A nonempty unterminated tail has an empty delimiter.
type Frame struct {
	Body, Delimiter string
}

// dataLine splits the body around the first data field, leaving its line ending in after.
func (f Frame) dataLine() (before, data, after string, ok bool) {
	const field = "data:"
	rest, found := strings.CutPrefix(f.Body, field)
	if !found {
		prefix, value, found := strings.Cut(f.Body, "\n"+field)
		if !found {
			return "", "", "", false
		}
		before, rest = f.Body[:len(prefix)+1], value
	}
	line, _, terminated := strings.Cut(rest, "\n")
	if terminated {
		line = strings.TrimSuffix(line, "\r")
	}
	return before, line, rest[len(line):], true
}

// Data returns the bytes after the first line-leading "data:", without trimming them.
func (f Frame) Data() (string, bool) {
	_, data, _, ok := f.dataLine()
	return data, ok
}

// WithData replaces the first data line by "data: " plus payload, joins the lines with LF,
// and restores the original delimiter. A frame without a data line is unchanged.
func (f Frame) WithData(payload string) string {
	before, _, after, ok := f.dataLine()
	if !ok {
		return f.Body + f.Delimiter
	}
	return strings.ReplaceAll(before, "\r\n", "\n") + "data: " + payload + strings.ReplaceAll(after, "\r\n", "\n") + f.Delimiter
}

// Scanner buffers incomplete frames between chunks. Its zero value is ready to use.
// A Scanner must not be copied after first use.
type Scanner struct {
	pending strings.Builder
}

// Feed returns every frame completed by chunk, in wire order.
func (s *Scanner) Feed(chunk []byte) []Frame {
	var frames []Frame
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			s.pending.Write(chunk)
			break
		}
		s.pending.Write(chunk[:i+1])
		chunk = chunk[i+1:]

		// Only the new LF can finish a delimiter. Prefer the longest spelling
		// so the CR of the first CRLF belongs to the delimiter, not the body.
		wire := s.pending.String()
		for _, delim := range [...]string{"\r\n\r\n", "\n\r\n", "\r\n\n", "\n\n"} {
			if body, ok := strings.CutSuffix(wire, delim); ok {
				frames = append(frames, Frame{Body: body, Delimiter: delim})
				s.pending.Reset()
				break
			}
		}
	}
	return frames
}

// End returns the remaining unterminated frame, if any, and drains the scanner.
func (s *Scanner) End() []Frame {
	if s.pending.Len() == 0 {
		return nil
	}
	tail := s.pending.String()
	s.pending.Reset()
	return []Frame{{Body: tail}}
}
