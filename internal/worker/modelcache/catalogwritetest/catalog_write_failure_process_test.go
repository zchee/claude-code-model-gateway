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
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
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
	// unlimitedArg makes the child keep its file size limit and check it leaves room for the saved
	// catalog plus catalogSaveMargin.
	unlimitedArg = "unlimited"

	// catalogSaveLimit is a file size limit far below the fetched catalog and far above the failure record.
	catalogSaveLimit = 64 << 10
	// catalogSaveMargin is the room the control asks for beyond the saved catalog document the child
	// encodes itself, so a small difference from the document the cache writes cannot fail the save.
	catalogSaveMargin = 16 << 10
	// catalogRowCount makes the fetched catalog page several times larger than catalogSaveLimit.
	catalogRowCount = 2000

	// staleCatalogNow is a clock reading more than an hour after the seeded catalog's update time,
	// and the child's first clock reading.
	staleCatalogNow = 1_800_000_000_000
	// clockStepMillis is how far each further clock reading of the child is after the one before, so
	// each reading is a distinct literal.
	clockStepMillis = 1000
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

// setCatalogFileLimit applies the file size limit arg selects and returns the original limit; it never
// raises a limit. The unlimited case keeps the inherited limit and checks it leaves room for a saved
// catalog of savedSize bytes plus catalogSaveMargin. The limited case lowers the soft limit to
// catalogSaveLimit unless it is already lower, comparing with an untyped constant because the limit's
// field type differs between platforms (int64 on FreeBSD and DragonFly; uint64 on NetBSD, OpenBSD,
// macOS and Linux). The unlimited case compares with a size known only at run time, so it converts
// the limit to uint64, which keeps every value: no platform has a negative soft limit, and the BSDs'
// unlimited value is the largest int64. A write past the soft limit fails with EFBIG; the Go runtime ignores the accompanying SIGXFSZ.
func setCatalogFileLimit(t testing.TB, arg string, savedSize int) unix.Rlimit {
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("read the file size limit: %v", err)
	}
	switch arg {
	case unlimitedArg:
		if need := savedSize + catalogSaveMargin; uint64(original.Cur) < uint64(need) {
			t.Fatalf("test precondition: soft file size limit %d is below the %d bytes the saved catalog needs (%d-byte document and %d-byte margin)", original.Cur, need, savedSize, catalogSaveMargin)
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
// reads the saved catalog the refresh left behind. The cache's clock gives successive readings
// staleCatalogNow, staleCatalogNow+clockStepMillis, and so on, so the parent can tell which reading
// each saved time is. The child fails unless the cache read its clock exactly twice.
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
	// saved is the document a successful refresh saves, encoded the way the cache encodes it.
	saved, err := jsonx.EncodeState(struct {
		Data      []idDisplay `json:"data"`
		UpdatedAt int64       `json:"updatedAt"`
		Error     *string     `json:"error"`
	}{Data: rows, UpdatedAt: staleCatalogNow})
	if err != nil {
		t.Fatalf("encode the saved catalog: %v", err)
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
	// The clock is read on the caller's goroutine and on the refresh goroutine.
	var clockReads atomic.Int64
	clock := func() time.Time {
		return time.UnixMilli(staleCatalogNow + (clockReads.Add(1)-1)*clockStepMillis)
	}
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            resolver,
		ProxyBaseURL:     "http://127.0.0.1:19991",
		AnthropicBaseURL: "http://anthropic-catalog.test",
		DefaultBaseURL:   "http://127.0.0.1:19999",
		Client:           &http.Client{Transport: handlerTransport{handler: upstream}},
		Clock:            clock,
		Routes:           &route.Store{},
		Sentry:           &sentry.State{},
		Logf:             logs.logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	original := setCatalogFileLimit(t, args[0], len(saved))
	_, refreshErr := cache.ModelsResponse(t.Context(), http.Header{"Authorization": {"Bearer " + authBearerPayload}})
	cache.Wait()
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("restore the file size limit: %v", err)
	}
	if refreshErr != nil {
		t.Fatalf("ModelsResponse: %s", redact(refreshErr.Error()))
	}
	// One reading finds the saved catalog stale and one is the refresh's time. A further reading, even
	// one whose value is not saved, fails here: the saved times alone cannot show it.
	if n := clockReads.Load(); n != 2 {
		t.Fatalf("the cache read its clock %d times, want 2 (the stale check and the refresh)", n)
	}
	if logged := logs.String(); logged != "" {
		t.Fatalf("the refresh logged %q, want no log line", redact(logged))
	}
	fmt.Println(catalogWriteDone)
}

// savedCatalog is the part of anthropic-models.json this test compares. UpdatedAt and AttemptedAt are
// zero when the field is absent.
type savedCatalog struct {
	IDs         []string
	Error       string
	UpdatedAt   float64
	AttemptedAt float64
}

func readSavedCatalog(t *testing.T, path string) savedCatalog {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read anthropic-models.json: %v", err)
	}
	var doc struct {
		Data        []idDisplay `json:"data"`
		Error       *string     `json:"error"`
		UpdatedAt   *float64    `json:"updatedAt"`
		AttemptedAt *float64    `json:"attemptedAt"`
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
	if doc.UpdatedAt != nil {
		saved.UpdatedAt = *doc.UpdatedAt
	}
	if doc.AttemptedAt != nil {
		saved.AttemptedAt = *doc.AttemptedAt
	}
	return saved
}

// TestCatalogSuccessWriteFailureIsRecorded fetches a valid catalog far larger than catalogSaveLimit
// in a child process whose soft file size limit is lowered to it, so saving the fetched rows fails
// and only the small failure record fits. The saved catalog keeps the cached rows and their update
// time with the failure message for a failed save. The control runs the same child without lowering
// the limit, after the child checked the limit leaves room for the catalog, and saves the fetched
// rows, so the size limit is the only difference. Only the child's limit changes; the test binary
// keeps its own.
//
// The child's clock gives a new literal on every reading. The first reading finds the saved catalog
// stale; the second is the time of the refresh. The control saves that second reading as its update
// time, and the failure record carries the same second reading as its attempt time: the clock is not
// read again after the failed save, which would record the third reading. The child also fails on a
// third clock reading anywhere, even one whose value is not saved.
func TestCatalogSuccessWriteFailureIsRecorded(t *testing.T) {
	const (
		// refreshReading is the child's second clock reading, staleCatalogNow + clockStepMillis.
		refreshReading = 1_800_000_001_000
		// seededUpdatedAt is the update time of seededCatalog.
		seededUpdatedAt = 1
	)
	_, fetchedIDs := catalogRows()
	tests := map[string]struct {
		arg  string
		want savedCatalog
	}{
		"success: a fetched catalog that can be saved replaces the cached rows": {
			arg:  unlimitedArg,
			want: savedCatalog{IDs: fetchedIDs, UpdatedAt: refreshReading},
		},
		"error: a fetched catalog that cannot be saved records catalog write failed": {
			arg:  limitedArg,
			want: savedCatalog{IDs: []string{"claude-cached-1"}, Error: "catalog write failed", UpdatedAt: seededUpdatedAt, AttemptedAt: refreshReading},
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
