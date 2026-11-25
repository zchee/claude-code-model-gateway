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

package gatewaytest

import (
	"context"
	"crypto/rand"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// StartUpstream serves a scripted HTTP handler on a private ephemeral loopback port.
// It returns the base URL and shuts down the server at test cleanup.
func (env Environment) StartUpstream(t testing.TB, handler http.Handler) string {
	t.Helper()
	if handler == nil {
		handler = http.NotFoundHandler()
	}
	base, _ := env.serve(t, &http.Server{Handler: handler})
	return base
}

func (env Environment) serve(t testing.TB, server *http.Server) (string, <-chan struct{}) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := env.Listen(t.Context(), "tcp4", "127.0.0.1:0", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		err := server.Serve(ln)
		close(stopped)
		done <- err
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(errors.Join(err, server.Close()))
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return "http://" + ln.Addr().String(), stopped
}

// StartModelProxy serves a model catalog and sends message requests to the supplied JSON or SSE handler.
func (env Environment) StartModelProxy(t testing.TB, models []string, messages http.Handler) string {
	t.Helper()
	catalog := make([]map[string]string, 0, len(models))
	for _, model := range models {
		catalog = append(catalog, map[string]string{"id": model})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, map[string]any{"data": catalog}); err != nil {
			t.Error(err)
		}
	})
	if messages != nil {
		mux.Handle("POST /v1/messages", messages)
	}
	return env.StartUpstream(t, mux)
}

// CountingProxy observes outbound proxy attempts without contacting their destinations.
type CountingProxy struct {
	URL         string
	connections atomic.Int64
	mu          sync.Mutex
	targets     []string
}

// Connections returns the number of accepted connections, including those with no request.
func (p *CountingProxy) Connections() int64 { return p.connections.Load() }

// Targets returns a snapshot of the request destinations in arrival order.
func (p *CountingProxy) Targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.targets)
}

// StartCountingProxy starts an HTTP proxy that records destinations and always answers 502.
func (env Environment) StartCountingProxy(t testing.TB) *CountingProxy {
	t.Helper()
	p := new(CountingProxy)
	server := &http.Server{ConnState: func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			p.connections.Add(1)
		}
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.Method + " " + r.RequestURI + " " + r.Proto
		if r.Method == http.MethodConnect {
			target = r.RequestURI
		} else if r.URL.Scheme == "http" || r.URL.Scheme == "https" {
			port := r.URL.Port()
			if port == "" {
				port = "80"
				if r.URL.Scheme == "https" {
					port = "443"
				}
			}
			target = net.JoinHostPort(r.URL.Hostname(), port)
		}
		p.mu.Lock()
		p.targets = append(p.targets, target)
		p.mu.Unlock()
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusBadGateway)
	})}
	p.URL, _ = env.serve(t, server)
	return p
}

// StartUsageCollector starts a recording HTTP collector and shuts it down at test cleanup.
func (env Environment) StartUsageCollector(t testing.TB) *UsageCollector {
	t.Helper()
	state := new(usageCollectorState)
	base, stopped := env.serve(t, &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			t.Errorf("read usage request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		state.requests = append(state.requests, UsageRequest{r.Method, r.RequestURI, r.Header.Get("Content-Type"), string(body)})
		state.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})})
	state.endpoint, state.stopped = base+"/v1/logs", stopped
	ctx, cancel := context.WithCancel(t.Context())
	state.ctx = ctx
	// Cleanup cancels children before waiting for them and before the server releases the port.
	t.Cleanup(func() {
		cancel()
		state.mu.Lock()
		idle := state.idle
		state.mu.Unlock()
		if idle == nil {
			return
		}
		select {
		case <-idle:
		case <-time.After(fixtureTimeout):
			t.Error("timed out waiting for usage fixture commands to finish after cancellation")
		}
	})
	return &UsageCollector{URL: state.endpoint, state: state}
}

// HelperCommand prepares a subprocess helper in its own private sandbox directory.
// A consuming package must declare exactly one TestGatewayFixtureHelper that calls DispatchHelper under the process build tag.
// That entry is a caller contract, not an enforced check. Assert on helper-produced output or a file, not exit status alone.
// Custom names match [a-z][a-z0-9-]{0,63}. Use RunFixture on the sandbox environment to launch the helper.
func (env Environment) HelperCommand(t testing.TB, helper string, args ...string) (*exec.Cmd, error) {
	t.Helper()
	dispatch := append([]string{helperSelector, "--", helperDispatchPrefix + helper}, args...)
	if !validHelperDispatch(dispatch) {
		return nil, errors.New("invalid gateway fixture helper dispatch")
	}
	if dispatch[2] == usageHelperDispatch && !validUsageEndpoint(dispatch[3]) {
		return nil, ErrUsageEndpoint
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	root, err := env.openSandboxDir(env["HOME"], true)
	if err != nil {
		return nil, err
	}
	name := "gatewaytest-helper-" + rand.Text()
	dir := filepath.Join(root.Name(), name)
	if err := root.Mkdir(name, 0o700); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	t.Cleanup(func() {
		if err := errors.Join(root.RemoveAll(name), root.Close()); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, dispatch...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, []string{}, fixtureWaitDelay
	return cmd, nil
}

// HelperFunc runs a named subprocess helper with its arguments in the checked fixture directory.
type HelperFunc func(t testing.TB, args []string)

// DispatchHelper handles the reserved subprocess invocation, doing nothing during ordinary test runs.
// A consuming package must declare exactly one TestGatewayFixtureHelper that calls DispatchHelper under the process build tag.
// This is a caller contract: entries in both internal and external test packages can both run, and a missing entry can exit successfully without dispatching.
// Assert on helper-produced output or a file, not exit status alone.
// Pass nil for built-in helpers only; the usage helper returns its validated endpoint argument.
func DispatchHelper(t testing.TB, helpers map[string]HelperFunc) string {
	t.Helper()
	if len(os.Args) < 3 || os.Args[1] != helperSelector || os.Args[2] != "--" {
		return ""
	}
	if !validHelperDispatch(os.Args[1:]) {
		t.Fatal("invalid gateway fixture helper dispatch")
	}
	// The parent passes the checked working directory as the first ExtraFiles descriptor.
	dir := os.NewFile(3, "fixture working directory")
	if dir == nil {
		t.Fatal("missing fixture working directory")
	}
	if err := errors.Join(dir.Chdir(), dir.Close()); err != nil {
		t.Fatal(err)
	}
	for name := range helpers {
		if name == claudeHelperName || name == usageHelperName || legacyHelperName(name) {
			t.Fatalf("cannot override built-in gateway fixture helper %q", name)
		}
	}
	name, args := os.Args[3][len(helperDispatchPrefix):], os.Args[4:]
	switch name {
	case claudeHelperName:
		if err := FakeClaude(args, os.Stdout); err != nil {
			t.Fatal(err)
		}
	case usageHelperName:
		if !validUsageEndpoint(args[0]) {
			t.Fatal(ErrUsageEndpoint)
		}
		return args[0]
	default:
		helper := helpers[name]
		if helper == nil {
			t.Fatalf("unregistered gateway fixture helper %q", name)
		}
		helper(t, args)
	}
	return ""
}

// FakeClaude answers the version and stream-json requests used by a fixture Claude executable.
func FakeClaude(args []string, stdout io.Writer) error {
	if slices.Contains(args, "--version") {
		_, err := fmt.Fprintln(stdout, "fake-claude 2.0.0")
		return err
	}
	i := slices.Index(args, "--model")
	if i < 0 || i+1 >= len(args) {
		return errors.New("fixture claude requires --version or --model ALIAS")
	}
	model := "claude-" + args[i+1] + "-5"
	if args[i+1] == "opus" {
		model = "claude-opus-5-5"
	}
	data, err := json.Marshal(map[string]string{"type": "system", "subtype": "init", "model": model})
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}
