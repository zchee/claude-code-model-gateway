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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestDarwinMappedVnodeOutput(t *testing.T) {
	const pid = 701
	tests := map[string]struct {
		path, field string
		wantInode   uint64
	}{
		"success: running shell and another txt mapping":           {"/fixture/shell", "/fixture/shell", 1506186249},
		"success: renamed executable":                              {"/fixture/shell.moved", "/fixture/shell.moved", 1506186251},
		"success: unlinked executable retains its path":            {"/fixture/shell", "/fixture/shell", 1506186253},
		"success: overwritten executable retains its mapped inode": {"/fixture/shell", "/fixture/shell", 1506186255},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, filepath.Base(tt.path))
			field := filepath.Join(dir, filepath.Base(tt.field))
			data := []byte(fmt.Sprintf("p701\x00\nftxt\x00D0x100000d\x00i%d\x00n%s\x00\nftxt\x00D0x100000d\x00i1152921500312633471\x00n/usr/share/locale/en_US.UTF-8/LC_COLLATE\x00\n", tt.wantInode, field))
			got, err := mappedImage(pid, path, func() ([]byte, error) { return data, nil })
			if err != nil {
				t.Fatal(err)
			}
			if d := gocmp.Diff(Image{field, 0x100000d, tt.wantInode}, got); d != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestUnlinkedMappedVnode(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "private")
	other := filepath.Join(root, "other")
	alias := filepath.Join(root, "alias")
	for _, d := range []string{actual, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	const leaf = "private-cat"
	const loader = "ftxt\x00D0x100000f\x00i1152921500312609216\x00n/usr/lib/dyld\x00\n"
	row := func(parent string) string {
		return "ftxt\x00D0x100000f\x00i2120580\x00n" + filepath.Join(parent, leaf) + "\x00\n"
	}
	gone := filepath.Join(root, "gone")
	correct := Image{Path: filepath.Join(actual, leaf), Device: 0x100000f, Inode: 2120580}
	tests := map[string]struct {
		parent, rows string
		want         Image
		err          error
	}{
		"success: symlinked parent of unlinked executable":      {alias, row(actual) + loader, correct, nil},
		"error: same leaf in a different directory":             {alias, row(other) + loader, Image{}, ErrUnknown},
		"error: launch parent no longer exists":                 {gone, row(gone) + loader, Image{}, ErrUnknown},
		"error: mapped parent no longer exists":                 {alias, row(gone) + loader, Image{}, ErrUnknown},
		"error: two matching records even with identical inode": {alias, row(actual) + row(actual) + loader, Image{}, ErrUnknown},
		"error: loader is not an executable candidate":          {alias, loader, Image{}, ErrUnknown},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := darwinImageEvidence(701, "", filepath.Join(tt.parent, leaf), func() ([]byte, error) { return []byte("p701\x00\n" + tt.rows), nil })
			if !errors.Is(err, tt.err) {
				t.Fatalf("mapped image error=%v want %v", err, tt.err)
			}
			if d := gocmp.Diff(ImageEvidence{Image: tt.want, DirectFromExecutable: false}, got); d != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestRetargetedImageAlias(t *testing.T) {
	original, replacement := t.TempDir(), t.TempDir()
	linkRoot := t.TempDir()
	alias := filepath.Join(linkRoot, "install")
	if err := os.Symlink(original, alias); err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(alias, "tool")
	renamed := filepath.Join(original, "renamed")
	putFile(t, launch, "original")
	if err := os.Rename(launch, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias+".new"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(alias+".new", alias); err != nil {
		t.Fatal(err)
	}
	putFile(t, launch, "different mapping")
	newPath := filepath.Join(replacement, "tool")
	newImage := Image{Path: newPath, Device: 12, Inode: 42}
	row := func(path string, inode int) string {
		return fmt.Sprintf("ftxt\x00D0xc\x00i%d\x00n%s\x00\n", inode, path)
	}
	oldRow := row(filepath.Join(original, "tool"), 41)
	newRow := row(newPath, 42)
	tests := map[string]struct {
		rows string
		want ImageEvidence
		err  error
	}{
		"error: two launch leaves with old parent first":                       {rows: oldRow + newRow, err: ErrUnknown},
		"error: two launch leaves with current parent first":                   {rows: newRow + oldRow, err: ErrUnknown},
		"success: only replacement row visible is still correlated":            {rows: newRow, want: ImageEvidence{Image: newImage, DirectFromExecutable: false}},
		"success: sole launch leaf in retargeted directory is only correlated": {rows: row(renamed, 41) + newRow, want: ImageEvidence{Image: newImage, DirectFromExecutable: false}},
		"error: incomplete unrelated row cannot be ruled out":                  {rows: newRow + "ftxt\x00D0xc\x00n/usr/lib/dyld\x00\n", err: ErrUnknown},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := darwinImageEvidence(701, "", launch, func() ([]byte, error) { return []byte("p701\x00\n" + tt.rows), nil })
			if !errors.Is(err, tt.err) {
				t.Fatalf("error=%v want %v", err, tt.err)
			}
			if d := gocmp.Diff(tt.want, got); d != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestKernelPathRenamedBeforeVnodeList(t *testing.T) {
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "tool")
	replacement := filepath.Join(dir, "other")
	renamed := filepath.Join(dir, "renamed")
	putFile(t, kernelPath, "executable")
	putFile(t, replacement, "other mapping")
	got, err := darwinImageEvidence(701, kernelPath, "", func() ([]byte, error) {
		if err := os.Rename(kernelPath, renamed); err != nil {
			return nil, err
		}
		if err := os.Rename(replacement, kernelPath); err != nil {
			return nil, err
		}
		return []byte(fmt.Sprintf("p701\x00\nftxt\x00D0xc\x00i41\x00n%s\x00\nftxt\x00D0xc\x00i42\x00n%s\x00\n", renamed, kernelPath)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ImageEvidence{Path: kernelPath, Device: 12, Inode: 42, DirectFromExecutable: false}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
}

func TestDarwinSocketToolOutput(t *testing.T) {
	got, err := lsofListener([]byte("p701\x00\nf3\x00d0x1020304050607080\x00\n"), 0)
	if got != (Listener{ListenerIdentified, 701}) || err != nil {
		t.Fatalf("lsof device-character record: %+v err=%v", got, err)
	}
}

func TestDarwinCommandDiagnostic(t *testing.T) {
	tests := map[string]struct{ stderr, want string }{"success: clean": {}, "error: raw marker and controls are discarded": {"private-marker\n\x1b", ", stderr present (16 bytes)"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s stderrSize
			if n, err := s.Write([]byte(tt.stderr)); n != len(tt.stderr) || err != nil {
				t.Fatal("stderr not fully consumed")
			}
			if got := s.diagnostic(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListenerFailureDetail(t *testing.T) {
	cause := errors.New("lsof listener warning")
	p := Probe{sockets: func(context.Context) ([]socketEvidence, error) { return nil, cause }}
	got, err := p.ListenerEvidence(t.Context(), 27123)
	if got.State != ListenerUnknown || !errors.Is(err, cause) {
		t.Fatalf("state=%s err=%v", got.State, err)
	}
}
