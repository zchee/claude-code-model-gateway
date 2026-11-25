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

package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestInjectedLookupsStayIndependent(t *testing.T) {
	base := t.TempDir()
	firstHome, secondHome := filepath.Join(base, "first"), filepath.Join(base, "second")
	firstEnv := map[string]string{"MODEL_GATEWAY_STATE_DIR": "first-state"}
	first := New(func(key string) (string, bool) { v, ok := firstEnv[key]; return v, ok }, func() (string, error) { return firstHome, nil }, nil)
	second := New(nil, func() (string, error) { return secondHome, nil }, nil)
	tests := map[string]struct {
		resolver Resolver
		state    string
		home     string
	}{
		"success: first uses its override":      {first, "first-state", firstHome},
		"success: second uses its own fallback": {second, filepath.Join(secondHome, ".claude", "model-gateway"), secondHome},
	}
	for range 2 {
		for label, tt := range tests {
			t.Run(label, func(t *testing.T) {
				state, stateErr := tt.resolver.StateDir()
				home, homeErr := tt.resolver.HomeDir()
				if stateErr != nil || homeErr != nil {
					t.Fatalf("state and home lookups: %v, %v", stateErr, homeErr)
				}
				if diff := gocmp.Diff([]string{tt.state, tt.home}, []string{state, home}); diff != "" {
					t.Errorf("state and home (-want +got):\n%s", diff)
				}
			})
		}
	}
}

func TestDirectories(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	gateway := filepath.Join(home, ".claude", "model-gateway")
	tests := map[string]struct {
		platform, fallback string
		env                map[string]string
		want               []string
	}{
		"success: injected lookup and defaults": {platform: "linux", fallback: home, want: []string{
			home, filepath.Join(home, ".claude"), gateway, gateway,
			filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json"),
			filepath.Join(home, ".claude", "projects"), filepath.Join(home, ".claude", "cache", "gateway-models.json"),
			filepath.Join(home, ".config", "claude-code-proxy"), filepath.Join(home, ".grok"), filepath.Join(gateway, "request-body"),
			filepath.Join(gateway, "logs", "request-routes.jsonl"), filepath.Join(gateway, "dispatch-routes.json"), filepath.Join(gateway, "gateway.sock"), "/etc/hosts",
		}},
		"success: POSIX overrides do not move the stable install or settings": {
			platform: "linux",
			env: map[string]string{
				"HOME": home, "USERPROFILE": "ignored", "CLAUDE_CONFIG_DIR": "other-config", "MODEL_GATEWAY_STATE_DIR": "instance-state",
				"XDG_CONFIG_HOME": "xdg", "CODEX_GATEWAY_GROK_HOME": "grok", "CODEX_GATEWAY_HOSTS_FILE": "hosts",
			},
			want: []string{
				home, "other-config", gateway, "instance-state", filepath.Join(home, ".claude", "settings.json"),
				filepath.Join("other-config", ".claude.json"), filepath.Join("other-config", "projects"), filepath.Join("other-config", "cache", "gateway-models.json"),
				filepath.Join("xdg", "claude-code-proxy"), "grok", filepath.Join("instance-state", "request-body"),
				filepath.Join("instance-state", "logs", "request-routes.jsonl"), filepath.Join("instance-state", "dispatch-routes.json"), filepath.Join("instance-state", "gateway.sock"), "hosts",
			},
		},
		"success: Windows selects profile, AppData, SystemRoot and named pipe": {
			platform: "windows", env: map[string]string{"USERPROFILE": home, "HOME": "ignored", "APPDATA": "roaming", "XDG_CONFIG_HOME": "ignored", "SystemRoot": "system"},
			want: []string{
				home, filepath.Join(home, ".claude"), gateway, gateway,
				filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "projects"),
				filepath.Join(home, ".claude", "cache", "gateway-models.json"), filepath.Join("roaming", "claude-code-proxy"), filepath.Join(home, ".grok"),
				filepath.Join(gateway, "request-body"), filepath.Join(gateway, "logs", "request-routes.jsonl"), filepath.Join(gateway, "dispatch-routes.json"),
				`\\.\pipe\model-gateway`, filepath.Join("system", "System32", "drivers", "etc", "hosts"),
			},
		},
		"success: macOS ignores XDG; direct artifact overrides remain raw": {
			platform: "darwin", fallback: home, env: map[string]string{
				"CLAUDE_CONFIG_DIR": "", "MODEL_GATEWAY_STATE_DIR": "", "XDG_CONFIG_HOME": "ignored",
				"MODEL_GATEWAY_REQUEST_BODY_DIR": "body", "CODEX_GATEWAY_REQUEST_LOG_PATH": "log", "CODEX_GATEWAY_DISPATCH_CACHE_PATH": "dispatch", "CODEX_GATEWAY_SOCKET_PATH": "socket",
			},
			want: []string{
				home, filepath.Join(home, ".claude"), gateway, gateway, filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json"),
				filepath.Join(home, ".claude", "projects"), filepath.Join(home, ".claude", "cache", "gateway-models.json"), filepath.Join(home, ".config", "claude-code-proxy"),
				filepath.Join(home, ".grok"), "body", "log", "dispatch", "socket", "/etc/hosts",
			},
		},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			r := New(func(key string) (string, bool) { value, ok := tt.env[key]; return value, ok }, func() (string, error) {
				if tt.fallback == "" {
					t.Fatal("home lookup invoked despite an environment override")
				}
				return tt.fallback, nil
			}, nil)
			r.platform = tt.platform
			read := []func() (string, error){
				r.HomeDir, r.ClaudeConfigDir, r.GatewayHome, r.StateDir, r.UserSettingsPath,
				r.GlobalConfigPath, r.TranscriptsDir, r.DiscoveryCachePath, r.ProxyConfigDir, r.GrokHome,
				r.RequestBodyDir, r.RequestLogPath, r.DispatchCachePath, r.SocketPath,
			}
			var got []string
			for i, fn := range read {
				value, err := fn()
				if err != nil {
					t.Fatalf("path for %q: %v", tt.want[i], err)
				}
				got = append(got, value)
			}
			got = append(got, r.HostsFile())
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("resolved paths (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRegistries(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	claude := filepath.Join(base, "claude")
	cache := filepath.Join(base, "cache")
	plugin := filepath.Join(base, "plugin")
	fallback := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	legacy := filepath.Join(claude, "plugins", "installed_plugins.json")
	preferred := filepath.Join(cache, "installed_plugins.json")
	tests := map[string]struct {
		env      map[string]string
		registry []string
		launcher []string
		root     string
	}{
		"success: fallback": {registry: []string{fallback}, launcher: []string{fallback}, root: home},
		"success: cache searched first, launcher home only affects launcher": {
			env:      map[string]string{"CLAUDE_CODE_PLUGIN_CACHE_DIR": cache, "CLAUDE_CONFIG_DIR": claude, "CLAUDE_PLUGIN_ROOT": plugin, "MODEL_GATEWAY_CLAUDE_HOME": "launcher-home"},
			registry: []string{preferred, legacy}, launcher: []string{filepath.Join("launcher-home", "plugins", "installed_plugins.json")}, root: plugin,
		},
		"success: deduplicate identical cache and config registries": {
			env:      map[string]string{"CLAUDE_CODE_PLUGIN_CACHE_DIR": filepath.Join(claude, "plugins"), "CLAUDE_CONFIG_DIR": claude},
			registry: []string{legacy}, launcher: []string{legacy}, root: home,
		},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			r := New(func(key string) (string, bool) { value, ok := tt.env[key]; return value, ok }, func() (string, error) { return home, nil }, nil)
			registry, err := r.PluginRegistryPaths()
			if err != nil {
				t.Fatal(err)
			}
			launcher, err := r.LauncherRegistryPaths()
			if err != nil {
				t.Fatal(err)
			}
			root, err := r.PluginRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff([][]string{tt.registry, tt.launcher, {tt.root}}, [][]string{registry, launcher, {root}}); diff != "" {
				t.Errorf("registry, launcher, root (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHomeLookupFailures(t *testing.T) {
	badHome := errors.New("fake home lookup failure")
	tests := map[string]struct {
		lookup func() (string, error)
		want   error
	}{
		"error: lookup error propagates":                {lookup: func() (string, error) { return "ignored", badHome }, want: badHome},
		"error: empty lookup result does not fall back": {lookup: func() (string, error) { return "", nil }},
		"error: missing lookup does not fall back":      {},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			r := New(nil, tt.lookup, nil)
			for _, fn := range []func() (string, error){r.HomeDir, r.ClaudeConfigDir, r.GatewayHome, r.StateDir} {
				got, err := fn()
				if got != "" || err == nil || tt.want != nil && !errors.Is(err, tt.want) {
					t.Errorf("got %q, %v: want empty path and error wrapping %v if set", got, err, tt.want)
				}
			}
		})
	}
}

func TestEmptyHOMEFailsWithoutAccountLookup(t *testing.T) {
	base := t.TempDir()
	tests := map[string]struct {
		resolve func(Resolver) (string, error)
	}{
		"error: home":            {Resolver.HomeDir},
		"error: Claude config":   {Resolver.ClaudeConfigDir},
		"error: gateway home":    {Resolver.GatewayHome},
		"error: state directory": {Resolver.StateDir},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			r := New(func(key string) (string, bool) { return "", key == "HOME" }, func() (string, error) {
				t.Error("explicitly empty HOME reached the account home lookup")
				return base, nil
			}, nil)
			r.platform = "linux"
			got, err := tt.resolve(r)
			if got != "" || err == nil || !strings.Contains(err.Error(), "HOME") {
				t.Errorf("got %q, %v; want no path and an error naming HOME", got, err)
			}
		})
	}
}

func TestRelativePathsUsePhysicalWorkingDirectory(t *testing.T) {
	base := t.TempDir()
	logical := filepath.Join(base, "logical")
	target := filepath.Join(base, "physical", "work")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, logical); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(logical)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(physical)
	if parent == filepath.Dir(logical) {
		t.Fatal("fixture needs distinct logical and physical parents")
	}
	env := map[string]string{"CLAUDE_CONFIG_DIR": "../claude", "CLAUDE_CODE_PLUGIN_CACHE_DIR": "../cache", "CLAUDE_PLUGIN_ROOT": "../plugin"}
	r := New(func(key string) (string, bool) { v, ok := env[key]; return v, ok }, nil, func() (string, error) { return physical, nil })
	r.platform = "linux"
	registries := []string{filepath.Join(parent, "cache", "installed_plugins.json"), filepath.Join(parent, "claude", "plugins", "installed_plugins.json")}
	tests := map[string]struct {
		read func() ([]string, error)
		want []string
	}{
		"success: runtime registries":  {r.PluginRegistryPaths, registries},
		"success: launcher registries": {r.LauncherRegistryPaths, registries},
		"success: plugin root":         {func() ([]string, error) { root, err := r.PluginRoot("ignored"); return []string{root}, err }, []string{filepath.Join(parent, "plugin")}},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			got, err := tt.read()
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("resolved paths (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWorkingDirectoryLookupFailures(t *testing.T) {
	failure := errors.New("working directory unavailable")
	tests := map[string]struct {
		lookup func() (string, error)
		want   string
	}{
		"error: missing lookup":         {want: "no lookup"},
		"error: lookup failure":         {lookup: func() (string, error) { return "ignored", failure }, want: failure.Error()},
		"error: relative lookup result": {lookup: func() (string, error) { return "relative", nil }, want: "not absolute"},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			r := New(nil, nil, tt.lookup)
			r.platform = "linux"
			got, err := r.PluginRoot("../plugin")
			if got != "" || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %q, %v; want no path and a %q error", got, err, tt.want)
			}
		})
	}
}

func TestTempDir(t *testing.T) {
	tests := map[string]struct {
		platform string
		env      map[string]string
		want     string
	}{
		"success: POSIX TMPDIR precedence and one trailing slash":            {"linux", map[string]string{"TMPDIR": "/chosen//", "TMP": "/ignored", "TEMP": "/ignored"}, "/chosen/"},
		"success: POSIX empty TMPDIR falls through to TMP":                   {"linux", map[string]string{"TMPDIR": "", "TMP": "/second/", "TEMP": "/ignored"}, "/second"},
		"success: POSIX TEMP fallback preserves root":                        {"linux", map[string]string{"TMP": "", "TEMP": "/"}, "/"},
		"success: POSIX default":                                             {"linux", map[string]string{"TEMP": "", "SystemRoot": "ignored"}, "/tmp"},
		"success: Windows TEMP beats TMP and TMPDIR":                         {"windows", map[string]string{"TEMP": `C:\chosen\\`, "TMP": `C:\ignored`, "TMPDIR": `C:\ignored`}, `C:\chosen\`},
		"success: Windows TMP fallback preserves drive root":                 {"windows", map[string]string{"TEMP": "", "TMP": `D:\`}, `D:\`},
		"success: Windows does not strip a forward slash":                    {"windows", map[string]string{"TEMP": "D:/work/"}, "D:/work/"},
		"success: Windows SystemRoot fallback concatenates without cleaning": {"windows", map[string]string{"SystemRoot": `C:\Windows\`, "windir": `D:\ignored`}, `C:\Windows\\temp`},
		"success: Windows windir fallback":                                   {"windows", map[string]string{"SystemRoot": "", "windir": `D:\Windows`}, `D:\Windows\temp`},
		"success: Windows empty windir concatenation":                        {"windows", map[string]string{"windir": ""}, `\temp`},
		"success: Windows undefined windir concatenation":                    {"windows", nil, `undefined\temp`},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			r := New(func(key string) (string, bool) { value, ok := tt.env[key]; return value, ok }, nil, nil)
			r.platform = tt.platform
			if got := r.TempDir(); got != tt.want {
				t.Errorf("temp directory: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOverrideDoesNotRequireHome(t *testing.T) {
	tests := map[string]struct {
		env  map[string]string
		read func(Resolver) (string, error)
		want string
	}{
		"success: state override alone":                       {map[string]string{"MODEL_GATEWAY_STATE_DIR": "state"}, Resolver.StateDir, "state"},
		"success: config override alone":                      {map[string]string{"CLAUDE_CONFIG_DIR": "config"}, Resolver.ClaudeConfigDir, "config"},
		"success: proxy override wins over the platform path": {map[string]string{"CCP_CONFIG_DIR": "proxy", "APPDATA": "ignored", "XDG_CONFIG_HOME": "ignored"}, Resolver.ProxyConfigDir, "proxy"},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			r := New(func(key string) (string, bool) { value, ok := tt.env[key]; return value, ok }, nil, nil)
			got, err := tt.read(r)
			if got != tt.want || err != nil {
				t.Errorf("got %q, %v, want %q, nil", got, err, tt.want)
			}
		})
	}
}
