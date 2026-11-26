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

// Package upstreamstate persists the evidence and expiry of Codex dispatch refusals.
package upstreamstate

import (
	"cmp"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

const (
	blockedFile      = "codex-upstream-blocked.json"
	unavailableFile  = "codex-upstream-unavailable.json"
	blockedState     = "upstream-blocked"
	unavailableState = "upstream-unavailable"
	rateLimitDefault = time.Minute
	unavailableTTL   = 30 * time.Second
	isoLayout        = "2006-01-02T15:04:05.000Z"
	maxDateMillis    = 8_640_000_000_000_000
)

// Store uses a gateway state directory, a clock, and the stable binary path used in guidance.
type Store struct {
	Dir         string
	Now         func() time.Time
	CommandPath string
}

// Blocked describes an upstream rejection; only rate limits have an expiry.
type Blocked struct {
	State       string   `json:"state"`
	ObservedAt  string   `json:"observedAt"`
	StatusCode  int      `json:"statusCode"`
	Evidence    string   `json:"evidence"`
	ExpiresAt   string   `json:"expiresAt,omitzero"`
	ExpiresAtMs *float64 `json:"expiresAtMs,omitzero"`
}

// Unavailable describes a transient upstream failure and its recovery window.
type Unavailable struct {
	State        string `json:"state"`
	ObservedAt   string `json:"observedAt"`
	ObservedAtMs int64  `json:"observedAtMs"`
	ExpiresAt    string `json:"expiresAt"`
	StatusCode   int    `json:"statusCode"`
}

// SetBlocked persists the upstream rejection and returns the recorded state.
func (s Store) SetBlocked(statusCode int, evidence string, headers http.Header) (*Blocked, error) {
	now := s.Now().UnixMilli()
	block := &Blocked{State: blockedState, ObservedAt: isoTime(now), StatusCode: statusCode, Evidence: evidence}
	if statusCode == http.StatusTooManyRequests {
		expires := rateLimitExpiry(headers, float64(now))
		if math.Abs(expires) > maxDateMillis {
			return nil, errors.New("upstream rate limit expiry is outside the supported date range")
		}
		block.ExpiresAt = isoTime(int64(expires))
		block.ExpiresAtMs = new(expires)
	}
	if err := writeState(s.statePath(blockedFile), block); err != nil {
		return nil, err
	}
	return block, nil
}

func rateLimitExpiry(headers http.Header, now float64) float64 {
	const retryAfterName = "Retry-After"
	var retryKey string
	var resetValues []string
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		switch {
		case strings.EqualFold(key, retryAfterName):
			// A hand-built Header may split spellings across keys, losing wire order.
			if retryKey == "" || key == retryAfterName || retryKey != retryAfterName && key < retryKey {
				retryKey = key
			}
		case strings.EqualFold(key, "Anthropic-Ratelimit-Unified-Reset"):
			resetValues = append(resetValues, values...)
		}
	}
	if retryKey != "" {
		if retryAfter := headers[retryKey][0]; retryAfter != "" {
			if seconds := config.JSNumber(retryAfter); !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
				return now + max(0, seconds)*1000
			}
			if when, ok := retryAfterDate(retryAfter); ok {
				return float64(when.UnixMilli())
			}
		}
	}
	// Node joins repeated custom header values with ", "; only a single
	// occurrence can therefore produce a number.
	if reset := config.JSNumber(strings.Join(resetValues, ", ")); reset > 0 {
		return reset * 1000
	}
	return now + float64(rateLimitDefault.Milliseconds())
}

func retryAfterDate(value string) (time.Time, bool) {
	if date, err := time.Parse("Monday, 02-Jan-06 15:04:05 GMT", value); err == nil {
		// V8 rolls two-digit years back at 50, rather than Go's 69.
		if date.Year() >= 2050 {
			date = date.AddDate(-100, 0, 0)
		}
		return date, true
	}
	for _, layout := range []string{http.TimeFormat, time.ANSIC, time.RFC3339Nano, "2006-01-02T15:04Z07:00", time.DateOnly, time.RFC1123Z, "Mon, 02 Jan 2006 15:04:05 UTC"} {
		if date, err := time.Parse(layout, value); err == nil {
			return date, true
		}
	}
	return time.Time{}, false
}

func isoTime(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	if year := t.Year(); year < 0 || year > 9999 {
		return fmt.Sprintf("%+07d", year) + t.Format("-01-02T15:04:05.000Z")
	}
	return t.Format(isoLayout)
}

// ReadBlocked returns an active block, or nil for missing, unreadable or expired state.
func (s Store) ReadBlocked() *Blocked {
	now := s.Now().UnixMilli()
	record := readState(s.statePath(blockedFile))
	if stateMember[string](record["state"]) != blockedState {
		return nil
	}
	var expiresAtMs *float64
	// Missing expiry is indefinite; a present value participates in a JavaScript numeric comparison.
	if expiry := record["expiresAtMs"]; expiry != nil {
		if expiry.Kind() == jsontext.KindNumber {
			ms := config.JSNumber(string(expiry))
			if float64(now) >= ms {
				return nil
			}
			expiresAtMs = new(ms)
		} else {
			var value any
			if err := json.Unmarshal(expiry, &value, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err == nil && float64(now) >= config.JSNumber(value) {
				return nil
			}
		}
	}
	return &Blocked{
		State:       blockedState,
		ObservedAt:  stateMember[string](record["observedAt"]),
		StatusCode:  stateMember[int](record["statusCode"]),
		Evidence:    stateMember[string](record["evidence"]),
		ExpiresAt:   stateMember[string](record["expiresAt"]),
		ExpiresAtMs: expiresAtMs,
	}
}

// ClearBlocked removes the recorded rejection if present.
func (s Store) ClearBlocked() { _ = syscall.Unlink(s.statePath(blockedFile)) }

// SetUnavailable records a transient failure unless a block is active.
func (s Store) SetUnavailable(statusCode int) (*Unavailable, error) {
	now := s.Now().UnixMilli()
	if s.ReadBlocked() != nil {
		return nil, nil
	}
	u := &Unavailable{
		State:        unavailableState,
		ObservedAt:   isoTime(now),
		ObservedAtMs: now,
		ExpiresAt:    isoTime(now + unavailableTTL.Milliseconds()),
		StatusCode:   statusCode,
	}
	if err := writeState(s.statePath(unavailableFile), u); err != nil {
		return nil, err
	}
	return u, nil
}

// ReadUnavailable returns an active transient failure, or nil if none can be read.
func (s Store) ReadUnavailable() *Unavailable {
	now := s.Now().UnixMilli()
	record := readState(s.statePath(unavailableFile))
	observed := record["observedAtMs"]
	if stateMember[string](record["state"]) != unavailableState || observed.Kind() != jsontext.KindNumber {
		return nil
	}
	observedAtMs := config.JSNumber(string(observed))
	if math.IsInf(observedAtMs, 0) || float64(now)-observedAtMs >= float64(unavailableTTL.Milliseconds()) {
		return nil
	}
	return &Unavailable{
		State:        unavailableState,
		ObservedAt:   stateMember[string](record["observedAt"]),
		ObservedAtMs: int64(observedAtMs),
		ExpiresAt:    stateMember[string](record["expiresAt"]),
		StatusCode:   stateMember[int](record["statusCode"]),
	}
}

// ClearUnavailable removes the recorded transient failure if present.
func (s Store) ClearUnavailable() { _ = syscall.Unlink(s.statePath(unavailableFile)) }

func (s Store) statePath(name string) string {
	if !filepath.IsAbs(s.Dir) || filepath.Clean(s.Dir) != s.Dir {
		return ""
	}
	return filepath.Join(s.Dir, name)
}

func readState(file string) map[string]jsontext.Value {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	// Decode raw members first so duplicates are last-wins even if an earlier
	// value has a different type, and unrelated metadata cannot erase a hold.
	var record map[string]jsontext.Value
	if err := json.Unmarshal(data, &record, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		return nil
	}
	return record
}

func stateMember[T any](raw jsontext.Value) T {
	var value T
	if err := json.Unmarshal(raw, &value, jsontext.AllowInvalidUTF8(true)); err != nil {
		var zero T
		return zero
	}
	return value
}

func writeState(file string, state any) error {
	if file == "" {
		return errors.New("upstream state directory must be clean and absolute")
	}
	data, err := jsonx.EncodeState(state)
	if err != nil {
		return err
	}
	compact := jsontext.Value(data)
	if err := compact.Compact(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		return err
	}
	return atomicfile.WriteFile(file, append(compact, '\n'), 0o600)
}

// BlockedReadinessMessage describes the block and how to resume dispatch using the stable binary.
func (s Store) BlockedReadinessMessage(block Blocked) string {
	if block.ExpiresAt != "" {
		return "Codex is rate-limited by OpenAI (429) until " + block.ExpiresAt + ". The block lifts by itself then, or sooner when a Codex request succeeds; wait, or explicitly re-route this ticket."
	}
	return "Codex is blocked by an OpenAI rejection. Run `\"" + s.CommandPath + "\" setup`; if it persists, wait for a claude-code-proxy update or explicitly re-route this ticket. Codex tickets remain blocked."
}

// ReadinessMessage describes when the transient failure occurred and when it lifts.
func (u Unavailable) ReadinessMessage() string {
	if u.ObservedAt == "" {
		return "Codex had a terminal upstream failure in the last 30 seconds. Wait briefly, then retry; /v1/models only proves the local proxy is answering."
	}
	return fmt.Sprintf("Codex had a terminal upstream failure (HTTP %d) at %s. Dispatch holds until %s, or until a Codex request succeeds; /v1/models only proves the local proxy is answering.", u.StatusCode, u.ObservedAt, cmp.Or(u.ExpiresAt, "about 30 seconds later"))
}
