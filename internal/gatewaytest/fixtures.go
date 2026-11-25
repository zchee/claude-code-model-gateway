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
	return env.serve(t, &http.Server{Handler: handler})
}

func (env Environment) serve(t testing.TB, server *http.Server) string {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := env.Listen(t.Context(), "tcp4", "127.0.0.1:0", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
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
	return "http://" + ln.Addr().String()
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
	p.URL = env.serve(t, server)
	return p
}

// HelperCommand prepares a registered helper in its own private sandbox directory.
// The caller's test binary must define TestGatewayFixtureHelper(t *testing.T) calling
// DispatchHelper(t). Use RunFixture on the sandbox environment to launch the helper.
func (env Environment) HelperCommand(t testing.TB, helper string, args ...string) (*exec.Cmd, error) {
	t.Helper()
	dispatch := append([]string{helperSelector, "--", "--gatewaytest-helper=" + helper}, args...)
	if !registeredDispatch(dispatch) {
		return nil, errors.New("unregistered gateway fixture helper")
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, dispatch...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, []string{}, time.Second
	return cmd, nil
}

// DispatchHelper handles the reserved subprocess invocation, doing nothing during ordinary test runs.
func DispatchHelper(t testing.TB) {
	t.Helper()
	if len(os.Args) >= 4 && os.Args[1] == helperSelector && registeredDispatch(os.Args[1:]) {
		// The parent passes the checked working directory as the first ExtraFiles descriptor.
		dir := os.NewFile(3, "fixture working directory")
		if dir == nil {
			t.Fatal("missing fixture working directory")
		}
		if err := errors.Join(dir.Chdir(), dir.Close()); err != nil {
			t.Fatal(err)
		}
		if err := FakeClaude(os.Args[4:], os.Stdout); err != nil {
			t.Fatal(err)
		}
	}
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
