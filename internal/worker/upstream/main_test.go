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

package upstream_test

import (
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

const (
	testEnvProxyHost = "env-proxy.invalid:3128"
	testEnvProxyURL  = "http://" + testEnvProxyHost
)

func TestMain(m *testing.M) {
	venue.Require()
	// Seed proxy environment variables before any net/http transport can cache
	// ProxyFromEnvironment through its process-wide sync.Once.
	_ = os.Setenv("HTTP_PROXY", testEnvProxyURL)
	_ = os.Setenv("HTTPS_PROXY", testEnvProxyURL)
	_ = os.Setenv("http_proxy", testEnvProxyURL)
	_ = os.Setenv("https_proxy", testEnvProxyURL)
	_ = os.Setenv("NO_PROXY", "")
	_ = os.Setenv("no_proxy", "")
	goleak.VerifyTestMain(m)
}
