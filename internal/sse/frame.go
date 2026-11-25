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

// Frame contains the bytes preceding a blank line and the matched blank-line delimiter.
// A nonempty unterminated tail has an empty delimiter.
type Frame struct {
	Body, Delimiter string
}

// Data returns the bytes after the first line-leading "data:", without trimming them.
func (f Frame) Data() (string, bool) { return "", false }

// WithData replaces the first data line by "data: " plus payload, joins the lines with LF,
// and restores the original delimiter. A frame without a data line is unchanged.
func (f Frame) WithData(payload string) string { return "" }

// Scanner buffers incomplete frames between chunks. Its zero value is ready to use.
type Scanner struct{}

// Feed returns every frame completed by chunk, in wire order.
func (s *Scanner) Feed(chunk []byte) []Frame { return nil }

// End returns the remaining unterminated frame, if any, and drains the scanner.
func (s *Scanner) End() []Frame { return nil }
