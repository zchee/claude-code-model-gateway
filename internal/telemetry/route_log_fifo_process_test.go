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

package telemetry

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRouteLogFIFO(t *testing.T) {
	tests := map[string]struct{ callers int }{
		"error: FIFO at the log path never blocks one append":                  {callers: 1},
		"error: FIFO at the log path never blocks appends from other requests": {callers: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(root+"/logs", 0o700); err != nil {
				t.Fatal(err)
			}
			path := root + "/logs/request-routes.jsonl"
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			opts := recorderOptions(nil)
			opts.LogPath = path
			var diagnostics bytes.Buffer
			opts.Diagnostics = &diagnostics
			r := NewRecorder(opts)
			var wg sync.WaitGroup
			for range tt.callers {
				wg.Go(func() {
					req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages?key=query-sentinel", nil)
					req.Header.Set("X-Claude-Code-Session-Id", "header-session")
					r.LogRoute(req, LogEntry{Backend: "anthropic", Model: new("claude-fable-5")})
				})
			}
			returned := make(chan struct{})
			go func() {
				wg.Wait()
				close(returned)
			}()
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Error("LogRoute blocked on a FIFO at the log path")
				// Release the blocked writers by holding the read side open until they return, so the
				// appending goroutines finish before the test ends. Their few short lines fit in the
				// pipe buffer, so nothing needs to read them.
				reader, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
				if err != nil {
					t.Fatalf("could not open the FIFO to release a blocked LogRoute: %v", err)
				}
				<-returned
				_ = reader.Close()
				return
			}
			checkRouteLogDiagnostics(t, diagnostics.String(), root, tt.callers)
			if info, err := os.Lstat(path); err != nil || info.Mode().Type() != fs.ModeNamedPipe {
				t.Errorf("log path is no longer the FIFO: %v %v", info, err)
			}
		})
	}
}
