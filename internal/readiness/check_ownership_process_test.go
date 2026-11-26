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

package readiness_test

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
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

const (
	checkEnvProxyHelper = "readiness-env-proxy"
	// checkEnvProxyDone is the child's last output line; without it the child did not run the check.
	checkEnvProxyDone = "readiness env-proxy child completed"
	// checkEnvProxyTarget is a non-loopback proxy address, so the environment proxy would apply to it.
	checkEnvProxyTarget = "http://proxy-models.invalid:1/v1/models"
)

// TestGatewayFixtureHelper dispatches the child processes this package launches through gatewaytest.
func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		checkEnvProxyHelper: func(_ testing.TB, args []string) { runCheckEnvProxyChild(t, args) },
	})
}

// TestProxyCheckIgnoresEnvironmentProxy runs the proxy check in a child test process whose environment
// names a decoy proxy for every scheme. Whether the check gets no client or a client with no transport,
// the decoy receives no request. The child process is needed because net/http reads the proxy
// environment once per process.
func TestProxyCheckIgnoresEnvironmentProxy(t *testing.T) {
	tests := map[string]struct {
		clientShape string
	}{
		"success: check given no client":                  {clientShape: "none"},
		"success: check given a client with no transport": {clientShape: "empty"},
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
			child, err := env.HelperCommand(t, checkEnvProxyHelper, decoy.URL, tt.clientShape)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := env.RunFixtureWithin(child, 30*time.Second); err != nil {
				t.Fatalf("child test process: %v\n%s", err, &output)
			}
			if !strings.Contains(output.String(), checkEnvProxyDone) {
				t.Fatalf("child test process did not run the check:\n%s", &output)
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

// runCheckEnvProxyChild is the child half of TestProxyCheckIgnoresEnvironmentProxy. Its arguments are
// the decoy proxy URL and the client shape.
func runCheckEnvProxyChild(t *testing.T, args []string) {
	if len(args) != 2 {
		t.Fatalf("child arguments = %q, want the decoy URL and the client shape", args)
	}
	decoyURL, clientShape := args[0], args[1]
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, checkEnvProxyTarget, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := http.ProxyFromEnvironment(req); err != nil || u == nil || u.String() != decoyURL {
		t.Fatalf("test precondition: ProxyFromEnvironment = (%v, %v), want the decoy %s", u, err, decoyURL)
	}
	t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)
	probe := readiness.ProxyAuthProbe{ProxyBinary: fakeProxyBinary(t), ProxyModelsURL: checkEnvProxyTarget}
	if clientShape == "empty" {
		probe.Client = &http.Client{}
	}
	if got := readiness.ProbeProxyAndAuth(t.Context(), probe); !got.ProxyBinary {
		t.Fatalf("test precondition: the proxy check did not run: %+v", got)
	}
	fmt.Println(checkEnvProxyDone)
}
