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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		"rewrite": checkRewriteProcess,
		"directories": func(t testing.TB, _ []string) {
			previous := unix.Umask(0o027)
			defer unix.Umask(previous)
			for _, path := range []string{"single", "one/two/three"} {
				d, err := atomicfile.OpenDirCreate(path, 0o733)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := unix.Mkfifo("fifo", 0o600); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"fifo", "fifo/child"} {
				d, err := atomicfile.OpenDirCreate(path, 0o700)
				if d != nil {
					_ = d.Close()
				}
				if err == nil || d != nil {
					t.Fatalf("OpenDirCreate(%q) = %v, %v; want nil and an error", path, d, err)
				}
			}
		},
	})
}

func TestOpenDirCreateProcess(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	cmd, err := env.HelperCommand(t, "directories")
	if err != nil {
		t.Fatal(err)
	}
	// The launcher changes cmd.Dir before execution; the helper enters the original directory by descriptor.
	dir := cmd.Dir
	var log strings.Builder
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := env.RunFixtureWithin(cmd, 10*time.Second); err != nil {
		t.Fatalf("directory helper: %v: %s", err, log.String())
	}
	tests := map[string]struct{ path string }{
		"success: single level":       {"single"},
		"success: first level":        {"one"},
		"success: intermediate level": {"one/two"},
		"success: final level":        {"one/two/three"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			info, err := os.Stat(filepath.Join(dir, tt.path))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(fs.ModeDir|0o710, info.Mode()&(fs.ModeType|fs.ModePerm)); diff != "" {
				t.Errorf("created directory mode (-want +got):\n%s", diff)
			}
		})
	}
	info, err := os.Lstat(filepath.Join(dir, "fifo"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Type() != fs.ModeNamedPipe {
		t.Errorf("FIFO changed type: %v", info.Mode())
	}
}
