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

package config

import (
	"math"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestJSNumber(t *testing.T) {
	// ECMA-262 ToNumber, StringToNumber, and Array.prototype.toString.
	negativeZero := math.Copysign(0, -1)
	tests := map[string]struct {
		value any
		want  float64
	}{
		"success: empty string is zero":                                {"", 0},
		"success: ECMAScript whitespace":                               {string(rune(0xFEFF)) + "\t\v" + string(rune(0x2029)), 0},
		"error: NEL is not ECMAScript whitespace":                      {string(rune(0x85)), math.NaN()},
		"success: trimmed decimal exponent":                            {" 3e5 ", 300000},
		"success: trimmed signed fractional exponent":                  {" +.3e-1 ", .03},
		"success: trailing decimal point":                              {"1.", 1},
		"success: leading zero decimal":                                {"077", 77},
		"success: radix prefix":                                        {"0x10", 16},
		"success: binary prefix":                                       {"0B101", 5},
		"success: octal prefix":                                        {"0o12", 10},
		"success: rounding past exact integers":                        {"0x20000000000001", 1 << 53},
		"success: unsigned integer larger than uint64":                 {"0x10000000000000000", 1 << 64},
		"success: decimal overflow":                                    {"1e400", math.Inf(1)},
		"success: hex overflow":                                        {"0x1" + strings.Repeat("0", 256), math.Inf(1)},
		"success: decimal signed underflow":                            {"-1e-999", negativeZero},
		"success: negative zero":                                       {"-0", negativeZero},
		"success: positive infinity":                                   {"+Infinity", math.Inf(1)},
		"success: negative infinity":                                   {"-Infinity", math.Inf(-1)},
		"error: incomplete infinity":                                   {"Inf", math.NaN()},
		"error: infinity is case-sensitive":                            {"infinity", math.NaN()},
		"error: NaN":                                                   {"NaN", math.NaN()},
		"error: signed prefix":                                         {"-0x10", math.NaN()},
		"error: signs only":                                            {"+", math.NaN()},
		"error: trailing garbage":                                      {"12ms", math.NaN()},
		"error: hex fraction":                                          {"0x1p4", math.NaN()},
		"error: Go digit separator":                                    {"1_000", math.NaN()},
		"error: empty prefix":                                          {"0x", math.NaN()},
		"error: invalid base digit":                                    {"0b12", math.NaN()},
		"success: JSON null":                                           {nil, 0},
		"success: JSON true":                                           {true, 1},
		"success: JSON false":                                          {false, 0},
		"success: empty array":                                         {[]any{}, 0},
		"success: numeric array":                                       {[]any{float64(5)}, 5},
		"success: nested array":                                        {[]any{[]any{"0x10"}}, 16},
		"success: array with null":                                     {[]any{nil}, 0},
		"success: array with numeric negative zero serializes as zero": {[]any{negativeZero}, 0},
		"success: array with string negative zero retains its sign":    {[]any{"-0"}, negativeZero},
		"error: array with boolean stringifies to true":                {[]any{true}, math.NaN()},
		"error: mixed array serializes with comma":                     {[]any{float64(1), nil}, math.NaN()},
		"error: object":                                                {map[string]any{}, math.NaN()},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) { assertNumber(t, JSNumber(tt.value), tt.want) })
	}
}

func TestJSParseInt(t *testing.T) {
	// ECMA-262 parseInt uses the longest prefix of radix digits after the optional sign.
	tests := map[string]struct {
		value string
		radix int
		want  float64
	}{
		"success: explicit radix 16":                   {"ff", 16, 255},
		"success: hexadecimal prefix":                  {" +0x10rest", 0, 16},
		"success: decimal truncates exponent":          {"1e4", 10, 1},
		"success: decimal truncates hex":               {"0x10", 10, 0},
		"success: decimal truncates suffix":            {"123ms", 10, 123},
		"success: leading zeros are decimal":           {"077", 0, 77},
		"success: negative zero":                       {"-0foo", 10, math.Copysign(0, -1)},
		"success: leading BOM and trailing whitespace": {string(rune(0xFEFF)) + "-12 ", 10, -12},
		"error: empty":                                 {"", 0, math.NaN()},
		"error: radix outside range":                   {"12", 1, math.NaN()},
		"error: no integer prefix":                     {"Infinity", 10, math.NaN()},
		"error: blank before first digit":              {"- 1", 10, math.NaN()},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) { assertNumber(t, JSParseInt(tt.value, tt.radix), tt.want) })
	}
}

func TestJSParseFloat(t *testing.T) {
	// ECMA-262 parseFloat accepts the longest decimal or Infinity prefix, not radix prefixes.
	tests := map[string]struct {
		value string
		want  float64
	}{
		"success: decimal exponent and trailing garbage": {" +1.e4x", 10000},
		"success: stop before incomplete exponent":       {"2e-", 2},
		"success: hexadecimal stops at x":                {"0x10", 0},
		"success: Infinity prefix":                       {"-Infinity!", math.Inf(-1)},
		"success: negative zero":                         {"-.0hello", math.Copysign(0, -1)},
		"error: empty":                                   {"", math.NaN()},
		"error: NaN":                                     {"NaN", math.NaN()},
		"error: Infinity is case-sensitive":              {"infinity", math.NaN()},
		"error: sign and dot without digits":             {"+.", math.NaN()},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) { assertNumber(t, JSParseFloat(tt.value), tt.want) })
	}
}

func assertNumber(t *testing.T, got, want float64) {
	t.Helper()
	if !gocmp.Equal(want, got, cmpopts.EquateNaNs()) || got == 0 && math.Signbit(want) != math.Signbit(got) {
		t.Errorf("got %v, want %v", got, want)
	}
}
