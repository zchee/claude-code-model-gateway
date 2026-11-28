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

package wiring_test

import (
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

// recpTRoot stands for the per-row project root in registry templates and expected paths.
const recpTRoot = "@ROOT@"

// recpTBound is the size bound of the landed bounded reader that every registry read goes through
// (maxSnapshotBytes in snapshot_unix.go and snapshot_other.go): a file of exactly this size is read, one byte
// more is refused.
const recpTBound = 16 << 20

// recpTFixedTime is the modification time every fixture file and directory carries before the call, so that
// any rewrite, staging file or new directory entry shows up as a changed time.
var recpTFixedTime = time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)

// recpTRegistry says what sits at the registry path before the call.
type recpTRegistry int

const (
	recpTRegistryFile recpTRegistry = iota
	recpTRegistryAbsent
	recpTRegistryNoStateDir
	recpTRegistryDirectory
	recpTRegistryPadded
	recpTRegistryAncestorFile
	recpTRegistryLoop
	recpTRegistryDangling
	recpTRegistryLink
)

// recpTEntry is the observable state of one path below a fixture tree.
type recpTEntry struct {
	Mode    fs.FileMode
	ModTime time.Time
	SHA256  string
	Link    string
}

// recpTFixture is one row's isolated home, state directory and project root. defaultState is the state
// directory without an override; state is the one the store must read.
type recpTFixture struct {
	home, defaultState, state, registry, root string
	env                                       map[string]string
}

func TestRecordedProjects(t *testing.T) {
	recpTRun(t, wiring.Store.RecordedProjects)
}

// recpTRun decides the read-only enumeration of recorded project paths for list, which is
// wiring.Store.RecordedProjects in the delivered test.
func recpTRun(t *testing.T, list func(wiring.Store) ([]string, error)) {
	t.Helper()
	tests := map[string]struct {
		kind           recpTRegistry
		registry       string
		size           int
		padBeforeClose bool
		override       bool
		decoy          string
		homeFails      bool
		dirs           []string
		links          map[string]string
		files          map[string]string
		oversized      []string
		project        string
		want           []string
		wantWindows    []string
		wantErr        bool
	}{
		// Catches an implementation that reports a missing registry as an error, or creates an empty one.
		"success: missing registry gives an empty list": {
			kind: recpTRegistryAbsent,
		},
		// Catches an implementation that creates the state directory or the registry while reading.
		"success: missing state directory gives an empty list and creates nothing": {
			kind: recpTRegistryNoStateDir,
		},
		// Catches an implementation that fails, or invents entries, on an empty recorded list.
		"success: empty projects list gives an empty list": {
			registry: `{"projects":[]}`,
		},
		// Catches an implementation that sorts the paths or loses the recorded order, or that rejects other
		// root members.
		"success: recorded order is kept and other root members are ignored": {
			registry: `{"version":2,"projects":["@ROOT@/zeta","@ROOT@/alpha","@ROOT@/mid"],"extra":{"projects":1}}`,
			dirs:     []string{"zeta", "alpha", "mid"},
			want:     []string{"@ROOT@/zeta", "@ROOT@/alpha", "@ROOT@/mid"},
		},
		// Catches an implementation that keeps only projects whose local settings are gateway-wired, as
		// Projects(true) does, or that reads project settings at all.
		"success: unwired, shared-only, foreign and malformed projects are all listed": {
			registry: `{"projects":["@ROOT@/plain","@ROOT@/shared","@ROOT@/foreign","@ROOT@/malformed","@ROOT@/settingsdir"]}`,
			dirs:     []string{"plain", "shared", "foreign", "malformed", "settingsdir/.claude/settings.local.json"},
			files: map[string]string{
				"shared/.claude/settings.json":          `{"env":{"ANTHROPIC_BASE_URL":"http://api.anthropic.com"}}`,
				"foreign/.claude/settings.local.json":   `{"env":{"ANTHROPIC_BASE_URL":"https://example.invalid"}}`,
				"malformed/.claude/settings.local.json": `{`,
			},
			want: []string{"@ROOT@/plain", "@ROOT@/shared", "@ROOT@/foreign", "@ROOT@/malformed", "@ROOT@/settingsdir"},
		},
		// Catches an implementation that keeps only directories, drops gateway-wired projects, or reads the
		// local settings and fails on one beyond the size bound.
		"success: a recorded regular file, a wired project and one with oversized local settings are listed": {
			registry:  `{"projects":["@ROOT@/file.txt","@ROOT@/wired","@ROOT@/big"]}`,
			dirs:      []string{"wired", "big"},
			files:     map[string]string{"file.txt": "not a directory\n", "wired/.claude/settings.local.json": `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:18764"}}`},
			oversized: []string{"big/.claude/settings.local.json"},
			want:      []string{"@ROOT@/file.txt", "@ROOT@/wired", "@ROOT@/big"},
		},
		// Catches an implementation that prunes or skips projects whose directory no longer exists.
		"success: missing directories are listed": {
			registry: `{"projects":["@ROOT@/gone","@ROOT@/also/gone/deeper","@ROOT@/here"]}`,
			dirs:     []string{"here"},
			want:     []string{"@ROOT@/gone", "@ROOT@/also/gone/deeper", "@ROOT@/here"},
		},
		// Catches an implementation that accepts non-string, empty, relative or not well-formed elements,
		// unwraps an object or a one-element array, refuses every surrogate escape, or stops at the first
		// unusable element.
		"success: elements the path rule rejects are skipped": {
			registry: `{"projects":[17,null,true,false,{"path":"@ROOT@/object-only"},["@ROOT@/array-only"],"","relative/kept","./kept","~/kept","@ROOT@/bad\ud800","@ROOT@/low\udc00","@ROOT@/pair\uD83D\uDE00","@ROOT@/kept"]}`,
			dirs:     []string{"kept"},
			want:     []string{"@ROOT@/pair\U0001F600", "@ROOT@/kept"},
		},
		// Catches an implementation that does not deduplicate, or deduplicates by the raw string, so that a
		// trailing separator or dot segments make a second entry; and one that keeps the last spelling.
		"success: lexical duplicates keep the first spelling": {
			registry: `{"projects":["@ROOT@/real","@ROOT@/other","@ROOT@/real/","@ROOT@/./real","@ROOT@/other/../real","@ROOT@/real"]}`,
			dirs:     []string{"real", "other"},
			want:     []string{"@ROOT@/real", "@ROOT@/other"},
		},
		// Catches an implementation that returns the lexically cleaned path instead of the recorded spelling.
		"success: an unclean first spelling is kept as written": {
			registry: `{"projects":["@ROOT@/./real/","@ROOT@/other","@ROOT@/real"]}`,
			dirs:     []string{"real", "other"},
			want:     []string{"@ROOT@/./real/", "@ROOT@/other"},
		},
		// Catches an implementation that returns the canonical registry key instead of the recorded
		// spelling, or keeps the later real path instead of the first alias.
		"success: an alias listed first is kept with its own spelling": {
			registry: `{"projects":["@ROOT@/alias","@ROOT@/real"]}`,
			dirs:     []string{"real"},
			links:    map[string]string{"alias": "real"},
			want:     []string{"@ROOT@/alias"},
		},
		// Catches an implementation that deduplicates by the lexical path, so that an alias of an earlier
		// project is listed again.
		"success: a later alias of a recorded project is dropped": {
			registry: `{"projects":["@ROOT@/real","@ROOT@/other","@ROOT@/alias"]}`,
			dirs:     []string{"real", "other"},
			links:    map[string]string{"alias": "real"},
			want:     []string{"@ROOT@/real", "@ROOT@/other"},
		},
		// Catches an implementation that keys a missing directory by its lexical path instead of the real
		// path of its nearest existing ancestor.
		"success: missing directories below an alias share the key of the real path": {
			registry: `{"projects":["@ROOT@/alias/gone","@ROOT@/real/gone","@ROOT@/real/gone/child"]}`,
			dirs:     []string{"real"},
			links:    map[string]string{"alias": "real"},
			want:     []string{"@ROOT@/alias/gone", "@ROOT@/real/gone/child"},
		},
		// Catches an implementation that resolves only the immediate parent of a missing path, so that two
		// missing levels below an alias keep a lexical key.
		"success: a path two missing levels below an alias shares the key of the real path": {
			registry: `{"projects":["@ROOT@/alias/gone/child","@ROOT@/real/gone/child"]}`,
			dirs:     []string{"real"},
			links:    map[string]string{"alias": "real"},
			want:     []string{"@ROOT@/alias/gone/child"},
		},
		// Catches an implementation that folds case where the landed key does not: two absent siblings that
		// differ only in case keep distinct keys except on Windows, where the key is lower-cased.
		"success: absent siblings that differ only in case": {
			registry:    `{"projects":["@ROOT@/Case","@ROOT@/case"]}`,
			want:        []string{"@ROOT@/Case", "@ROOT@/case"},
			wantWindows: []string{"@ROOT@/Case"},
		},
		// Catches an implementation that adds the current project or moves it to the front.
		"success: the current project is listed only where it is recorded": {
			registry: `{"projects":["@ROOT@/other","@ROOT@/current","@ROOT@/last"]}`,
			dirs:     []string{"other", "current", "last"},
			project:  "current",
			want:     []string{"@ROOT@/other", "@ROOT@/current", "@ROOT@/last"},
		},
		// Catches an implementation that adds the current project when it is not recorded, even though its
		// local settings carry the base URL this store owns.
		"success: an unrecorded wired current project is not listed": {
			registry: `{"projects":["@ROOT@/other"]}`,
			dirs:     []string{"other", "current"},
			project:  "current",
			files:    map[string]string{"current/.claude/settings.local.json": `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:18764"}}`},
			want:     []string{"@ROOT@/other"},
		},
		// Catches an implementation that ignores the state directory override and reads the default registry.
		"success: the state directory override is read and the default registry is not": {
			override: true,
			registry: `{"projects":["@ROOT@/override"]}`,
			decoy:    `{"projects":["@ROOT@/decoy"]}`,
			want:     []string{"@ROOT@/override"},
		},
		// Catches an implementation that falls back to the default registry when the override has none.
		"success: an absent registry under the override is empty without falling back": {
			kind:     recpTRegistryAbsent,
			override: true,
			decoy:    `{"projects":["@ROOT@/decoy"]}`,
		},
		// Catches an implementation that needs a working home lookup before it consults the override.
		"success: the override is read even when the home lookup fails": {
			override:  true,
			homeFails: true,
			registry:  `{"projects":["@ROOT@/override"]}`,
			want:      []string{"@ROOT@/override"},
		},
		// Catches an implementation that treats a state directory it cannot resolve as an absent registry.
		"error: the state directory cannot be resolved": {
			kind:      recpTRegistryNoStateDir,
			homeFails: true,
			wantErr:   true,
		},
		// Catches an implementation that rejects or truncates a valid registry at exactly the size bound: the
		// padding sits before the final closing brace, so losing the last byte leaves invalid JSON.
		"success: valid JSON padded to exactly the size bound is read": {
			kind:           recpTRegistryPadded,
			registry:       `{"projects":["@ROOT@/kept"]}`,
			size:           recpTBound,
			padBeforeClose: true,
			dirs:           []string{"kept"},
			want:           []string{"@ROOT@/kept"},
		},
		// Catches an implementation that reads the registry without the size bound.
		"error: valid JSON one byte over the size bound": {
			kind:     recpTRegistryPadded,
			registry: `{"projects":["@ROOT@/kept"]}`,
			size:     recpTBound + 1,
			dirs:     []string{"kept"},
			wantErr:  true,
		},
		// Catches an implementation that treats a registry path it cannot read as absent.
		"error: registry path is a directory": {
			kind:    recpTRegistryDirectory,
			wantErr: true,
		},
		// Catches an implementation that takes every open error for absence: a regular file in place of a
		// directory on the path is not a missing registry.
		"error: a regular file stands in place of an ancestor of the state directory": {
			kind:    recpTRegistryAncestorFile,
			wantErr: true,
		},
		// Catches an implementation that takes every open error for absence: a symlink loop is not a missing
		// registry.
		"error: the registry is a symlink loop": {
			kind:    recpTRegistryLoop,
			wantErr: true,
		},
		// Catches an implementation that reports a registry symlink to a missing target as an error, or
		// removes or replaces the link.
		"success: a dangling registry symlink is absence and the link is kept": {
			kind: recpTRegistryDangling,
		},
		// Catches an implementation that refuses to follow a registry symlink to a regular file.
		"success: a registry symlink to valid JSON is read": {
			kind:     recpTRegistryLink,
			registry: `{"projects":["@ROOT@/kept"]}`,
			dirs:     []string{"kept"},
			want:     []string{"@ROOT@/kept"},
		},
		// Catches an implementation that treats a malformed registry as an empty list, as the repairing
		// reader does.
		"error: invalid JSON": {
			registry: `{"projects":["@ROOT@/real"]`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
		// Catches an implementation that treats an empty file as an empty list.
		"error: empty registry file": {
			registry: ``,
			wantErr:  true,
		},
		// Catches an implementation that accepts trailing data after the root value.
		"error: trailing data after the root": {
			registry: `{"projects":["@ROOT@/real"]} {}`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
		// Catches an implementation that accepts a bare array as the recorded list.
		"error: root is an array": {
			registry: `["@ROOT@/real"]`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
		// Catches an implementation that treats a null root as an empty list.
		"error: root is null": {
			registry: `null`,
			wantErr:  true,
		},
		// Catches an implementation that treats a boolean root as an empty list.
		"error: root is true": {
			registry: `true`,
			wantErr:  true,
		},
		// Catches an implementation that treats a number root as an empty list.
		"error: root is a number": {
			registry: `17`,
			wantErr:  true,
		},
		// Catches an implementation that accepts a bare string as one recorded path.
		"error: root is a string": {
			registry: `"@ROOT@/real"`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
		// Catches an implementation that treats a root without a projects member as an empty list.
		"error: projects member is missing": {
			registry: `{"project":["@ROOT@/real"]}`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
		// Catches an implementation that matches the member name without regard to case.
		"error: projects member spelled with another case": {
			registry: `{"Projects":["@ROOT@/real"]}`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
		// Catches an implementation that lets the first of duplicated members win; the landed decoder keeps
		// the last, as JSON.parse does.
		"success: the last of duplicated projects members wins": {
			registry: `{"projects":["@ROOT@/first"],"projects":["@ROOT@/last"]}`,
			dirs:     []string{"first", "last"},
			want:     []string{"@ROOT@/last"},
		},
		// Catches an implementation that rejects a non-array member before a later duplicate replaces it.
		"success: a later list duplicate overrides an earlier non-array member": {
			registry: `{"projects":1,"projects":["@ROOT@/last"]}`,
			dirs:     []string{"last"},
			want:     []string{"@ROOT@/last"},
		},
		// Catches an implementation that lets an earlier list win over a later non-array duplicate.
		"error: a later non-array duplicate overrides an earlier list": {
			registry: `{"projects":["@ROOT@/first"],"projects":1}`,
			dirs:     []string{"first"},
			wantErr:  true,
		},
		// Catches an implementation that treats a non-array projects member as an empty list.
		"error: projects is true": {
			registry: `{"projects":true}`,
			wantErr:  true,
		},
		// Catches an implementation that treats a numeric projects member as an empty list.
		"error: projects is a number": {
			registry: `{"projects":0}`,
			wantErr:  true,
		},
		// Catches an implementation that treats a null projects member as an empty list.
		"error: projects is null": {
			registry: `{"projects":null}`,
			wantErr:  true,
		},
		// Catches an implementation that accepts a single string as the recorded list.
		"error: projects is a string": {
			registry: `{"projects":"@ROOT@/real"}`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
		// Catches an implementation that enumerates the values of an object as the recorded list.
		"error: projects is an object": {
			registry: `{"projects":{"0":"@ROOT@/real"}}`,
			dirs:     []string{"real"},
			wantErr:  true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := recpTNewFixture(t, tt.override)
			for _, dir := range tt.dirs {
				recpTCheck(t, os.MkdirAll(filepath.Join(f.root, dir), 0o755))
			}
			for name, target := range tt.links {
				recpTCheck(t, os.Symlink(filepath.Join(f.root, target), filepath.Join(f.root, name)))
			}
			for name, data := range tt.files {
				recpTWrite(t, filepath.Join(f.root, name), []byte(data), 0o644)
			}
			for _, name := range tt.oversized {
				recpTWrite(t, filepath.Join(f.root, name), recpTPad([]byte(`{"env":{}}`), recpTBound+1), 0o644)
			}
			registry := []byte(recpTJSONTemplate(t, tt.registry, f.root))
			switch tt.kind {
			case recpTRegistryFile:
				recpTWrite(t, f.registry, registry, 0o640)
			case recpTRegistryPadded:
				data := recpTPad(registry, tt.size)
				if tt.padBeforeClose {
					data = append(recpTPad([]byte(strings.TrimSuffix(string(registry), "}")), tt.size-1), '}')
				}
				recpTWrite(t, f.registry, data, 0o640)
			case recpTRegistryAbsent:
				recpTCheck(t, os.MkdirAll(f.state, 0o755))
			case recpTRegistryNoStateDir:
				recpTCheck(t, os.MkdirAll(filepath.Dir(f.state), 0o755))
			case recpTRegistryDirectory:
				recpTCheck(t, os.MkdirAll(f.registry, 0o750))
			case recpTRegistryAncestorFile:
				recpTWrite(t, filepath.Dir(f.state), []byte("not a directory\n"), 0o640)
			case recpTRegistryLoop:
				recpTCheck(t, os.MkdirAll(f.state, 0o755))
				recpTCheck(t, os.Symlink(filepath.Base(f.registry), f.registry))
			case recpTRegistryDangling:
				recpTCheck(t, os.MkdirAll(f.state, 0o755))
				recpTCheck(t, os.Symlink("missing-target.json", f.registry))
			case recpTRegistryLink:
				recpTWrite(t, filepath.Join(f.state, "target.json"), registry, 0o640)
				recpTCheck(t, os.Symlink("target.json", f.registry))
			}
			if tt.decoy != "" {
				recpTWrite(t, filepath.Join(f.defaultState, "wired-projects.json"), []byte(recpTJSONTemplate(t, tt.decoy, f.root)), 0o640)
			}
			recpTFreeze(t, f.home)
			recpTFreeze(t, f.root)
			homeBefore, rootBefore := recpTSnapshot(t, f.home), recpTSnapshot(t, f.root)
			registryBefore, registryErr := os.Lstat(f.registry)

			project := filepath.Join(f.root, "unrecorded")
			if tt.project != "" {
				project = filepath.Join(f.root, tt.project)
			}
			lookup := func(key string) (string, bool) { v, ok := f.env[key]; return v, ok }
			home := func() (string, error) { return f.home, nil }
			if tt.homeFails {
				home = func() (string, error) { return "", errors.New("no home directory in this fixture") }
			}
			store := wiring.Store{
				Paths:   paths.New(lookup, home, func() (string, error) { return project, nil }),
				Project: project,
				Lookup:  lookup,
			}
			got, panicked, err := recpTCall(list, store)

			wantPaths := tt.want
			if runtime.GOOS == "windows" && tt.wantWindows != nil {
				wantPaths = tt.wantWindows
			}
			switch {
			case panicked != nil:
				t.Errorf("RecordedProjects panicked: %v", panicked)
			case tt.wantErr && err == nil:
				t.Errorf("RecordedProjects() = %q, nil; want an error", got)
			case !tt.wantErr && err != nil:
				t.Errorf("RecordedProjects() error = %v; want nil", err)
			case !tt.wantErr:
				want := make([]string, len(wantPaths))
				for i, p := range wantPaths {
					want[i] = strings.ReplaceAll(p, recpTRoot, f.root)
				}
				if diff := gocmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("RecordedProjects() mismatch (-want +got):\n%s", diff)
				}
			}

			// Reading never rewrites, creates, removes or re-times anything, whatever the result.
			if diff := gocmp.Diff(homeBefore, recpTSnapshot(t, f.home)); diff != "" {
				t.Errorf("state tree changed (-before +after):\n%s", diff)
			}
			if diff := gocmp.Diff(rootBefore, recpTSnapshot(t, f.root)); diff != "" {
				t.Errorf("project tree changed (-before +after):\n%s", diff)
			}
			registryAfter, registryAfterErr := os.Lstat(f.registry)
			switch {
			case registryErr != nil && registryAfterErr == nil:
				t.Errorf("registry %s was created", f.registry)
			case registryErr == nil && registryAfterErr != nil:
				t.Errorf("registry %s disappeared: %v", f.registry, registryAfterErr)
			case registryErr == nil && !os.SameFile(registryBefore, registryAfter):
				t.Errorf("registry %s was replaced by another file", f.registry)
			}
		})
	}
}

// recpTCall runs list and turns a panic into a value, so that a row fails on its own.
func recpTCall(list func(wiring.Store) ([]string, error), store wiring.Store) (got []string, panicked any, err error) {
	defer func() { panicked = recover() }()
	got, err = list(store)
	return got, nil, err
}

// recpTNewFixture creates nothing on disk but the two temporary roots. With override, the state directory
// is MODEL_GATEWAY_STATE_DIR, supplied through the store's own lookup.
func recpTNewFixture(t *testing.T, override bool) recpTFixture {
	t.Helper()
	home := t.TempDir()
	f := recpTFixture{home: home, defaultState: filepath.Join(home, ".claude", "model-gateway"), root: t.TempDir(), env: map[string]string{}}
	f.state = f.defaultState
	if override {
		f.state = filepath.Join(home, "override-state")
		f.env["MODEL_GATEWAY_STATE_DIR"] = f.state
	}
	f.registry = filepath.Join(f.state, "wired-projects.json")
	return f
}

// recpTWrite creates the parent directories and writes data; os.WriteFile closes the file on every path.
func recpTWrite(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	recpTCheck(t, os.MkdirAll(filepath.Dir(path), 0o755))
	recpTCheck(t, os.WriteFile(path, data, mode))
}

// recpTPad appends spaces, which JSON allows after the root value, until data is size bytes long.
func recpTPad(data []byte, size int) []byte {
	return append(data, []byte(strings.Repeat(" ", size-len(data)))...)
}

// recpTJSONTemplate substitutes the project root, escaped as JSON string content, into a registry template.
func recpTJSONTemplate(t *testing.T, template, root string) string {
	t.Helper()
	quoted, err := json.Marshal(root)
	recpTCheck(t, err)
	return strings.ReplaceAll(template, recpTRoot, string(quoted[1:len(quoted)-1]))
}

// recpTFreeze sets the fixed modification time on every file and directory below dir, without following links.
func recpTFreeze(t *testing.T, dir string) {
	t.Helper()
	recpTCheck(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		return os.Chtimes(path, recpTFixedTime, recpTFixedTime)
	}))
}

// recpTSnapshot records mode, modification time, content digest and link target of every path below dir,
// without following links; a link keeps its own modification time.
func recpTSnapshot(t *testing.T, dir string) map[string]recpTEntry {
	t.Helper()
	out := map[string]recpTEntry{}
	recpTCheck(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := recpTEntry{Mode: info.Mode(), ModTime: info.ModTime().UTC()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if entry.Link, err = os.Readlink(path); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry.SHA256 = hex.EncodeToString(sum[:])
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = entry
		return nil
	}))
	return out
}

func recpTCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
