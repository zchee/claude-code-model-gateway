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

package usage

import (
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestTranscriptCWDUnicode(t *testing.T) {
	tests := map[string]struct{ raw, want string }{
		"success: other fields need not be UTF-8":             {raw: "{\"other\":\"\xff\",\"cwd\":\"/valid\"}", want: "/valid"},
		"success: another field may contain a lone surrogate": {raw: `{"other":"\ud800","cwd":"/valid"}`, want: "/valid"},
		"error: invalid UTF-8 in the path":                    {raw: "{\"cwd\":\"/bad\xff\"}"},
		"error: unpaired surrogate in the path":               {raw: `{"cwd":"/bad\ud800"}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "transcript")
			if err := os.WriteFile(p, []byte(tt.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if diff := gocmp.Diff(tt.want, transcriptCWD(f)); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestWorktreeCommonDirFallback(t *testing.T) {
	tests := map[string]struct{ commonDir bool }{
		"success: absent common pointer falls back to gitdir":                 {},
		"success: directory instead of a common pointer falls back to gitdir": {commonDir: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "main")
			git := filepath.Join(repo, ".git", "worktrees", "topic")
			if err := os.MkdirAll(git, 0o700); err != nil {
				t.Fatal(err)
			}
			if tt.commonDir {
				if err := os.Mkdir(filepath.Join(git, "commondir"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if diff := gocmp.Diff(repo, worktreeRoot(t.TempDir(), string(rune(0xfeff))+"gitdir: "+git+"\n")); diff != "" {
				t.Error(diff)
			}
		})
	}
}
