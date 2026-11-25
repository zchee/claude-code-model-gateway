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

// Package config reads the gateway's environment through an injected lookup.
package config

import (
	"cmp"
	"maps"
	"math"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// LookupEnv retrieves a raw environment value and reports whether it exists.
// A nil lookup represents an empty environment.
type LookupEnv func(string) (string, bool)

// Variable documents one supported environment variable and where the pinned source reads it.
type Variable struct {
	Name    string
	Default string
	Meaning string
	Rule    string
	Readers string
}

// Key is a documented environment variable with a typed result.
type Key[T any] struct {
	variable Variable
	parse    func(string, bool) T
}

// Get parses the variable from lookup without consulting the process environment.
func (k Key[T]) Get(lookup LookupEnv) T {
	var raw string
	var set bool
	if lookup != nil {
		raw, set = lookup(k.variable.Name)
	}
	if !set {
		raw = ""
	}
	return k.parse(raw, set)
}

func key[T any](name, fallback, meaning, rule, readers string, parse func(string, bool) T) Key[T] {
	return Key[T]{Variable{Name: name, Default: fallback, Meaning: meaning, Rule: rule, Readers: readers}, parse}
}

func stringKey(name, fallback, meaning, readers string) Key[string] {
	return key(name, cmp.Or(fallback, "unset"), meaning, "Use any nonempty string without trimming.", readers, func(s string, _ bool) string {
		return cmp.Or(s, fallback)
	})
}

func candidate(name, fallback, meaning, rule, readers string) Key[string] {
	return key(name, fallback, meaning, rule, readers, func(s string, _ bool) string { return s })
}

func rawKey(name, meaning, rule, readers string) Key[*string] {
	return key(name, "unset", meaning, rule, readers, func(s string, set bool) *string {
		if set {
			return &s
		}
		return nil
	})
}

func trimmedKey(name, fallback, meaning, readers string) Key[string] {
	return key(name, fallback, meaning, "Trim ECMAScript whitespace; blank is unset.", readers, func(s string, _ bool) string {
		return jsTrim(s)
	})
}

func toggle(name, meaning, readers string, onByDefault bool) Key[bool] {
	if onByDefault {
		return key(name, "on", meaning, `Only the exact string "0" disables it.`, readers, func(s string, _ bool) bool { return s != "0" })
	}
	return key(name, "off", meaning, `Only the exact string "1" enables it.`, readers, func(s string, _ bool) bool { return s == "1" })
}

func envNumber(s string, set bool) float64 {
	if !set {
		return math.NaN()
	}
	return jsNumber(s)
}

func numericOr(n, fallback float64) float64 {
	if n == 0 || math.IsNaN(n) {
		return fallback
	}
	return n
}

func finite(n float64) bool  { return !math.IsInf(n, 0) && !math.IsNaN(n) }
func integer(n float64) bool { return finite(n) && math.Trunc(n) == n }

func numberKey(name, meaning, rule, readers string, fallback float64, accept func(float64) bool) Key[float64] {
	return key(name, strconv.FormatFloat(fallback, 'f', -1, 64), meaning, "Apply Number, then "+rule, readers, func(s string, set bool) float64 {
		if n := envNumber(s, set); accept(n) {
			return n
		}
		return fallback
	})
}

func finiteNonnegative(n float64) bool  { return finite(n) && n >= 0 }
func finitePositive(n float64) bool     { return finite(n) && n > 0 }
func integerNonnegative(n float64) bool { return integer(n) && n >= 0 }
func integerPositive(n float64) bool    { return integer(n) && n > 0 }
func truthyNumber(n float64) bool       { return n != 0 && !math.IsNaN(n) }

func portKey(name, meaning, readers, fallback string) Key[float64] {
	return key(name, fallback, meaning, "Use the default only if unset or empty, otherwise apply Number without validating the result.", readers, func(s string, _ bool) float64 {
		return jsNumber(cmp.Or(s, fallback))
	})
}

// ContextWindow selects the full window or a fixed token count. Its zero value is unset.
type ContextWindow struct {
	Full   bool
	Tokens int
}

// LlamaPort distinguishes a missing port from a malformed one. A configured zero port is invalid.
type LlamaPort struct {
	Port       uint16
	Configured bool
	Invalid    string
}

var llamaPortPattern = regexp.MustCompile(`^\+?[0-9]+$`)

func parseLlamaPort(s string, set bool) LlamaPort {
	if !set {
		return LlamaPort{}
	}
	if llamaPortPattern.MatchString(s) {
		if n := jsNumber(s); n >= 1 && n <= 65535 {
			return LlamaPort{Port: uint16(n), Configured: true}
		}
	}
	return LlamaPort{Configured: true, Invalid: s}
}

// WorkerPort resolves the runtime worker port, defaulting to the parsed public port.
func WorkerPort(lookup LookupEnv) float64 {
	if worker := WorkerPortValue.Get(lookup); worker != "" {
		return jsNumber(worker)
	}
	return PublicPort.Get(lookup)
}

// SupervisorWorkerPort resolves the worker-port override; zero requests an ephemeral port.
func SupervisorWorkerPort(lookup LookupEnv) float64 {
	return jsNumber(cmp.Or(WorkerPortValue.Get(lookup), "0"))
}

// RecoveryPublicPort returns the port spelling used in the launcher's string comparison.
func RecoveryPublicPort(lookup LookupEnv) string {
	if lookup == nil {
		return defaultPublicPort("")
	}
	s, set := lookup(PublicPort.variable.Name)
	if !set {
		s = ""
	}
	return defaultPublicPort(s)
}

func defaultPublicPort(s string) string { return cmp.Or(s, publicPortDefault) }

// SSEHeartbeatMilliseconds preserves the source's seconds-to-milliseconds conversion.
func SSEHeartbeatMilliseconds(lookup LookupEnv) float64 {
	return SSEHeartbeatSeconds.Get(lookup) * 1000
}

// ProbeTimeoutFor applies the platform-specific ownership probe timeout.
func ProbeTimeoutFor(lookup LookupEnv, platform string) float64 {
	if n := ProbeTimeoutValue.Get(lookup); n != nil {
		return *n
	}
	if platform == "win32" || platform == "windows" {
		return 8000
	}
	return 2000
}

// ProbeTimeout returns the ownership probe timeout for the runtime platform.
func ProbeTimeout(lookup LookupEnv) float64 { return ProbeTimeoutFor(lookup, runtime.GOOS) }

// ProxyRequestCompression selects the effective proxy compression override, preserving an explicitly empty value.
func ProxyRequestCompression(lookup LookupEnv) *string {
	return cmp.Or(ProxyCompression.Get(lookup), GatewayCompression.Get(lookup))
}

// NoProxyForPinProbe returns the inherited NO_PROXY entries with the probe's loopback exceptions appended.
func NoProxyForPinProbe(lookup LookupEnv) string {
	var values []string
	for _, k := range []Key[string]{NoProxy, NoProxyLower} {
		if value := k.Get(lookup); value != "" {
			values = append(values, value)
		}
	}
	return strings.Join(append(values, "127.0.0.1", "localhost"), ",")
}

// PinProbeEnvironment returns a copy with inherited native authentication and model keys removed.
func PinProbeEnvironment(env map[string]string) map[string]string {
	excluded := [...]string{
		anthropicAPIKey.variable.Name, anthropicAuth.variable.Name, AnthropicBaseURL.variable.Name,
		anthropicFable.variable.Name, anthropicOpus.variable.Name, anthropicSonnet.variable.Name, claudeOAuth.variable.Name,
	}
	out := maps.Clone(env)
	maps.DeleteFunc(out, func(name, _ string) bool { return slices.Contains(excluded[:], strings.ToUpper(name)) })
	return out
}

// NativePickerEnvironment returns the inherited environment minus session markers and gateway-owned keys.
func NativePickerEnvironment(env, gatewayEnv map[string]string) map[string]string {
	markers := [...]string{claudeCode.variable.Name, claudeEntrypoint.variable.Name, claudeSSEPort.variable.Name, claudeSessionID.variable.Name}
	out := maps.Clone(env)
	maps.DeleteFunc(out, func(name, _ string) bool {
		_, owned := gatewayEnv[name]
		return owned || slices.Contains(markers[:], name)
	})
	return out
}
