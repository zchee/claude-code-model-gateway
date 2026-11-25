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

package jsonx_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"math"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

func TestNodeCorpus(t *testing.T) {
	var tests map[string]struct {
		JSON string `json:"json"`
		Hex  string `json:"hex"`
	}
	var golden map[string]struct {
		Wire  string `json:"wire"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(fixture(t, "node-inputs.json"), &tests); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture(t, "node-golden.json"), &golden); err != nil {
		t.Fatal(err)
	}
	if len(tests) == 0 || len(tests) != len(golden) {
		t.Fatalf("got %d inputs and %d golden pairs", len(tests), len(golden))
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want, ok := golden[name]
			if !ok {
				t.Fatal("missing golden")
			}
			data := []byte(tt.JSON)
			if tt.Hex != "" {
				var err error
				data, err = hex.DecodeString(tt.Hex)
				if err != nil {
					t.Fatal(err)
				}
			}
			v, err := jsonx.DecodeWire(data)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := jsonx.EncodeWire(v)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(want.Wire, string(wire)); diff != "" {
				t.Errorf("wire (-Node +Go):\n%s", diff)
			}
			state, err := jsonx.EncodeState(v)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(want.State, string(state)); diff != "" {
				t.Errorf("state (-Node +Go):\n%s", diff)
			}
		})
	}
}

func TestInvalidWire(t *testing.T) {
	tests := map[string]struct{ input string }{
		"error: trailing document":     {`{"model":"m"} []`},
		"error: truncated after model": {`{"model":"m",`},
		"error: unknown escape":        {`"\x45"`},
		"error: byte order mark":       {"\ufeff{}"},
		"error: missing document":      {" "},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := jsonx.DecodeWire([]byte(tt.input)); err == nil {
				t.Error("accepted invalid wire input")
			}
		})
	}
}

func TestWriteTypedState(t *testing.T) {
	type record struct {
		BaseURL   string            `json:"baseUrl"`
		FetchedAt int64             `json:"fetchedAt"`
		Models    []jsonx.WireValue `json:"models"`
		Missing   bool              `json:"missing,omitzero"`
	}
	var golden map[string]struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(fixture(t, "node-golden.json"), &golden); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		value any
		want  string
	}{
		"success: discovery cache": {record{BaseURL: "http://localhost", FetchedAt: 9007199254740993, Models: []jsonx.WireValue{}}, golden["discovery cache"].State},
		"success: nonfinite":       {[]float64{math.Inf(1), math.NaN(), math.Copysign(0, -1)}, "[\n  null,\n  null,\n  0\n]\n"},
		"success: embedded tree":   {[]jsonx.WireValue{{Kind: '"', Text: []uint16{0xd800}}}, "[\n  \"\\ud800\"\n]\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want := tt.want
			path := filepath.Join(t.TempDir(), "state.json")
			if err := jsonx.WriteState(path, tt.value, 0o600); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(want, string(data)); diff != "" {
				t.Errorf("state file (-want +got):\n%s", diff)
			}
		})
	}
}

func FuzzSettingsNoEdit(f *testing.F) {
	for _, seed := range []string{"{}", " \t{\"a\":1.00,\"a\":-0}\r\n", "\ufeff{\"text\":\"\\ud800\",\"1\":[{},[]]}", "{\"badUTF8\":\"\xff\"}", "[1]", "{} {}", "{"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		doc := jsontext.Value(bytes.TrimPrefix(input, []byte("\ufeff")))
		valid := doc.Kind() == '{' && doc.IsValid(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
		out, err := jsonx.EditSettings(input)
		if (err == nil) != valid {
			t.Fatalf("valid=%t: %v", valid, err)
		}
		if valid && !bytes.Equal(input, out) {
			t.Fatalf("no-edit bytes (-input +output):\n%s", gocmp.Diff(input, out))
		}
	})
}
