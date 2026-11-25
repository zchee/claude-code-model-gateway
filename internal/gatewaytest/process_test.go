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
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const (
	helperTest   = "-test.run=^TestEmptyEnvironmentHelper$"
	helperPrefix = "--gatewaytest-helper="
	reportName   = "report.json"
)

func privateWorkdir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), "gatewaytest-child-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fixture"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func helperCommand(t *testing.T, dir, mode string) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, helperTest, "--", helperPrefix+mode)
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.WaitDelay = time.Second
	return cmd
}

func readReport(t *testing.T, dir string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, reportName))
	if err != nil {
		t.Errorf("helper report: %v", err)
		return
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Errorf("decode helper report: %v", err)
	}
}

func writeReport(t *testing.T, value any) {
	t.Helper()
	file, err := os.OpenFile(reportName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.MarshalWrite(file, value, json.FormatNilMapAsNull(true)); err != nil {
		t.Error(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

type fixtureObservation struct {
	Home, Profile, Config, Binary, Sandbox, Runner string
	Refused                                        bool
	WriteCalls                                     int
}

func TestFixtureProcess(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			root := privateWorkdir(t)
			home := filepath.Join(root, "home")
			parent := map[string]string{"HOME": "/inherited-home", "USERPROFILE": "/inherited-profile", "CLAUDE_CONFIG_DIR": "/inherited-config", "CODEX_GATEWAY_CLAUDE_BIN": "claude", "PATH": root}
			var wantSandbox, wantRunner string
			if key == runner {
				wantRunner = "child"
				parent[runner] = wantRunner
				parent["TMPDIR"] = home
			} else {
				wantSandbox = home
			}
			env := gatewaytest.NewEnv(home, parent, nil, nil)
			if key == runner {
				delete(env, sandbox)
			}
			cmd := helperCommand(t, root, "fixture")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := env.RunFixture(cmd); err != nil {
				t.Errorf("fixture launcher: %v; %s", err, &output)
			}
			var got fixtureObservation
			readReport(t, root, &got)
			t.Run("home", func(t *testing.T) {
				if got.Home != home || got.Profile != home {
					t.Errorf("effective home=%q profile=%q, want %q", got.Home, got.Profile, home)
				}
				insideDirectory(t, os.TempDir(), got.Home)
			})
			t.Run("config", func(t *testing.T) {
				if want := filepath.Join(home, ".claude"); got.Config != want {
					t.Errorf("child config=%q, want %q", got.Config, want)
				}
			})
			t.Run("probe_binary", func(t *testing.T) {
				if want := filepath.Join(home, "missing-claude"); got.Binary != want {
					t.Errorf("child probe executable=%q, want %q", got.Binary, want)
				}
				insideDirectory(t, os.TempDir(), filepath.Dir(got.Binary))
			})
			t.Run("guarded_write", func(t *testing.T) {
				if got.Sandbox != wantSandbox || got.Runner != wantRunner || !got.Refused || got.WriteCalls != 0 {
					t.Errorf("child guards=(%q,%q) refusal=%t calls=%d, want (%q,%q), true, zero", got.Sandbox, got.Runner, got.Refused, got.WriteCalls, wantSandbox, wantRunner)
				}
				wantAbsent(t, filepath.Join(root, "untouched"))
			})
		})
	}
}

func TestEphemeralBind(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			env, _ := guardFixture(t, key)
			const addr = "127.0.0.1:0"
			calls := 0
			var bound net.Listener
			got, err := env.Listen(t.Context(), "tcp", addr, func(ctx context.Context, network, address string) (net.Listener, error) {
				calls++
				if network != "tcp" || address != addr || calls != 1 {
					t.Fatalf("unexpected bind %s %s (call %d)", network, address, calls)
				}
				var cfg net.ListenConfig
				ln, err := cfg.Listen(ctx, "tcp", addr)
				if err == nil {
					bound = ln
					t.Cleanup(func() {
						if err := ln.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				return ln, err
			})
			if err != nil || calls != 1 || got == nil || got != bound {
				t.Errorf("Listen: error=%v calls=%d returned bound listener=%t, want nil, one, true", err, calls, got != nil && got == bound)
			}
		})
	}
}

func probeChild(t *testing.T, key, mode string) string {
	t.Helper()
	dir := privateWorkdir(t)
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "claude")
	marker := filepath.Join(dir, "launched")
	// The marker belongs to the executable, even if its caller drops every variable.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n: > \"${0%/*}/launched\"\nprintf '9.9.9\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + dir, "TMPDIR=" + home}
	if key == sandbox {
		env = append(env, sandbox+"="+home)
	} else {
		env = append(env, runner+"=child")
	}
	if mode == "alias" {
		// An ambient fallback would start only our canary. The explicit probe environment lacks this override.
		env = append(env, "CODEX_GATEWAY_CLAUDE_BIN="+bin)
	}
	canary := exec.CommandContext(t.Context(), bin, "--version")
	canary.Dir = dir
	canary.Env = env
	canary.WaitDelay = time.Second
	if err := canary.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("canary did not leave marker: %v", err)
	}
	cmd := helperCommand(t, dir, mode)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("probe helper: %v; %s", err, out)
	}
	wantAbsent(t, marker)
	return dir
}

type aliasObservation struct {
	Alias        *string
	ElapsedNanos int64
	Requests     int64
}

func TestNoNamedAliasProbe(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			dir := probeChild(t, key, "alias")
			var got aliasObservation
			readReport(t, dir, &got)
			if got.Alias != nil || time.Duration(got.ElapsedNanos) >= time.Second || got.Requests != 0 {
				t.Errorf("unnamed probe: alias=%v elapsed=%v requests=%d; want nil, under one second and no requests", got.Alias, time.Duration(got.ElapsedNanos), got.Requests)
			}
		})
	}
}

func TestPathClaudeNeverLaunched(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			dir := probeChild(t, key, "pins")
			var got map[string]any
			readReport(t, dir, &got)
			if diff := gocmp.Diff(map[string]any{"pins": map[string]any{}, "alias": nil}, got); diff != "" {
				t.Errorf("probe result (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEmptyEnvironmentHelper(t *testing.T) {
	if len(os.Args) == 4 && os.Args[1] == helperTest && os.Args[2] == "--" {
		switch os.Args[3] {
		case helperPrefix + "fixture", helperPrefix + "alias", helperPrefix + "pins", helperPrefix + "empty":
			runChild(t, strings.TrimPrefix(os.Args[3], helperPrefix))
			return
		}
	}
	dir := privateWorkdir(t)
	cmd := helperCommand(t, dir, "empty")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("empty-environment helper: %v; %s", err, out)
	}
	var checked bool
	readReport(t, dir, &checked)
	if !checked {
		t.Error("empty-environment helper did not finish")
	}
}

func runChild(t *testing.T, mode string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !strings.HasPrefix(filepath.Base(dir), "gatewaytest-child-") {
		t.Fatal("helper requires a private fixture working directory")
	}
	marker, err := os.Lstat(".fixture")
	if err != nil || !marker.Mode().IsRegular() || marker.Mode().Perm() != 0o600 || marker.Size() != 0 {
		t.Fatal("helper requires a private fixture marker")
	}
	home := filepath.Join(dir, "home")
	if mode == "empty" {
		// TestMain has already checked the password-database home and both listener tables without connecting.
		if n := len(os.Environ()); n != 0 {
			t.Fatalf("helper inherited %d variables, want zero", n)
		}
		if got := gatewaytest.NewEnv(home, nil, nil, nil)["HOME"]; got != home {
			t.Errorf("isolated helper HOME=%q, want %q", got, home)
		}
		wantAbsent(t, filepath.Join(home, ".claude", "model-gateway"))
		writeReport(t, true)
		return
	}
	env := gatewaytest.Environment{}
	for _, key := range []string{"HOME", "USERPROFILE", "CLAUDE_CONFIG_DIR", "CODEX_GATEWAY_CLAUDE_BIN", "TMPDIR", "PATH", sandbox, runner} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	if mode == "fixture" {
		effectiveHome, err := os.UserHomeDir()
		if err != nil {
			t.Error(err)
		}
		calls, err := refusedWrite(env, filepath.Join(dir, "untouched", ".claude", "settings.json"))
		writeReport(t, fixtureObservation{effectiveHome, env["USERPROFILE"], env["CLAUDE_CONFIG_DIR"], env["CODEX_GATEWAY_CLAUDE_BIN"], env[sandbox], env[runner], errors.Is(err, gatewaytest.ErrOutsideSandbox), calls})
		return
	}
	sandboxOnly := env[sandbox] == home && env[runner] == ""
	runnerOnly := env[runner] == "child" && env[sandbox] == ""
	if env["HOME"] != home || env["PATH"] != dir || env["TMPDIR"] != home || (!sandboxOnly && !runnerOnly) {
		t.Fatal("probe helper environment is not isolated")
	}
	if mode == "alias" {
		if env["CODEX_GATEWAY_CLAUDE_BIN"] != filepath.Join(dir, "claude") {
			t.Fatal("unsafe ambient probe executable")
		}
		delete(env, "CODEX_GATEWAY_CLAUDE_BIN")
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
		defer server.Close()
		start := time.Now()
		alias, err := env.ProbeAlias(t.Context(), "sonnet", server.URL)
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("ProbeAlias: %v", err)
		}
		writeReport(t, aliasObservation{alias, elapsed.Nanoseconds(), requests.Load()})
		return
	}
	if _, exists := env["CODEX_GATEWAY_CLAUDE_BIN"]; exists {
		t.Fatal("PATH probe must not name a binary")
	}
	pins, pinErr := env.DetectedPins(t.Context())
	alias, aliasErr := env.ProbeAlias(t.Context(), "sonnet", "http://127.0.0.1:9")
	if pinErr != nil || aliasErr != nil {
		t.Errorf("pin refresh: %v; alias probe: %v", pinErr, aliasErr)
	}
	writeReport(t, map[string]any{"pins": pins, "alias": alias})
}
