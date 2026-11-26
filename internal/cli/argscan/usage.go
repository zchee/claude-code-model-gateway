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

package argscan

import (
	"cmp"
	"fmt"
	"path/filepath"
)

// Usage returns the source command list with the invoked name and the active paths.
// An empty requestLogPath selects the default under stateDir.
func Usage(name, stateDir, requestLogPath string) string {
	logs := filepath.Join(stateDir, "logs")
	return fmt.Sprintf(usage, name, filepath.Join(stateDir, "bin"), logs,
		filepath.Join(stateDir, "catalog.json"), filepath.Join(stateDir, "pins.json"),
		filepath.Join(stateDir, "context-window.json"), cmp.Or(requestLogPath, filepath.Join(logs, "request-routes.jsonl")))
}

const usage = `usage: %s <command>

  setup            download the claude-code-proxy binary (v-latest) into %s
  login [--device] run the ChatGPT OAuth flow (--device for headless device-code)
  start | stop     start/stop the proxy + shim (detached, logs in %s)
  ensure [--quiet] maintain installed wiring and start unless intentionally stopped
  recover          restore readiness without settings, pin or cache maintenance
  status           show what's running
  models           show the model list the shim advertises to Claude Code
  catalog [--json] [--refresh] print the sidequest-readable model catalog (%s);
                   --refresh exits non-zero and says why on stderr when it could not write a
                   fresh one, and prints the retained catalog unchanged
  pin [--opus|--sonnet|--fable <model|default>]
                   show pin sources and newest catalog ids, or persist an override (%s);
                   default removes the override and follows detected/catalog defaults, never backwards
  context-window [--claude <tokens|full>] [--codex <tokens|full>] [--grok <tokens|full>]
                   show or persist the context window per backend (%s); the
                   orchestrator and every executor share it. Defaults: claude full, codex 272000,
                   grok full. Without compact-at, a gateway cap compacts 85000 below the cap.
                   --codex-compact-at <tokens|cap> and --grok-compact-at <tokens|cap> set a direct
                   maximum before compaction (positive whole tokens); cap removes the override.
                   The cap and backend window minus 40000 still limit the effective maximum.
                   A saved compact-at ignores CODEX_GATEWAY_COMPACT_TRIGGER, reported unchanged.
                   Example: --codex-compact-at 242000 leaves the 272000 cap unchanged. OpenAI bills
                   input above 272k tokens at 2x; the crossing turn and compaction request can still
                   exceed 272k. Existing installations keep their current policy until configured.
                   --claude sets the native autoCompactWindow in project-wired settings; native
                   engine headroom applies and the exact trigger is unverified. Claude compact-at
                   is unsupported. No same-model main/subagent split is applied.
                   CODEX_GATEWAY_CONTEXT_WINDOW is superseded and only applies when no codex value is saved.
  env [--write-project | --write-user | --remove] [--reconcile]
                   print the Claude Code env block, or merge/remove wiring
                   --remove clears owned routing, discovery, static env and alias pins; preserves and names user pins
                   (--write-project writes .claude/settings.local.json; --write-user
                   is an opt-in shared fallback in ~/.claude/settings.json)
  doctor           full health check, Anthropic catalog age/fallback, pin sources and newest ids
  serve-shim       (internal) run the router in the foreground

  Request route logging (on by default; set CODEX_GATEWAY_REQUEST_LOG=0 to opt out):
    CODEX_GATEWAY_REQUEST_LOG=0
    Writes JSONL route metadata to %s. Override the path with
    CODEX_GATEWAY_REQUEST_LOG_PATH. It records no request bodies, prompts, tools, or auth.`
