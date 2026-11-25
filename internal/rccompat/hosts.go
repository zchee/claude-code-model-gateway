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

// Package rccompat interprets hosts-file text for remote-control compatibility.
package rccompat

import "errors"

// Entry records a matched loopback address and its trimmed original line.
type Entry struct {
	IP, Line string
}

// ParseEntry returns the first compatible entry, or nil when none matches.
func ParseEntry(text string) *Entry { return nil }

// Detect reads only path through readFile, returning nil on read failure or no match.
func Detect(path string, readFile func(string) ([]byte, error)) *Entry { return nil }

// BlockState classifies the first marker pair as absent, partial, valid or invalid.
func BlockState(text string) string { return "" }

// AddBlock returns the transformed text and whether it changed, refusing malformed blocks.
func AddBlock(text string) (string, bool, error) { return "", false, errors.New("not implemented") }

// RemoveBlock returns the remaining text and whether it changed, refusing malformed blocks.
func RemoveBlock(text string) (string, bool, error) { return "", false, errors.New("not implemented") }

// Conflicts returns the non-loopback mappings for the compatibility hostname in file order.
func Conflicts(text string) []string { return nil }

// UnsafeProcessOverride returns the original process base URL when it is unsafe for compatibility, or nil.
func UnsafeProcessOverride(baseURL string) *string { return nil }
