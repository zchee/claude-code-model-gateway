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

// Package discoverymodetest checks the discovery cache's file mode in child processes. It is a
// separate package because the model cache's own tests already declare the single fixture helper
// entry a package may have, and that entry does not dispatch this child.
package discoverymodetest

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	// discoveryUmaskHelper names the child that writes the discovery cache under a creation mask.
	discoveryUmaskHelper = "discovery-umask"
	// discoveryUmaskDone starts the child's last output line; the sync state follows it.
	discoveryUmaskDone = "discovery umask child wrote state="
	// gatewayURL is both the default and the effective base URL, so the cache counts as wired.
	gatewayURL = "http://127.0.0.1:19999"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

// TestGatewayFixtureHelper dispatches the child processes this package launches through gatewaytest.
func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		discoveryUmaskHelper: runDiscoveryUmaskChild,
	})
}

// refusingTransport fails every request, so the child's cache never reaches a network.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("proxy unavailable")
}

// discoveryChildHome is the home directory the child creates inside its fixture directory.
func discoveryChildHome(dir string) string { return filepath.Join(dir, "home") }

// discoveryChildCachePath is where the child's discovery cache lives below dir.
func discoveryChildCachePath(dir string) string {
	return filepath.Join(discoveryChildHome(dir), ".claude", "cache", "gateway-models.json")
}

// runDiscoveryUmaskChild prepares a wired cache in its working directory, sets the creation mask
// named by its single octal argument only around the discovery sync, and prints the sync state.
// Every directory exists before the mask applies, so the mask can affect only the cache file.
func runDiscoveryUmaskChild(t testing.TB, args []string) {
	if len(args) != 1 {
		t.Fatalf("child arguments = %q, want one octal creation mask", args)
	}
	mask, err := strconv.ParseUint(args[0], 8, 9)
	if err != nil {
		t.Fatalf("creation mask %q: %v", args[0], err)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	home := discoveryChildHome(dir)
	values := map[string]string{
		"HOME":                    home,
		"CLAUDE_CONFIG_DIR":       filepath.Join(home, ".claude"),
		"MODEL_GATEWAY_STATE_DIR": filepath.Join(home, ".claude", "model-gateway"),
		"CODEX_GATEWAY_GROK_HOME": filepath.Join(home, ".grok"),
		"CCP_CONFIG_DIR":          filepath.Join(home, "proxy-config"),
	}
	lookup := func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
	resolver := paths.New(lookup, func() (string, error) { return home, nil }, func() (string, error) { return home, nil })
	cachePath, err := resolver.DiscoveryCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if cachePath != discoveryChildCachePath(dir) {
		t.Fatalf("test precondition: discovery cache path = %q, want %q", cachePath, discoveryChildCachePath(dir))
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            resolver,
		ProxyBaseURL:     "http://127.0.0.1:19991",
		DefaultBaseURL:   gatewayURL,
		Client:           &http.Client{Transport: refusingTransport{}},
		Clock:            time.Now,
		Routes:           &route.Store{},
		Sentry:           &sentry.State{},
		EffectiveBaseURL: func() (string, error) { return gatewayURL, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	previous := unix.Umask(int(mask))
	result, syncErr := cache.SyncDiscoveryCache(false)
	unix.Umask(previous)
	if syncErr != nil {
		t.Fatalf("SyncDiscoveryCache under umask %s: %v", args[0], syncErr)
	}
	fmt.Println(discoveryUmaskDone + result.State)
}

// TestDiscoveryCacheModeFollowsCreationMask writes the discovery cache in a child process under each
// creation mask. The cache requests mode 0600 in its atomic replacement and changes the mode of the
// destination name no further, so the creation mask removes permission bits from 0600: a mask that
// removes the owner's write bit leaves a read-only 0400 file, where a later chmod of the destination
// to 0600 would have restored the write bit.
func TestDiscoveryCacheModeFollowsCreationMask(t *testing.T) {
	tests := map[string]struct {
		mask string
		want fs.FileMode
	}{
		"success: permissive mask keeps 0600": {
			mask: "0000",
			want: 0o600,
		},
		"success: ordinary mask keeps 0600": {
			mask: "0022",
			want: 0o600,
		},
		"success: private mask keeps 0600": {
			mask: "0077",
			want: 0o600,
		},
		"success: mask without owner write leaves 0400": {
			mask: "0200",
			want: 0o400,
		},
		"success: mask without owner read leaves 0200": {
			mask: "0400",
			want: 0o200,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			cmd, err := env.HelperCommand(t, discoveryUmaskHelper, tt.mask)
			if err != nil {
				t.Fatal(err)
			}
			// The launcher changes cmd.Dir before execution; the child enters this directory by descriptor.
			dir := cmd.Dir
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := env.RunFixtureWithin(cmd, 30*time.Second); err != nil {
				t.Fatalf("discovery umask child: %v\n%s", err, output.String())
			}
			if !strings.Contains(output.String(), discoveryUmaskDone+"wrote") {
				t.Fatalf("discovery umask child did not report a written cache:\n%s", output.String())
			}
			info, err := os.Lstat(discoveryChildCachePath(dir))
			if err != nil {
				t.Fatalf("stat the discovery cache: %v", err)
			}
			if !info.Mode().IsRegular() {
				t.Fatalf("discovery cache type = %v, want a regular file", info.Mode().Type())
			}
			if diff := gocmp.Diff(tt.want, info.Mode().Perm()); diff != "" {
				t.Errorf("discovery cache mode under umask %s mismatch (-want +got):\n%s", tt.mask, diff)
			}
		})
	}
}
