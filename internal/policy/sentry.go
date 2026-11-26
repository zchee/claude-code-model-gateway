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

package policy

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

const (
	compactHeadroom = 40000
	capMargin       = 2*compactHeadroom + 5000
	defaultCodexCap = 272000
)

// Sentry describes the earliest candidate and the backend window it protects.
type Sentry struct {
	BackendWindow  int64
	CompactTrigger float64
	Source         string
}

// EffectiveSentry selects the lowest trigger, or nil when the model has no sentry.
// A zero cap or compactAt indicates no limit. A nonpositive or nonfinite env value is ignored.
func EffectiveSentry(model *Model, env float64, cap, compactAt int64) (*Sentry, error) {
	if model == nil || model.Sentry != guarded {
		return nil, nil
	}
	reserve := model.CompactHeadroom
	if reserve <= 0 || reserve > maxSafe {
		reserve = compactHeadroom
	}
	if float64(model.BackendWindow) <= float64(reserve) {
		return nil, fmt.Errorf("model-gateway: invalid sentry backend window for %s", model.BackendID)
	}
	s := Sentry{BackendWindow: model.BackendWindow, CompactTrigger: float64(model.BackendWindow) - float64(reserve), Source: "derived"}
	consider := func(trigger float64, source string) {
		if trigger < s.CompactTrigger {
			s.CompactTrigger = trigger
			s.Source = source
		}
	}
	// Strict comparisons preserve the source's stable order on equal triggers.
	if compactAt != 0 {
		consider(float64(CompactTrigger(cap, compactAt)), "compact-at")
	} else {
		if env > 0 && !math.IsInf(env, 1) {
			consider(env, "env")
		}
		if cap != 0 {
			consider(float64(cap)-capMargin, "cap")
		}
	}
	return &s, nil
}

// CompactTrigger returns the capped direct maximum or the cap less its reserve; zero means absent.
// It accepts validated settings: cap is at most 1,000,000 and compactAt is at most 2^53-1.
// Neither may be negative.
func CompactTrigger(cap, compactAt int64) int64 {
	if cap == 0 {
		return compactAt
	}
	if compactAt != 0 {
		return min(compactAt, cap)
	}
	// Configured caps are positive and at most a million tokens.
	if cap < math.MinInt64+capMargin {
		return math.MinInt64
	}
	return cap - capMargin
}

// BillingNote describes Codex input billing for the cap and direct compaction maximum.
func BillingNote(cap, compactAt int64) string {
	const prefix = "OpenAI bills input above 272k tokens at 2x; "
	if compactAt != 0 || cap != 0 && cap <= defaultCodexCap {
		return prefix + "the crossing turn and compaction request can still exceed 272k and pay double"
	}
	return prefix + "requests past 272k pay double"
}

// AutoCompact describes a native window restriction and its configuration source.
type AutoCompact struct {
	Window int64
	Source string
}

// Report formats the effective Claude, Codex and Grok windows, in that order.
func (w Windows) Report(auto *AutoCompact, lookup config.LookupEnv) []string {
	n := w.Cap("claude")
	desc := "full (1M through the [1m] alias pins)"
	if n == 0 {
		n = clientLimit
	} else {
		desc = fmt.Sprintf("%d (autoCompactWindow in project-wired settings)", n)
	}
	native := fmt.Sprintf("claude: %s [%s]", desc, w["claude"].Source)
	if auto != nil && auto.Window < n {
		n = auto.Window
		native += fmt.Sprintf("; autoCompactWindow %d from %s caps this session", n, auto.Source)
	}
	lines := []string{native + fmt.Sprintf("; native window %d; exact compaction trigger unverified (native engine headroom applies)", n)}
	for _, backend := range []string{"codex", "grok"} {
		setting := w[backend]
		cap := w.Cap(backend)
		desc := "full (each model's backend window, compacting 40000 below it)"
		if cap != 0 {
			desc = fmt.Sprintf("%d cap", cap)
		}
		line := fmt.Sprintf("%s: %s [%s]", backend, desc, setting.Source)
		if note := WindowNote(backend, cap, setting.CompactAt, nil, lookup); note != "" {
			line += "; " + note
		}
		lines = append(lines, line)
	}
	return lines
}

// WindowNote formats compaction and billing information, returning an empty note on failure.
// Callers that must report failure, including catalog refresh, must use CheckedWindowNote.
func WindowNote(backend string, cap, compactAt int64, model *Model, lookup config.LookupEnv) string {
	note, _ := CheckedWindowNote(backend, cap, compactAt, model, lookup)
	return note
}

// CheckedWindowNote formats compaction and billing information for a resolved backend row.
// A direct compaction maximum requires a valid synthetic sentry.
func CheckedWindowNote(backend string, cap, compactAt int64, model *Model, lookup config.LookupEnv) (string, error) {
	var notes []string
	if compactAt != 0 {
		if model == nil {
			for i := range modelRows {
				if modelRows[i].Backend == backend {
					model = &modelRows[i]
					break
				}
			}
		}
		env, raw, set := config.CompactTrigger.Read(lookup)
		s, err := EffectiveSentry(model, env, cap, compactAt)
		if err != nil {
			return "", err
		}
		if s == nil {
			return "", errors.New("cannot format a direct compaction maximum without a synthetic sentry")
		}
		row := ""
		if model.BackendID != "" && model.BackendID != "default" {
			row = " (" + model.BackendID + ")"
		}
		limit := strconv.FormatFloat(s.CompactTrigger, 'f', -1, 64)
		notes = append(notes, fmt.Sprintf("requested compact-at %d; backend window %d%s; effective maximum %s (%s); compacts past %s", compactAt, s.BackendWindow, row, limit, s.Source, limit))
		if set {
			notes = append(notes, "CODEX_GATEWAY_COMPACT_TRIGGER="+raw+" ignored: compact-at is saved")
		}
	} else if cap != 0 {
		notes = append(notes, fmt.Sprintf("compacts past %d", CompactTrigger(cap, 0)))
	}
	if backend == "codex" {
		notes = append(notes, BillingNote(cap, compactAt))
	}
	return strings.Join(notes, "; "), nil
}
