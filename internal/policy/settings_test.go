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

package policy_test

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

func lookup(env map[string]string) config.LookupEnv {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func window(tokens int, source string) policy.Setting {
	return policy.Setting{Value: config.ContextWindow{Full: tokens == 0, Tokens: tokens}, Source: source}
}

func TestReadWindows(t *testing.T) {
	const legacy = "CODEX_GATEWAY_CONTEXT_WINDOW"
	tests := map[string]struct {
		file, env           string
		claude, codex, grok policy.Setting
		err                 string
	}{
		"success: absent file":                                          {claude: window(0, "default"), codex: window(272000, "default"), grok: window(0, "default")},
		"success: env overrides default":                                {env: "250000", claude: window(0, "default"), codex: window(250000, legacy), grok: window(0, "default")},
		"success: saved takes precedence":                               {file: `{"claude":50000,"codex":"full","grok":400000}`, env: "250000", claude: window(0, "default"), codex: window(0, "saved"), grok: window(400000, "saved")},
		"success: corrupt file ignored":                                 {file: "not json", claude: window(0, "default"), codex: window(272000, "default"), grok: window(0, "default")},
		"success: invalid saved value falls back to env":                {file: `{"codex":"184999"}`, env: "0x3d090", claude: window(0, "default"), codex: window(250000, legacy), grok: window(0, "default")},
		"success: invalid legacy setting falls back":                    {env: "FULL", claude: window(0, "default"), codex: window(272000, "default"), grok: window(0, "default")},
		"success: JSON strings and singleton arrays coerce for windows": {file: `{"claude":"1e5","codex":[["0x3d090"]],"grok":true,"compactAt":{"codex":1}}`, claude: window(100000, "saved"), codex: policy.Setting{Value: config.ContextWindow{Tokens: 250000}, Source: "saved", CompactAt: 1}, grok: window(0, "default")},
		"error: compact maximum cannot be a string in saved settings":   {file: `{"compactAt":{"codex":"1"}}`, err: "invalid compactAt.codex"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "context-window.json")
			if tt.file != "" {
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{}
			if tt.env != "" {
				env[legacy] = tt.env
			}
			got, err := policy.ReadWindows(path, lookup(env))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("ReadWindows error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			equal(t, policy.Windows{"claude": tt.claude, "codex": tt.codex, "grok": tt.grok}, got)
		})
	}
}

func TestDefaultClientWindows(t *testing.T) {
	r := policy.Resolver{}
	tests := map[string]struct {
		id, client string
		tokens     int64
	}{
		"success: default Codex":              {"gpt-5.6-sol", "claude-gpt-5.6-sol[1m]", 272000},
		"success: native Grok":                {"claude-grok-4.5[1m]", "claude-grok-4.5[1m]", 500000},
		"success: native Claude":              {"claude-opus-5-5[1m]", "claude-opus-5-5", 0},
		"success: Vertex OpenAI":              {"claude-vertex/openai/gpt-6.1-sol[1m]", "claude-vertex/openai/gpt-6.1-sol[1m]", 922000},
		"success: Codex Sol":                  {"claude-gpt-6.1-sol[1m]", "claude-gpt-6.1-sol[1m]", 272000},
		"success: unknown Codex":              {"claude-gpt-9", "claude-gpt-9[1m]", 272000},
		"success: native Grok without suffix": {"claude-grok-4.6", "claude-grok-4.6[1m]", 500000},
		"success: excluded legacy id":         {"claude-codex-gpt-5.6-sol", "claude-codex-gpt-5.6-sol", 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			equal(t, tt.tokens, r.AdvertisedWindow(tt.id))
			equal(t, tt.client, r.ClientID(tt.id))
		})
	}
	equal(t, int64(272000), r.AdvertisedWindow("claude-gpt-5.6-sol[1m]"))
	equal(t, (*policy.Model)(nil), r.Resolve("claude-codex-gpt-5.6-sol"))
	equal(t, int64(920000), r.CodexWindow("unknown"))
	equal(t, int64(272000), r.CodexWindow("gpt-6.1-sol"))
	equal(t, "default", r.WindowModelID(""))
	equal(t, "gpt-6.1-sol", r.WindowModelID("claude-gpt-6.1-sol-ultrafast[1m]"))
	equal(t, "lmstudio/a-fast", r.WindowModelID("claude-lmstudio/a-fast"))
}

func TestBackendCaps(t *testing.T) {
	w := policy.Windows{"codex": window(272000, "saved"), "grok": window(300000, "saved")}
	equal(t, int64(0), w.Cap("vertex"))
	equal(t, int64(0), w.Cap("muse"))
	equal(t, int64(272000), w.Cap("codex"))
	equal(t, int64(300000), w.Cap("grok"))
	r := policy.Resolver{Windows: policy.Windows{"codex": window(200000, "saved")}}
	equal(t, "claude-gpt-5.6-sol", r.ClientID("gpt-5.6-sol"))
	equal(t, int64(200000), r.AdvertisedWindow("gpt-5.6-sol"))
}

func TestWindowFlags(t *testing.T) {
	const usage = "context-window expects --claude, --codex, or --grok followed by a token count or full; --codex-compact-at or --grok-compact-at accepts tokens or cap"
	const unsupported = "direct compaction maxima are supported for Codex and Grok only; use --claude <tokens|full> for the native window"
	type flagCase = struct {
		flag string
		raw  *string
		want policy.Update
		msg  string
	}
	tests := map[string]flagCase{
		"success: Codex":                                      {"--codex", new("272000"), policy.Update{Backend: "codex", Field: "window", Tokens: 272000}, ""},
		"success: Claude full":                                {"--claude", new("full"), policy.Update{Backend: "claude", Field: "window"}, ""},
		"success: Grok full":                                  {"--grok", new("full"), policy.Update{Backend: "grok", Field: "window"}, ""},
		"success: Codex full":                                 {"--codex", new("full"), policy.Update{Backend: "codex", Field: "window"}, ""},
		"success: direct Codex":                               {"--codex-compact-at", new("242000"), policy.Update{Backend: "codex", Field: "compactAt", Tokens: 242000}, ""},
		"success: direct Grok":                                {"--grok-compact-at", new("1"), policy.Update{Backend: "grok", Field: "compactAt", Tokens: 1}, ""},
		"success: clear direct":                               {"--codex-compact-at", new("cap"), policy.Update{Backend: "codex", Field: "compactAt"}, ""},
		"success: largest exact integer":                      {"--grok-compact-at", new("9007199254740991"), policy.Update{Backend: "grok", Field: "compactAt", Tokens: 9007199254740991}, ""},
		"success: JS rounds fractional decimal to an integer": {"--codex-compact-at", new("1.0000000000000001"), policy.Update{Backend: "codex", Field: "compactAt", Tokens: 1}, ""},
		"error: no prefix":                                    {"codex", new("272000"), policy.Update{}, usage},
		"error: single hyphen":                                {"-codex", new("272000"), policy.Update{}, usage},
		"error: unknown backend":                              {"--haiku", new("full"), policy.Update{}, usage},
		"error: wrong flag case":                              {"--Codex", new("full"), policy.Update{}, usage},
		"error: equals spelling":                              {"--codex=272000", nil, policy.Update{}, usage},
		"error: Claude compact flag":                          {"--claude-compact-at", new("242000"), policy.Update{}, "unsupported compactAt.claude: " + unsupported},
		"error: arbitrary compact flag":                       {"--z-compact-at", new("1"), policy.Update{}, "unsupported compactAt.z: " + unsupported},
	}
	for _, backend := range []string{"claude", "codex", "grok"} {
		low := 185000
		if backend == "claude" {
			low = 100000
		}
		for _, n := range []int{low, 1000000} {
			s := fmt.Sprint(n)
			tests[backend+" inclusive "+s] = flagCase{"--" + backend, new(s), policy.Update{Backend: backend, Field: "window", Tokens: int64(n)}, ""}
		}
		for _, s := range []string{fmt.Sprint(low - 1), "1000001", "FULL", " full ", "1.5", "1_000_000"} {
			msg := fmt.Sprintf("invalid %s context window \"%s\": use full or a whole number of tokens from %d to 1000000", backend, s, low)
			tests[backend+" rejects "+s] = flagCase{"--" + backend, new(s), policy.Update{}, msg}
		}
	}
	for _, s := range []string{" 250000 ", "+250000.", "2.5e5", "0x3d090", "0o750220", "0b111101000010010000", string(rune(0xfeff)) + "250000" + string(rune(0x00a0))} {
		tests["numeric spelling "+s] = flagCase{"--codex", new(s), policy.Update{Backend: "codex", Field: "window", Tokens: 250000}, ""}
	}
	for _, s := range []string{"0", "-1", "1.5", "Infinity", "NaN", "", "9007199254740992", "full", "CAP", " cap "} {
		tests["compact rejects "+s] = flagCase{"--codex-compact-at", new(s), policy.Update{}, fmt.Sprintf("invalid codex compact-at \"%s\": use cap or a positive whole token count", s)}
	}
	tests["error: missing compact argument"] = flagCase{"--codex-compact-at", nil, policy.Update{}, `invalid codex compact-at "undefined": use cap or a positive whole token count`}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := policy.ParseUpdate(tt.flag, tt.raw)
			if tt.msg != "" {
				if err == nil {
					t.Fatal("invalid update accepted")
				}
				equal(t, tt.msg, err.Error())
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			equal(t, tt.want, got)
		})
	}
}

func TestCompactAtTypes(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want int64
	}{
		"error: boolean":           {"true", 0},
		"error: array":             {"[1]", 0},
		"error: null":              {"null", 0},
		"success: string exponent": {`"2.42e5"`, 242000},
		"success: string radix":    {`"0x10"`, 16},
		"success: number":          {"1", 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { equal(t, tt.want, policy.ParseCompactAt(jsontext.Value(tt.raw))) })
	}
}

func TestWriteWindows(t *testing.T) {
	tests := map[string]struct{ raw, msg string }{
		"error: unsupported backend": {`{"compactAt":{"claude":242000}}`, "unsupported compactAt.claude"},
		"error: zero maximum":        {`{"compactAt":{"codex":0}}`, "invalid compactAt.codex"},
		"error: string maximum":      {`{"compactAt":{"codex":"242000"}}`, "invalid compactAt.codex"},
		"error: null maximum":        {`{"compactAt":{"codex":null}}`, "invalid compactAt.codex"},
		"error: unsafe maximum":      {`{"compactAt":{"grok":9007199254740992}}`, "invalid compactAt.grok"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "nested")
			err := policy.WriteWindows(filepath.Join(dir, "windows.json"), jsontext.Value(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("WriteWindows error = %v, want %q", err, tt.msg)
			}
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("validation created parent directory or stat failed: %v", err)
			}
		})
	}
	t.Run("success: readable nested state", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "windows.json")
		err := policy.WriteWindows(path, jsontext.Value(`{"codex":"full","claude":50000,"grok":400000}`))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, "{\n  \"codex\": \"full\",\n  \"claude\": 50000,\n  \"grok\": 400000\n}\n", string(data))
		got, err := policy.ReadWindows(path, lookup(map[string]string{"CODEX_GATEWAY_CONTEXT_WINDOW": "250000"}))
		if err != nil {
			t.Fatal(err)
		}
		equal(t, policy.Windows{"claude": window(0, "default"), "codex": window(0, "saved"), "grok": window(400000, "saved")}, got)
	})
}

func compactJSON(t *testing.T, v jsontext.Value) string {
	t.Helper()
	if err := v.Compact(); err != nil {
		t.Fatal(err)
	}
	return string(v)
}

func TestSavedWindows(t *testing.T) {
	w := policy.Windows{"claude": window(0, "default"), "codex": window(300000, "saved"), "grok": window(0, "saved")}
	equal(t, `{"codex":300000,"grok":"full"}`, compactJSON(t, w.Saved()))
	c := w["codex"]
	c.CompactAt = 1
	w["codex"] = c
	equal(t, `{"codex":300000,"grok":"full","compactAt":{"codex":1}}`, compactJSON(t, w.Saved()))
	tests := map[string]struct {
		start, backend, field string
		n                     int64
		want                  string
	}{
		"success: remove last maximum":    {`{"compactAt":{"grok":1}}`, "grok", "compactAt", 0, `{}`},
		"success: preserve other maximum": {`{"compactAt":{"grok":1,"codex":2}}`, "grok", "compactAt", 0, `{"compactAt":{"codex":2}}`},
		"success: set maximum":            {`{}`, "codex", "compactAt", 3, `{"compactAt":{"codex":3}}`},
		"success: set full":               {`{}`, "claude", "window", 0, `{"claude":"full"}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := (policy.Update{Backend: tt.backend, Field: tt.field, Tokens: tt.n}).Apply(jsontext.Value(tt.start))
			if err != nil {
				t.Fatal(err)
			}
			equal(t, tt.want, compactJSON(t, got))
		})
	}
}

const (
	crossing = "OpenAI bills input above 272k tokens at 2x; the crossing turn and compaction request can still exceed 272k and pay double"
	past     = "OpenAI bills input above 272k tokens at 2x; requests past 272k pay double"
)

func TestWindowReport(t *testing.T) {
	tests := map[string]struct {
		windows policy.Windows
		auto    *policy.AutoCompact
		want    []string
	}{
		"success: session-wide cap": {policy.Windows{"claude": window(0, "default"), "codex": window(272000, "default"), "grok": window(400000, "saved")}, &policy.AutoCompact{Window: 325000, Source: "settings user"}, []string{"claude: full (1M through the [1m] alias pins) [default]; autoCompactWindow 325000 from settings user caps this session; native window 325000; exact compaction trigger unverified (native engine headroom applies)", "codex: 272000 cap [default]; compacts past 187000; " + crossing, "grok: 400000 cap [saved]; compacts past 315000"}},
		"success: no session cap":   {policy.Windows{"claude": window(500000, "saved"), "codex": window(0, "saved"), "grok": window(0, "default")}, nil, []string{"claude: 500000 (autoCompactWindow in project-wired settings) [saved]; native window 500000; exact compaction trigger unverified (native engine headroom applies)", "codex: full (each model's backend window, compacting 40000 below it) [saved]; " + past, "grok: full (each model's backend window, compacting 40000 below it) [default]"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { equal(t, tt.want, tt.windows.Report(tt.auto, lookup(nil))) })
	}
	equal(t, crossing, policy.BillingNote(272000, 0))
	equal(t, past, policy.BillingNote(0, 0))
	equal(t, crossing, policy.BillingNote(0, 1))
	equal(t, "requested compact-at 242000; backend window 500000 (grok-4.7); effective maximum 242000 (compact-at); compacts past 242000; CODEX_GATEWAY_COMPACT_TRIGGER= ignored: compact-at is saved", policy.WindowNote("grok", 0, 242000, &policy.Model{BackendID: "grok-4.7", BackendWindow: 500000, Sentry: "synthetic-413"}, lookup(map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": ""})))
}

func TestCompactTrigger(t *testing.T) {
	tests := map[string]struct{ cap, direct, want int64 }{
		"success: direct":             {272000, 242000, 242000},
		"success: legacy reserve":     {272000, 0, 187000},
		"success: no cap":             {0, 0, 0},
		"success: direct clipped":     {272000, 300000, 272000},
		"success: direct without cap": {0, 242000, 242000},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { equal(t, tt.want, policy.CompactTrigger(tt.cap, tt.direct)) })
	}
}

func TestEffectiveSentry(t *testing.T) {
	const oldSol = "claude-gpt-5.6-sol[1m]"
	const sol = "claude-gpt-6.1-sol[1m]"
	tests := map[string]struct {
		id          string
		window      int64
		env         float64
		cap, direct int64
		trigger     float64
		source      string
	}{
		"success: default capped":            {oldSol, 920012, math.NaN(), 272000, 0, 187000, "cap"},
		"success: early env":                 {oldSol, 920012, 150000, 272000, 0, 150000, "env"},
		"success: direct beats env":          {sol, 920012, 320000, 272000, 242000, 242000, "compact-at"},
		"success: legacy cap beats env":      {sol, 920012, 320000, 272000, 0, 187000, "cap"},
		"success: direct clipped at cap":     {sol, 920012, 1, 272000, 300000, 272000, "compact-at"},
		"success: direct clipped at derived": {sol, 920012, 320000, 0, 900000, 880012, "derived"},
		"success: derived Grok":              {"claude-grok-4.5[1m]", 500000, 1, 0, 900000, 460000, "derived"},
		"success: default Codex env":         {"gpt-5.2", 920000, 320000, 0, 0, 320000, "env"},
		"success: default Codex derived":     {"gpt-5.2", 920000, math.NaN(), 0, 0, 880000, "derived"},
		"success: smaller model":             {"", 300000, 320000, 0, 0, 260000, "derived"},
		"success: earlier derived wins tie":  {"", 300000, 260000, 345000, 0, 260000, "derived"},
		"success: env before cap on tie":     {"gpt-5.2", 920000, 187000, 272000, 0, 187000, "env"},
		"success: fractional env trigger":    {sol, 920012, 150000.5, 272000, 0, 150000.5, "env"},
		"success: infinite env ignored":      {"gpt-5.2", 920000, math.Inf(1), 0, 0, 880000, "derived"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			model := policy.Model{Backend: "codex", BackendID: "gpt-test-300k", BackendWindow: 300000, Sentry: "synthetic-413"}
			if tt.id != "" {
				model = modelFor(t, policy.Resolver{}, tt.id)
			}
			got, err := policy.EffectiveSentry(&model, tt.env, tt.cap, tt.direct)
			if err != nil {
				t.Fatal(err)
			}
			equal(t, &policy.Sentry{BackendWindow: tt.window, CompactTrigger: tt.trigger, Source: tt.source}, got)
		})
	}
	native := modelFor(t, policy.Resolver{}, "claude-opus-5-5[1m]")
	for _, p := range []*policy.Model{nil, &native} {
		got, err := policy.EffectiveSentry(p, math.NaN(), 272000, 0)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, (*policy.Sentry)(nil), got)
	}
	_, err := policy.EffectiveSentry(&policy.Model{BackendID: "gpt-test-40k", BackendWindow: 40000, Sentry: "synthetic-413"}, 0, 0, 0)
	if err == nil || err.Error() != "model-gateway: invalid sentry backend window for gpt-test-40k" {
		t.Fatalf("invalid sentry error = %v", err)
	}
}
