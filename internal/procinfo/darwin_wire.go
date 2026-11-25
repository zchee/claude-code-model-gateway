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

// commandDiagnostic includes only a bounded, quoted first stderr line, never
// the probe's arguments, environment, or stdout (which may contain other pids).
func commandDiagnostic(stderr []byte) string {
	if len(stderr) == 0 {
		return ""
	}
	line, _, _ := bytes.Cut(stderr, []byte{'\n'})
	return fmt.Sprintf(", stderr %q", line[:min(len(line), 256)])
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

func countTCPListeners(out []byte, port uint16) (int, error) {
	var header bool
	count := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if !header {
			switch strings.Join(f, " ") {
			case "Active Internet connections (including servers)":
				continue
			case "Proto Recv-Q Send-Q Local Address Foreign Address (state)":
				header = true
				continue
			default:
				return 0, ErrUnknown
			}
		}
		if len(f) != 6 {
			return 0, ErrUnknown
		}
		switch f[0] {
		case "tcp4", "tcp6", "tcp46":
		default:
			return 0, ErrUnknown
		}
		for _, q := range f[1:3] {
			if _, err := strconv.ParseUint(q, 10, 64); err != nil {
				return 0, ErrUnknown
			}
		}
		_, suffix, ok := strings.CutLast(f[3], ".")
		if !ok {
			return 0, ErrUnknown
		}
		var local uint64
		if suffix != "*" {
			var err error
			local, err = strconv.ParseUint(suffix, 10, 16)
			if err != nil {
				return 0, ErrUnknown
			}
		}
		switch f[5] {
		case "LISTEN":
			if local == 0 {
				return 0, ErrUnknown
			}
			if uint16(local) == port {
				count++
			}
		case "CLOSED", "SYN_SENT", "SYN_RCVD", "ESTABLISHED", "CLOSE_WAIT", "FIN_WAIT_1", "CLOSING", "LAST_ACK", "FIN_WAIT_2", "TIME_WAIT":
		default:
			return 0, ErrUnknown
		}
	}
	if !header {
		return 0, fmt.Errorf("no TCP table header: %w", ErrUnknown)
	}
	return count, nil
}

// listenerFiles joins visible descriptors by the kernel socket identifier in
// lsof's device character field. Count includes sockets whose holders are hidden.
func listenerFiles(out []byte, port uint16, count int) ([]socketEvidence, error) {
	rows := make([]socketEvidence, count)
	for i := range rows {
		rows[i] = socketEvidence{port: port, listening: true}
	}
	if len(out) == 0 {
		return rows, nil
	}
	indices := make(map[uint64]int)
	var pid int
	var file, device, listed bool
	// Field mode terminates each field with NUL and each process/file set with LF.
	if !bytes.HasSuffix(out, []byte{0, '\n'}) {
		return nil, ErrUnknown
	}
	for field := range bytes.SplitSeq(out, []byte{0}) {
		field = bytes.TrimPrefix(field, []byte{'\n'})
		if len(field) == 0 {
			continue
		}
		data := string(field[1:])
		switch field[0] {
		case 'p':
			if file && !device || pid != 0 && !listed {
				return nil, ErrUnknown
			}
			n, err := strconv.ParseInt(data, 10, 32)
			if err != nil || n <= 0 {
				return nil, ErrUnknown
			}
			pid = int(n)
			file = false
			listed = false
		case 'f':
			if pid == 0 || file && !device {
				return nil, ErrUnknown
			}
			if _, err := strconv.ParseUint(data, 10, 32); err != nil {
				return nil, ErrUnknown
			}
			file = true
			device = false
			listed = true
		case 'd':
			if !file || device {
				return nil, ErrUnknown
			}
			v, ok := strings.CutPrefix(data, "0x")
			if !ok {
				return nil, ErrUnknown
			}
			key, err := strconv.ParseUint(v, 16, 64)
			if err != nil || key == 0 {
				return nil, ErrUnknown
			}
			i, ok := indices[key]
			if !ok {
				i = len(indices)
				if i >= count {
					return nil, ErrUnknown
				}
				indices[key] = i
			}
			rows[i].owners = append(rows[i].owners, pid)
			device = true
		default:
			return nil, ErrUnknown
		}
	}
	if !file || !device {
		return nil, ErrUnknown
	}
	return rows, nil
}
