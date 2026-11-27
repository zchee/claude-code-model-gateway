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

//go:build process && (darwin || linux)

package serveshim_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

const (
	fixtureVersion = "1.2.3"
	workerStderr   = "fixture worker stderr"
	fixtureStop    = "{\"type\":\"fixture-shutdown\"}\n"
)

// ownedWorkerKey selects the fixture worker that takes its listener and drain channel from the supervisor. It is set
// only in the environment of a supervisor whose test expects that worker.
const ownedWorkerKey = "SERVESHIM_OWNED_FIXTURE_WORKER"

func TestMain(m *testing.M) {
	venue.Require()
	if slices.Equal(os.Args[1:], []string{"serve-worker"}) && os.Getenv("MODEL_GATEWAY_TEST_SANDBOX") != "" {
		if err := ownedFixtureWorker(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	goleak.VerifyTestMain(m)
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{"supervisor": func(t testing.TB, _ []string) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		opts := serveshim.Options{
			Environment: os.Environ(), Home: os.UserHomeDir, Executable: exe, Version: fixtureVersion,
			Stdout: io.MultiWriter(os.Stdout, &output), Stderr: os.Stderr,
		}
		if err := serveshim.Run(t.Context(), opts); err != nil {
			t.Fatal(err)
		}
		if line, ok := strings.CutSuffix(output.String(), "\n"); !ok || strings.Contains(line, "\n") {
			t.Error("supervisor stdout must contain exactly one complete readiness line")
		}
	}})
}
