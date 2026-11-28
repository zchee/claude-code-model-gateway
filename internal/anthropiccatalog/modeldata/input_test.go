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

package modeldata_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog/modeldata"
)

// mdatTEmpty is the snapshot a missing or malformed native catalog yields.
const mdatTEmpty = `{"data":[],"updatedAt":0}`

// mdatTValid is a one-row valid catalog, already in the compact form Read returns.
const mdatTValid = `{"data":[{"id":"claude-opus-6","display_name":"Opus 6"}],"updatedAt":7}`

// mdatTOpen is the signature of the open call Read makes through the seam.
type mdatTOpen = func(name string, flag int, perm os.FileMode) (*os.File, error)

// mdatTResult is everything Read returns, in a form go-cmp compares directly. NilObject separates
// a nil value from an empty one, and Failed records only whether an error was returned.
type mdatTResult struct {
	Object    string
	NilObject bool
	UpdatedAt float64
	Failed    bool
}

// mdatTFailure is the result of a Read that reports an I/O error: no object and a zero timestamp.
var mdatTFailure = mdatTResult{NilObject: true, Failed: true}

// mdatTOutcome is one finished call of Read, including a recovered panic.
type mdatTOutcome struct {
	result   mdatTResult
	err      error
	panicked any
}

// mdatTCallRead calls Read and recovers a panic, so that a declaration fails only the calling row.
func mdatTCallRead(path string) (o mdatTOutcome) {
	defer func() { o.panicked = recover() }()
	object, updatedAt, err := modeldata.Read(path)
	o.err = err
	o.result = mdatTResult{Object: string(object), NilObject: object == nil, UpdatedAt: updatedAt, Failed: err != nil}
	return o
}

// check fails the row on a panic and logs a returned error for debugging.
func (o mdatTOutcome) check(t *testing.T, path string) mdatTResult {
	t.Helper()
	if o.panicked != nil {
		t.Fatalf("Read(%q) panicked: %v", path, o.panicked)
	}
	if o.err != nil {
		t.Logf("Read(%q) error: %v", path, o.err)
	}
	return o.result
}

// mdatTRead calls Read on the test goroutine.
func mdatTRead(t *testing.T, path string) mdatTResult {
	t.Helper()
	return mdatTCallRead(path).check(t, path)
}

// mdatTWrite creates name in dir with body and returns its path.
func mdatTWrite(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mdatTCatalog returns a setup that writes body as the native catalog file.
func mdatTCatalog(body string) func(*testing.T, string) string {
	return func(t *testing.T, dir string) string {
		return mdatTWrite(t, dir, "anthropic-models.json", body)
	}
}

// mdatTCloseCheck registers, before f is handed to Read, a cleanup that requires f to be closed when
// the row ends: a Close that succeeds then means Read left it open, and fails the row, and the
// descriptor is reclaimed either way.
func mdatTCloseCheck(t *testing.T, f *os.File) {
	t.Cleanup(func() {
		switch err := f.Close(); {
		case err == nil:
			t.Errorf("Read left the handle it opened open")
		case !errors.Is(err, os.ErrClosed):
			t.Errorf("close the fixture handle: %v", err)
		}
	})
}

// mdatTHandle returns an open call for the seam that ignores the path and flags Read asks for and
// returns the handle open gives for a fixture under dir, checked by mdatTCloseCheck.
func mdatTHandle(open func(t *testing.T, dir string) (*os.File, error)) func(*testing.T, string, mdatTOpen) mdatTOpen {
	return func(t *testing.T, dir string, _ mdatTOpen) mdatTOpen {
		return func(string, int, os.FileMode) (*os.File, error) {
			f, err := open(t, dir)
			if err != nil {
				t.Errorf("fixture handle: %v", err)
				return nil, err
			}
			mdatTCloseCheck(t, f)
			return f, nil
		}
	}
}

// mdatTListing describes every entry under dir without following links: the mode and size of
// each entry, the SHA-256 of each regular file's contents and the target of each symbolic link. A
// row compares it before and after Read, so that Read is seen to create, remove and rewrite nothing.
func mdatTListing(t *testing.T, dir string) []string {
	t.Helper()
	var listing []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		entry := fmt.Sprintf("%s %v %d", rel, info.Mode(), info.Size())
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry += " sha256:" + hex.EncodeToString(sum[:])
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry += " -> " + target
		}
		listing = append(listing, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return listing
}

// TestNativeCatalogInput decides the shared native catalog reader: a missing or malformed catalog
// is the empty snapshot with no error, a nonregular file or any other I/O failure is an error with
// no snapshot, and a valid catalog keeps its unknown members, member order and row order while
// only the rows and the timestamp are normalized. Read never creates or rewrites a file.
//
// A row with open replaces the open call Read makes, so that the handle Read receives can differ
// from the file at the path or the flags can be checked, and requires Read to call it exactly once
// with the path. Every row calls Read on the test goroutine.
func TestNativeCatalogInput(t *testing.T) {
	const zeta = `{"id":"claude-zeta-1","display_name":"Zeta","z":42}`
	const alpha = `{"display_name":"","id":"claude-alpha._-2"}`
	tests := map[string]struct {
		setup func(t *testing.T, dir string) (path string)
		open  func(t *testing.T, dir string, previous mdatTOpen) mdatTOpen
		want  mdatTResult
	}{
		// Wrong: reports ENOENT as an error, which would stop a pin command on a fresh home.
		"success: missing catalog is the empty snapshot": {
			setup: func(_ *testing.T, dir string) string { return filepath.Join(dir, "anthropic-models.json") },
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: treats a zero-length file as an I/O failure instead of malformed JSON.
		"success: empty file is malformed": {
			setup: mdatTCatalog(""),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: returns a decode error to the caller instead of the empty snapshot.
		"success: truncated JSON is malformed": {
			setup: mdatTCatalog(`{"data":[`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: decodes only the first document and ignores what follows it.
		"success: text after the document is malformed": {
			setup: mdatTCatalog(`{"data":[],"updatedAt":5} {}`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: dereferences a null root or reports it as an error.
		"success: null root is the empty snapshot": {
			setup: mdatTCatalog(`null`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: accepts the first element of an array root as the catalog.
		"success: array root is the empty snapshot": {
			setup: mdatTCatalog(`[{"data":[],"updatedAt":5}]`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: accepts a scalar root.
		"success: string root is the empty snapshot": {
			setup: mdatTCatalog(`"claude-opus-6"`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: supplies an empty row list for a missing data member and keeps the other members.
		"success: missing data member is the empty snapshot": {
			setup: mdatTCatalog(`{"updatedAt":5,"other":1}`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: treats an object data member as an empty row list.
		"success: object data member is the empty snapshot": {
			setup: mdatTCatalog(`{"data":{},"error":"old","updatedAt":10}`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: treats a null data member as an empty row list.
		"success: null data member is the empty snapshot": {
			setup: mdatTCatalog(`{"data":null,"updatedAt":10}`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// The compact bytes of a valid snapshot are the promise: both parents compare and persist them.
		// Wrong: sorts rows, drops unknown row or top-level members, moves data or updatedAt to the end,
		// keeps rows whose id is not a lowercase Claude ID or whose display_name is not a string, or
		// returns the timestamp unconverted.
		"success: valid rows keep their order and unknown members": {
			setup: mdatTCatalog(`{"extra":1,"updatedAt":"0x10","error":"bad","data":[` + zeta +
				`,null,{},{"id":"Claude-a","display_name":"a"},{"id":"claude-a[1m]","display_name":"b"},` +
				`{"id":"claude-a","display_name":42},{"id":["claude-opus-6"],"display_name":"Array"},` +
				`{"id":"claude-","display_name":"empty"},{"id":"claude-A","display_name":"UPPER"},` + alpha +
				`],"attemptedAt":22}`),
			want: mdatTResult{
				Object:    `{"extra":1,"updatedAt":16,"error":"bad","data":[` + zeta + `,` + alpha + `],"attemptedAt":22}`,
				UpdatedAt: 16,
			},
		},
		// Wrong: strips the lifecycle member, drops retired rows, removes a later row with the same id,
		// or keeps the whitespace of the input instead of returning compact bytes.
		"success: duplicate ids and lifecycle are kept in order": {
			setup: mdatTCatalog("{\n  \"data\": [\n" +
				"    {\"id\": \"claude-opus-6\", \"display_name\": \"Opus 6\", \"lifecycle\": \"retired\"},\n" +
				"    {\"id\": \"claude-opus-6\", \"display_name\": \"Opus 6 again\"}\n" +
				"  ],\n  \"updatedAt\": 3\n}\n"),
			want: mdatTResult{
				Object: `{"data":[{"id":"claude-opus-6","display_name":"Opus 6","lifecycle":"retired"},` +
					`{"id":"claude-opus-6","display_name":"Opus 6 again"}],"updatedAt":3}`,
				UpdatedAt: 3,
			},
		},
		// Wrong: accepts any lowercase id, not only one with the Claude prefix.
		"success: lowercase non-Claude id is dropped": {
			setup: mdatTCatalog(`{"data":[{"id":"gpt-5","display_name":"GPT 5"},` + zeta + `],"updatedAt":4}`),
			want:  mdatTResult{Object: `{"data":[` + zeta + `],"updatedAt":4}`, UpdatedAt: 4},
		},
		// Wrong: accepts a row without a display_name.
		"success: valid id without display_name is dropped": {
			setup: mdatTCatalog(`{"data":[{"id":"claude-opus-6"},` + zeta + `],"updatedAt":2}`),
			want:  mdatTResult{Object: `{"data":[` + zeta + `],"updatedAt":2}`, UpdatedAt: 2},
		},
		// Wrong: omits a missing timestamp or inserts it before the existing members.
		"success: absent timestamp is zero after the other members": {
			setup: mdatTCatalog(`{"data":[` + zeta + `],"note":"x"}`),
			want:  mdatTResult{Object: `{"data":[` + zeta + `],"note":"x","updatedAt":0}`},
		},
		// Wrong: keeps an unparseable timestamp or rejects the whole catalog because of it.
		"success: unparseable timestamp is zero in place": {
			setup: mdatTCatalog(`{"updatedAt":"abc","data":[` + zeta + `]}`),
			want:  mdatTResult{Object: `{"updatedAt":0,"data":[` + zeta + `]}`},
		},
		// Wrong: returns the timestamp read back from the serialized object, where infinity is null.
		"success: infinite timestamp is returned and serialized as null": {
			setup: mdatTCatalog(`{"data":[],"updatedAt":1e400}`),
			want:  mdatTResult{Object: `{"data":[],"updatedAt":null}`, UpdatedAt: math.Inf(1)},
		},
		// Wrong: gives zero for every timestamp that is not a number or a string.
		"success: true timestamp is one": {
			setup: mdatTCatalog(`{"data":[],"updatedAt":true}`),
			want:  mdatTResult{Object: `{"data":[],"updatedAt":1}`, UpdatedAt: 1},
		},
		// Wrong: gives zero for an array timestamp instead of converting its single element.
		"success: nested singleton timestamp is converted": {
			setup: mdatTCatalog(`{"data":[],"extra":true,"updatedAt":[["0b101"]]}`),
			want:  mdatTResult{Object: `{"data":[],"extra":true,"updatedAt":5}`, UpdatedAt: 5},
		},
		// Wrong: rejects the catalog for an object timestamp or drops the other members.
		"success: object timestamp is zero and other members are kept": {
			setup: mdatTCatalog(`{"data":[],"extra":true,"updatedAt":{}}`),
			want:  mdatTResult{Object: `{"data":[],"extra":true,"updatedAt":0}`},
		},
		// Wrong: gives zero for a timestamp whose conversion would throw, instead of the whole empty snapshot.
		"success: shadowed conversion is the whole empty snapshot": {
			setup: mdatTCatalog(`{"data":[],"extra":true,"updatedAt":[1,{"toString":false}]}`),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: reads a directory as a missing or malformed catalog and so permits overwriting it.
		"error: directory is not a regular file": {
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "anthropic-models.json")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: mdatTFailure,
		},
		// Wrong: checks the path rather than the handle it opened, so a regular file at the path lets
		// it read a directory handle.
		"error: directory handle is refused": {
			setup: func(t *testing.T, dir string) string {
				if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
					t.Fatal(err)
				}
				return mdatTWrite(t, dir, "anthropic-models.json", mdatTValid)
			},
			open: mdatTHandle(func(_ *testing.T, dir string) (*os.File, error) {
				return os.Open(filepath.Join(dir, "sub"))
			}),
			want: mdatTFailure,
		},
		// Wrong: converts a failure of the read itself, after the handle was found regular, into the
		// empty catalog. A write-only handle of a regular file stats as regular and cannot be read.
		"error: read failure after a regular open is an I/O error": {
			setup: mdatTCatalog(mdatTValid),
			open: mdatTHandle(func(_ *testing.T, dir string) (*os.File, error) {
				return os.OpenFile(filepath.Join(dir, "anthropic-models.json"), os.O_WRONLY, 0)
			}),
			want: mdatTFailure,
		},
	}
	maps.Copy(tests, mdatTPlatformRows())
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)
			var opened []string
			if tt.open != nil {
				modeldata.MdatTReplaceOpenInput(t, func(previous mdatTOpen) mdatTOpen {
					open := tt.open(t, dir, previous)
					return func(name string, flag int, perm os.FileMode) (*os.File, error) {
						opened = append(opened, name)
						return open(name, flag, perm)
					}
				})
			}
			before := mdatTListing(t, dir)
			got := mdatTRead(t, path)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Read(%q) (-want +got):\n%s", path, diff)
			}
			if tt.open != nil {
				if diff := gocmp.Diff([]string{path}, opened); diff != "" {
					t.Errorf("paths given to the opener (-want +got):\n%s", diff)
				}
			}
			if diff := gocmp.Diff(before, mdatTListing(t, dir)); diff != "" {
				t.Errorf("Read changed the directory (-before +after):\n%s", diff)
			}
		})
	}
}

// TestNativeInputImports checks direct imports only: no non-test file of the package, for any
// target, imports the pin or the catalog package.
func TestNativeInputImports(t *testing.T) {
	const module = "github.com/zchee/claude-code-model-gateway"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	imports := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			imports[path] = append(imports[path], name)
		}
	}
	if len(imports) == 0 {
		t.Fatal("no imports found in the package's non-test files")
	}
	tests := map[string]struct{ forbidden string }{
		// Wrong: delegates the model-ID helpers to the pin package instead of owning them.
		"success: no import of the pin package": {module + "/internal/pins"},
		// Wrong: reuses the catalog package's reader instead of owning it.
		"success: no import of the catalog package": {module + "/internal/anthropiccatalog"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if files, ok := imports[tt.forbidden]; ok {
				slices.Sort(files)
				t.Errorf("%s imported by %v", tt.forbidden, files)
			}
		})
	}
}
