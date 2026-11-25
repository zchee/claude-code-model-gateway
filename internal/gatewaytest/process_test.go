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

//go:build process

package gatewaytest_test

import (
	"context"
	json "encoding/json/v2"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

func TestNoNamedAliasProbe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", home)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(server.Close)
	env := gatewaytest.Environment{"NODE_TEST_CONTEXT": "child", "HOME": home, "PATH": home}
	start := time.Now()
	got, err := env.ProbeAlias(t.Context(), "sonnet", server.URL)
	elapsed := time.Since(start)
	if err != nil || got != nil {
		t.Errorf("ProbeAlias=(%v, %v), want (nil, nil)", got, err)
	}
	if elapsed >= time.Second {
		t.Errorf("unnamed probe took %v, want less than one second", elapsed)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("probe contacted upstream %d times, want none", got)
	}
}

func runHelper(t *testing.T, env []string, outputPath string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^"+t.Name()+"$", "--", outputPath)
	cmd.Dir = filepath.Dir(outputPath)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("helper exit: %v; output: %s", err, output)
	}
}

func TestPathClaudeNeverLaunched(t *testing.T) {
	type result struct {
		Pins  map[string]string `json:"pins"`
		Alias *string           `json:"alias"`
	}
	if flag.NArg() == 1 {
		env := gatewaytest.Environment{"HOME": os.Getenv("HOME"), "PATH": os.Getenv("PATH"), "NODE_TEST_CONTEXT": os.Getenv("NODE_TEST_CONTEXT")}
		pins, pinErr := env.DetectedPins(t.Context())
		alias, aliasErr := env.ProbeAlias(t.Context(), "sonnet", "http://127.0.0.1:9")
		if pinErr != nil || aliasErr != nil {
			t.Errorf("pin refresh: %v; alias probe: %v", pinErr, aliasErr)
		}
		data, err := json.Marshal(result{pins, alias}, json.FormatNilMapAsNull(true))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(flag.Arg(0), data, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	marker := filepath.Join(root, "launched")
	bin := filepath.Join(root, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n: > \"$LAUNCH_MARKER\"\nprintf '9.9.9\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{"NODE_TEST_CONTEXT=child", "HOME=" + root, "PATH=" + root, "LAUNCH_MARKER=" + marker}
	// Verify the only executable the probes can find really does leave the marker.
	canary := exec.CommandContext(t.Context(), bin, "--version")
	canary.Env = env
	if err := canary.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(root, "probe.json")
	runHelper(t, env, report)
	wantAbsent(t, marker)
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	// Compare the wire shape: neither a missing property nor null pins is the source result.
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(map[string]any{"pins": map[string]any{}, "alias": nil}, got); diff != "" {
		t.Errorf("probe result (-want +got):\n%s", diff)
	}
}

func TestEmptyEnvironmentHelper(t *testing.T) {
	if flag.NArg() == 1 {
		// TestMain has already checked the account's actual home and both listener tables,
		// independently of HOME, and without connecting to either port.
		if len(os.Environ()) != 0 {
			t.Fatalf("helper inherited %d environment entries, want zero", len(os.Environ()))
		}
		home := filepath.Dir(flag.Arg(0))
		env := gatewaytest.NewEnv(home, nil, nil, nil)
		if env["HOME"] != home {
			t.Errorf("isolated helper HOME=%q, want %q", env["HOME"], home)
		}
		wantAbsent(t, filepath.Join(home, ".claude", "model-gateway"))
		if err := os.WriteFile(flag.Arg(0), []byte("checked\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	report := filepath.Join(t.TempDir(), "empty-helper")
	runHelper(t, []string{}, report)
	got, err := os.ReadFile(report)
	if err != nil || string(got) != "checked\n" {
		t.Errorf("empty-environment helper did not finish: %q, %v", got, err)
	}
}
