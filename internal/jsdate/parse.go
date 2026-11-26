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

package jsdate

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	hour        = `(?:[01][0-9]|2[0-3])`
	minute      = `[0-5][0-9]`
	clockFields = `(` + hour + `):(` + minute + `)`
	secondField = `:([0-5][0-9])`
	months      = "janfebmaraprmayjunjulaugsepoctnovdec"
)

var (
	isoDate = regexp.MustCompile(`\A([0-9]{4})-([0-9]{2})-([0-9]{2})(?:[Tt]` + clockFields +
		`(?:` + secondField + `(?:\.([0-9]{1,9}))?)?([Zz]|[+-]` + hour + `:` + minute + `))?\z`)
	imfDate = regexp.MustCompile(`\A(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun), ([0-9]{2}) ([A-Za-z]{3}) ([0-9]{4}) ` +
		clockFields + secondField + ` (GMT|UTC|[+-]` + hour + minute + `)\z`)
	rfc850Date = regexp.MustCompile(`\A(?:Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday), ([0-9]{2})-([A-Za-z]{3})-([0-9]{2}) ` +
		clockFields + secondField + ` (GMT)\z`)
)

// Parse returns Unix milliseconds for a supported date and false otherwise.
// It accepts YYYY-MM-DD in UTC, ISO date-times with an explicit zone, IMF-fixdate
// (also with UTC or a numeric zone), and RFC 850 HTTP dates with V8's year pivot.
// A weekday must be spelled as in the format but need not match the date.
func Parse(s string) (unixMilli int64, ok bool) {
	if match := isoDate.FindStringSubmatch(s); match != nil {
		// V8 reads at most the first three fractional digits and pads on the right.
		ms := decimal((match[7] + "000")[:3])
		return timestamp(decimal(match[1]), time.Month(decimal(match[2])), decimal(match[3]), decimal(match[4]), decimal(match[5]), decimal(match[6]), ms, match[8])
	}

	match := imfDate.FindStringSubmatch(s)
	if match == nil {
		match = rfc850Date.FindStringSubmatch(s)
		if match == nil {
			return 0, false
		}
	}
	year := decimal(match[3])
	if len(match[3]) == 2 {
		// V8 applies the pivot to non-ISO years.
		if year < 50 {
			year += 2000
		} else {
			year += 1900
		}
	} else if year < 100 {
		return 0, false
	}
	month := strings.Index(months, strings.ToLower(match[2]))
	if month < 0 || month%3 != 0 {
		return 0, false
	}
	return timestamp(year, time.Month(month/3+1), decimal(match[1]), decimal(match[4]), decimal(match[5]), decimal(match[6]), 0, match[7])
}

func decimal(s string) int {
	n, _ := strconv.Atoi(s) // The patterns guarantee digits or an absent field.
	return n
}

func timestamp(year int, month time.Month, day, h, m, s, ms int, zone string) (int64, bool) {
	wall := time.Date(year, month, day, h, m, s, ms*int(time.Millisecond), time.UTC)
	if y, mon, d := wall.Date(); y != year || mon != month || d != day {
		return 0, false
	}
	t := wall.UnixMilli()
	if len(zone) >= 5 { // A numeric zone is either ±HHMM or ±HH:MM.
		offset := int64(decimal(zone[1:3])*60+decimal(zone[len(zone)-2:])) * time.Minute.Milliseconds()
		if zone[0] == '-' {
			offset = -offset
		}
		t -= offset
	}
	return t, true
}
