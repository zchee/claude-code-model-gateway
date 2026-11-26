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

// Package catalogwritetest checks how the model cache records an Anthropic catalog it fetched but
// could not save. Saving is made to fail by lowering the file size limit, which applies to a whole
// process, so the refresh runs in a child process and the test binary itself keeps its limit. It is
// a separate package because the model cache's own tests already declare the single fixture helper
// entry a package may have, and that entry does not dispatch this child.
package catalogwritetest

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	// catalogWriteHelper names the child that refreshes the catalog under the file size limit its
	// single argument selects.
	catalogWriteHelper = "catalog-write"
	// catalogWriteDone is the child's last output line after every check in the child passed.
	catalogWriteDone = "catalog write child refreshed the catalog"
	// limitedArg makes the child lower its soft file size limit to catalogSaveLimit.
	limitedArg = "limited"
	// unlimitedArg makes the child keep its file size limit and check it has catalogHeadroom.
	unlimitedArg = "unlimited"

	// catalogSaveLimit is a file size limit far below the fetched catalog and far above the failure record.
	catalogSaveLimit = 64 << 10
	// catalogHeadroom is the file size the control needs: well over the fetched catalog, which the
	// child checks, so a saved catalog cannot fail for lack of room.
	catalogHeadroom = 16 << 20
	// catalogRowCount makes the fetched catalog page several times larger than catalogSaveLimit.
	catalogRowCount = 2000

	// staleCatalogNow is a clock reading more than an hour after the seeded catalog's update time.
	staleCatalogNow = 1_800_000_000_000
	// seededCatalog is a valid saved Anthropic catalog that is stale at staleCatalogNow.
	seededCatalog = `{"data":[{"id":"claude-cached-1","display_name":"Cached One"}],"updatedAt":1}`
	// authBearerPayload is a synthetic credential; output that could carry it is redacted.
	authBearerPayload = "anthropic-auth-synth-marker-5e17"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

// TestGatewayFixtureHelper dispatches the child processes this package launches through gatewaytest.
func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		catalogWriteHelper: runCatalogWriteChild,
	})
}

type idDisplay struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// catalogRows returns the rows of the fetched catalog and their IDs in order.
func catalogRows() ([]idDisplay, []string) {
	rows := make([]idDisplay, 0, catalogRowCount)
	ids := make([]string, 0, catalogRowCount)
	for i := range catalogRowCount {
		id := fmt.Sprintf("claude-save-%04d", i)
		rows = append(rows, idDisplay{ID: id, DisplayName: "Saved row " + strings.Repeat("x", 200)})
		ids = append(ids, id)
	}
	return rows, ids
}

// childHome is the home directory of the child's fixture paths below its working directory dir.
func childHome(dir string) string { return filepath.Join(dir, "home") }

// childCatalogPath is where the child's saved Anthropic catalog lives below dir.
func childCatalogPath(dir string) string {
	return filepath.Join(childHome(dir), ".claude", "model-gateway", "anthropic-models.json")
}

// handlerTransport answers every request in memory from handler.
type handlerTransport struct {
	handler http.Handler
}

func (ht handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	ht.handler.ServeHTTP(rec, req)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func redact(s string) string {
	return strings.ReplaceAll(s, authBearerPayload, "<auth-bearer-marker>")
}

// setCatalogFileLimit applies the file size limit arg selects and returns the original limit. The
// limited case lowers the soft limit to catalogSaveLimit and never raises one that is already lower;
// the comparisons use untyped constants because the limit's field type differs between platforms.
// A write past the soft limit fails with EFBIG; the Go runtime ignores the accompanying SIGXFSZ.
func setCatalogFileLimit(t testing.TB, arg string, pageSize int) unix.Rlimit {
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("read the file size limit: %v", err)
	}
	switch arg {
	case unlimitedArg:
		if pageSize >= catalogHeadroom {
			t.Fatalf("test precondition: the %d-byte page does not fit the %d-byte headroom", pageSize, catalogHeadroom)
		}
		if original.Cur < catalogHeadroom {
			t.Fatalf("test precondition: soft file size limit %d is below the %d-byte headroom the saved catalog needs", original.Cur, catalogHeadroom)
		}
	case limitedArg:
		if original.Cur > catalogSaveLimit {
			lowered := original
			lowered.Cur = catalogSaveLimit
			if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &lowered); err != nil {
				t.Fatalf("lower the file size limit: %v", err)
			}
		}
	default:
		t.Fatalf("child argument %q, want %q or %q", arg, limitedArg, unlimitedArg)
	}
	return original
}

// runCatalogWriteChild seeds a stale saved catalog in its working directory, applies the file size
// limit its single argument selects, refreshes the catalog from an in-memory upstream that serves
// catalogRows, and prints catalogWriteDone when the refresh succeeded and logged nothing. The parent
// reads the saved catalog the refresh left behind.
func runCatalogWriteChild(t testing.TB, args []string) {
	if len(args) != 1 {
		t.Fatalf("child arguments = %q, want one file size limit selector", args)
	}
	rows, _ := catalogRows()
	page, err := json.Marshal(struct {
		Data    []idDisplay `json:"data"`
		HasMore bool        `json:"has_more"`
	}{Data: rows, HasMore: false})
	if err != nil {
		t.Fatalf("marshal the catalog page: %v", err)
	}
	if len(page) < 4*catalogSaveLimit {
		t.Fatalf("test precondition: the %d-byte page is not far over the %d-byte limit", len(page), catalogSaveLimit)
	}

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	home := childHome(dir)
	values := map[string]string{
		"HOME":                    home,
		"CLAUDE_CONFIG_DIR":       filepath.Join(home, ".claude"),
		"MODEL_GATEWAY_STATE_DIR": filepath.Join(home, ".claude", "model-gateway"),
		"CODEX_GATEWAY_GROK_HOME": filepath.Join(home, ".grok"),
		"CCP_CONFIG_DIR":          filepath.Join(home, "proxy-config"),
	}
	lookup := func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
	resolver := paths.New(lookup, func() (string, error) { return home, nil }, func() (string, error) { return home, nil })
	stateDir, err := resolver.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	catalogPath := filepath.Join(stateDir, "anthropic-models.json")
	if catalogPath != childCatalogPath(dir) {
		t.Fatalf("test precondition: saved catalog path = %q, want %q", catalogPath, childCatalogPath(dir))
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, []byte(seededCatalog), 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(page)
	})
	var logs logCapture
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            resolver,
		ProxyBaseURL:     "http://127.0.0.1:19991",
		AnthropicBaseURL: "http://anthropic-catalog.test",
		DefaultBaseURL:   "http://127.0.0.1:19999",
		Client:           &http.Client{Transport: handlerTransport{handler: upstream}},
		Clock:            func() time.Time { return time.UnixMilli(staleCatalogNow) },
		Routes:           &route.Store{},
		Sentry:           &sentry.State{},
		Logf:             logs.logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	original := setCatalogFileLimit(t, args[0], len(page))
	_, refreshErr := cache.ModelsResponse(t.Context(), http.Header{"Authorization": {"Bearer " + authBearerPayload}})
	cache.Wait()
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("restore the file size limit: %v", err)
	}
	if refreshErr != nil {
		t.Fatalf("ModelsResponse: %s", redact(refreshErr.Error()))
	}
	if logged := logs.String(); logged != "" {
		t.Fatalf("the refresh logged %q, want no log line", redact(logged))
	}
	fmt.Println(catalogWriteDone)
}

// savedCatalog is the part of anthropic-models.json this test compares.
type savedCatalog struct {
	IDs   []string
	Error string
}

func readSavedCatalog(t *testing.T, path string) savedCatalog {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read anthropic-models.json: %v", err)
	}
	var doc struct {
		Data  []idDisplay `json:"data"`
		Error *string     `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal anthropic-models.json: %v", err)
	}
	saved := savedCatalog{IDs: []string{}}
	for _, row := range doc.Data {
		saved.IDs = append(saved.IDs, row.ID)
	}
	if doc.Error != nil {
		saved.Error = *doc.Error
	}
	return saved
}

// TestCatalogSuccessWriteFailureIsRecorded fetches a valid catalog far larger than catalogSaveLimit
// in a child process whose soft file size limit is lowered to it, so saving the fetched rows fails
// and only the small failure record fits. The saved catalog keeps the cached rows with the failure
// message for a failed save. The control runs the same child without lowering the limit, after the
// child checked the limit leaves room for the catalog, and saves the fetched rows, so the size limit
// is the only difference. Only the child's limit changes; the test binary keeps its own.
func TestCatalogSuccessWriteFailureIsRecorded(t *testing.T) {
	_, fetchedIDs := catalogRows()
	tests := map[string]struct {
		arg  string
		want savedCatalog
	}{
		"success: a fetched catalog that can be saved replaces the cached rows": {
			arg:  unlimitedArg,
			want: savedCatalog{IDs: fetchedIDs},
		},
		"error: a fetched catalog that cannot be saved records catalog write failed": {
			arg:  limitedArg,
			want: savedCatalog{IDs: []string{"claude-cached-1"}, Error: "catalog write failed"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			cmd, err := env.HelperCommand(t, catalogWriteHelper, tt.arg)
			if err != nil {
				t.Fatal(err)
			}
			// The launcher changes cmd.Dir before execution; the child enters this directory by descriptor.
			dir := cmd.Dir
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := env.RunFixtureWithin(cmd, 30*time.Second); err != nil {
				t.Fatalf("catalog write child: %v\n%s", err, redact(output.String()))
			}
			if !strings.Contains(output.String(), catalogWriteDone) {
				t.Fatalf("catalog write child did not report a finished refresh:\n%s", redact(output.String()))
			}
			if diff := gocmp.Diff(tt.want, readSavedCatalog(t, childCatalogPath(dir))); diff != "" {
				t.Errorf("saved catalog after the refresh mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
