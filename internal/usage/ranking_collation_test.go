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

package usage

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

type otlpAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func otlpLogAttributes(t testing.TB, payload []byte) []otlpAttr {
	t.Helper()
	var envelope struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []otlpAttr `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal OTLP envelope: %v", err)
	}
	if len(envelope.ResourceLogs) != 1 {
		t.Fatalf("OTLP resourceLogs count = %d, want 1", len(envelope.ResourceLogs))
	}
	scopeLogs := envelope.ResourceLogs[0].ScopeLogs
	if len(scopeLogs) != 1 {
		t.Fatalf("OTLP scopeLogs count = %d, want 1", len(scopeLogs))
	}
	logRecords := scopeLogs[0].LogRecords
	if len(logRecords) != 1 {
		t.Fatalf("OTLP logRecords count = %d, want 1", len(logRecords))
	}
	return logRecords[0].Attributes
}

type mcpToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
}

type mcpServerShare struct {
	Server string  `json:"server"`
	Tokens float64 `json:"tokens"`
}

type mcpCollationCase struct {
	locale       string
	tied         []mcpToolDef
	tiedBytes    int
	wantKept     [2]string
	wantDropped  string
	tailoredKept [2]string
}

const (
	mcpRankingHeavyCount = 18
	mcpRankingHeavyBytes = 50
)

func mcpCollationCases() map[string]mcpCollationCase {
	return map[string]mcpCollationCase{
		"posix": {
			locale: "C",
			tied: []mcpToolDef{
				{Name: "mcp__a1b__a", Description: "xxxxx"},
				{Name: "mcp__a0b__a", Description: "xxxxx"},
				{Name: "mcp__a-b__a"},
				{Name: "mcp__A_B__b"},
			},
			tiedBytes:   44,
			wantKept:    [2]string{"a_b", "a0b"},
			wantDropped: "a1b",
		},
		"estonian": {
			locale: "et_EE.UTF-8",
			tied: []mcpToolDef{
				{Name: "mcp__za__a", Description: "xxxx"},
				{Name: "mcp__ta__a"},
				{Name: "mcp__ta__b"},
				{Name: "mcp__sa__a", Description: "xxxx"},
			},
			tiedBytes:    42,
			wantKept:     [2]string{"sa", "ta"},
			wantDropped:  "za",
			tailoredKept: [2]string{"sa", "za"},
		},
		"czech": {
			locale: "cs_CZ.UTF-8",
			tied: []mcpToolDef{
				{Name: "mcp__ha__a", Description: "xxxx"},
				{Name: "mcp__da__a", Description: "xxxx"},
				{Name: "mcp__ch__a"},
				{Name: "mcp__ch__b"},
			},
			tiedBytes:    42,
			wantKept:     [2]string{"ch", "da"},
			wantDropped:  "ha",
			tailoredKept: [2]string{"da", "ha"},
		},
	}
}

func wantMCPCollationRanking(tc mcpCollationCase) []mcpServerShare {
	want := make([]mcpServerShare, 0, mcpRankingHeavyCount+len(tc.wantKept))
	for i := 1; i <= mcpRankingHeavyCount; i++ {
		want = append(want, mcpServerShare{
			Server: fmt.Sprintf("zzzz_%02d", i),
			Tokens: float64(mcpRankingHeavyBytes),
		})
	}
	for _, kept := range tc.wantKept {
		want = append(want, mcpServerShare{
			Server: kept,
			Tokens: float64(tc.tiedBytes),
		})
	}
	return want
}

func measureMCPCollation(t testing.TB, tc mcpCollationCase) (bool, []mcpServerShare, []mcpServerShare) {
	t.Helper()
	tools := make([]mcpToolDef, 0, len(tc.tied)+mcpRankingHeavyCount)
	tools = append(tools, tc.tied...)
	for i := mcpRankingHeavyCount; i >= 1; i-- {
		tools = append(tools, mcpToolDef{
			Name:        fmt.Sprintf("mcp__zzzz_%02d__tool", i),
			Description: "pads",
		})
	}

	totalMCPBytes := mcpRankingHeavyCount*mcpRankingHeavyBytes + 3*tc.tiedBytes
	body, err := json.Marshal(map[string]any{"tools": tools})
	if err != nil {
		t.Fatal(err)
	}
	decodedBody, err := jsonx.DecodeWire(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		ProjectsDir: t.TempDir(),
		Now:         func() time.Time { return time.Unix(123, 0) },
		Random:      bytes.NewReader(make([]byte, 8192)),
	}
	records := NewBuilder(opts).Build(Request{
		Body:       decodedBody,
		StatusCode: 200,
		Usage:      map[string]any{"input_tokens": totalMCPBytes},
	})

	primaries := events(records, "gateway.token.usage")
	if len(primaries) != 1 {
		t.Fatalf("got %d %q records, want 1", len(primaries), "gateway.token.usage")
	}
	primary := primaries[0]

	gotBytes, ok := primary.Attributes["input_mcp_tools_bytes"].(int64)
	if !ok {
		t.Fatalf("primary input_mcp_tools_bytes = %T (%#v), want int64", primary.Attributes["input_mcp_tools_bytes"], primary.Attributes["input_mcp_tools_bytes"])
	}
	if diff := gocmp.Diff(int64(totalMCPBytes), gotBytes); diff != "" {
		t.Errorf("primary input_mcp_tools_bytes (-want +got):\n%s", diff)
	}
	gotTokens, ok := primary.Attributes["input_mcp_tools_tokens"].(float64)
	if !ok {
		t.Fatalf("primary input_mcp_tools_tokens = %T (%#v), want float64", primary.Attributes["input_mcp_tools_tokens"], primary.Attributes["input_mcp_tools_tokens"])
	}
	if diff := gocmp.Diff(float64(totalMCPBytes), gotTokens); diff != "" {
		t.Errorf("primary input_mcp_tools_tokens (-want +got):\n%s", diff)
	}
	_, droppedPresent := primary.Attributes["input_mcp_tools_"+tc.wantDropped+"_tokens"]

	payload, err := primary.OTLP()
	if err != nil {
		t.Fatal(err)
	}
	var gotOTLP []mcpServerShare
	for _, attr := range otlpLogAttributes(t, payload) {
		if rest, ok := strings.CutPrefix(attr.Key, "input_mcp_tools_"); ok && attr.Key != "input_mcp_tools_tokens" {
			if server, ok := strings.CutSuffix(rest, "_tokens"); ok {
				raw, ok := attr.Value["intValue"].(string)
				if !ok || len(attr.Value) != 1 {
					t.Fatalf("OTLP %s value = %#v, want single string intValue", attr.Key, attr.Value)
				}
				tokens, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					t.Fatalf("parse OTLP %s intValue %q: %v", attr.Key, raw, err)
				}
				gotOTLP = append(gotOTLP, mcpServerShare{Server: server, Tokens: float64(tokens)})
			}
		}
	}

	footprints := events(records, "gateway.mcp.footprint")
	gotFootprints := make([]mcpServerShare, 0, len(footprints))
	for i, foot := range footprints {
		server, ok := foot.Attributes["mcp_server"].(string)
		if !ok {
			t.Fatalf("footprint[%d] mcp_server = %T (%#v), want string", i, foot.Attributes["mcp_server"], foot.Attributes["mcp_server"])
		}
		tokens, ok := foot.Attributes["input_mcp_tools_tokens"].(float64)
		if !ok {
			t.Fatalf("footprint[%d] (%q) input_mcp_tools_tokens = %T (%#v), want float64", i, server, foot.Attributes["input_mcp_tools_tokens"], foot.Attributes["input_mcp_tools_tokens"])
		}
		gotFootprints = append(gotFootprints, mcpServerShare{Server: server, Tokens: tokens})
	}
	return droppedPresent, gotOTLP, gotFootprints
}

func TestMCPServerRootCollation(t *testing.T) {
	// lib/usage-observability.js:494-498 ranks the top 20 MCP servers by descending aggregate
	// serialized tool-definition bytes and breaks equal-byte ties with host-locale localeCompare.
	// The port uses root collation for that tie-break regardless of host locale or byte order.
	cases := mcpCollationCases()
	tests := map[string]mcpCollationCase{
		"success: root order places underscore before digits rather than byte order": cases["posix"],
		"success: root order ignores Estonian locale tailoring of z before t":        cases["estonian"],
		"success: root order ignores Czech locale ch contraction after h":            cases["czech"],
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LC_ALL", tt.locale)
			t.Setenv("LANG", tt.locale)

			droppedPresent, gotOTLP, gotFootprints := measureMCPCollation(t, tt)
			if diff := gocmp.Diff(false, droppedPresent); diff != "" {
				t.Errorf("dropped server %q present in primary attributes (-want +got):\n%s", tt.wantDropped, diff)
			}
			wantRanked := wantMCPCollationRanking(tt)
			if diff := gocmp.Diff(wantRanked, gotOTLP); diff != "" {
				t.Errorf("OTLP top-20 ranked servers (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(wantRanked, gotFootprints); diff != "" {
				t.Errorf("footprint records (-want +got):\n%s", diff)
			}
		})
	}

	runStartupLocaleCollationChecks(t)
}
