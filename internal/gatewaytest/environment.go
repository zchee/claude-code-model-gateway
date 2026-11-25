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

// Package gatewaytest isolates gateway fixtures and guards product operations under test.
// Child environments are exact on the Linux and macOS test venues. Windows is compile-only:
// environment names there are case-insensitive and os/exec may supply SYSTEMROOT.
// The built-in fixture responses do not forward requests or redirect them. The probe checks
// its initial endpoint, not subsequent child traffic; the venue must contain misbehaving fixtures.
package gatewaytest

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
)

var (
	// ErrReservedPort identifies a bind refused before opening a socket.
	ErrReservedPort = errors.New("reserved gateway port")
	// ErrRealClaude identifies a launch refused before starting Claude.
	ErrRealClaude = errors.New("refusing to launch real claude")
	// ErrOutsideSandbox identifies settings access outside the test sandbox.
	ErrOutsideSandbox = errors.New("refusing to touch settings outside the sandbox")
	// ErrInvalidEnvironment identifies a child environment entry os/exec cannot represent exactly.
	ErrInvalidEnvironment = errors.New("invalid fixture environment entry")
)

// Environment is the environment of a gateway process, not its parent's live environment.
type Environment map[string]string

// NewEnv builds an isolated environment in the caller's temporary home. Overrides cannot move the sandbox root.
func NewEnv(home string, parent, overrides, isolatedOverrides map[string]string) Environment {
	socket := filepath.Join(home, "gateway.sock")
	if runtime.GOOS == "windows" {
		socket = `\\.\pipe\model-gateway-test-` + filepath.Base(home)
	}
	env := Environment{
		"HOME":                              home,
		"USERPROFILE":                       home,
		"CLAUDE_CONFIG_DIR":                 filepath.Join(home, ".claude"),
		"CODEX_HOME":                        filepath.Join(home, ".codex"),
		"MODEL_GATEWAY_REQUEST_BODY_DIR":    filepath.Join(home, ".claude", "model-gateway", "request-body"),
		"HTTP_PROXY":                        "",
		"HTTPS_PROXY":                       "",
		"ALL_PROXY":                         "",
		"http_proxy":                        "",
		"https_proxy":                       "",
		"all_proxy":                         "",
		"NO_PROXY":                          "*",
		"no_proxy":                          "*",
		"CODEX_GATEWAY_PORT":                "0",
		"CODEX_GATEWAY_PROXY_PORT":          "0",
		"CODEX_GATEWAY_WORKER_PORT":         "0",
		"CODEX_GATEWAY_COMPAT_PORT":         "0",
		"CODEX_GATEWAY_SOCKET_PATH":         socket,
		"CODEX_GATEWAY_GROK_HOME":           filepath.Join(home, ".grok"),
		"CODEX_GATEWAY_GROK_ENDPOINT":       "http://127.0.0.1:9/v1/responses",
		"CODEX_GATEWAY_ANTHROPIC_UPSTREAM":  "http://127.0.0.1:9",
		"CODEX_GATEWAY_CLAUDE_BIN":          filepath.Join(home, "missing-claude"),
		"CODEX_GATEWAY_HOSTS_FILE":          filepath.Join(home, "hosts"),
		"CODEX_GATEWAY_DISPATCH_CACHE_PATH": filepath.Join(home, "dispatch-routes.json"),
		"CODEX_GATEWAY_REQUEST_LOG_PATH":    filepath.Join(home, "request-routes.jsonl"),
		"CODEX_GATEWAY_TELEMETRY_ENDPOINT":  "http://127.0.0.1:9",
	}
	isolated := func(key string) bool {
		_, forced := env[key]
		return forced || strings.HasPrefix(key, "CODEX_GATEWAY_") || strings.HasPrefix(key, "MODEL_GATEWAY_") || key == "ANTHROPIC_BASE_URL" || key == "ANTHROPIC_UNIX_SOCKET" || key == "CLAUDE_CODE_PLUGIN_CACHE_DIR"
	}
	for key, value := range parent {
		if !isolated(key) {
			env[key] = value
		}
	}
	for key, value := range overrides {
		if inherited, exists := parent[key]; exists && inherited == value && isolated(key) {
			continue
		}
		env[key] = value
	}
	maps.Copy(env, isolatedOverrides)
	env[sandboxKey] = home
	return env
}

const sandboxKey = "MODEL_GATEWAY_TEST_SANDBOX"

func (env Environment) lookup(key string) (string, bool) {
	value, ok := env[key]
	return value, ok
}

func (env Environment) guarded() bool {
	_, sandbox := env[sandboxKey]
	_, runner := env["NODE_TEST_CONTEXT"]
	return sandbox || runner
}

func (env Environment) sandboxRoot() string {
	if root, explicit := env[sandboxKey]; explicit {
		return root
	}
	return paths.New(env.lookup, nil, nil).TempDir()
}

func (env Environment) contains(path string) bool {
	root := env.sandboxRoot()
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil || root == filepath.Dir(root) {
		return false
	}
	// A settings directory may not exist yet. Resolve every existing ancestor;
	// a dangling symlink is an entry, not permission to treat its target as local.
	ancestor := path
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || ancestor == filepath.Dir(ancestor) {
			return false
		}
		ancestor = filepath.Dir(ancestor)
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return false
	}
	suffix, err := filepath.Rel(ancestor, path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Join(resolved, suffix))
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// openSandboxDir opens an existing, clean absolute directory relative to the sandbox handle.
// The returned handle, not the original path, anchors subsequent filesystem operations.
func (env Environment) openSandboxDir(path string, allowRoot bool) (*os.Root, error) {
	base := env.sandboxRoot()
	if !env.guarded() || !filepath.IsAbs(base) || filepath.Clean(base) != base || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrOutsideSandbox
	}
	base, err := filepath.EvalSymlinks(base)
	if err != nil || base == filepath.Dir(base) {
		return nil, ErrOutsideSandbox
	}
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.Join(ErrOutsideSandbox, err)
	}
	rel, err := filepath.Rel(base, dir)
	if err != nil || !filepath.IsLocal(rel) || (!allowRoot && rel == ".") {
		return nil, ErrOutsideSandbox
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return root, nil
	}
	child, openErr := root.OpenRoot(rel)
	if err := errors.Join(openErr, root.Close()); err != nil {
		if child != nil {
			err = errors.Join(err, child.Close())
		}
		return nil, errors.Join(ErrOutsideSandbox, err)
	}
	return child, nil
}

const helperSelector = "-test.run=^TestGatewayFixtureHelper$"

func registeredDispatch(args []string) bool {
	if len(args) < 3 || args[1] != "--" {
		return false
	}
	switch args[0] {
	case helperSelector:
		return args[2] == "--gatewaytest-helper=claude"
	case "-test.run=^TestEmptyEnvironmentHelper$":
		mode, ok := strings.CutPrefix(args[2], "--gatewaytest-helper=")
		return ok && len(args) == 3 && slices.Contains([]string{"empty", "fixture", "alias", "pins", "named-alias", "named-pins"}, mode)
	default:
		return false
	}
}

// RunFixture runs a sandbox executable or a registered helper dispatch with exactly env.
// Only the legacy isolation check may run without a sandbox.
func (env Environment) RunFixture(cmd *exec.Cmd) error {
	entries := make([]string, 0, len(env)) // A nil slice inherits the ambient environment.
	for name, value := range env {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return ErrInvalidEnvironment
		}
		entries = append(entries, name+"="+value)
	}
	if cmd == nil || len(cmd.Args) == 0 || !filepath.IsAbs(cmd.Path) || cmd.Args[0] != cmd.Path {
		return ErrRealClaude
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return err
	}
	program, err := os.Stat(cmd.Path)
	if cmd.Path == self || (err == nil && os.SameFile(program, selfInfo)) {
		if !registeredDispatch(cmd.Args[1:]) || (cmd.Args[1] != helperSelector && !filepath.IsAbs(cmd.Dir)) {
			return ErrRealClaude
		}
		if cmd.Args[1] == helperSelector {
			root, err := env.openSandboxDir(cmd.Dir, false)
			if err != nil {
				return err
			}
			dir, err := root.Open(".")
			if err != nil {
				return errors.Join(err, root.Close())
			}
			// Bootstrap in the trusted sandbox root, not a second lookup of the requested
			// directory. DispatchHelper enters the checked handle before doing fixture work.
			cmd.Dir = env.sandboxRoot()
			cmd.ExtraFiles = append([]*os.File{dir}, cmd.ExtraFiles...)
			cmd.Env = entries
			return errors.Join(cmd.Run(), dir.Close(), root.Close())
		}
	} else if !env.guarded() || !env.contains(cmd.Path) {
		return ErrRealClaude
	}
	cmd.Env = entries
	return cmd.Run()
}

func reservedPort(port int) bool { return port == 18764 || port == 18765 || port == 80 }

// Listen invokes listen only if the bind is permitted and returns its listener.
func (env Environment) Listen(ctx context.Context, network, address string, listen func(context.Context, string, string) (net.Listener, error)) (net.Listener, error) {
	if env.guarded() && slices.Contains([]string{"tcp", "tcp4", "tcp6", "udp", "udp4", "udp6"}, network) {
		_, service, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		port, err := net.DefaultResolver.LookupPort(ctx, network, service)
		if err != nil {
			return nil, err
		}
		if reservedPort(port) {
			return nil, ErrReservedPort
		}
	}
	return listen(ctx, network, address)
}

// StartClaude invokes start with the selected executable only if its path is permitted.
func (env Environment) StartClaude(ctx context.Context, start func(context.Context, string) error) error {
	path := config.ClaudeBin.Get(env.lookup)
	if env.guarded() {
		if !env.contains(path) {
			return ErrRealClaude
		}
		info, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist): // The launcher reports an explicitly named missing fixture.
		case err != nil || !info.Mode().IsRegular():
			return ErrRealClaude
		case runtime.GOOS != "windows":
			// For an absolute Unix path LookPath checks execute access without searching PATH.
			if _, err := exec.LookPath(path); err != nil {
				return ErrRealClaude
			}
		}
	}
	return start(ctx, path)
}

// UserSettingsPath resolves the user settings file subject to the product guard.
func (env Environment) UserSettingsPath() (string, error) {
	path, err := paths.New(env.lookup, nil, nil).UserSettingsPath()
	if err != nil {
		return "", err
	}
	if env.guarded() && !env.contains(path) {
		return "", ErrOutsideSandbox
	}
	return path, nil
}

// WriteSettings invokes write with the supplied settings only if the destination is permitted.
func (env Environment) WriteSettings(path string, settings map[string]any, write func(string, map[string]any) error) error {
	if env.guarded() && !env.contains(path) {
		return ErrOutsideSandbox
	}
	return write(path, settings)
}
