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

package worker_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/worker"
)

func socketOwner(t *testing.T, path string) {
	t.Helper()
	ln, err := worker.ListenUnix(path)
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	if ln == nil {
		t.Fatal("nil Unix listener")
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "owner") }))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

func checkOwner(t *testing.T, path string) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	status, _, body := request(t, &http.Client{Transport: transport}, "GET", "http://localhost/", "", nil)
	equal(t, 200, status)
	equal(t, "owner", body)
}

func TestUnixListenerOwnership(t *testing.T) {
	tests := map[string]struct{ run func(*testing.T, string) }{
		"success: live owner is not replaced": {func(t *testing.T, path string) {
			socketOwner(t, path)
			ln, err := worker.ListenUnix(path)
			if ln != nil {
				defer func() { _ = ln.Close() }()
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("second ListenUnix: %v, want EADDRINUSE", err)
			}
			checkOwner(t, path)
		}},
		"success: refused socket is rebound": {func(t *testing.T, path string) {
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			ln.SetUnlinkOnClose(false)
			if err := ln.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSocket == 0 {
				t.Fatalf("leftover mode %v", info.Mode())
			}
			socketOwner(t, path)
			checkOwner(t, path)
		}},
		"error: plain file is not removed": {func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			ln, err := worker.ListenUnix(path)
			if ln != nil {
				defer func() { _ = ln.Close() }()
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("ListenUnix on file: %v, want EADDRINUSE", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			equal(t, "keep me", string(body))
		}},
	}
	// A subtest's descriptive name can by itself exceed sockaddr_un.sun_path.
	dir, err := os.MkdirTemp("", "mg-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "gw.sock")
			t.Cleanup(func() { _ = os.Remove(path) })
			tt.run(t, path)
		})
	}
}
