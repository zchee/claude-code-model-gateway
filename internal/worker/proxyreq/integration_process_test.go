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

package proxyreq_test

import (
	"bufio"
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/cli"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

type startup struct {
	Trigger     int64
	Policy      *policy.Sentry
	Environment string
	Models      []catalog.Model
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		"worker": func(t testing.TB, _ []string) {
			if status := cli.Run([]string{"model-gateway", "serve-worker"}, os.Stdin, os.Stdout, os.Stderr, os.LookupEnv, os.UserHomeDir); status != 0 {
				t.Fatalf("serve-worker returned %d", status)
			}
		},
		"inspect-settings": func(t testing.TB, _ []string) {
			dir, err := paths.New(os.LookupEnv, os.UserHomeDir, nil).StateDir()
			if err != nil {
				t.Fatal(err)
			}
			windows, err := policy.ReadWindows(filepath.Join(dir, "context-window.json"), os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}
			inputs := policy.ResolverInputs{Windows: windows}
			ids := []string{"claude-gpt-6.1-sol[1m]", "claude-grok-4.5[1m]"}
			prepared, err := catalog.Prepare(ids, catalog.Inputs{}, inputs, os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}
			_, raw, _ := config.CompactTrigger.Read(os.LookupEnv)
			trigger := windows["codex"].CompactAt
			h, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: "routes.json", Now: time.Now, Lookup: os.LookupEnv, Windows: windows, Sentry: new(sentry.State)})
			if err != nil {
				t.Fatal(err)
			}
			s := route.NewSnapshot(policy.NewResolver(inputs), "", nil, nil)
			out, _ := prepare(t, h, s, "/v1/messages", `{"model":"claude-gpt-6.1-sol[1m]"}`, http.Header{"X-Claude-Code-Session-Id": {"startup"}})
			b, err := json.Marshal(startup{trigger, out.Target.Policy, raw, prepared.Build(t.Context()).Models})
			if err != nil {
				t.Fatal(err)
			}
			fmt.Printf("startup=%s\n", b)
		},
	})
}

func TestStartupCompactAt(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": "320000"})
	path := filepath.Join(env["HOME"], ".claude", "model-gateway", "context-window.json")
	if err := policy.WriteWindows(path, []byte(`{"codex":272000,"compactAt":{"codex":242000,"grok":900000}}`)); err != nil {
		t.Fatal(err)
	}
	cmd, err := env.HelperCommand(t, "inspect-settings")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := env.RunFixture(cmd); err != nil {
		t.Fatalf("child: %v: %s", err, &output)
	}
	_, line, ok := strings.Cut(output.String(), "startup=")
	if !ok {
		t.Fatalf("no startup result: %s", &output)
	}
	line, _, _ = strings.Cut(line, "\n")
	var got startup
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatal(err)
	}
	if got.Trigger != 242000 || got.Environment != "320000" {
		t.Errorf("trigger %d, environment %q", got.Trigger, got.Environment)
	}
	if diff := gocmp.Diff(&policy.Sentry{BackendWindow: 920012, CompactTrigger: 242000, Source: "compact-at"}, got.Policy); diff != "" {
		t.Error(diff)
	}
	if len(got.Models) != 2 || got.Models[0].ContextWindow != 272000 {
		t.Fatalf("unexpected catalog: %+v", got.Models)
	}
	for _, fragment := range []string{"requested compact-at 242000; backend window 920012 (gpt-6.1-sol); effective maximum 242000 (compact-at)", "320000 ignored", "can still exceed 272k"} {
		if !strings.Contains(got.Models[0].ContextWindowNote, fragment) {
			t.Errorf("Codex note %q lacks %q", got.Models[0].ContextWindowNote, fragment)
		}
	}
	if !strings.Contains(got.Models[1].ContextWindowNote, "effective maximum 460000 (derived)") {
		t.Errorf("Grok note: %s", got.Models[1].ContextWindowNote)
	}
	if err := os.WriteFile(path, []byte(`{"compactAt":{"claude":242000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ReadWindows(path, nil); err == nil || !strings.Contains(err.Error(), "unsupported compactAt.claude") {
		t.Errorf("invalid settings: %v", err)
	}
}

// Keep draining worker output through shutdown so a request cannot block on its own logs.
func withWorker(t *testing.T, env gatewaytest.Environment, proxy string, run func(string)) (policies []string) {
	t.Helper()
	u, err := url.Parse(proxy)
	if err != nil {
		t.Fatal(err)
	}
	env["CODEX_GATEWAY_PROXY_PORT"] = u.Port()
	env["NODE_CHANNEL_FD"] = "4"
	env["CODEX_GATEWAY_REQUEST_LOG"] = "0"
	cmd, err := env.HelperCommand(t, "worker")
	if err != nil {
		t.Fatal(err)
	}
	live, stop, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	cmd.ExtraFiles = []*os.File{live}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	cmd.Stdout, cmd.Stderr = writer, writer
	done := make(chan error, 1)
	go func() { err := env.RunFixtureWithin(cmd, 20*time.Second); _ = writer.Close(); done <- err }()
	ready := make(chan string, 1)
	drained := make(chan []string, 1)
	go func() {
		defer close(ready)
		scan := bufio.NewReader(reader)
		portRE := regexp.MustCompile(`listening on 127\.0\.0\.1:(\d+)`)
		var lines []string
		var port string
		announced := false
		for {
			text, err := scan.ReadString('\n')
			line := strings.TrimRight(text, "\r\n")
			if strings.Contains(line, "sentry policy id=") {
				lines = append(lines, line)
			}
			if !announced {
				t.Log(line)
				if m := portRE.FindStringSubmatch(line); len(m) > 0 {
					port = m[1]
				}
				if port != "" && len(lines) >= len(policy.Rows()) {
					announced = true
					ready <- "http://127.0.0.1:" + port
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					t.Errorf("read worker output: %v", err)
				}
				drained <- lines
				return
			}
		}
	}()
	defer func() {
		_ = stop.Close()
		policies = <-drained
		if err := <-done; err != nil {
			t.Errorf("worker exit: %v", err)
		}
	}()
	base, ok := <-ready
	if !ok {
		t.Fatal("worker exited before announcing its port and policies")
	}
	if run != nil {
		run(base)
	}
	return policies
}

// Startup policy logs require model discovery to be wired into the running worker.
func TestStartupPolicyLog(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	proxy := env.StartModelProxy(t, []string{"gpt-5.2", "gpt-5.6-sol"}, nil)
	lines := withWorker(t, env, proxy, nil)
	for _, row := range policy.Rows() {
		id := "sentry policy id=" + row.BackendID + " "
		matching := slices.DeleteFunc(slices.Clone(lines), func(s string) bool { return !strings.Contains(s, id) })
		if len(matching) != 1 {
			t.Errorf("%s: got %d policy lines", row.BackendID, len(matching))
			continue
		}
		window := "null"
		if row.BackendWindow != 0 {
			window = strconv.FormatInt(row.BackendWindow, 10)
		}
		prefix := fmt.Sprintf("%sbackendWindow=%s sentry=%s", id, window, row.Sentry)
		pattern := regexp.QuoteMeta(prefix)
		if row.Sentry != "none" {
			pattern += ` effectiveTrigger=\d+ source=(env|derived|cap)`
		}
		if !regexp.MustCompile(pattern + "$").MatchString(matching[0]) {
			t.Errorf("unexpected policy line %q", matching[0])
		}
	}
}

func post(t *testing.T, client *http.Client, base, path, body string, trace *httptrace.ClientTrace) (int, []byte) {
	t.Helper()
	ctx := t.Context()
	if trace != nil {
		ctx = httptrace.WithClientTrace(ctx, trace)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Claude-Code-Session-Id", "test-session")
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	if err := errors.Join(err, res.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

// The turn sequence requires discovery, streaming usage and request preparation in the running worker.
func TestLMTierHTTP(t *testing.T) {
	const baseModel = "lmstudio/qwen3-coder-30b"
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	env["CCP_LMSTUDIO_ENABLED"] = "1"
	env["CCP_LMSTUDIO_BASE_URL"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"models":[{"type":"llm","key":"qwen3-coder-30b","loaded_instances":[{"id":"qwen3-coder-30b","config":{"context_length":65536}}]},{"type":"llm","key":"qwen3-coder-30b-fast","loaded_instances":[{"id":"qwen3-coder-30b-fast","config":{"context_length":65536}}]}]}`)
	}))
	var received []string
	var mu sync.Mutex
	proxy := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"display_name":%q},{"id":%q,"display_name":%q}]}`, baseModel, baseModel+" (lmstudio)", baseModel+"-fast", baseModel+"-fast (lmstudio)")
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.UnmarshalRead(r.Body, &payload); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		received = append(received, payload.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"model\":\"qwen3-coder-30b\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":45000,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":45000,\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	withWorker(t, env, proxy, func(base string) {
		client := &http.Client{Transport: &http.Transport{}}
		defer client.CloseIdleConnections()
		for i, tc := range []struct {
			id     string
			status int
		}{{baseModel, 200}, {baseModel + "-fast", 200}, {baseModel, 413}} {
			body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, "claude-"+tc.id)
			status, response := post(t, client, base, "/v1/messages", body, nil)
			if status != tc.status {
				t.Fatalf("turn %d got HTTP %d: %s", i, status, response)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if diff := gocmp.Diff([]string{baseModel, baseModel + "-fast"}, received); diff != "" {
			t.Error(diff)
		}
	})
}

func TestKeepAliveRefusals(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	h := newHandler(t, env["HOME"], nil, new(sentry.State))
	base := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		s := route.Snapshot{}
		d := s.Classify(r.Method, r.RequestURI, raw)
		out, err := h.Prepare(w, r, s, d, raw)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if out.LocalStatus == 0 {
			http.Error(w, "unexpected forward", http.StatusBadGateway)
		}
	}))
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
	defer client.CloseIdleConnections()
	connections := make(map[net.Conn]bool)
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { connections[info.Conn] = true }}
	var baseline goleak.Option
	for i := range 15 {
		status, b := post(t, client, base, "/v1/messages", `{"model":"claude-gpt-5.6-terra","thread":{"type":"resume"},"messages":[{"role":"user","content":"current turn"}],"max_tokens":1}`, trace)
		if status != http.StatusBadRequest || !bytes.Contains(b, []byte("thread_unsupported_request")) {
			t.Fatalf("request %d returned %d: %s", i, status, b)
		}
		if i == 0 {
			baseline = goleak.IgnoreCurrent()
		}
	}
	if len(connections) != 1 {
		t.Errorf("15 requests used %d sockets", len(connections))
	}
	if err := goleak.Find(baseline); err != nil {
		t.Errorf("goroutines accumulated while the keep-alive connection was still open: %v", err)
	}
}
