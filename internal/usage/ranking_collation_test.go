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
	json "encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestMCPServerRootCollation(t *testing.T) {
	// lib/usage-observability.js:494-498 ranks the top 20 MCP servers by descending aggregate
	// serialized tool-definition bytes and breaks equal-byte ties with host-locale localeCompare.
	// The port uses root collation for that tie-break regardless of host locale or byte order.
	type toolDef struct {
		Name        string `json:"name"`
		Description string `json:"description,omitzero"`
	}
	type serverShare struct {
		Server string
		Tokens int
	}
	tests := map[string]struct {
		locale      string
		tied        []toolDef
		tiedBytes   int
		wantKept    [2]string
		wantDropped string
	}{
		"success: root order places underscore before digits rather than byte order": {
			locale: "C",
			tied: []toolDef{
				{Name: "mcp__a1b__a", Description: "xxxxx"},
				{Name: "mcp__a0b__a", Description: "xxxxx"},
				{Name: "mcp__a-b__a"},
				{Name: "mcp__A_B__b"},
			},
			tiedBytes:   44,
			wantKept:    [2]string{"a_b", "a0b"},
			wantDropped: "a1b",
		},
		"success: root order ignores Estonian locale tailoring of z before t": {
			locale: "et_EE.UTF-8",
			tied: []toolDef{
				{Name: "mcp__za__a", Description: "xxxx"},
				{Name: "mcp__ta__a"},
				{Name: "mcp__ta__b"},
				{Name: "mcp__sa__a", Description: "xxxx"},
			},
			tiedBytes:   42,
			wantKept:    [2]string{"sa", "ta"},
			wantDropped: "za",
		},
		"success: root order ignores Czech locale ch contraction after h": {
			locale: "cs_CZ.UTF-8",
			tied: []toolDef{
				{Name: "mcp__ha__a", Description: "xxxx"},
				{Name: "mcp__da__a", Description: "xxxx"},
				{Name: "mcp__ch__a"},
				{Name: "mcp__ch__b"},
			},
			tiedBytes:   42,
			wantKept:    [2]string{"ch", "da"},
			wantDropped: "ha",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LC_ALL", tt.locale)
			t.Setenv("LANG", tt.locale)

			const heavyCount = 18
			const heavyBytes = 50
			tools := make([]toolDef, 0, len(tt.tied)+heavyCount)
			tools = append(tools, tt.tied...)
			for i := heavyCount; i >= 1; i-- {
				tools = append(tools, toolDef{
					Name:        fmt.Sprintf("mcp__zzzz_%02d__tool", i),
					Description: "pads",
				})
			}

			totalMCPBytes := heavyCount*heavyBytes + 3*tt.tiedBytes
			body := encoded(t, map[string]any{"tools": tools})
			records := NewBuilder(options(t)).Build(Request{
				Body:       wire(t, string(body)),
				StatusCode: 200,
				Usage:      map[string]any{"input_tokens": totalMCPBytes},
			})

			primary := onlyEvent(t, records, "gateway.token.usage")
			checkJSON(t, totalMCPBytes, primary.Attributes["input_mcp_tools_bytes"])
			checkJSON(t, totalMCPBytes, primary.Attributes["input_mcp_tools_tokens"])
			_, droppedPresent := primary.Attributes["input_mcp_tools_"+tt.wantDropped+"_tokens"]
			if diff := gocmp.Diff(false, droppedPresent); diff != "" {
				t.Errorf("dropped server %q present in primary attributes (-want +got):\n%s", tt.wantDropped, diff)
			}

			wantRanked := make([]serverShare, 0, 20)
			for i := 1; i <= heavyCount; i++ {
				wantRanked = append(wantRanked, serverShare{
					Server: fmt.Sprintf("zzzz_%02d", i),
					Tokens: heavyBytes,
				})
			}
			for _, kept := range tt.wantKept {
				wantRanked = append(wantRanked, serverShare{
					Server: kept,
					Tokens: tt.tiedBytes,
				})
			}

			payload, err := primary.OTLP()
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				ResourceLogs []struct {
					ScopeLogs []struct {
						LogRecords []struct {
							Attributes []struct {
								Key   string            `json:"key"`
								Value map[string]string `json:"value"`
							} `json:"attributes"`
						} `json:"logRecords"`
					} `json:"scopeLogs"`
				} `json:"resourceLogs"`
			}
			if err := json.Unmarshal(payload, &envelope); err != nil {
				t.Fatal(err)
			}
			var gotOTLP []serverShare
			for _, attr := range envelope.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
				if rest, ok := strings.CutPrefix(attr.Key, "input_mcp_tools_"); ok && attr.Key != "input_mcp_tools_tokens" {
					if server, ok := strings.CutSuffix(rest, "_tokens"); ok {
						tokens, err := strconv.Atoi(attr.Value["intValue"])
						if err != nil {
							t.Fatal(err)
						}
						gotOTLP = append(gotOTLP, serverShare{Server: server, Tokens: tokens})
					}
				}
			}
			if diff := gocmp.Diff(wantRanked, gotOTLP); diff != "" {
				t.Errorf("OTLP top-20 ranked servers (-want +got):\n%s", diff)
			}

			footprints := events(records, "gateway.mcp.footprint")
			gotFootprints := make([]serverShare, 0, len(footprints))
			for _, foot := range footprints {
				server, _ := foot.Attributes["mcp_server"].(string)
				tokens, _ := foot.Attributes["input_mcp_tools_tokens"].(float64)
				gotFootprints = append(gotFootprints, serverShare{Server: server, Tokens: int(tokens)})
			}
			if diff := gocmp.Diff(wantRanked, gotFootprints); diff != "" {
				t.Errorf("footprint records (-want +got):\n%s", diff)
			}
		})
	}
}
