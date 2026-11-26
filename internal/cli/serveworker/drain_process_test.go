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
	"bufio"
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"regexp"
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

func TestDrainInterruptedByStop(t *testing.T) {
	const body = `{"model":"claude-grok-test\ud800"}`
	tests := map[string]struct {
		useUnix   bool
		parentEOF bool
	}{
		"error: cancellation ends draining worker with stalled TCP upload":  {},
		"error: cancellation ends draining worker with stalled Unix upload": {useUnix: true},
		"error: parent EOF ends draining worker with stalled upload":        {parentEOF: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, err := os.MkdirTemp("", "mg-stop-") // sockaddr_un paths have a small platform limit.
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.RemoveAll(home); err != nil {
					t.Error(err)
				}
			}()
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			state := filepath.Join(env["CLAUDE_CONFIG_DIR"], "model-gateway")
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(drainFixtureToken+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			uploadCtx, cancelUpload := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancelUpload()
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })

			var public string
			done := make(chan struct{})
			var runErr error
			var triggerStop func()
			var cleanup func()

			if tt.parentEOF {
				reportR, reportW := pipe(t)
				liveR, liveW := pipe(t)
				env["MODEL_GATEWAY_CONTROL_FDS"] = "3,4"
				env["MODEL_GATEWAY_WORKER_GENERATION"] = "fixture-gen"
				cmd, err := env.HelperCommand(t, "worker", "supervised")
				if err != nil {
					t.Fatal(err)
				}
				cmd.ExtraFiles = []*os.File{reportW, liveR}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				stdoutR, stdoutW := io.Pipe()
				cmd.Stdout = stdoutW
				portCh := make(chan string, 1)
				pattern := regexp.MustCompile(`^model-gateway shim listening on 127\.0\.0\.1:([0-9]+)(?:\s|$)`)
				var scanWG sync.WaitGroup
				scanWG.Go(func() {
					s := bufio.NewScanner(stdoutR)
					for s.Scan() {
						if m := pattern.FindStringSubmatch(s.Text()); len(m) > 1 {
							select {
							case portCh <- "127.0.0.1:" + m[1]:
							default:
							}
						}
					}
					if err := s.Err(); err != nil {
						t.Errorf("read worker stdout: %v", err)
					}
				})
				go func() {
					defer close(done)
					runErr = env.RunFixtureWithin(cmd, 10*time.Second)
					_ = stdoutW.Close()
				}()
				triggerStop = sync.OnceFunc(func() { _ = liveW.Close() })
				cleanup = func() {
					triggerStop()
					cancelUpload()
					unblock()
					<-done
					scanWG.Wait()
					_ = stdoutR.Close()
				}
				defer cleanup()
				select {
				case public = <-portCh:
				case <-done:
					t.Fatalf("supervised worker exited before readiness: %v; stderr=%s", runErr, &stderr)
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for supervised worker readiness")
				}
				_ = reportW.Close()
				_, _ = io.Copy(io.Discard, io.LimitReader(reportR, 257))
			} else {
				serviceCtx, cancelService := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancelService()
				portCh := make(chan string, 1)
				go func() {
					defer close(done)
					runErr = serveworker.Run(serviceCtx, serveworker.Options{
						Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
						Home:   func() (string, error) { return home, nil },
						Cwd:    func() (string, error) { return home, nil },
						Stdout: logSink(func(p []byte) (int, error) {
							if line, ok := strings.CutPrefix(string(p), "model-gateway shim listening on 127.0.0.1:"); ok {
								port, _, _ := strings.Cut(strings.TrimSpace(line), " ")
								select {
								case portCh <- "127.0.0.1:" + port:
								default:
								}
							}
							return len(p), nil
						}),
					})
				}()
				triggerStop = cancelService
				cleanup = func() {
					cancelService()
					cancelUpload()
					unblock()
					<-done
				}
				defer cleanup()
				select {
				case public = <-portCh:
				case <-done:
					t.Fatalf("worker exited before readiness: %v", runErr)
				case <-serviceCtx.Done():
					t.Fatal(serviceCtx.Err())
				}
			}

			tr := &http.Transport{ExpectContinueTimeout: 10 * time.Second, DisableKeepAlives: true}
			if tt.useUnix {
				socket := env["CODEX_GATEWAY_SOCKET_PATH"]
				tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socket)
				}
			}
			started := make(chan struct{}, 1)
			uploadDone := make(chan error, 1)
			trace := &httptrace.ClientTrace{Got100Continue: func() {
				select {
				case started <- struct{}{}:
				default:
				}
			}}
			req, err := http.NewRequestWithContext(httptrace.WithClientTrace(uploadCtx, trace), http.MethodPost, "http://"+public+"/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Body = io.NopCloser(pausedReader{Reader: strings.NewReader(body), release: release, ctx: uploadCtx})
			req.ContentLength = int64(len(body))
			req.Header.Set("Expect", "100-continue")
			var reqWG sync.WaitGroup
			reqWG.Go(func() {
				defer tr.CloseIdleConnections()
				resp, err := tr.RoundTrip(req)
				if err != nil {
					uploadDone <- err
					return
				}
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				uploadDone <- err
			})
			defer func() { cancelUpload(); unblock(); reqWG.Wait() }()

			select {
			case <-started:
			case err := <-uploadDone:
				t.Fatalf("stalled upload ended early: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for 100-continue on stalled upload")
			}

			status, active, err := postDrain(uploadCtx, public, drainFixtureToken)
			if err != nil || status != http.StatusAccepted || active != 1 {
				t.Fatalf("drain status=%d, active=%d, err=%v", status, active, err)
			}

			// Signal stop while the upload remains stalled; the draining worker must exit promptly.
			triggerStop()
			select {
			case <-done:
				if runErr != nil {
					t.Errorf("draining worker exited with error after stop: %v", runErr)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("draining worker stayed alive after stop while upload remained stalled")
			}
		})
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
