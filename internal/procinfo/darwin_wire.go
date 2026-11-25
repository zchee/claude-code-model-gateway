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

package procinfo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// stderrSize counts and discards tool diagnostics. Even the first line can
// contain names from unrelated mounts or processes; retain no bytes.
type stderrSize uint64

func (s *stderrSize) Write(data []byte) (int, error) {
	*s += stderrSize(len(data))
	return len(data), nil
}

func (s stderrSize) diagnostic() string {
	if s == 0 {
		return ""
	}
	return fmt.Sprintf(", stderr present (%d bytes)", s)
}

// decodeProcArgs reads the 64-bit XNU kern.procargs2 layout. XNU aligns the
// original executable_path= string before argv, but strips its prefix on export.
// Skipping an arbitrary run of NULs would mistake an empty argv[0] for padding.
func decodeProcArgs(raw []byte) (string, []string, error) {
	if len(raw) < 5 {
		return "", nil, fmt.Errorf("missing argc/path: %w", ErrUnknown)
	}
	argc := int(binary.LittleEndian.Uint32(raw))
	path, rest, ok := bytes.Cut(raw[4:], []byte{0})
	if !ok || len(path) == 0 || argc < 1 {
		return "", nil, fmt.Errorf("invalid argc or executable path terminator: %w", ErrUnknown)
	}
	pad := -(len("executable_path=") + len(path) + 1) & 7
	if len(rest) <= pad || len(bytes.Trim(rest[:pad], "\x00")) != 0 || rest[pad] == 0 || argc > len(rest)-pad {
		return "", nil, fmt.Errorf("invalid argv boundary (argc %d, path length %d, expected padding %d): %w", argc, len(path), pad, ErrUnknown)
	}
	rest = rest[pad:]
	args := make([]string, 0, min(argc, 32))
	for range argc {
		arg, next, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return "", nil, ErrUnknown
		}
		args = append(args, string(arg))
		rest = next
	}
	return string(path), args, nil
}

// checkedTableRow only joins argv to the original birth. If the second birth
// is unreadable, the original snapshot still proves the process was seen.
func checkedTableRow(before, after Identity, argv []string) (Identity, bool) {
	if after.PID != 0 {
		if before.PID != after.PID || !before.Birth.Equal(after.Birth) {
			return Identity{}, false
		}
		before.Argv = argv
	}
	return before, true
}

func mappedImage(out []byte, pid int, path string) (Image, error) {
	if !bytes.HasSuffix(out, []byte{0, '\n'}) {
		return Image{}, fmt.Errorf("truncated lsof output: %w", ErrUnknown)
	}
	var match, current Image
	var seenPID, text, device bool
	var mappings int
	finish := func() error {
		if !text || current.Path != path {
			return nil
		}
		if !device || current.Inode == 0 || match.Inode != 0 && current != match {
			return fmt.Errorf("incomplete or conflicting vnode: %w", ErrUnknown)
		}
		match = current
		return nil
	}
	for field := range bytes.SplitSeq(out, []byte{0}) {
		field = bytes.TrimPrefix(field, []byte{'\n'})
		if len(field) == 0 {
			continue
		}
		value := string(field[1:])
		switch field[0] {
		case 'p':
			if seenPID || value != strconv.Itoa(pid) {
				return Image{}, fmt.Errorf("lsof pid mismatch: %w", ErrUnknown)
			}
			seenPID = true
		case 'f':
			if err := finish(); err != nil {
				return Image{}, err
			}
			current = Image{}
			text = value == "txt"
			device = false
			if text {
				mappings++
			}
		case 'D':
			n, err := strconv.ParseUint(value, 0, 64)
			if err != nil {
				return Image{}, fmt.Errorf("invalid vnode device: %w", ErrUnknown)
			}
			current.Device = n
			device = true
		case 'i':
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return Image{}, fmt.Errorf("invalid vnode inode: %w", ErrUnknown)
			}
			current.Inode = n
		case 'n':
			current.Path = value
		default:
			return Image{}, fmt.Errorf("unexpected lsof field %q: %w", field[0], ErrUnknown)
		}
	}
	if err := finish(); err != nil {
		return Image{}, err
	}
	if !seenPID || match.Inode == 0 {
		return Image{}, fmt.Errorf("no executable match across %d txt records: %w", mappings, ErrUnknown)
	}
	return match, nil
}

// lsofListener decodes a stderr-free listing filtered to one TCP listening port.
// Validate every process and file set before interpreting any ownership evidence.
func lsofListener(out []byte, status int) (Listener, error) {
	unknown := Listener{State: ListenerUnknown}
	if status == 1 && len(out) == 0 {
		return Listener{State: ListenerAbsent}, nil
	}
	if status != 0 {
		return unknown, fmt.Errorf("lsof listener status %d: %w", status, ErrUnknown)
	}
	records, ok := strings.CutSuffix(string(out), "\x00\n")
	if !ok {
		return unknown, fmt.Errorf("incomplete lsof listener listing: %w", ErrUnknown)
	}
	var pid, owner int
	var listed, ambiguous bool
	for record := range strings.SplitSeq(records, "\x00\n") {
		if val, ok := strings.CutPrefix(record, "p"); ok {
			n, err := strconv.ParseUint(val, 10, 31)
			if err != nil || n == 0 || pid != 0 && !listed {
				return unknown, fmt.Errorf("invalid lsof process set: %w", ErrUnknown)
			}
			pid = int(n)
			listed = false
			continue
		}
		fd, dev, ok := strings.Cut(record, "\x00d0x")
		fd, hasFD := strings.CutPrefix(fd, "f")
		if !ok || !hasFD || pid == 0 {
			return unknown, fmt.Errorf("invalid lsof file set: %w", ErrUnknown)
		}
		if _, err := strconv.ParseUint(fd, 10, 32); err != nil {
			return unknown, fmt.Errorf("invalid lsof descriptor: %w", ErrUnknown)
		}
		socket, err := strconv.ParseUint(dev, 16, 64)
		if err != nil || socket == 0 {
			return unknown, fmt.Errorf("invalid lsof socket identifier: %w", ErrUnknown)
		}
		if owner == 0 {
			owner = pid
		} else if owner != pid {
			ambiguous = true
		}
		listed = true
	}
	if !listed {
		return unknown, fmt.Errorf("lsof process has no file set: %w", ErrUnknown)
	}
	if ambiguous {
		return Listener{State: ListenerAmbiguous}, nil
	}
	return Listener{State: ListenerIdentified, PID: owner}, nil
}
