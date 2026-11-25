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

package controlauth_test

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestAuthenticated(t *testing.T) {
	const host = "127.0.0.1:12345"
	token := strings.Repeat("a", 64)
	tests := map[string]struct {
		method, host, contentType, server, supplied string
		origin                                      *string
		ports                                       []int
		compat, want                                bool
	}{
		"success: matching request":      {method: "POST", host: host, contentType: "application/json", server: token, supplied: token, ports: []int{0, 12345}, want: true},
		"error: missing header":          {method: "POST", host: host, contentType: "application/json", server: token, ports: []int{12345}},
		"error: wrong token":             {method: "POST", host: host, contentType: "application/json", server: token, supplied: strings.Repeat("b", 64), ports: []int{12345}},
		"error: short token":             {method: "POST", host: host, contentType: "application/json", server: token, supplied: "short", ports: []int{12345}},
		"error: invalid matching tokens": {method: "POST", host: host, contentType: "application/json", server: strings.Repeat("A", 64), supplied: strings.Repeat("A", 64), ports: []int{12345}},
		"error: browser origin":          {method: "POST", host: host, contentType: "application/json", server: token, supplied: token, origin: new("https://fixture.invalid"), ports: []int{12345}},
		"error: null origin":             {method: "POST", host: host, contentType: "application/json", server: token, supplied: token, origin: new("null"), ports: []int{12345}},
		"error: foreign host":            {method: "POST", host: "fixture.invalid:12345", contentType: "application/json", server: token, supplied: token, ports: []int{12345}},
		"error: incorrect port":          {method: "POST", host: host, contentType: "application/json", server: token, supplied: token, ports: []int{12346}},
		"error: zero port":               {method: "POST", host: "127.0.0.1:0", contentType: "application/json", server: token, supplied: token, ports: []int{0}},
		"error: plain text":              {method: "POST", host: host, contentType: "text/plain", server: token, supplied: token, ports: []int{12345}},
		"error: charset parameter":       {method: "POST", host: host, contentType: "application/json; charset=utf-8", server: token, supplied: token, ports: []int{12345}},
		"error: wrong method":            {method: "GET", host: host, contentType: "application/json", server: token, supplied: token, ports: []int{12345}},
		"error: compatibility listener":  {method: "POST", host: host, contentType: "application/json", server: token, supplied: token, ports: []int{12345}, compat: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), tt.method, "/restart", nil)
			r.Host = tt.host
			r.Header.Set("Content-Type", tt.contentType)
			if tt.supplied != "" {
				r.Header.Set("X-Model-Gateway-Control-Token", tt.supplied)
			}
			if tt.origin != nil {
				r.Header.Set("Origin", *tt.origin)
			}
			if diff := gocmp.Diff(tt.want, controlauth.Authenticated(r, tt.server, tt.ports, tt.compat)); diff != "" {
				t.Fatalf("authorized (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLocalRequestAllowed(t *testing.T) {
	hosts := []string{"localhost:12345", "127.0.0.1:12345"}
	tests := map[string]struct {
		host   string
		origin *string
		want   bool
	}{
		"success: exact host":               {host: "localhost:12345", want: true},
		"error: different host case":        {host: "LOCALHOST:12345"},
		"error: empty origin still present": {host: "127.0.0.1:12345", origin: new("")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
			r.Host = tt.host
			if tt.origin != nil {
				r.Header.Set("Origin", *tt.origin)
			}
			if diff := gocmp.Diff(tt.want, controlauth.LocalRequestAllowed(r, hosts)); diff != "" {
				t.Fatalf("local request (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMissingToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "control-token")
	token, err := controlauth.ReadToken(file)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff("", token); diff != "" {
		t.Fatal(diff)
	}
	headers, err := controlauth.RequestHeaders(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 0 {
		t.Fatalf("absent token supplied %d headers", len(headers))
	}
}

func TestEnsureToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "control-token")
	want := strings.Repeat("ab", 32)
	token, err := controlauth.EnsureToken(file, bytes.NewReader(bytes.Repeat([]byte{0xab}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(want, token); diff != "" {
		t.Fatalf("generated token (-want +got):\n%s", diff)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(want+"\n", string(data)); diff != "" {
		t.Fatalf("token file (-want +got):\n%s", diff)
	}
	again, err := controlauth.EnsureToken(file, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(token, again); diff != "" {
		t.Fatalf("reopened token (-want +got):\n%s", diff)
	}
	headers, err := controlauth.RequestHeaders(file)
	if err != nil {
		t.Fatal(err)
	}
	expected := make(http.Header)
	expected.Set("X-Model-Gateway-Control-Token", token)
	if diff := gocmp.Diff(expected, headers); diff != "" {
		t.Fatalf("control headers (-want +got):\n%s", diff)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if diff := gocmp.Diff(fs.FileMode(0o600), info.Mode().Perm()); diff != "" {
			t.Fatalf("token permissions (-want +got):\n%s", diff)
		}
	}
}

func TestInvalidToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(file, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := controlauth.EnsureToken(file, bytes.NewReader(make([]byte, 32)))
	if err == nil || !strings.Contains(err.Error(), "invalid local control token") {
		t.Fatalf("EnsureToken error = %v, want invalid local control token", err)
	}
	_, err = controlauth.RequestHeaders(file)
	if err == nil || !strings.Contains(err.Error(), "invalid local control token") {
		t.Fatalf("RequestHeaders error = %v, want invalid local control token", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff("invalid", string(data)); diff != "" {
		t.Fatal(diff)
	}
}
