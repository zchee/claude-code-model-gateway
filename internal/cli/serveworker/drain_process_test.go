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

package serveworker_test

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveworker"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const drainFixtureToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func postDrain(ctx context.Context, public, token string) (int, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/drain", strings.NewReader("{}"))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("X-Model-Gateway-Control-Token", token)
	req.Header.Set("Content-Type", "application/json")
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var drain struct {
		ActiveRequests int `json:"activeRequests"`
	}
	err = json.UnmarshalRead(resp.Body, &drain)
	return resp.StatusCode, drain.ActiveRequests, err
}

func TestDrainAcrossListeners(t *testing.T) {
	const body = `{"model":"claude-grok-test\ud800"}`
	home, err := os.MkdirTemp("", "mg-drain-") // sockaddr_un paths have a small platform limit.
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	}()
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	state := filepath.Join(home, "state")
	env["MODEL_GATEWAY_STATE_DIR"] = state
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(drainFixtureToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	address := make(chan string, 1)
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = serveworker.Run(ctx, serveworker.Options{
			Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
			Home:   func() (string, error) { return home, nil },
			Cwd:    func() (string, error) { return home, nil },
			Stdout: logSink(func(p []byte) (int, error) {
				if line, ok := strings.CutPrefix(string(p), "model-gateway shim listening on 127.0.0.1:"); ok {
					port, _, _ := strings.Cut(strings.TrimSpace(line), " ")
					address <- "127.0.0.1:" + port
				}
				return len(p), nil
			}),
		})
	}()
	defer func() { cancel(); <-done }()
	var public string
	select {
	case public = <-address:
	case <-done:
		t.Fatalf("no worker readiness: %v", runErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tests := map[string]struct{ socket string }{
		"success: TCP upload finishes":  {},
		"success: Unix upload finishes": {env["CODEX_GATEWAY_SOCKET_PATH"]},
	}
	type response struct {
		name   string
		status int
		err    error
	}
	started := make(chan struct{}, len(tests))
	finished := make(chan response, len(tests))
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var requests sync.WaitGroup
	defer func() { unblock(); cancel(); requests.Wait() }()
	for name, tt := range tests {
		tr := &http.Transport{ExpectContinueTimeout: 10 * time.Second, DisableKeepAlives: true}
		if tt.socket != "" {
			tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", tt.socket)
			}
		}
		trace := &httptrace.ClientTrace{Got100Continue: func() { started <- struct{}{} }}
		r := strings.NewReader(body)
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, "http://"+public+"/v1/messages", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Body = io.NopCloser(pausedReader{Reader: r, release: release, ctx: ctx})
		req.ContentLength = int64(len(body))
		req.Header.Set("Expect", "100-continue")
		requests.Go(func() {
			defer tr.CloseIdleConnections()
			resp, err := tr.RoundTrip(req)
			if err != nil {
				finished <- response{name: name, err: err}
				return
			}
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			finished <- response{name, resp.StatusCode, err}
		})
	}
	for range tests {
		select {
		case <-started:
		case res := <-finished:
			t.Fatalf("%s returned before upload: %d, %v", res.name, res.status, res.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	status, active, err := postDrain(ctx, public, drainFixtureToken)
	if err != nil || status != http.StatusAccepted || active != len(tests) {
		t.Fatalf("drain status=%d, active=%d, err=%v", status, active, err)
	}
	unblock()
	requests.Wait()
	for range tests {
		res := <-finished
		t.Run(res.name, func(t *testing.T) {
			if res.err != nil {
				t.Fatal(res.err)
			}
			if diff := gocmp.Diff(http.StatusBadRequest, res.status); diff != "" {
				t.Error(diff)
			}
		})
	}
	select {
	case <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-ctx.Done():
		t.Fatal(fmt.Errorf("worker did not finish draining: %w", ctx.Err()))
	}
}

type pausedReader struct {
	io.Reader
	release <-chan struct{}
	ctx     context.Context
}

func (p pausedReader) Read(b []byte) (int, error) {
	select {
	case <-p.release:
		return p.Reader.Read(b)
	case <-p.ctx.Done():
		return 0, p.ctx.Err()
	}
}
