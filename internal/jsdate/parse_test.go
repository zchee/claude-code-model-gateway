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

package jsdate_test

import (
	"os"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsdate"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestParse(t *testing.T) {
	tests := map[string]struct {
		input string
		ms    int64
		ok    bool
	}{
		"success: ISO date only is UTC":                {"2026-09-28", 1790553600000, true},
		"success: ISO valid leap day":                  {"2024-02-29", 1709164800000, true},
		"success: ISO no seconds":                      {"2026-09-28T10:05Z", 1790589900000, true},
		"success: ISO lowercase T and Z":               {"2026-09-28t10:05:00z", 1790589900000, true},
		"success: ISO nine fractional digits":          {"2026-09-28T10:05:00.012999999Z", 1790589900012, true},
		"success: ISO one fractional digit and offset": {"2026-09-28T10:05:00.1+02:30", 1790580900100, true},
		"success: IMF fixdate":                         {"Mon, 28 Sep 2026 10:05:00 GMT", 1790589900000, true},
		"success: IMF UTC and lowercase month":         {"Mon, 28 sep 2026 10:05:00 UTC", 1790589900000, true},
		"success: IMF negative numeric zone":           {"Mon, 28 Sep 2026 10:05:00 -0230", 1790598900000, true},
		"success: IMF weekday mismatch is ignored":     {"Tue, 28 Sep 2026 10:05:00 GMT", 1790589900000, true},
		"success: RFC 850 ordinary date":               {"Monday, 28-Sep-26 10:05:00 GMT", 1790589900000, true},
		"success: RFC 850 year forty-nine":             {"Friday, 01-Jan-49 00:00:00 GMT", 2493072000000, true},
		"success: RFC 850 year fifty":                  {"Sunday, 01-Jan-50 00:00:00 GMT", -631152000000, true},
		"error: asctime has no zone":                   {input: "Mon Sep 28 10:05:00 2026"},
		"error: ISO time without a zone":               {input: "2026-09-28T10:05"},
		"error: ISO offset hour 24":                    {input: "2026-09-29T10:05+24:00"},
		"error: ISO offset minute 60":                  {input: "2026-09-29T10:05+00:60"},
		"error: ISO comma fraction":                    {input: "2026-09-28T10:05:00,123Z"},
		"error: ISO one-digit hour":                    {input: "2026-09-28T1:05:00Z"},
		"error: ISO ten fractional digits":             {input: "2026-09-28T10:05:00.0123456789Z"},
		"error: ISO fraction without seconds":          {input: "2026-09-28T10:05.123Z"},
		"error: ISO leap second":                       {input: "2026-09-28T10:05:60Z"},
		"error: calendar-invalid day":                  {input: "2026-02-30"},
		"error: calendar-invalid month":                {input: "2026-13-01"},
		"error: calendar-invalid hour":                 {input: "2026-09-28T24:00Z"},
		"error: IMF year 0049 would pivot":             {input: "Mon, 28 Sep 0049 10:05:00 GMT"},
		"error: IMF leap second":                       {input: "Mon, 28 Sep 2026 10:05:60 GMT"},
		"error: IMF fraction":                          {input: "Mon, 28 Sep 2026 10:05:00.123 GMT"},
		"error: IMF unknown zone":                      {input: "Mon, 28 Sep 2026 10:05:00 XYZ"},
		"error: IMF invalid calendar day":              {input: "Sat, 29 Feb 2025 10:05:00 GMT"},
		"error: trailing bytes":                        {input: "2026-09-28T10:05Z\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ms, ok := jsdate.Parse(tt.input)
			if diff := gocmp.Diff(tt.ok, ok); diff != "" {
				t.Fatalf("accepted %q (-want +got):\n%s", tt.input, diff)
			}
			if diff := gocmp.Diff(tt.ms, ms); diff != "" {
				t.Errorf("milliseconds for %q (-want +got):\n%s", tt.input, diff)
			}
		})
	}
}

func FuzzParseUTCRoundTrip(f *testing.F) {
	for _, seed := range []string{
		"2026-09-28", "2026-09-28t10:05:00.012999999z", "2026-01-01T00:00:00+02:30",
		"Mon, 28 Sep 2026 10:05:00 -0230", "Sunday, 01-Jan-50 00:00:00 GMT", "2026-02-30",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		ms, ok := jsdate.Parse(input)
		if !ok {
			return
		}
		utc := time.UnixMilli(ms).UTC()
		// An explicit offset can move a boundary date outside four-digit years.
		if utc.Year() < 0 || utc.Year() > 9999 {
			return
		}
		canonical := utc.Format("2006-01-02T15:04:05.000Z")
		roundTrip, ok := jsdate.Parse(canonical)
		if !ok || roundTrip != ms {
			t.Errorf("UTC round trip of %q via %q: got (%d, %t), want (%d, true)", input, canonical, roundTrip, ok, ms)
		}
	})
}
