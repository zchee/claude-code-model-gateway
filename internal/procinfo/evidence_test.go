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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRecordRefusals(t *testing.T) {
	tests := map[string]struct {
		entry     string
		mirror    bool
		directory bool
	}{"error: linked authoritative record": {entry: "guardian.pid.json"}, "error: linked mirror": {entry: "guardian.pid", mirror: true}, "error: directory mirror": {entry: "guardian.pid", mirror: true, directory: true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			r, wire := recordFixture(root)
			jsonPath := filepath.Join(root, "guardian.pid.json")
			if tt.mirror {
				putFile(t, jsonPath, wire)
			}
			entry := filepath.Join(root, tt.entry)
			outside := filepath.Join(t.TempDir(), "target")
			putFile(t, outside, wire)
			var err error
			if tt.directory {
				err = os.Mkdir(entry, 0o700)
			} else {
				err = os.Symlink(outside, entry)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadRecord(root, "guardian")
			want := Record{}
			if tt.mirror {
				want = r
			}
			if !errors.Is(err, ErrRecordEntry) || errors.Is(err, ErrMirrorRepair) != tt.mirror {
				t.Fatalf("read refusal: %v", err)
			}
			if d := gocmp.Diff(want, got); d != "" {
				t.Fatal(d)
			}
			err = WriteRecord(root, "guardian", r)
			if !errors.Is(err, ErrRecordEntry) || errors.Is(err, ErrMirrorPublish) != tt.mirror {
				t.Fatalf("write refusal: %v", err)
			}
			if got := readFile(t, outside); got != wire {
				t.Fatal("symlink target changed")
			}
		})
	}
}

func TestRecordValidation(t *testing.T) {
	tests := map[string]struct {
		start   string
		unclean bool
		want    error
	}{"error: unclean root": {unclean: true, want: ErrStateRoot}, "error: weekday disagrees": {start: "Sun Feb  3 22:15:09 2001", want: ErrStartedAt}, "error: unpadded day": {start: "Sat Feb 3 22:15:09 2001", want: ErrStartedAt}, "error: malformed start": {start: "unknown", want: ErrStartedAt}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			r, wire := recordFixture(root)
			if tt.unclean {
				root += string(os.PathSeparator) + "."
			} else {
				wire = strings.Replace(wire, r.StartedAt, tt.start, 1)
				r.StartedAt = tt.start
			}
			putFile(t, filepath.Join(root, "guardian.pid.json"), wire)
			if err := WriteRecord(root, "guardian", r); !errors.Is(err, tt.want) {
				t.Errorf("write error=%v want %v", err, tt.want)
			}
			if _, err := ReadRecord(root, "guardian"); !errors.Is(err, tt.want) {
				t.Errorf("read error=%v want %v", err, tt.want)
			}
		})
	}
}

func TestProcArgsBoundaries(t *testing.T) {
	// The path and its NUL plus the stripped executable_path= prefix use 23
	// bytes, so XNU adds exactly one zero byte before the argument strings.
	base := []byte("\x02\x00\x00\x00/bin/x\x00\x00")
	tests := map[string]struct {
		tail string
		want []string
		err  error
	}{"success: arguments exclude environment": {tail: "x\x00arg\x00PRIVATE=value\x00", want: []string{"x", "arg"}}, "success: empty second argument": {tail: "x\x00\x00PRIVATE=value\x00", want: []string{"x", ""}}, "error: empty first argument is not padding": {tail: "\x00x\x00PRIVATE=value\x00", err: ErrUnknown}, "error: missing argument terminator": {tail: "x\x00arg", err: ErrUnknown}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, got, err := decodeProcArgs(append(bytes.Clone(base), tt.tail...))
			if !errors.Is(err, tt.err) {
				t.Errorf("decode error=%v want %v", err, tt.err)
			}
			if d := gocmp.Diff(tt.want, got); d != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestNetstatLayout(t *testing.T) {
	const hdr = "Proto Recv-Q Send-Q Local Address Foreign Address (state)\n"
	tests := map[string]struct {
		text  string
		count int
		err   error
	}{"success: known header and no rows": {text: hdr}, "success: listen and established": {text: hdr + "tcp4 0 0 127.0.0.1.27000 *.* LISTEN\ntcp6 0 3 ::1.27000 ::1.27001 ESTABLISHED", count: 1}, "error: unknown header": {text: "another table", err: ErrUnknown}, "error: unknown state": {text: hdr + "tcp4 0 0 *.27000 *.* FUTURE", err: ErrUnknown}, "error: truncated row": {text: hdr + "tcp6 0", err: ErrUnknown}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := countTCPListeners([]byte(tt.text), 27000)
			if got != tt.count || !errors.Is(err, tt.err) {
				t.Errorf("got %d,%v want %d,%v", got, err, tt.count, tt.err)
			}
		})
	}
}

func TestListenerFiles(t *testing.T) {
	const first = "p17\x00\nf5\x00d0x12\x00\n"
	tests := map[string]struct {
		text  string
		count int
		want  Listener
	}{"success: same holder two sockets": {first + "f6\x00d0x13\x00\n", 2, Listener{ListenerIdentified, 17}}, "success: inherited socket": {first + "p18\x00\nf4\x00d0x12\x00\n", 1, Listener{State: ListenerAmbiguous}}, "success: duplicate descriptors leave one hidden socket": {first + "f6\x00d0x12\x00\n", 2, Listener{State: ListenerAmbiguous}}, "error: incomplete identifier": {first + "f9\x00\n", 2, Listener{State: ListenerUnknown}}, "error: truncated output": {strings.TrimSuffix(first, "\x00\n"), 1, Listener{State: ListenerUnknown}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rows, err := listenerFiles([]byte(tt.text), 27000, tt.count)
			if d := gocmp.Diff(tt.want, classifyListener(rows, err, 27000)); d != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestTableBirthRecheck(t *testing.T) {
	before := Identity{PID: 701, ParentPID: 1, Birth: time.Unix(99, 0)}
	newer := before
	newer.Birth = time.Unix(100, 0)
	tests := map[string]struct {
		after      Identity
		keep, args bool
	}{"success: matching birth": {before, true, true}, "error: recycled process": {newer, false, false}, "error: unreadable second sample": {Identity{}, true, false}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r, keep := checkedTableRow(before, tt.after, []string{"worker"})
			if keep != tt.keep || (r.Argv != nil) != tt.args {
				t.Errorf("row=%+v keep=%t", r, keep)
			}
		})
	}
}

func TestEmptyArgvIdentity(t *testing.T) {
	tests := map[string]struct{ argv []string }{"error: absent argv": {}, "error: empty first argument": {[]string{"", "worker"}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := Probe{inspect: func(context.Context, int) (Identity, error) {
				return Identity{PID: 100, Argv: tt.argv, Birth: time.Unix(99, 0)}, nil
			}, existence: func(context.Context, int) error { return nil }}
			_, err := p.Identity(t.Context(), 100)
			if !errors.Is(err, ErrUnknown) {
				t.Fatalf("got %v want unknown", err)
			}
		})
	}
}
