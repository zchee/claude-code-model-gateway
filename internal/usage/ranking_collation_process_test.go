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

package usage

import (
	"bytes"
	json "encoding/json/v2"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const (
	mcpCollationHelper        = "mcp-ranking-collation"
	mcpCollationWitnessPrefix = "mcp-ranking-collation-witness:"
)

type mcpCollationWitness struct {
	LCAll          string           `json:"lc_all"`
	Lang           string           `json:"lang"`
	DroppedPresent bool             `json:"dropped_present"`
	OTLP           []mcpServerShare `json:"otlp"`
	Footprints     []mcpServerShare `json:"footprints"`
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{mcpCollationHelper: runMCPServerCollationHelper})
}

func runMCPServerCollationHelper(t testing.TB, args []string) {
	t.Helper()
	if len(args) != 1 {
		t.Fatalf("unexpected helper arguments %q", args)
	}
	tc, ok := mcpCollationCases()[args[0]]
	if !ok || tc.tailoredKept == ([2]string{}) {
		t.Fatalf("unknown startup-locale collation case %q", args[0])
	}
	lcAll, lang := os.Getenv("LC_ALL"), os.Getenv("LANG")
	if diff := gocmp.Diff(tc.locale, lcAll); diff != "" {
		t.Fatalf("child LC_ALL (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(tc.locale, lang); diff != "" {
		t.Fatalf("child LANG (-want +got):\n%s", diff)
	}

	// Confirm the bundled CLDR collation data recognizes the startup locale tag and
	// produces the tailored top-2 order that diverges from root collation.
	tagName, _, _ := strings.Cut(lcAll, ".")
	tag, err := language.Parse(tagName)
	if err != nil {
		t.Fatalf("parse startup locale %q: %v", lcAll, err)
	}
	col := collate.New(tag)
	tailored := []string{tc.wantKept[0], tc.wantKept[1], tc.wantDropped}
	slices.SortFunc(tailored, col.CompareString)
	if diff := gocmp.Diff(tc.tailoredKept[:], tailored[:2]); diff != "" {
		t.Fatalf("bundled locale data for %q tailored top-2 (-want +got):\n%s", lcAll, diff)
	}

	droppedPresent, gotOTLP, gotFootprints := measureMCPCollation(t, tc)
	raw, err := json.Marshal(mcpCollationWitness{
		LCAll:          lcAll,
		Lang:           lang,
		DroppedPresent: droppedPresent,
		OTLP:           gotOTLP,
		Footprints:     gotFootprints,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(os.Stdout, mcpCollationWitnessPrefix+string(raw)+"\n"); err != nil {
		t.Fatal(err)
	}
}

func runStartupLocaleCollationChecks(t *testing.T) {
	cases := mcpCollationCases()
	tests := map[string]struct {
		key string
		tc  mcpCollationCase
	}{
		"success: startup Estonian locale ignores tailoring of z before t": {
			key: "estonian",
			tc:  cases["estonian"],
		},
		"success: startup Czech locale ignores ch contraction after h": {
			key: "czech",
			tc:  cases["czech"],
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := gatewaytest.NewEnv(home, nil, map[string]string{
				"LC_ALL": tt.tc.locale,
				"LANG":   tt.tc.locale,
			}, nil)
			cmd, err := env.HelperCommand(t, mcpCollationHelper, tt.key)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := env.RunFixtureWithin(cmd, 25*time.Second); err != nil {
				t.Fatalf("collation helper: %v\n%s", err, output.String())
			}

			var witnessLines []string
			for line := range strings.Lines(output.String()) {
				line = strings.TrimRight(line, "\r\n")
				if payload, ok := strings.CutPrefix(line, mcpCollationWitnessPrefix); ok {
					witnessLines = append(witnessLines, payload)
				}
			}
			if len(witnessLines) != 1 {
				t.Fatalf("got %d collation witness lines, want 1:\n%s", len(witnessLines), output.String())
			}

			var witness mcpCollationWitness
			if err := json.Unmarshal([]byte(witnessLines[0]), &witness); err != nil {
				t.Fatalf("unmarshal collation witness %q: %v", witnessLines[0], err)
			}
			if diff := gocmp.Diff(tt.tc.locale, witness.LCAll); diff != "" {
				t.Errorf("witness LC_ALL (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.tc.locale, witness.Lang); diff != "" {
				t.Errorf("witness LANG (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(false, witness.DroppedPresent); diff != "" {
				t.Errorf("dropped server %q present in child primary attributes (-want +got):\n%s", tt.tc.wantDropped, diff)
			}
			wantRanked := wantMCPCollationRanking(tt.tc)
			if diff := gocmp.Diff(wantRanked, witness.OTLP); diff != "" {
				t.Errorf("child OTLP top-20 ranked servers (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(wantRanked, witness.Footprints); diff != "" {
				t.Errorf("child footprint records (-want +got):\n%s", diff)
			}
		})
	}
}
