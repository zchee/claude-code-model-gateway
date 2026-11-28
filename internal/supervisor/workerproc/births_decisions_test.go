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

//go:build unix

package workerproc_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// decideTFolds reports whether the file system of a new temporary directory of the test finds a file under a name
// that differs from the stored one only in letter case. Every temporary directory of one test lies in one parent
// directory, so the answer holds for the base directory storeTRun then gets in the same test.
func decideTFolds(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	storeTWrite(t, filepath.Join(dir, "case-probe"), "")
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	switch {
	case err == nil:
		return true
	case errors.Is(err, fs.ErrNotExist):
		return false
	}
	t.Fatalf("fixture: find out whether %s folds letter case: %v", dir, err)
	return false
}

// decideTSpellings returns other spellings of the record name of the "first" birth that differ from it only in letter
// case, by what differs.
func decideTSpellings(t *testing.T) map[string]string {
	t.Helper()
	name := storeTNames["first"]
	pid, rest, ok := strings.Cut(name, "-")
	digest, isJSON := strings.CutSuffix(rest, ".json")
	letter := strings.IndexAny(digest, "abcdef")
	if !ok || !isJSON || letter < 0 {
		t.Fatalf("fixture: the record name %s is not a process number, a digest holding a letter and .json", name)
	}
	spellings := map[string]string{
		"the digest in uppercase":               pid + "-" + strings.ToUpper(digest) + ".json",
		"one letter of the digest in uppercase": pid + "-" + digest[:letter] + strings.ToUpper(digest[letter:letter+1]) + digest[letter+1:] + ".json",
		"the extension in uppercase":            pid + "-" + digest + ".JSON",
	}
	for what, spelled := range spellings {
		if spelled == name || !strings.EqualFold(spelled, name) {
			t.Fatalf("fixture: the spelling with %s, %s, is not another letter case of %s", what, spelled, name)
		}
	}
	return spellings
}

// TestBirthStoreStoredSpelling decides that Retire and Signal act only on an entry whose stored name is the record
// name byte for byte. On a file system that folds letter case, a file stored under another letter case of the record
// name is found under the record name, holding exactly the record; it is still not the record (Read refuses that
// spelling), so Retire must not remove it and Signal must not send. Both report what they report for a missing
// record: false and no error.
func TestBirthStoreStoredSpelling(t *testing.T) {
	cases := map[string]storeTCase{}
	for what, spelled := range decideTSpellings(t) {
		setup := func(t *testing.T, base string) string {
			root := storeTEmptyRoot(t, base)
			dir := filepath.Join(root, "worker-births")
			storeTMkdir(t, dir, 0o700)
			storeTWrite(t, filepath.Join(dir, spelled), storeTEncoding(t, "first", root))
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("fixture: list %s: %v", dir, err)
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if !slices.Equal(names, []string{spelled}) {
				t.Fatalf("fixture: the birth directory lists %q; want only the spelling %s", names, spelled)
			}
			if info, err := os.Lstat(filepath.Join(dir, storeTNames["first"])); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("fixture: the record name does not find the file stored as %s (info %v, error %v): the file system does not fold case after all", spelled, info, err)
			}
			return root
		}
		// A method that finds the stored spelling may have made the lock file; the stored file stays as it is.
		delta := map[string]string{storeTDir + "/births.lock": "lock?"}
		cases["error: Signal does not send to the exact record stored with "+what] = storeTCase{
			setup: setup,
			steps: []storeTStep{{op: "signal"}},
			delta: delta,
		}
		cases["error: Retire does not remove the exact record stored with "+what] = storeTCase{
			setup: setup,
			steps: []storeTStep{{op: "retire"}},
			delta: delta,
		}
		cases["error: Signal, then Retire, keep the exact record stored with "+what] = storeTCase{
			setup: setup,
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: delta,
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if !decideTFolds(t) {
				t.Log("the file system of this test's temporary directory tells names apart by letter case: another spelling of a record name is another entry here, so this row decides nothing; it decides on a file system that folds case")
				return
			}
			storeTRun(t, tc)
		})
	}
}
