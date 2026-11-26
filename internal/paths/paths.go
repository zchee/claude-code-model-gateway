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

// Package paths resolves gateway and Claude paths without accessing the filesystem.
// Windows path forms follow the host's path rules; while Windows is compile-only,
// parity with the source's Windows path functions is not claimed, including for
// bare drive prefixes such as C: joined with a name.
package paths

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

// Resolver resolves paths using only the provided environment, home and working directory lookups.
type Resolver struct {
	env      config.LookupEnv
	home     func() (string, error)
	cwd      func() (string, error)
	platform string
}

// New constructs a resolver for the current platform without calling any lookup.
// cwd must return an absolute physical working directory, not a logical PWD spelling.
func New(env config.LookupEnv, home, cwd func() (string, error)) Resolver {
	return Resolver{env: env, home: home, cwd: cwd, platform: runtime.GOOS}
}

// HomeDir uses HOME on POSIX or USERPROFILE on Windows before calling the injected home lookup.
// It returns an error for an explicitly empty HOME, a missing lookup, a lookup failure, or an empty result.
func (r Resolver) HomeDir() (string, error) {
	if r.platform == "windows" {
		if dir := config.UserProfile.Get(r.env); dir != "" {
			return dir, nil
		}
	} else if dir := config.Home.Get(r.env); dir != nil {
		if *dir == "" {
			return "", errors.New("resolve home directory: HOME is set but empty")
		}
		return *dir, nil
	}
	if r.home == nil {
		return "", errors.New("resolve home directory: no home lookup")
	}
	home, err := r.home()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if home == "" {
		return "", errors.New("resolve home directory: empty home")
	}
	return home, nil
}

// ClaudeConfigDir returns CLAUDE_CONFIG_DIR or the .claude directory under the user home.
func (r Resolver) ClaudeConfigDir() (string, error) {
	return r.childPath(config.ClaudeConfig.Get(r.env), r.HomeDir, ".claude")
}

// GatewayHome returns the stable install directory, independent of Claude and state overrides.
func (r Resolver) GatewayHome() (string, error) {
	return r.childPath("", r.HomeDir, ".claude", "model-gateway")
}

// StateDir returns MODEL_GATEWAY_STATE_DIR or the gateway home.
func (r Resolver) StateDir() (string, error) {
	return r.childPath(config.StateDirectory.Get(r.env), r.GatewayHome)
}

// UserSettingsPath returns the user settings path, ignoring CLAUDE_CONFIG_DIR as in the source.
func (r Resolver) UserSettingsPath() (string, error) {
	return r.childPath("", r.HomeDir, ".claude", "settings.json")
}

// GlobalConfigPath returns the Claude global config, which lives directly in the home unless overridden.
func (r Resolver) GlobalConfigPath() (string, error) {
	if dir := config.ClaudeConfig.Get(r.env); dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	return r.childPath("", r.HomeDir, ".claude.json")
}

// TranscriptsDir returns the projects directory below the effective Claude config directory.
func (r Resolver) TranscriptsDir() (string, error) {
	return r.childPath("", r.ClaudeConfigDir, "projects")
}

// DiscoveryCachePath returns the model cache below the effective Claude config directory.
func (r Resolver) DiscoveryCachePath() (string, error) {
	return r.childPath("", r.ClaudeConfigDir, "cache", "gateway-models.json")
}

// PluginRegistryPaths returns the plugin registries, searching the cache override first and removing duplicates.
func (r Resolver) PluginRegistryPaths() ([]string, error) {
	var paths []string
	if dir := config.PluginCache.Get(r.env); dir != "" {
		absolute, err := r.resolve(dir)
		if err != nil {
			return nil, err
		}
		paths = append(paths, filepath.Join(absolute, "installed_plugins.json"))
	}
	conf, err := r.ClaudeConfigDir()
	if err != nil {
		return nil, err
	}
	absolute, err := r.resolve(conf)
	if err != nil {
		return nil, err
	}
	legacy := filepath.Join(absolute, "plugins", "installed_plugins.json")
	if !slices.Contains(paths, legacy) {
		paths = append(paths, legacy)
	}
	return paths, nil
}

// LauncherRegistryPaths also honors the source launcher's whole-config-directory override.
func (r Resolver) LauncherRegistryPaths() ([]string, error) {
	if dir := config.RegistryClaudeHome.Get(r.env); dir != "" {
		return []string{filepath.Join(dir, "plugins", "installed_plugins.json")}, nil
	}
	return r.PluginRegistryPaths()
}

// PluginRoot returns the absolute path to the configured or caller-provided plugin root.
func (r Resolver) PluginRoot(fallback string) (string, error) {
	if override := config.PluginRoot.Get(r.env); override != "" {
		return r.resolve(override)
	}
	return r.resolve(fallback)
}

// ProxyConfigDir resolves the proxy configuration directory for the resolver's platform.
func (r Resolver) ProxyConfigDir() (string, error) {
	if dir := config.ProxyConfig.Get(r.env); dir != "" {
		return dir, nil
	}
	const name = "claude-code-proxy"
	switch r.platform {
	case "windows":
		if dir := config.AppData.Get(r.env); dir != "" {
			return filepath.Join(dir, name), nil
		}
		return r.childPath("", r.HomeDir, "AppData", "Roaming", name)
	case "darwin":
		return r.childPath("", r.HomeDir, ".config", name)
	default:
		if dir := config.XDGConfigHome.Get(r.env); dir != "" {
			return filepath.Join(dir, name), nil
		}
		return r.childPath("", r.HomeDir, ".config", name)
	}
}

// GrokHome resolves the Grok CLI's state directory.
func (r Resolver) GrokHome() (string, error) {
	return r.childPath(config.GrokHome.Get(r.env), r.HomeDir, ".grok")
}

// RequestBodyDir resolves the request-body high-water mark directory.
func (r Resolver) RequestBodyDir() (string, error) {
	return r.childPath(config.RequestBodyDir.Get(r.env), r.StateDir, "request-body")
}

// RequestLogPath resolves the route log file.
func (r Resolver) RequestLogPath() (string, error) {
	return r.childPath(config.RequestLogPath.Get(r.env), r.StateDir, "logs", "request-routes.jsonl")
}

// DispatchCachePath resolves the dispatch route cache file.
func (r Resolver) DispatchCachePath() (string, error) {
	return r.childPath(config.DispatchCachePath.Get(r.env), r.StateDir, "dispatch-routes.json")
}

// SocketPath resolves the gateway control socket or Windows named pipe.
func (r Resolver) SocketPath() (string, error) {
	if r.platform == "windows" {
		return r.childPath(config.SocketPath.Get(r.env), func() (string, error) { return `\\.\pipe\model-gateway`, nil })
	}
	return r.childPath(config.SocketPath.Get(r.env), r.StateDir, "gateway.sock")
}

// HostsFile resolves the path doctor and the migration check read to detect a block written by the Node gateway.
// The Go gateway never writes the hosts file.
func (r Resolver) HostsFile() string {
	if file := config.HostsFile.Get(r.env); file != "" {
		return file
	}
	if r.platform == "windows" {
		return filepath.Join(config.SystemRoot.Get(r.env), "System32", "drivers", "etc", "hosts")
	}
	return "/etc/hosts"
}

// TempDir returns Node's platform temp directory using only the injected environment.
func (r Resolver) TempDir() string {
	if r.platform == "windows" {
		dir := config.Temp.Get(r.env)
		if dir == "" {
			dir = config.Tmp.Get(r.env)
		}
		if dir == "" {
			_, root, _ := config.SystemRoot.Read(r.env)
			if root == "" {
				_, windir, set := config.WinDir.Read(r.env)
				root = windir
				if !set {
					root = "undefined"
				}
			}
			dir = root + `\temp`
		}
		if len(dir) > 1 && dir[len(dir)-1] == '\\' && dir[len(dir)-2] != ':' {
			return dir[:len(dir)-1]
		}
		return dir
	}
	for _, key := range [...]config.Key[string]{config.TmpDir, config.Tmp, config.Temp} {
		if dir := key.Get(r.env); dir != "" {
			if len(dir) > 1 {
				dir = strings.TrimSuffix(dir, "/")
			}
			return dir
		}
	}
	return "/tmp"
}

func (r Resolver) childPath(override string, parent func() (string, error), elements ...string) (string, error) {
	if override != "" {
		return override, nil
	}
	dir, err := parent()
	if err != nil || len(elements) == 0 {
		return dir, err
	}
	return filepath.Join(append([]string{dir}, elements...)...), nil
}

func (r Resolver) workingDir() (string, error) {
	if r.cwd == nil {
		return "", errors.New("resolve working directory: no lookup")
	}
	dir, err := r.cwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return dir, nil
}

func (r Resolver) resolve(name string) (string, error) {
	if filepath.IsAbs(name) {
		return filepath.Clean(name), nil
	}
	dir, err := r.workingDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("resolve working directory: not absolute")
	}
	return filepath.Join(dir, name), nil
}
