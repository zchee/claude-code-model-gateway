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

//go:build darwin && process

package procinfo

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinUnlinkedImageEvidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private-cat")
	data, err := os.ReadFile("/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	original := st.Sys().(*syscall.Stat_t)
	pid := startChild(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	id, inspectErr := (Probe{}).Identity(t.Context(), pid)
	if inspectErr == nil && id.Image.Inode == original.Ino && id.Image.Device == uint64(original.Dev) {
		return
	}
	// This opt-in test diagnostic is confined to the child and its mappings. It
	// never reads argv or the environment and never includes raw tool stderr.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	out, err := darwinCommand(ctx, "/usr/sbin/lsof", "+w", "-nP", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F0pDfni")
	if err != nil {
		t.Fatalf("inspect=%v; txt census: %v", inspectErr, err)
	}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	var fields, device, inode, name string
	logMapping := func() {
		if fields == "" {
			return
		}
		kind, display := "relative", name
		switch {
		case name == "":
			kind = "empty"
		case filepath.IsAbs(name):
			kind = "absolute"
		}
		alias := false
		if kind == "absolute" {
			parent, e := filepath.EvalSymlinks(filepath.Dir(name))
			alias = e == nil && parent == canonicalDir && filepath.Base(name) == filepath.Base(path) && name != path
			switch {
			case parent == canonicalDir:
				display = "<fixture>/" + filepath.Base(name)
			case strings.HasPrefix(name, "/System/") || strings.HasPrefix(name, "/usr/lib/") || strings.HasPrefix(name, "/.vol/"):
			default:
				display = "<other>/" + filepath.Base(name)
			}
		}
		t.Logf("txt fields=%q device=%q inode=%q name=%q kind=%s same-name=%t symlink-prefix-only=%t", fields, device, inode, display, kind, name == path, alias)
	}
	for f := range bytes.SplitSeq(out, []byte{0}) {
		f = bytes.TrimPrefix(f, []byte{'\n'})
		if len(f) == 0 || f[0] == 'p' {
			continue
		}
		if f[0] == 'f' {
			logMapping()
			fields, device, inode, name = "", "", "", ""
		}
		fields += string(f[:1])
		switch f[0] {
		case 'D':
			device = string(f[1:])
		case 'i':
			inode = string(f[1:])
		case 'n':
			name = string(f[1:])
		}
	}
	logMapping()
	t.Fatalf("unlinked fixture: want device=%#x inode=%d; got device=%#x inode=%d; inspect=%v", original.Dev, original.Ino, id.Image.Device, id.Image.Inode, inspectErr)
}

func TestDarwinShortLivedCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := darwinCommand(ctx, "/usr/bin/true"); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinListenerEvidence(t *testing.T) {
	l := listenerOn(t, net.ListenConfig{}, "127.0.0.1", 0)
	defer func() { _ = l.Close() }()
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	got, err := (Probe{}).ListenerEvidence(t.Context(), port)
	if err != nil || got != (Listener{ListenerIdentified, os.Getpid()}) {
		t.Fatalf("private listener on %d: state=%s pid=%d: %v", port, got.State, got.PID, err)
	}
}
