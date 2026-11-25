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
	"os"
	"path/filepath"
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

func darwinImageEvidence(pid int, kernelPath, launchPath string, list func() ([]byte, error)) (ImageEvidence, error) {
	path := kernelPath
	if path == "" {
		path = launchPath
	}
	img, err := mappedImage(pid, path, list)
	if err != nil {
		return ImageEvidence{}, err
	}
	return ImageEvidence{Image: img, KernelNamed: kernelPath != ""}, nil
}

// mappedImage holds the executable's parent directory across the vnode query.
// Match only an exact leaf name whose parent resolves to that same directory;
// neither spelling of the executable leaf is statted or opened.
func mappedImage(pid int, path string, list func() ([]byte, error)) (Image, error) {
	parent, leaf := filepath.Split(path)
	if !filepath.IsAbs(path) || leaf == "" || leaf == "." || leaf == ".." {
		return Image{}, fmt.Errorf("invalid executable path: %w", ErrUnknown)
	}
	dir, err := os.Open(parent)
	if err != nil {
		return Image{}, fmt.Errorf("open executable parent: %w", ErrUnknown)
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		return Image{}, fmt.Errorf("inspect executable parent: %w", ErrUnknown)
	}
	out, err := list()
	if err != nil {
		return Image{}, err
	}
	listing, ok := strings.CutSuffix(string(out), "\x00\n")
	if !ok {
		return Image{}, fmt.Errorf("truncated lsof output: %w", ErrUnknown)
	}
	process, rows, ok := strings.Cut(listing, "\x00\n")
	if !ok || process != "p"+strconv.Itoa(pid) {
		return Image{}, fmt.Errorf("invalid lsof process set: %w", ErrUnknown)
	}
	var match Image
	for row := range strings.SplitSeq(rows, "\x00\n") {
		// An incomplete row cannot be ruled out, even if another row matches.
		item, err := parseTextVnode(row)
		if err != nil {
			return Image{}, err
		}
		otherDir, otherLeaf := filepath.Split(item.Path)
		if otherLeaf != leaf {
			continue
		}
		other, err := os.Stat(otherDir)
		if err != nil {
			return Image{}, fmt.Errorf("inspect mapped vnode parent: %w", ErrUnknown)
		}
		if !os.SameFile(info, other) {
			// The launch alias may now point at a different mapped file. Do not
			// discard this same-leaf candidate in favor of the current directory.
			return Image{}, fmt.Errorf("txt leaf in a different parent: %w", ErrUnknown)
		}
		if match.Inode != 0 {
			return Image{}, fmt.Errorf("multiple matching txt vnodes: %w", ErrUnknown)
		}
		match = item
	}
	if match.Inode == 0 {
		return Image{}, fmt.Errorf("no matching txt vnode: %w", ErrUnknown)
	}
	return match, nil
}

func parseTextVnode(row string) (Image, error) {
	fields, ok := strings.CutPrefix(row, "ftxt\x00")
	if !ok {
		return Image{}, fmt.Errorf("invalid lsof txt set: %w", ErrUnknown)
	}
	var v Image
	var device bool
	for f := range strings.SplitSeq(fields, "\x00") {
		if len(f) < 2 {
			return Image{}, ErrUnknown
		}
		switch f[0] {
		case 'D':
			n, err := strconv.ParseUint(f[1:], 0, 64)
			if err != nil || device {
				return Image{}, fmt.Errorf("invalid vnode device: %w", ErrUnknown)
			}
			v.Device = n
			device = true
		case 'i':
			n, err := strconv.ParseUint(f[1:], 10, 64)
			if err != nil || n == 0 || v.Inode != 0 {
				return Image{}, fmt.Errorf("invalid vnode inode: %w", ErrUnknown)
			}
			v.Inode = n
		case 'n':
			if v.Path != "" || !filepath.IsAbs(f[1:]) {
				return Image{}, fmt.Errorf("invalid vnode name: %w", ErrUnknown)
			}
			v.Path = f[1:]
		default:
			return Image{}, fmt.Errorf("unexpected lsof txt field: %w", ErrUnknown)
		}
	}
	if !device || v.Inode == 0 || v.Path == "" {
		return Image{}, fmt.Errorf("incomplete mapped vnode: %w", ErrUnknown)
	}
	return v, nil
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
