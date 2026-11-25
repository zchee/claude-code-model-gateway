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

package gatewaytest

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"maps"
	"net/netip"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

// ErrProbeEndpoint identifies a fixture endpoint that is not a safe loopback HTTP address.
var ErrProbeEndpoint = errors.New("fixture probe requires an HTTP loopback IP with an explicit non-reserved port")

// ProbeAlias reads the init event from an explicitly named sandbox fixture. A refused,
// missing, timed-out or unsuccessful fixture produces no model. It never falls back to PATH.
func (env Environment) ProbeAlias(ctx context.Context, alias, endpoint string) (*string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return nil, ErrProbeEndpoint
	}
	addr, err := netip.ParseAddrPort(u.Host)
	if err != nil || addr.Addr().Zone() != "" || !addr.Addr().Unmap().IsLoopback() || addr.Port() == 0 || reservedPort(int(addr.Port())) {
		return nil, ErrProbeEndpoint
	}
	if !env.guarded() {
		return nil, nil
	}
	var output initLines
	err = env.StartClaude(ctx, func(ctx context.Context, path string) error {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(config.PinProbeTimeout.Get(env.lookup))*time.Millisecond)
		defer cancel()
		child := Environment(config.PinProbeEnvironment(env))
		maps.DeleteFunc(child, func(key, _ string) bool {
			switch strings.ToUpper(key) {
			case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "ANTHROPIC_UNIX_SOCKET", "CODEX_GATEWAY_SOCKET_PATH":
				return true
			}
			return false
		})
		child["ANTHROPIC_BASE_URL"] = endpoint
		for _, name := range []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY", "DISABLE_ERROR_REPORTING"} {
			child[name] = "1"
		}
		cmd := exec.CommandContext(ctx, path, "--bare", "--no-session-persistence", "--model", alias, "-p", "--output-format", "stream-json", "--verbose")
		cmd.Stdin = strings.NewReader("/model " + alias + "\n")
		output.alias = strings.ToLower(alias)
		output.validModel = regexp.MustCompile(`(?i)^claude-[a-z0-9][a-z0-9._-]*$`)
		cmd.Stdout, cmd.Stderr = &output, &output
		cmd.WaitDelay = time.Second
		return child.RunFixture(cmd)
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || output.model == "" {
		return nil, nil
	}
	return &output.model, nil
}

type initLines struct {
	validModel   *regexp.Regexp
	pending      []byte
	alias, model string
}

func (p *initLines) Write(chunk []byte) (int, error) {
	length := len(chunk)
	for len(chunk) > 0 {
		line, rest, complete := bytes.Cut(chunk, []byte{'\n'})
		if len(p.pending)+len(line) > 262144 {
			return 0, errors.New("oversized fixture probe event")
		}
		p.pending = append(p.pending, line...)
		if complete {
			var event struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
				Model   string `json:"model"`
			}
			if err := json.Unmarshal(p.pending, &event); err == nil && event.Type == "system" && event.Subtype == "init" {
				model := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(event.Model, "[1M]", ""), "[1m]", ""))
				if p.validModel.MatchString(model) && slices.Contains(strings.Split(strings.ToLower(model), "-"), p.alias) {
					p.model = model + "[1m]"
				}
			}
			p.pending = p.pending[:0]
		}
		chunk = rest
	}
	return length, nil
}

// DetectedPins collects the init events from the sandbox fixture without reading or writing a pin cache.
func (env Environment) DetectedPins(ctx context.Context) (map[string]string, error) {
	pins := make(map[string]string)
	for _, alias := range []string{"opus", "sonnet", "fable"} {
		model, err := env.ProbeAlias(ctx, alias, "http://127.0.0.1:9")
		if err != nil {
			return nil, err
		}
		if model != nil {
			pins[alias] = *model
		}
	}
	return pins, nil
}
