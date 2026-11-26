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

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	textunicode "golang.org/x/text/encoding/unicode"
)

const (
	compatHost = "api.anthropic.com"
	blockStart = "# >>> model-gateway RC compatibility >>>"
	blockEnd   = "# <<< model-gateway RC compatibility <<<"
	blockEntry = "127.0.0.1 " + compatHost
)

var surplusNewlines = regexp.MustCompile(`(?:\r?\n){3,}`)

// Entry records a matched loopback address and its trimmed original line.
type Entry struct {
	IP, Line string
}

// jsSpace matches the whitespace used by JavaScript trim and \s.
func jsSpace(r rune) bool {
	return r == 0xFEFF || (r != 0x85 && unicode.IsSpace(r))
}

func compatMapping(rawLine string) string {
	line, _, _ := strings.Cut(rawLine, "#")
	var ip string
	for field := range strings.FieldsFuncSeq(line, jsSpace) {
		if ip == "" {
			ip = field
			continue
		}
		if strings.EqualFold(strings.TrimSuffix(field, "."), compatHost) {
			return ip
		}
	}
	return ""
}

func loopback(ip string) bool { return ip == "127.0.0.1" || ip == "::1" }

// ParseEntry returns the first compatible entry, or nil when none matches.
func ParseEntry(text string) *Entry {
	for line := range strings.SplitSeq(text, "\n") {
		if ip := compatMapping(line); loopback(ip) {
			return &Entry{IP: ip, Line: strings.TrimFunc(line, jsSpace)}
		}
	}
	return nil
}

// Detect reads only path through readFile, returning nil on read failure or no match.
func Detect(path string, readFile func(string) ([]byte, error)) *Entry {
	text, err := readFile(path)
	if err != nil {
		return nil
	}
	if !utf8.Valid(text) {
		text, err = textunicode.UTF8.NewDecoder().Bytes(text)
		if err != nil {
			return nil
		}
	}
	return ParseEntry(string(text))
}

func parseBlock(text string) (state, before, after string) {
	start := strings.Index(text, blockStart)
	end := strings.Index(text, blockEnd)
	if start < 0 && end < 0 {
		return "absent", "", ""
	}
	if start < 0 || end < start {
		return "partial", "", ""
	}
	end += len(blockEnd)
	if ParseEntry(text[start:end]) != nil {
		return "valid", text[:start], text[end:]
	}
	return "invalid", text[:start], text[end:]
}

// BlockState classifies the block using the first occurrence of each marker
// independently. If the first end marker precedes the first start marker, the
// block is partial.
func BlockState(text string) string {
	state, _, _ := parseBlock(text)
	return state
}

func malformedBlock(state string) error {
	return fmt.Errorf("plugin-marked hosts block is %s; run remote-control doctor and repair it manually", state)
}

// AddBlock returns the transformed text and whether it changed, refusing malformed blocks.
func AddBlock(text string) (string, bool, error) {
	switch state := BlockState(text); state {
	case "valid":
		return text, false, nil
	case "absent":
	default:
		return "", false, malformedBlock(state)
	}

	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	var start int
	for line := range strings.SplitAfterSeq(text, "\n") {
		rawLine, terminated := strings.CutSuffix(line, "\n")
		if terminated {
			rawLine = strings.TrimSuffix(rawLine, "\r")
		}
		if loopback(compatMapping(rawLine)) {
			return text[:start] + blockStart + eol + rawLine + eol + blockEnd + eol + text[start+len(line):], true, nil
		}
		start += len(line)
	}

	separator := ""
	if text != "" && !strings.HasSuffix(text, "\n") {
		separator = eol
	}
	return text + separator + blockStart + eol + blockEntry + eol + blockEnd + eol, true, nil
}

// RemoveBlock returns the remaining text and whether it changed, refusing malformed blocks.
func RemoveBlock(text string) (string, bool, error) {
	state, before, after := parseBlock(text)
	switch state {
	case "absent":
		return text, false, nil
	case "valid":
	default:
		return "", false, malformedBlock(state)
	}

	if rest, ok := strings.CutPrefix(after, "\r\n"); ok {
		after = rest
	} else {
		after = strings.TrimPrefix(after, "\n")
	}
	// Match the original removal behavior, including newline runs away from the splice.
	remaining := surplusNewlines.ReplaceAllStringFunc(before+after, func(run string) string {
		if strings.Contains(run, "\r\n") {
			return "\r\n\r\n"
		}
		return "\n\n"
	})
	return remaining, true, nil
}

// Conflicts returns the non-loopback mappings for the compatibility hostname in file order.
func Conflicts(text string) []string {
	conflicts := []string{}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, blockStart) || strings.Contains(line, blockEnd) {
			continue
		}
		if ip := compatMapping(line); ip != "" && !loopback(ip) {
			conflicts = append(conflicts, strings.TrimFunc(line, jsSpace))
		}
	}
	return conflicts
}
