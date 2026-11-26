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

package readiness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

func lookupMap(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestProviderAuthState(t *testing.T) {
	configDir := t.TempDir()
	proxyBin := filepath.Join(t.TempDir(), "claude-code-proxy")
	var calls [][]string

	exitWith := func(code int, err error) readiness.StatusRunner {
		return func(_ context.Context, _ string, args []string) (int, error) {
			calls = append(calls, slices.Clone(args))
			return code, err
		}
	}

	t.Run("env_and_cli_exit_codes", func(t *testing.T) {
		tests := map[string]struct {
			provider string
			env      map[string]string
			run      readiness.StatusRunner
			want     string
			spawns   int
		}{
			"success: signed in": {
				provider: "kimi", run: exitWith(0, nil), want: "ready", spawns: 1,
			},
			"error: signed out": {
				provider: "cursor", run: exitWith(1, nil), want: "auth-missing", spawns: 1,
			},
			"error: status unavailable": {
				provider: "kimi", run: exitWith(2, nil), want: "auth-unknown", spawns: 1,
			},
			"success: Muse signed in": {
				provider: "muse", run: exitWith(0, nil), want: "ready", spawns: 1,
			},
			"error: Muse login refused, a new login is the fix": {
				provider: "muse", run: exitWith(1, nil), want: "auth-missing", spawns: 1,
			},
			"error: Muse key service unavailable": {
				provider: "muse", run: exitWith(2, nil), want: "auth-unknown", spawns: 1,
			},
			"success: Vertex credentials and settings resolve": {
				provider: "vertex", env: map[string]string{"CCP_VERTEX_PROJECT": "p"}, run: exitWith(0, nil), want: "ready", spawns: 1,
			},
			"error: Vertex without a Google access token": {
				provider: "vertex", env: map[string]string{"GOOGLE_CLOUD_PROJECT": " p "}, run: exitWith(1, nil), want: "auth-missing", spawns: 1,
			},
			"error: Vertex with a project and another invalid setting": {
				provider: "vertex", env: map[string]string{"CCP_VERTEX_PROJECT": "p"}, run: exitWith(2, nil), want: "config-invalid", spawns: 1,
			},
			"error: Vertex status timed out": {
				provider: "vertex", env: map[string]string{"CCP_VERTEX_PROJECT": "p"}, run: exitWith(0, context.DeadlineExceeded), want: "auth-unknown", spawns: 1,
			},
			"error: Vertex without a project": {
				provider: "vertex", env: map[string]string{}, run: exitWith(0, nil), want: "config-invalid", spawns: 0,
			},
			"error: Vertex with only blank project variables": {
				provider: "vertex", env: map[string]string{"CCP_VERTEX_PROJECT": "", "GOOGLE_CLOUD_PROJECT": "  "}, run: exitWith(0, nil), want: "config-invalid", spawns: 0,
			},
			"success: OpenCode key in the proxy env var": {
				provider: "opencode", env: map[string]string{"CCP_OPENCODE_API_KEY": "k"}, run: exitWith(99, nil), want: "ready", spawns: 0,
			},
			"success: OpenCode key in the OpenCode env var": {
				provider: "opencode", env: map[string]string{"OPENCODE_API_KEY": "k"}, run: exitWith(99, nil), want: "ready", spawns: 0,
			},
			"error: OpenCode without a key": {
				provider: "opencode", env: map[string]string{}, run: exitWith(99, nil), want: "auth-missing", spawns: 0,
			},
		}

		order := []string{
			"success: signed in",
			"error: signed out",
			"error: status unavailable",
			"success: Muse signed in",
			"error: Muse login refused, a new login is the fix",
			"error: Muse key service unavailable",
			"success: Vertex credentials and settings resolve",
			"error: Vertex without a Google access token",
			"error: Vertex with a project and another invalid setting",
			"error: Vertex status timed out",
			"error: Vertex without a project",
			"error: Vertex with only blank project variables",
			"success: OpenCode key in the proxy env var",
			"success: OpenCode key in the OpenCode env var",
			"error: OpenCode without a key",
		}

		for _, name := range order {
			tt := tests[name]
			before := len(calls)
			got := readiness.ProviderAuthState(t.Context(), tt.provider, readiness.ProviderAuthOptions{
				Lookup:      lookupMap(tt.env),
				ConfigDir:   configDir,
				ProxyBinary: proxyBin,
				RunStatus:   tt.run,
			})
			if got != tt.want {
				t.Errorf("%s: ProviderAuthState = %q, want %q", name, got, tt.want)
			}
			if spawned := len(calls) - before; spawned != tt.spawns {
				t.Errorf("%s: status processes started = %d, want %d", name, spawned, tt.spawns)
			}
		}

		wantCalls := [][]string{
			{"kimi", "auth", "status"},
			{"cursor", "auth", "status"},
			{"kimi", "auth", "status"},
			{"muse", "auth", "status"},
			{"muse", "auth", "status"},
			{"muse", "auth", "status"},
			{"vertex", "auth", "status"},
			{"vertex", "auth", "status"},
			{"vertex", "auth", "status"},
			{"vertex", "auth", "status"},
		}
		if diff := gocmp.Diff(wantCalls, calls); diff != "" {
			t.Errorf("recorded status calls mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("config_file", func(t *testing.T) {
		cfg := `{"opencode":{"apiKey":"from-config"},"vertex":{"project":"from-config"}}`
		if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(cfg), 0o600); err != nil {
			t.Fatalf("write config.json: %v", err)
		}
		openCode := readiness.ProviderAuthState(t.Context(), "opencode", readiness.ProviderAuthOptions{
			Lookup:      lookupMap(nil),
			ConfigDir:   configDir,
			ProxyBinary: proxyBin,
			RunStatus:   exitWith(99, errors.New("opencode must not spawn")),
		})
		if openCode != "ready" {
			t.Errorf("opencode from config.json = %q, want %q", openCode, "ready")
		}

		before := len(calls)
		vertex := readiness.ProviderAuthState(t.Context(), "vertex", readiness.ProviderAuthOptions{
			Lookup:      lookupMap(nil),
			ConfigDir:   configDir,
			ProxyBinary: proxyBin,
			RunStatus:   exitWith(0, nil),
		})
		if vertex != "ready" {
			t.Errorf("vertex from config.json = %q, want %q", vertex, "ready")
		}
		if len(calls) == before {
			t.Fatal("vertex from config.json did not invoke status command")
		}
		if diff := gocmp.Diff([]string{"vertex", "auth", "status"}, calls[len(calls)-1]); diff != "" {
			t.Errorf("last status call mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("proxy_config_dir", func(t *testing.T) {
		tests := map[string]struct {
			env      map[string]string
			platform string
			home     string
			want     string
		}{
			"success: explicit CCP_CONFIG_DIR": {
				env: map[string]string{"CCP_CONFIG_DIR": "/custom"}, platform: "linux", home: "/h", want: "/custom",
			},
			"success: darwin default": {
				env: map[string]string{}, platform: "darwin", home: "/h", want: filepath.Join("/h", ".config", "claude-code-proxy"),
			},
			"success: linux XDG_CONFIG_HOME": {
				env: map[string]string{"XDG_CONFIG_HOME": "/x"}, platform: "linux", home: "/h", want: filepath.Join("/x", "claude-code-proxy"),
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				got := readiness.ProxyConfigDir(lookupMap(tt.env), tt.platform, tt.home)
				if got != tt.want {
					t.Errorf("ProxyConfigDir = %q, want %q", got, tt.want)
				}
			})
		}
	})
}

func TestProxyProviderReadiness(t *testing.T) {
	dir := t.TempDir()
	msgOpts := readiness.ProviderMessageOptions{
		ConfigDir:   dir,
		ProxyBinary: filepath.Join(dir, "claude-code-proxy"),
		CommandPath: filepath.Join(dir, "model-gateway.js"),
	}
	healthy := readiness.SharedChecks{ProxyBinary: true, ProxyModels: true, ShimRunning: true, ServingVersionMatches: true}

	t.Run("shared_checks", func(t *testing.T) {
		tests := map[string]struct {
			checks  readiness.SharedChecks
			auth    string
			state   string
			message *regexp.Regexp
		}{
			"success: every check passes": {
				checks: healthy, auth: "ready", state: "ready",
				message: regexp.MustCompile(`^Kimi readiness confirms local binary, /v1/models, authentication`),
			},
			"error: binary missing wins over sign-in": {
				checks: readiness.SharedChecks{ProxyBinary: false, ProxyModels: true, ShimRunning: true, ServingVersionMatches: true},
				auth:   "ready", state: "binary-missing",
				message: regexp.MustCompile(`claude-code-proxy is missing`),
			},
			"error: proxy down": {
				checks: readiness.SharedChecks{ProxyBinary: true, ProxyModels: false, ShimRunning: true, ServingVersionMatches: true},
				auth:   "ready", state: "proxy-down",
				message: regexp.MustCompile(`not answering on /v1/models`),
			},
			"error: stale shim": {
				checks: readiness.SharedChecks{ProxyBinary: true, ProxyModels: true, ShimRunning: true, ServingVersionMatches: false},
				auth:   "ready", state: "serving-version-mismatch",
				message: regexp.MustCompile(`stale shim`),
			},
			"error: signed out": {
				checks: healthy, auth: "auth-missing", state: "auth-missing",
				message: regexp.MustCompile(`has no Kimi sign-in\. Run ` + "`" + `".*" kimi auth login` + "`" + `\.`),
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				called := false
				got := readiness.ProxyProviderReadiness("kimi", tt.checks, func(string) string {
					called = true
					return tt.auth
				}, msgOpts)
				if got.State != tt.state {
					t.Errorf("State = %q, want %q", got.State, tt.state)
				}
				if got.Ready != (tt.state == "ready") {
					t.Errorf("Ready = %v, want %v", got.Ready, tt.state == "ready")
				}
				if !tt.message.MatchString(got.Message) {
					t.Errorf("Message = %q, does not match %s", got.Message, tt.message)
				}
				allHealthy := tt.checks.ProxyBinary && tt.checks.ProxyModels && tt.checks.ShimRunning && tt.checks.ServingVersionMatches
				if !allHealthy && called {
					t.Error("authState callback ran despite a failed shared check")
				}
			})
		}
	})

	t.Run("muse_auth_missing", func(t *testing.T) {
		muse := readiness.ProxyProviderReadiness("muse", healthy, func(string) string { return "auth-missing" }, msgOpts)
		want := regexp.MustCompile(`has no Muse sign-in\. Run ` + "`" + `".*" muse auth login` + "`" + `\.`)
		if !want.MatchString(muse.Message) {
			t.Errorf("muse Message = %q, does not match %s", muse.Message, want)
		}
	})

	t.Run("opencode_auth_missing", func(t *testing.T) {
		openCode := readiness.ProxyProviderReadiness("opencode", healthy, func(string) string { return "auth-missing" }, msgOpts)
		want := regexp.MustCompile(`OpenCode Go needs an API key: set CCP_OPENCODE_API_KEY or OPENCODE_API_KEY`)
		if !want.MatchString(openCode.Message) {
			t.Errorf("opencode Message = %q, does not match %s", openCode.Message, want)
		}
	})
}

func TestVertexReadiness(t *testing.T) {
	dir := t.TempDir()
	msgOpts := readiness.ProviderMessageOptions{
		ConfigDir:   dir,
		ProxyBinary: filepath.Join(dir, "claude-code-proxy"),
		CommandPath: filepath.Join(dir, "model-gateway.js"),
	}
	healthy := readiness.SharedChecks{ProxyBinary: true, ProxyModels: true, ShimRunning: true, ServingVersionMatches: true}

	t.Run("vertex_states", func(t *testing.T) {
		tests := map[string]struct {
			auth    string
			message *regexp.Regexp
			doctor  *regexp.Regexp
		}{
			"success: credentials and settings resolve": {
				auth:    "ready",
				message: regexp.MustCompile(`^Vertex AI readiness confirms local binary, /v1/models, Google credentials and Vertex settings, shim, and serving-version checks\.`),
				doctor:  regexp.MustCompile(`^authenticated$`),
			},
			"error: no Google access token": {
				auth:    "auth-missing",
				message: regexp.MustCompile(`^claude-code-proxy got no Google access token for Vertex AI: Application Default Credentials are missing or expired, or Google could not be reached\. Run ` + "`gcloud auth application-default login`" + `, or set GOOGLE_APPLICATION_CREDENTIALS to a credentials file where the gateway starts; ` + "`" + `".*" vertex auth status` + "`" + ` prints the reason\.$`),
				doctor:  regexp.MustCompile(`^MISSING \(claude-code-proxy got no Google access token for Vertex AI: .*Run ` + "`gcloud auth application-default login`"),
			},
			"error: no project, or another invalid setting": {
				auth:    "config-invalid",
				message: regexp.MustCompile(`^claude-code-proxy reports a missing or invalid Vertex AI setting, most often an unset Google Cloud project: set vertex\.project in .*config\.json, or CCP_VERTEX_PROJECT or GOOGLE_CLOUD_PROJECT where the gateway starts\. Run ` + "`" + `".*" vertex auth status` + "`" + ` to see which setting\.$`),
				doctor:  regexp.MustCompile(`^NOT CONFIGURED \(claude-code-proxy reports a missing or invalid Vertex AI setting`),
			},
			"error: status did not answer": {
				auth:    "auth-unknown",
				message: regexp.MustCompile(`^claude-code-proxy could not report Vertex AI sign-in status\.$`),
				doctor:  regexp.MustCompile(`^status unavailable$`),
			},
		}
		vertexLogin := regexp.MustCompile(`vertex auth login`)
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				got := readiness.ProxyProviderReadiness("vertex", healthy, func(string) string { return tt.auth }, msgOpts)
				if got.State != tt.auth {
					t.Errorf("State = %q, want %q", got.State, tt.auth)
				}
				if got.Ready != (tt.auth == "ready") {
					t.Errorf("Ready = %v, want %v", got.Ready, tt.auth == "ready")
				}
				if !tt.message.MatchString(got.Message) {
					t.Errorf("Message = %q, does not match %s", got.Message, tt.message)
				}
				if vertexLogin.MatchString(got.Message) {
					t.Errorf("Message must never suggest vertex auth login: %q", got.Message)
				}
				line := readiness.DescribeProviderAuth("vertex", tt.auth, msgOpts)
				if !tt.doctor.MatchString(line) {
					t.Errorf("DescribeProviderAuth = %q, does not match %s", line, tt.doctor)
				}
			})
		}
	})

	t.Run("other_providers_describe", func(t *testing.T) {
		if got := readiness.DescribeProviderAuth("kimi", "ready", msgOpts); got != "authenticated" {
			t.Errorf("kimi ready = %q, want %q", got, "authenticated")
		}
		if got := readiness.DescribeProviderAuth("opencode", "ready", msgOpts); got != "API key present" {
			t.Errorf("opencode ready = %q, want %q", got, "API key present")
		}
		kimiMissing := regexp.MustCompile(`^MISSING \(claude-code-proxy has no Kimi sign-in\. Run ` + "`" + `".*" kimi auth login` + "`" + `\.\)$`)
		if got := readiness.DescribeProviderAuth("kimi", "auth-missing", msgOpts); !kimiMissing.MatchString(got) {
			t.Errorf("kimi auth-missing = %q, does not match %s", got, kimiMissing)
		}
		if got := readiness.DescribeProviderAuth("muse", "auth-unknown", msgOpts); got != "status unavailable" {
			t.Errorf("muse auth-unknown = %q, want %q", got, "status unavailable")
		}
	})
}
