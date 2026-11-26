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

//go:build process && unix

package dircreatetest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

func checkRewriteProcess(t testing.TB, _ []string) {
	previous := unix.Umask(0o022)
	defer unix.Umask(previous)
	d, err := atomicfile.OpenDir(".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := unix.Mkfifo("fifo", 0o600); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ reader bool }{
		"without a reader": {},
		"with a reader":    {true},
	}
	for name, tt := range tests {
		var reader *os.File
		if tt.reader {
			reader, err = os.OpenFile("fifo", os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
		}
		err := d.RewriteFile("fifo", []byte("record"))
		if reader != nil {
			if closeErr := reader.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		if _, ok := errors.AsType[*atomicfile.NonRegularError](err); !ok {
			t.Fatalf("FIFO %s: expected NonRegularError, got %v", name, err)
		}
	}
	if err := os.WriteFile("journal", []byte("original contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat("journal")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RewriteFile("journal", []byte("trimmed")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat("journal")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		t.Fatalf("rewrite under umask 022: inode preserved=%v mode=%04o", os.SameFile(before, after), after.Mode().Perm())
	}
}

func TestDirRewriteFileProcess(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	cmd, err := env.HelperCommand(t, "rewrite")
	if err != nil {
		t.Fatal(err)
	}
	dir := cmd.Dir
	var log strings.Builder
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := env.RunFixtureWithin(cmd, 10*time.Second); err != nil {
		t.Fatalf("rewrite helper: %v: %s", err, log.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff("trimmed", string(got)); diff != "" {
		t.Errorf("journal (-want +got):\n%s", diff)
	}
}
