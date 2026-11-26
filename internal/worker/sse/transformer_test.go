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

package sse_test

import (
	"bytes"
	"encoding/json/jsontext"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

func TestTransformer(t *testing.T) {
	planEvents := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"gpt-5.6-sol\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"keep \xf0\x9f\x8e\xb5\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"plan\",\"name\":\"EnterPlanMode\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"bash\",\"name\":\"Bash\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"pwd\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")
	emojiSplit := strings.Index(planEvents, "\xf0\x9f\x8e\xb5") + 2

	wantPlanKeptBash := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-gpt-5.6-sol\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"keep \xf0\x9f\x8e\xb5\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"bash\",\"name\":\"Bash\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"pwd\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")

	onlyPlanTool := strings.Join([]string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"exit\",\"name\":\"ExitPlanMode\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n",
	}, "")

	wantOnlyPlanTool := strings.Join([]string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n",
	}, "")

	tests := map[string]struct {
		cfg          sse.TransformerConfig
		chunks       [][]byte
		wantOut      []byte
		wantObserved []string
	}{
		"success: model rewrite replaces gpt prefix and exact backend model recursively before output, after observe": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-kimi-k3[1m]",
				BackendModel:    "kimi-k3",
				Provider:        "kimi",
			},
			chunks: [][]byte{
				[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"kimi-k3\",\"nested\":{\"model\":\"gpt-5.6-sol\"},\"items\":[{\"model\":\"claude-opus-5\"},{\"model\":42},{\"model\":\"kimi-k3\"}]}}\n\n"),
			},
			wantOut: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-kimi-k3[1m]\",\"nested\":{\"model\":\"claude-kimi-k3[1m]\"},\"items\":[{\"model\":\"claude-opus-5\"},{\"model\":42},{\"model\":\"claude-kimi-k3[1m]\"}]}}\n\n"),
			wantObserved: []string{
				`{"type":"message_start","message":{"model":"kimi-k3","nested":{"model":"gpt-5.6-sol"},"items":[{"model":"claude-opus-5"},{"model":42},{"model":"kimi-k3"}]}}`,
			},
		},
		"success: plan tool filtering drops EnterPlanMode, shifts later indices, preserves split UTF-8, keeps tool_use when Bash remains": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-gpt-5.6-sol",
				BackendModel:    "gpt-5.6-sol",
				Provider:        "codex",
				FilterPlanTools: true,
			},
			chunks: [][]byte{
				[]byte(planEvents[:emojiSplit]),
				[]byte(planEvents[emojiSplit:]),
			},
			wantOut: []byte(wantPlanKeptBash),
			wantObserved: []string{
				`{"type":"message_start","message":{"id":"m1","model":"gpt-5.6-sol"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"keep \xf0\x9f\x8e\xb5\"}}",
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"plan","name":"EnterPlanMode","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"bash","name":"Bash","input":{}}}`,
				`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
				`{"type":"content_block_stop","index":2}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
				`{"type":"message_stop"}`,
			},
		},
		"success: plan tool filtering rewrites stop_reason to end_turn when only plan tool was dropped": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-gpt-5.6-sol",
				BackendModel:    "gpt-5.6-sol",
				Provider:        "codex",
				FilterPlanTools: true,
			},
			chunks:  [][]byte{[]byte(onlyPlanTool)},
			wantOut: []byte(wantOnlyPlanTool),
		},
		"success: plan tool filtering disabled keeps EnterPlanMode and ExitPlanMode intact": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-gpt-5.6-sol",
				BackendModel:    "gpt-5.6-sol",
				Provider:        "codex",
				FilterPlanTools: false,
			},
			chunks:  [][]byte{[]byte(onlyPlanTool)},
			wantOut: []byte(onlyPlanTool),
		},
		"success: LM Studio exclusive cache accounting subtracts cached input before observe and output": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-lmstudio/qwen3-coder-30b",
				BackendModel:    "lmstudio/qwen3-coder-30b",
				Provider:        sse.LMStudioProvider,
			},
			chunks: [][]byte{
				[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"qwen3-coder-30b\",\"usage\":{\"input_tokens\":30000,\"output_tokens\":0,\"cache_read_input_tokens\":29990}}}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":30000,\"output_tokens\":2,\"cache_read_input_tokens\":29990}}\n\n"),
			},
			wantOut: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"qwen3-coder-30b\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_read_input_tokens\":29990}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":29990}}\n\n"),
			wantObserved: []string{
				`{"type":"message_start","message":{"model":"qwen3-coder-30b","usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":29990}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":29990}}`,
			},
		},
		"success: LM Studio leaves input below cache_read_input_tokens untouched": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-lmstudio/qwen3-coder-30b",
				BackendModel:    "lmstudio/qwen3-coder-30b",
				Provider:        sse.LMStudioProvider,
			},
			chunks: [][]byte{
				[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":22,\"output_tokens\":2,\"cache_read_input_tokens\":3000}}\n\n"),
			},
			wantOut: []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":22,\"output_tokens\":2,\"cache_read_input_tokens\":3000}}\n\n"),
			wantObserved: []string{
				`{"type":"message_delta","usage":{"input_tokens":22,"output_tokens":2,"cache_read_input_tokens":3000}}`,
			},
		},
		"success: non-LM-Studio provider does not subtract cache_read_input_tokens": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-gpt-5.6-sol",
				BackendModel:    "gpt-5.6-sol",
				Provider:        "codex",
			},
			chunks: [][]byte{
				[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":30000,\"cache_read_input_tokens\":29990}}\n\n"),
			},
			wantOut: []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":30000,\"cache_read_input_tokens\":29990}}\n\n"),
			wantObserved: []string{
				`{"type":"message_delta","usage":{"input_tokens":30000,"cache_read_input_tokens":29990}}`,
			},
		},
		"success: untouched frames preserve raw bytes including malformed UTF-8, comments, empty data, DONE, and tail": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-gpt-5.6-sol",
				BackendModel:    "gpt-5.6-sol",
				Provider:        "codex",
			},
			chunks: [][]byte{
				[]byte(": raw\xffcomment\r\n\r\ndata:\r\n\r\ndata: [DONE]\n\nevent: x\r\ndata: {bad\xfe\n\r\ndata: tail\xc0"),
			},
			wantOut:      []byte(": raw\xffcomment\r\n\r\ndata:\r\n\r\ndata: [DONE]\n\nevent: x\r\ndata: {bad\xfe\n\r\ndata: tail\xc0"),
			wantObserved: nil,
		},
		"success: rewritten frame decodes surviving comments and metadata as Node UTF-8 and preserves delimiter": {
			cfg: sse.TransformerConfig{
				AdvertisedModel: "claude-gpt-5.6-sol",
				BackendModel:    "gpt-5.6-sol",
				Provider:        "codex",
			},
			chunks: [][]byte{
				[]byte(": note\xff\xfe\r\nevent: msg\x80\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"gpt-5.6-sol\",\"text\":\"a\xffb\"}}\r\ndata: second\x81\r\n\r\n"),
			},
			wantOut: []byte(": note��\nevent: msg�\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-gpt-5.6-sol\",\"text\":\"a�b\"}}\ndata: second�\r\n\r\n"),
			wantObserved: []string{
				"{\"type\":\"message_start\",\"message\":{\"model\":\"gpt-5.6-sol\",\"text\":\"a�b\"}}",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var observed []string
			cfg := tt.cfg
			cfg.Observe = func(event jsontext.Value) {
				observed = append(observed, string(event))
			}
			tr := sse.NewTransformer(cfg)
			var out bytes.Buffer
			emit := func(frame []byte) error {
				out.Write(frame)
				return nil
			}
			for _, chunk := range tt.chunks {
				if err := tr.Feed(chunk, emit); err != nil {
					t.Fatalf("Feed() error = %v", err)
				}
			}
			if err := tr.End(emit); err != nil {
				t.Fatalf("End() error = %v", err)
			}
			if diff := gocmp.Diff(tt.wantOut, out.Bytes()); diff != "" {
				t.Errorf("wire output mismatch (-want +got):\n%s", diff)
			}
			if tt.wantObserved != nil {
				if diff := gocmp.Diff(tt.wantObserved, observed); diff != "" {
					t.Errorf("observed events mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestAttempt(t *testing.T) {
	tests := map[string]struct {
		events         []string
		wantFinished   bool
		wantDeliver    bool
		wantTerminal   bool
		wantTermCode   string
		wantVisible    bool
		wantSawError   bool
		wantErrorCode  string
		wantFatal      bool
		wantMergedJSON string
	}{
		"success: explicit message_stop is terminal, finished, and deliverable": {
			events: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":8,"output_tokens":0}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
				`{"type":"message_stop"}`,
			},
			wantFinished:   true,
			wantDeliver:    true,
			wantTerminal:   true,
			wantTermCode:   "message_stop",
			wantVisible:    true,
			wantMergedJSON: `{"input_tokens":12,"cache_read_input_tokens":8,"output_tokens":5}`,
		},
		"success: end_turn with closed blocks and missing message_stop is finished and deliverable": {
			events: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hand-back report"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			},
			wantFinished:   true,
			wantDeliver:    true,
			wantTerminal:   false,
			wantVisible:    true,
			wantMergedJSON: `{"input_tokens":1,"output_tokens":3}`,
		},
		"success: stop_sequence with closed blocks and missing message_stop is finished and deliverable": {
			events: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"stop_sequence"}}`,
			},
			wantFinished: true,
			wantDeliver:  true,
		},
		"error: open content block prevents inferred completion": {
			events: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			},
			wantFinished: false,
			wantDeliver:  false,
			wantVisible:  true,
		},
		"error: content block after end_turn prevents inferred completion": {
			events: []string{
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_stop","index":0}`,
			},
			wantFinished: false,
			wantDeliver:  false,
		},
		"error: tool_use stop_reason without message_stop is not finished": {
			events: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"Bash"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			},
			wantFinished: false,
			wantDeliver:  false,
		},
		"error: max_tokens stop_reason without message_stop is not finished": {
			events: []string{
				`{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
			},
			wantFinished: false,
			wantDeliver:  false,
		},
		"error: retryable websocket_missing_terminal error after message_stop makes attempt undeliverable": {
			events: []string{
				`{"type":"message_stop"}`,
				`{"type":"error","error":{"type":"api_error","message":"websocket_missing_terminal"}}`,
			},
			wantFinished:  true,
			wantDeliver:   false,
			wantTerminal:  true,
			wantTermCode:  "message_stop",
			wantSawError:  true,
			wantErrorCode: "websocket_missing_terminal",
			wantFatal:     false,
		},
		"error: fatal rate_limit_error sets Fatal and ErrorCode": {
			events: []string{
				`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			},
			wantFinished:  false,
			wantDeliver:   false,
			wantSawError:  true,
			wantErrorCode: "rate_limit_error",
			wantFatal:     true,
		},
		"error: unrecognized error type classifies as unknown_error and non-fatal": {
			events: []string{
				`{"type":"error","error":{"type":"api_error","message":"transient backend hiccup"}}`,
			},
			wantFinished:  false,
			wantDeliver:   false,
			wantSawError:  true,
			wantErrorCode: "unknown_error",
			wantFatal:     false,
		},
		"success: whitespace-only text delta and input_json_delta do not count as visible text": {
			events: []string{
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" \t\r\n "}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
				`{"type":"message_stop"}`,
			},
			wantFinished: true,
			wantDeliver:  true,
			wantTerminal: true,
			wantTermCode: "message_stop",
			wantVisible:  false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var attempt sse.Attempt
			for _, raw := range tt.events {
				attempt.NoteEvent(jsontext.Value(raw))
			}
			if got := attempt.Finished(); got != tt.wantFinished {
				t.Errorf("Finished() = %t, want %t", got, tt.wantFinished)
			}
			if got := attempt.Deliverable(); got != tt.wantDeliver {
				t.Errorf("Deliverable() = %t, want %t", got, tt.wantDeliver)
			}
			if attempt.Terminal != tt.wantTerminal {
				t.Errorf("Terminal = %t, want %t", attempt.Terminal, tt.wantTerminal)
			}
			if attempt.TerminalCode != tt.wantTermCode {
				t.Errorf("TerminalCode = %q, want %q", attempt.TerminalCode, tt.wantTermCode)
			}
			if attempt.HasVisibleText != tt.wantVisible {
				t.Errorf("HasVisibleText = %t, want %t", attempt.HasVisibleText, tt.wantVisible)
			}
			if attempt.SawError != tt.wantSawError {
				t.Errorf("SawError = %t, want %t", attempt.SawError, tt.wantSawError)
			}
			if attempt.ErrorCode != tt.wantErrorCode {
				t.Errorf("ErrorCode = %q, want %q", attempt.ErrorCode, tt.wantErrorCode)
			}
			if attempt.Fatal != tt.wantFatal {
				t.Errorf("Fatal = %t, want %t", attempt.Fatal, tt.wantFatal)
			}
			if tt.wantMergedJSON != "" {
				if diff := gocmp.Diff(tt.wantMergedJSON, string(attempt.MergedUsage)); diff != "" {
					t.Errorf("MergedUsage (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestEventObserver(t *testing.T) {
	exactLimitFrame := "data: {\"type\":\"ping\",\"pad\":\"" + strings.Repeat("a", 64) + "\"}\n\n"
	tests := map[string]struct {
		maxBytes      int
		chunks        [][]byte
		wantEvents    []string
		wantOverflows int
	}{
		"success: parses valid frames and skips comments, empty data, DONE, and malformed JSON": {
			maxBytes: 1024,
			chunks: [][]byte{
				[]byte(": ping\n\ndata:\n\ndata: [DONE]\n\ndata: {not-json}\n\nevent: message_start\ndata: {\"type\":\"message_start\"}\r\n\r\ndata: {\"type\":\"message_delta\"}"),
			},
			wantEvents: []string{
				`{"type":"message_start"}`,
				`{"type":"message_delta"}`,
			},
			wantOverflows: 0,
		},
		"success: pending frame at exact byte limit observes without overflow": {
			maxBytes: len(exactLimitFrame),
			chunks: [][]byte{
				[]byte(exactLimitFrame),
			},
			wantEvents: []string{
				`{"type":"ping","pad":"` + strings.Repeat("a", 64) + `"}`,
			},
			wantOverflows: 0,
		},
		"error: pending frame one byte over limit overflows once and ignores subsequent frames": {
			maxBytes: len(exactLimitFrame) - 1,
			chunks: [][]byte{
				[]byte(exactLimitFrame),
				[]byte("data: {\"type\":\"after_overflow\"}\n\n"),
			},
			wantEvents:    nil,
			wantOverflows: 1,
		},
		"success: cumulative stream larger than pending budget succeeds when each frame fits": {
			maxBytes: 128,
			chunks: [][]byte{
				[]byte("data: {\"type\":\"e1\",\"v\":\"" + strings.Repeat("x", 40) + "\"}\n\n"),
				[]byte("data: {\"type\":\"e2\",\"v\":\"" + strings.Repeat("y", 40) + "\"}\n\n"),
				[]byte("data: {\"type\":\"e3\",\"v\":\"" + strings.Repeat("z", 40) + "\"}\n\n"),
			},
			wantEvents: []string{
				`{"type":"e1","v":"` + strings.Repeat("x", 40) + `"}`,
				`{"type":"e2","v":"` + strings.Repeat("y", 40) + `"}`,
				`{"type":"e3","v":"` + strings.Repeat("z", 40) + `"}`,
			},
			wantOverflows: 0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var got []string
			overflows := 0
			obs := sse.NewEventObserver(tt.maxBytes, func(event jsontext.Value) {
				got = append(got, string(event))
			}, func() {
				overflows++
			})
			for _, chunk := range tt.chunks {
				obs.Write(chunk)
			}
			obs.End()
			if diff := gocmp.Diff(tt.wantEvents, got); diff != "" {
				t.Errorf("observed events (-want +got):\n%s", diff)
			}
			if overflows != tt.wantOverflows {
				t.Errorf("overflow callbacks = %d, want %d", overflows, tt.wantOverflows)
			}
		})
	}
}
