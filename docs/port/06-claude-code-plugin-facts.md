# Claude Code plugin facts for the Go-binary port

## Scope and caveats

I fetched the plugin manifest, components, loading, hooks, marketplace, create-marketplace, LLM gateway, gateway-connect, gateway-protocol and model-config pages. I did not fetch `settings-reference` or `env-vars` directly. Quotes come from WebFetch output, so spot-check exact wording before relying on it. The hooks page is about 301k characters. I read the first 300k in three windows (0-100k, 100k-200k, 200k-300k) and did not read the final 1.7k, so anything in that last sliver is unread. Question 3 is the only one that depends on the hooks page.

## 1. `bin/` and PATH

Source: https://code.claude.com/docs/en/plugins/components.md#executables

- "Files in `bin/` at the plugin root are on the `PATH` of the Bash tool's shell while the plugin is enabled, so Claude can run them as bare commands."
- "A command that Claude runs with the PowerShell tool outside the sandbox finds them on its `PATH` too, and a sandboxed PowerShell command doesn't."
- "Plugin `bin/` directories come after the user's own `PATH` entries."

- Contexts documented: Bash tool, and PowerShell tool outside the sandbox.
- Hook commands, MCP server commands, monitors: not documented.
- Since which version: not documented.
- claude.ai and Cowork do not install a plugin with a top-level `bin/`.

## 2. `${CLAUDE_PLUGIN_DATA}`

Sources: https://code.claude.com/docs/en/plugins/manifest-reference.md#environment-variables and https://code.claude.com/docs/en/plugins/components.md#reference-plugin-paths-and-store-data

- Definition: "`~/.claude/plugins/data/<id>/`, created on first reference and kept across plugin updates. `<id>` is the plugin identifier with every character other than a letter, digit, `_`, or `-` replaced by `-`."
- On-disk location: `~/.claude/plugins/data/<id>/`. The plugins root is `~/.claude/plugins` unless `CLAUDE_CODE_PLUGIN_CACHE_DIR` is set (loading.md).
- Persistence: survives updates. Uninstall: "By default, Claude Code deletes the `${CLAUDE_PLUGIN_DATA}` directory when you uninstall the plugin from the last place it's installed."
- Substitution fields (manifest-reference table):
  - Hook commands: anywhere in `command` and `args`.
  - Monitor commands: anywhere in `command`.
  - MCP stdio: `command`, `args`, `env`. MCP http/sse/ws: not applicable.
  - LSP: `command`, `args`, `env`, `workspaceFolder`.
  - Skill, command, agent content: anywhere in the Markdown body.
- Exported as an environment variable: yes, to hook processes, MCP stdio servers and LSP servers. Monitors: "Not exported."
- Bash tool: "The variables aren't present in the environment of commands Claude runs through the Bash tool."
- Since which version: not documented.

## 3. Hook command execution

Source: https://code.claude.com/docs/en/hooks.md

- Shell form (no `args`): the command is "passed to a shell: `sh -c` on macOS and Linux, Git Bash on Windows, or PowerShell when Git Bash isn't installed."
- Exec form (`args` present): "`command` is resolved as an executable and spawned directly with `args` as the argument vector, with no shell involved."
- `shell` field: "Accepts `"bash"` or `"powershell"`. Defaults to `"bash"`, or to `"powershell"` on Windows when Git Bash isn't installed… Ignored when `args` is set." Whether `"powershell"` is accepted on non-Windows: not documented.
- Windows default shell: Git Bash if installed, otherwise PowerShell.

Timeouts:
- General default: 600 seconds for `command`, `http`, `mcp_tool`.
- UserPromptSubmit default: 30 seconds for `command`, `http`, `mcp_tool`. Quote: "`UserPromptSubmit` hooks have a default timeout of 30 seconds for `command`, `http`, and `mcp_tool` types, shorter than the 600-second default for those types on most other events." A hook that reaches its timeout is canceled and its output is discarded. `onFailure: "block"` blocks the prompt instead.
- SessionStart default timeout: not documented. The section says only "SessionStart runs on every session, so keep these hooks fast."
- Maximum for command hooks: not documented. The only ceiling I found is SessionEnd's shared budget, raised up to 60 seconds.

Exit codes:
- Exit 0 (general): "Claude Code applies any JSON output fields your hook printed."
- SessionStart, exit 2: the event table lists it under "Context only" and says stderr is "shown to user only". Claude "doesn't see it, and the session or subagent proceeds." The session is not blocked.
- UserPromptSubmit, exit 2: "Blocks the prompt." "If your hook blocks by exiting 2, its stderr text is treated the same way as `reason`."

Stdout and JSON fields:
- Plain-text stdout on exit 0 is added as context for SessionStart and UserPromptSubmit. Quote: "Claude Code adds a SessionStart hook's plain-text stdout to Claude's context." For UserPromptSubmit: "Neither channel produces a visible transcript entry."
- JSON counts only when the whole stdout is one JSON object.
- SessionStart, `hookSpecificOutput` fields (from "SessionStart decision control"):
  - `additionalContext`: "String added to Claude's context at the start of the conversation, before the first prompt."
  - `initialUserMessage`, `sessionTitle`, `watchPaths`, `reloadSkills`.
  - `systemMessage`: mentioned only as an example in the input section. Its definition is not in the portions I read.
  - `decision`, `reason`, `continue`, `suppressOutput`: not documented for SessionStart in the portions I read. The section says it accepts the "JSON output fields available to all hooks", but that section was not in the portions I read.
- UserPromptSubmit (from "UserPromptSubmit decision control"; "All JSON output fields are available"):
  - `decision`: `"block"` rejects the prompt. Omit it to allow the prompt.
  - `reason`: shown to the user when blocked.
  - `hookSpecificOutput.additionalContext`: added to Claude's context alongside the prompt.
  - `hookSpecificOutput.sessionTitle`: sets the session title.
  - `hookSpecificOutput.suppressOriginalPrompt`: when `true`, leaves the prompt text out of the block message.
  - `systemMessage`, `continue`, `suppressOutput`: not documented in the portions I read.
- Example (UserPromptSubmit):

```json
{
  "decision": "block",
  "reason": "Explanation for decision",
  "hookSpecificOutput": {
    "hookEventName": "UserPromptSubmit",
    "additionalContext": "My additional context here",
    "sessionTitle": "My session title",
    "suppressOriginalPrompt": true
  }
}
```

- Hook types: SessionStart supports only `command` and `mcp_tool` hooks. Setup supports only `command` hooks. UserPromptSubmit is listed among events supporting all five types (`command`, `http`, `mcp_tool`, `prompt`, `agent`).
- Async SessionStart: "An async `SessionStart` hook runs every time the event fires. Claude Code skips the hook's output when the `additionalContext` and `systemMessage` text matches what Claude last received from that hook."

## 4. Install-time hooks and native dependencies

- Install or update hook for plugins: not documented.
- Setup event: "Fires only when you launch Claude Code with `--init-only`, or with `--init` or `--maintenance` in non-interactive mode with the `-p` flag." The docs add: "Because Setup doesn't fire on every launch, a plugin that needs a dependency installed can't rely on Setup alone."
- Earliest point plugin code can run: the docs do not name one. SessionStart is the earliest event they show for plugin hooks.
- Documented dependency pattern (Node only): components.md shows a SessionStart hook that copies `package.json` into `${CLAUDE_PLUGIN_DATA}` and runs `npm install` when it differs. hooks.md says "check for the dependency on first use and install on miss".
- Claude Code automatically installs eligible Node dependencies at plugin install, update, and first session start (loading.md, "Node.js package dependencies").
- Native binaries or Go: no documented pattern. The loading page lists "dependencies that aren't registry packages" and packages that need build steps as cases to install "from a hook into `${CLAUDE_PLUGIN_DATA}`". It gives no example.

## 5. `installed_plugins.json` and the cache

Source: https://code.claude.com/docs/en/plugins/loading.md#find-plugins-on-disk

- Location: `~/.claude/plugins/installed_plugins.json`, under the plugins root.
- Schema: "records each install with its `scope`, `installPath`, and `version`." No other fields are documented. `lastUpdated` appears only for `known_marketplaces.json`, so for `installed_plugins.json` it is not documented.
- `CLAUDE_CODE_PLUGIN_CACHE_DIR`: changes the plugins root from `~/.claude/plugins`.
- `CLAUDE_CONFIG_DIR`: not documented for plugin paths. It is documented only for the gateway model cache.
- Layout: `cache/<marketplace>/<plugin>/<version>/`. `<plugin>` is the marketplace entry name.
- Version resolution:
  1. The manifest `version`.
  2. The marketplace entry `version`.
  3. Otherwise by source: a 12-character commit SHA for `github`, `url`, `git-subdir`; a SHA-256 prefix for `archive`; `unknown` for `npm` and for a local directory outside git.
- Copy semantics: "Files outside the plugin directory aren't copied."
- Old versions: an `.orphaned_at` marker is written, and the directory is removed 14 days later.

## 6. Gateway model discovery

Sources: https://code.claude.com/docs/en/llm-gateway-protocol.md#model-discovery and https://code.claude.com/docs/en/llm-gateway-connect.md#add-gateway-models-to-the-model-picker

- Enable: `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`, in the shell or the `env` block, or through managed settings. Off by default.
- When it runs: Anthropic Messages format only. Not when any `CLAUDE_CODE_USE_*` variable is set, or when `ANTHROPIC_BASE_URL` is unset or `api.anthropic.com`.
- Endpoint: `GET /v1/models?limit=1000`, 3-second timeout by default (`CLAUDE_CODE_GATEWAY_MODEL_DISCOVERY_TIMEOUT_MS`, v2.1.269+). Redirects count as failure.
- Credentials sent: `Authorization` (from `ANTHROPIC_AUTH_TOKEN`, else `apiKeyHelper`) and `x-api-key`. If neither resolves, discovery is skipped.
- Response fields read: `data[].id`, plus optional `display_name` and `description`.
- Filter: keeps an `id` containing `claude` or `anthropic`, case-insensitive (v2.1.223+). Provider-prefixed ids pass.
- Labels: `display_name` if it differs from `id`; otherwise the recognized model name, otherwise the id. Description: the gateway's `description`, or "From gateway" if absent.
- Cache: "`~/.claude/cache/gateway-models.json`, or `%USERPROFILE%\.claude\cache\gateway-models.json` on Windows… If you set `CLAUDE_CONFIG_DIR`, the cache lives under that directory instead." Refreshed each startup. On failure it falls back to the previous cache, then to the built-in list.
- Debugging: `claude --debug`, then `[gatewayDiscovery]` lines in `~/.claude/debug/<session-id>.txt`.
- Model-id constraint: no rule that ids must start with `claude-` is documented. The only `claude-` reference is a typo warning on the Anthropic API. For `ANTHROPIC_CUSTOM_MODEL_OPTION`: "Claude Code skips validation for the model ID… so you can use any string your API endpoint accepts."
- `ANTHROPIC_CUSTOM_MODEL_OPTION` (model-config.md#add-a-custom-model-option): adds one entry to `/model` without replacing built-ins. Optional `_NAME` and `_DESCRIPTION`. Default description is `Custom model (<model-id>)`. Needed "only when discovery is disabled or does not return the model you want." If `availableModels` is set, the ID must be listed there too.
- `ANTHROPIC_DEFAULT_OPUS_MODEL`, `_SONNET_MODEL`, `_HAIKU_MODEL`, `_FABLE_MODEL`: control what the `opus`, `sonnet`, `haiku` and `fable` aliases resolve to. Each takes a full model name or the provider's ID. Setting one replaces that family's built-in picker rows with a single row. `_FABLE_MODEL` also tells Claude Code which ID counts as a Fable model for automatic fallback on third-party providers. Companion `_NAME`, `_DESCRIPTION` and `_SUPPORTED_CAPABILITIES` variables exist; `_NAME` and `_DESCRIPTION` apply when `ANTHROPIC_BASE_URL` points at a gateway.
- `[1m]` suffix: "append `[1m]` to a model alias or a full model name" to select the 1M-context variant. "Claude Code strips the suffix before sending the model ID to your provider." For unknown IDs containing `[1m]`, Claude Code assumes a 1M window.

## 7. Marketplace sources

Sources: https://code.claude.com/docs/en/plugins/create-marketplace.md#choose-a-plugin-source and https://code.claude.com/docs/en/plugins/marketplace-reference.md#plugin-sources

- A `github` plugin source is documented for "The plugin is a GitHub repository of its own", with `{ "source": "github", "repo": "your-org/my-first-plugin" }`. So a repo whose root is the plugin is the documented shape.
- The marketplace-reference page says "A repository that is both a project and a plugin often has submodules that only the project's build uses." No restriction on a Go module at the root is documented.
- `ref` and `sha` apply to `github`, `url` and `git-subdir`:
  - "`ref`: Branch, tag… Defaults to the repository's default branch."
  - "`sha`: a full 40-character lowercase commit SHA. When you set both `ref` and `sha`, Claude Code checks out `sha`."
  - So a tag or release branch is a documented source of plugin files.
- How a release pipeline produces the files (commits, built artifacts, tags): not documented.
- Version pinning: setting `version` in `plugin.json` "pins the plugin to that version until you change it". If it is unset, the commit SHA is the version, so an unchanged manifest version means users do not receive new commits.

## Implications for a Go-binary plugin (inference, not documented)

- Ship platform binaries inside the plugin directory, since the cache copies that directory. Per-OS/arch selection is not documented, so a small launcher script in `bin/` or a hook command would have to choose the binary.
- Use `${CLAUDE_PLUGIN_ROOT}/...` in hook `command`/`args` and MCP `command`. Prefer exec form (`args`) to avoid quoting and shell differences.
- Put state in `${CLAUDE_PLUGIN_DATA}`, not `${CLAUDE_PLUGIN_ROOT}`, since the root changes with every version.
- Do not rely on `bin/` being on PATH for hooks or MCP; call the path explicitly.

Sources: https://code.claude.com/docs/en/plugins/manifest-reference.md, https://code.claude.com/docs/en/plugins/components.md, https://code.claude.com/docs/en/plugins/loading.md, https://code.claude.com/docs/en/hooks.md, https://code.claude.com/docs/en/plugins/marketplace-reference.md, https://code.claude.com/docs/en/plugins/create-marketplace.md, https://code.claude.com/docs/en/llm-gateway-connect.md, https://code.claude.com/docs/en/llm-gateway-protocol.md, https://code.claude.com/docs/en/model-config.md
