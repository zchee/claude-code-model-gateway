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

package localprov_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
)

const (
	probeEnvProxyHelper = "localprov-env-proxy"
	// probeEnvProxyDone is the child's last output line; without it the child did not run the probe.
	probeEnvProxyDone = "localprov env-proxy child completed"
	// probeEnvProxyTarget is a non-loopback LM Studio origin, which must be HTTPS; the environment proxy
	// would carry it as a CONNECT tunnel.
	probeEnvProxyTarget = "https://lmstudio.invalid:1234"
)

// TestGatewayFixtureHelper dispatches the child processes this package launches through gatewaytest.
func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		probeEnvProxyHelper: func(_ testing.TB, args []string) { runProbeEnvProxyChild(t, args) },
	})
}

// TestProbeIgnoresEnvironmentProxy runs the LM Studio probe in a child test process whose environment
// names a decoy proxy for every scheme. Whether the probe gets no client or a client with no
// transport, the decoy receives no request. The child process is needed because net/http reads the
// proxy environment once per process.
func TestProbeIgnoresEnvironmentProxy(t *testing.T) {
	tests := map[string]struct {
		clientShape string
	}{
		"success: probe given no client":                  {clientShape: "none"},
		"success: probe given a client with no transport": {clientShape: "empty"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var decoyHits []string
			decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				decoyHits = append(decoyHits, r.Method+" "+r.RequestURI)
				mu.Unlock()
				w.Header().Set("Connection", "close")
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(decoy.Close)

			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			env["NO_PROXY"], env["no_proxy"] = "", ""
			for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
				env[key] = decoy.URL
			}
			child, err := env.HelperCommand(t, probeEnvProxyHelper, decoy.URL, tt.clientShape)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := env.RunFixtureWithin(child, 30*time.Second); err != nil {
				t.Fatalf("child test process: %v\n%s", err, redactStudioMarkers(output.String()))
			}
			if !strings.Contains(output.String(), probeEnvProxyDone) {
				t.Fatalf("child test process did not run the probe:\n%s", redactStudioMarkers(output.String()))
			}
			mu.Lock()
			hits := slices.Clone(decoyHits)
			mu.Unlock()
			if len(hits) != 0 {
				t.Errorf("the environment proxy received %d request(s), want none: %q", len(hits), hits)
			}
		})
	}
}

// runProbeEnvProxyChild is the child half of TestProbeIgnoresEnvironmentProxy. Its arguments are the
// decoy proxy URL and the client shape.
func runProbeEnvProxyChild(t *testing.T, args []string) {
	if len(args) != 2 {
		t.Fatalf("child arguments = %q, want the decoy URL and the client shape", args)
	}
	decoyURL, clientShape := args[0], args[1]
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, probeEnvProxyTarget+"/api/v1/models", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := http.ProxyFromEnvironment(req); err != nil || u == nil || u.String() != decoyURL {
		t.Fatalf("test precondition: ProxyFromEnvironment = (%v, %v), want the decoy %s", u, err, decoyURL)
	}
	t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)
	var opts localprov.ProbeOptions
	if clientShape == "empty" {
		opts.Client = &http.Client{}
	}
	state := localprov.ProbeStudio(t.Context(), mapLookup(map[string]string{
		"CCP_LMSTUDIO_BASE_URL": probeEnvProxyTarget,
		"HTTP_PROXY":            decoyURL,
		"http_proxy":            decoyURL,
	}), t.TempDir(), opts)
	if !state.Enabled || state.Invalid != nil || state.BaseURL != probeEnvProxyTarget {
		t.Fatalf("test precondition: LM Studio was not probed at %s: %+v", probeEnvProxyTarget, state)
	}
	fmt.Println(probeEnvProxyDone)
}
