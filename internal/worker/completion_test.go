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

package worker_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

func TestBodyCompletion(t *testing.T) {
	tests := map[string]struct {
		body                                                        string
		readErr                                                     error
		cancelOnRead, cancelBeforeAdmission, inputFailure, noBranch bool
		// aborted is whether the handler ends with http.ErrAbortHandler, so that net/http neither answers nor keeps
		// the connection. A departure the worker finds itself aborts; a branch that returned owns the response, and a
		// body that failed while the client remains is answered with the fixed 400.
		aborted bool
	}{
		"error: client leaves before classifier refusal":  {body: `{"model":"claude-grok-test\ud800"}`, cancelOnRead: true, aborted: true},
		"error: client leaves before unconfigured branch": {cancelOnRead: true, noBranch: true, aborted: true},
		"error: client leaves before branch selection":    {cancelOnRead: true, aborted: true},
		"error: client leaves before admission callback":  {cancelBeforeAdmission: true},
		"error: body fails while client remains":          {readErr: errors.New("corrupt chunk length"), inputFailure: true},
		"error: body ends early before context cancels":   {readErr: io.ErrUnexpectedEOF, aborted: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			var terminals []outcome.Result
			var diagnostics bytes.Buffer
			var forwarded, audits, refreshes int
			opts := worker.Options{
				Diagnostics: &diagnostics,
				BeforeBody: func(*http.Request) func(outcome.Result) {
					return func(r outcome.Result) {
						mu.Lock()
						defer mu.Unlock()
						terminals = append(terminals, r)
					}
				},
				LogRoute:         func(*http.Request, outcome.Audit) { audits++ },
				RefreshAnthropic: func(context.Context, http.Header) { refreshes++ },
			}
			if !tt.noBranch {
				opts.Anthropic = func(_ http.ResponseWriter, _ *http.Request, _ route.Snapshot, _ route.Decision, _ []byte, admit admission) (outcome.Result, error) {
					forwarded++
					if tt.cancelBeforeAdmission {
						cancel()
					}
					admit(outcome.Route{Backend: "anthropic"}, &outcome.Audit{Backend: "anthropic"})
					return outcome.Result{}, nil
				}
			}
			h := handler(t, opts)
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", readFunc(func(p []byte) (int, error) {
				if tt.readErr != nil {
					return 0, tt.readErr
				}
				if tt.cancelOnRead {
					cancel()
				}
				return copy(p, tt.body), io.EOF
			}))
			w := httptest.NewRecorder()
			w.Code = 0 // Distinguish no write from an explicit HTTP 200.
			var aborted bool
			func() {
				defer func() {
					if v := recover(); v != nil {
						if v != http.ErrAbortHandler { //nolint:errorlint // Only this exact panic is handled silently by net/http.
							panic(v)
						}
						aborted = true
					}
				}()
				h.ServeHTTP(w, r)
			}()
			if aborted != tt.aborted {
				t.Errorf("handler aborted the connection=%t, want %t", aborted, tt.aborted)
			}
			mu.Lock()
			gotTerminals := append([]outcome.Result(nil), terminals...)
			mu.Unlock()
			equal(t, 1, len(gotTerminals))
			if gotTerminals[0].Cancelled == tt.inputFailure || gotTerminals[0].Contacted {
				t.Errorf("cancelled=%t contacted=%t for input failure=%t", gotTerminals[0].Cancelled, gotTerminals[0].Contacted, tt.inputFailure)
			}
			if !tt.inputFailure && (w.Code != 0 || w.Body.Len() != 0 || diagnostics.Len() != 0) {
				t.Errorf("wrote status %d, %d body bytes, %d diagnostic bytes after cancellation", w.Code, w.Body.Len(), diagnostics.Len())
			}
			if tt.inputFailure {
				equal(t, http.StatusBadRequest, gotTerminals[0].Status)
				equal(t, http.StatusBadRequest, w.Code)
				equal(t, "application/json", w.Header().Get("Content-Type"))
				// As the supervisor's answer to the same failure, the 400 closes the connection and tells the client
				// not to retry.
				equal(t, "close", w.Header().Get("Connection"))
				equal(t, "false", w.Header().Get("X-Should-Retry"))
				equal(t, map[string]any{
					"type":  "error",
					"error": map[string]any{"type": "invalid_request_error", "message": "model-gateway could not read the request body"},
				}, jsonObject(t, w.Body.String()))
				for word := range strings.FieldsSeq(tt.readErr.Error()) {
					if strings.Contains(strings.ToLower(w.Body.String()), word) || strings.Contains(strings.ToLower(diagnostics.String()), word) {
						t.Errorf("response body %q or diagnostics %q leaked %q from read error", w.Body.String(), diagnostics.String(), word)
					}
				}
			}
			if (tt.cancelBeforeAdmission && forwarded != 1) || (!tt.cancelBeforeAdmission && forwarded != 0) || audits != 0 || refreshes != 0 {
				t.Errorf("forward calls=%d, admission audits=%d, refresh calls=%d", forwarded, audits, refreshes)
			}
			health := httptest.NewRecorder()
			h.ServeHTTP(health, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
			state := jsonObject(t, health.Body.String())
			equal(t, any(float64(0)), state["activeRequests"])
			equal(t, any(float64(0)), state["served"].(map[string]any)["anthropic"])
		})
	}
}

func TestModelsHookFailure(t *testing.T) {
	tests := map[string]struct {
		err          error
		cancelClient bool
	}{
		"error: catalog unavailable":                    {err: errors.New("private-catalog-detail")},
		"error: canceled catalog work with live client": {err: context.Canceled},
		"success: departed client suppresses error":     {err: context.Canceled, cancelClient: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls int
			h := handler(t, worker.Options{Models: func(context.Context, http.Header) (jsontext.Value, error) {
				calls++
				if tt.cancelClient {
					cancel()
				}
				return nil, tt.err
			}})
			w := httptest.NewRecorder()
			w.Code = 0 // Distinguish no write from an explicit HTTP 200.
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/models", nil)
			var aborted bool
			func() {
				defer func() {
					if v := recover(); v != nil {
						if !tt.cancelClient || v != http.ErrAbortHandler { //nolint:errorlint // Only this exact panic is handled silently by net/http.
							panic(v)
						}
						aborted = true
					}
				}()
				h.ServeHTTP(w, r)
			}()
			equal(t, 1, calls)
			equal(t, tt.cancelClient, aborted)
			if strings.Contains(w.Body.String(), tt.err.Error()) {
				t.Error("catalog error response leaked raw hook error detail")
			}
			if tt.cancelClient {
				if w.Code != 0 || w.Body.Len() != 0 {
					t.Errorf("departed client received status=%d and %d bytes", w.Code, w.Body.Len())
				}
				return
			}
			// The fixed answer restates modelsFailureBody in handler.go byte for byte: no listing rows, no hook detail.
			const failure = `{"type":"error","error":{"type":"api_error","message":"model-gateway could not build the model listing"}}`
			equal(t, http.StatusBadGateway, w.Code)
			equal(t, "application/json", w.Header().Get("Content-Type"))
			equal(t, failure, w.Body.String())
		})
	}
}
