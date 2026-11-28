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

package shadow

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

// spolTRecover runs call and turns a panic of the code under test into a failure of the current row only.
func spolTRecover(t *testing.T, call func()) (completed bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("the call panicked: %v", r)
		}
	}()
	call()
	return true
}

func TestAllowed(t *testing.T) {
	t.Run("Allowed", func(t *testing.T) {
		tests := map[string]struct {
			argv []string
			want bool
		}{
			// The eight admitted vectors. A gate that refuses any of them, for example one that
			// treats the first token as the executable name, fails here.
			"success: serve-shim":       {argv: []string{"serve-shim"}, want: true},
			"success: serve-worker":     {argv: []string{"serve-worker"}, want: true},
			"success: status":           {argv: []string{"status"}, want: true},
			"success: status --json":    {argv: []string{"status", "--json"}, want: true},
			"success: doctor":           {argv: []string{"doctor"}, want: true},
			"success: stop":             {argv: []string{"stop"}, want: true},
			"success: shadow preflight": {argv: []string{"shadow", "preflight"}, want: true},
			"success: shadow report":    {argv: []string{"shadow", "report"}, want: true},

			// No command at all. A gate that admits an empty vector would let the root print its
			// usage, which resolves paths, in shadow mode.
			"error: nil vector":         {argv: nil, want: false},
			"error: empty vector":       {argv: []string{}, want: false},
			"error: empty command name": {argv: []string{""}, want: false},

			// Every other registered command. A gate that admits whatever the root registers, or
			// that refuses only a few named commands, fails on one of these.
			"error: setup":          {argv: []string{"setup"}, want: false},
			"error: login":          {argv: []string{"login"}, want: false},
			"error: start":          {argv: []string{"start"}, want: false},
			"error: ensure":         {argv: []string{"ensure"}, want: false},
			"error: recover":        {argv: []string{"recover"}, want: false},
			"error: readiness-gate": {argv: []string{"readiness-gate"}, want: false},
			"error: models":         {argv: []string{"models"}, want: false},
			"error: catalog":        {argv: []string{"catalog"}, want: false},
			"error: pin":            {argv: []string{"pin"}, want: false},
			"error: context-window": {argv: []string{"context-window"}, want: false},
			"error: env":            {argv: []string{"env"}, want: false},

			// Commands that later phases register, and unknown names. A gate written as a deny list
			// of today's commands admits these.
			"error: migrate":         {argv: []string{"migrate"}, want: false},
			"error: hook":            {argv: []string{"hook", "session-start"}, want: false},
			"error: unknown command": {argv: []string{"restart"}, want: false},

			// Help, version and completion entry points, including Cobra's hidden completion
			// commands. A gate that lets the framework's built-ins through fails here.
			"error: help command":       {argv: []string{"help"}, want: false},
			"error: help flag":          {argv: []string{"--help"}, want: false},
			"error: short help flag":    {argv: []string{"-h"}, want: false},
			"error: version command":    {argv: []string{"version"}, want: false},
			"error: version flag":       {argv: []string{"--version"}, want: false},
			"error: completion command": {argv: []string{"completion", "zsh"}, want: false},
			// The root registers completions in the plural. A gate that admits it fails here.
			"error: completions command":     {argv: []string{"completions", "zsh"}, want: false},
			"error: hidden completion":       {argv: []string{"__complete", "status"}, want: false},
			"error: hidden completion plain": {argv: []string{"__completeNoDesc", "status"}, want: false},
			"error: help for an allowed verb": {
				argv: []string{"help", "status"}, want: false,
			},

			// Aliases and near spellings. A gate that matches by prefix, folds case or trims spaces
			// admits these.
			"error: prefix of serve-shim":   {argv: []string{"serve"}, want: false},
			"error: prefix of status":       {argv: []string{"stat"}, want: false},
			"error: upper-case status":      {argv: []string{"Status"}, want: false},
			"error: upper-case stop":        {argv: []string{"STOP"}, want: false},
			"error: status with a space":    {argv: []string{"status "}, want: false},
			"error: leading flag":           {argv: []string{"--json", "status"}, want: false},
			"error: executable name kept":   {argv: []string{"model-gateway", "status"}, want: false},
			"error: shadow without a verb":  {argv: []string{"shadow"}, want: false},
			"error: shadow with empty verb": {argv: []string{"shadow", ""}, want: false},
			"error: shadow verb in capitals": {
				argv: []string{"shadow", "Preflight"}, want: false,
			},

			// Extra tokens. A gate that checks only the first token, or that accepts any flag after
			// status, fails here.
			"error: serve-shim with a flag":    {argv: []string{"serve-shim", "--port", "21001"}, want: false},
			"error: serve-shim with one token": {argv: []string{"serve-shim", "x"}, want: false},
			"error: serve-worker with a token": {argv: []string{"serve-worker", "x"}, want: false},
			"error: doctor with --json":        {argv: []string{"doctor", "--json"}, want: false},
			"error: stop with a flag":          {argv: []string{"stop", "--force"}, want: false},
			"error: stop with an empty token":  {argv: []string{"stop", ""}, want: false},
			"error: status with --json twice":  {argv: []string{"status", "--json", "--json"}, want: false},
			"error: status with --JSON":        {argv: []string{"status", "--JSON"}, want: false},
			"error: status with --json=true":   {argv: []string{"status", "--json=true"}, want: false},
			"error: status with -j":            {argv: []string{"status", "-j"}, want: false},
			"error: status with --help":        {argv: []string{"status", "--help"}, want: false},
			"error: status with an empty token": {
				argv: []string{"status", ""}, want: false,
			},
			"error: status with a positional": {argv: []string{"status", "json"}, want: false},

			// Shadow subcommands other than the two, and extra tokens after them. A gate that
			// checks only that the first token is shadow fails here.
			"error: shadow status":           {argv: []string{"shadow", "status"}, want: false},
			"error: shadow help":             {argv: []string{"shadow", "help"}, want: false},
			"error: shadow --help":           {argv: []string{"shadow", "--help"}, want: false},
			"error: shadow preflight twice":  {argv: []string{"shadow", "preflight", "preflight"}, want: false},
			"error: shadow preflight --json": {argv: []string{"shadow", "preflight", "--json"}, want: false},
			"error: shadow report with path": {argv: []string{"shadow", "report", "/s/logs/soak.jsonl"}, want: false},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				argv := slices.Clone(tt.argv)
				var got bool
				if !spolTRecover(t, func() { got = Allowed(argv) }) {
					return
				}
				if got != tt.want {
					t.Errorf("Allowed(%q) = %t, want %t", tt.argv, got, tt.want)
				}
				// The gate inspects the vector; one that rewrites it in place would change what
				// the root later dispatches.
				if diff := gocmp.Diff(tt.argv, argv); diff != "" {
					t.Errorf("Allowed changed its argument (-before +after):\n%s", diff)
				}
			})
		}
	})

	t.Run("Enabled", func(t *testing.T) {
		tests := map[string]struct {
			env       map[string]string
			otherKeys string // When nonempty, every key absent from env is defined with this value.
			nilLookup bool
			want      bool
		}{
			// Only the exact string 1 of the one variable selects shadow mode.
			"success: exactly 1": {env: map[string]string{"MODEL_GATEWAY_SHADOW": "1"}, want: true},
			// A nil lookup is an empty environment. An implementation that calls it panics.
			"success: nil lookup is ordinary mode": {nilLookup: true, want: false},
			"success: unset is ordinary mode":      {env: map[string]string{}, want: false},
			"success: empty is ordinary mode":      {env: map[string]string{"MODEL_GATEWAY_SHADOW": ""}, want: false},
			"success: 0 is ordinary mode":          {env: map[string]string{"MODEL_GATEWAY_SHADOW": "0"}, want: false},
			// Truthy spellings. An implementation that parses a boolean or folds case admits these.
			"success: true is ordinary mode": {env: map[string]string{"MODEL_GATEWAY_SHADOW": "true"}, want: false},
			"success: TRUE is ordinary mode": {env: map[string]string{"MODEL_GATEWAY_SHADOW": "TRUE"}, want: false},
			"success: yes is ordinary mode":  {env: map[string]string{"MODEL_GATEWAY_SHADOW": "yes"}, want: false},
			"success: on is ordinary mode":   {env: map[string]string{"MODEL_GATEWAY_SHADOW": "on"}, want: false},
			// Numeric and padded spellings. An implementation that trims or parses a number admits these.
			"success: leading space is ordinary mode":  {env: map[string]string{"MODEL_GATEWAY_SHADOW": " 1"}, want: false},
			"success: trailing space is ordinary mode": {env: map[string]string{"MODEL_GATEWAY_SHADOW": "1 "}, want: false},
			"success: newline is ordinary mode":        {env: map[string]string{"MODEL_GATEWAY_SHADOW": "1\n"}, want: false},
			"success: 01 is ordinary mode":             {env: map[string]string{"MODEL_GATEWAY_SHADOW": "01"}, want: false},
			"success: +1 is ordinary mode":             {env: map[string]string{"MODEL_GATEWAY_SHADOW": "+1"}, want: false},
			"success: 1.0 is ordinary mode":            {env: map[string]string{"MODEL_GATEWAY_SHADOW": "1.0"}, want: false},
			// Every other variable says 1. An implementation that reads another name fails here.
			"success: only the shadow variable counts": {
				env: map[string]string{"MODEL_GATEWAY_SHADOW": "0"}, otherKeys: "1", want: false,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				var lookup config.LookupEnv
				if !tt.nilLookup {
					lookup = func(key string) (string, bool) {
						if value, ok := tt.env[key]; ok {
							return value, true
						}
						if tt.otherKeys != "" {
							return tt.otherKeys, true
						}
						return "", false
					}
				}
				var got bool
				if !spolTRecover(t, func() { got = Enabled(lookup) }) {
					return
				}
				if got != tt.want {
					t.Errorf("Enabled(%q) = %t, want %t", tt.env, got, tt.want)
				}
			})
		}
	})
}

// spolTFixture is a private temporary tree. Every absolute path a row names is written as if the
// fixture's base were the file system root: /s is the shadow root, /n the normal root and /o an
// outside directory, all three created private; any other path below base is prospective. No row
// depends on what exists at the host's root.
type spolTFixture struct {
	base, root, def, out string
}

func spolTNewFixture(t *testing.T) spolTFixture {
	t.Helper()
	// Resolve the temporary directory once, so that a declared path and its resolved form are
	// the same bytes unless a row adds a link on purpose.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := spolTFixture{
		base: base,
		root: filepath.Join(base, "s"),
		def:  filepath.Join(base, "n"),
		out:  filepath.Join(base, "o"),
	}
	for _, dir := range []string{fx.root, fx.def, fx.out} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

// relocate moves every absolute path token of s below the fixture's base and returns the result.
// A path token starts with a slash at the start of s, after a space or a newline, or after
// "unix:"; the slashes of a URL are not tokens. Each of tokens must occur in s as a complete path
// token, or the row fails: a literal copied from the contract cannot keep a host path unnoticed.
func (fx spolTFixture) relocate(t *testing.T, s string, tokens ...string) string {
	t.Helper()
	for _, token := range tokens {
		if !spolTHasToken(s, token) {
			t.Fatalf("literal %q does not contain the path token %q", s, token)
		}
	}
	var b strings.Builder
	for i := range len(s) {
		if s[i] == '/' && spolTPathStart(s, i) {
			b.WriteString(fx.base)
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func spolTPathStart(s string, i int) bool {
	return i == 0 || s[i-1] == ' ' || s[i-1] == '\n' || strings.HasSuffix(s[:i], "unix:")
}

// spolTHasToken reports whether token occurs in s as a complete path token: it starts a path token
// and is followed by the end of s, a newline, a space, or the full stop that ends a refusal line.
// A token that is only a prefix of a longer path, such as /s within /s/logs, does not count.
func spolTHasToken(s, token string) bool {
	for i := 0; i+len(token) <= len(s); i++ {
		if s[i] != '/' || !spolTPathStart(s, i) || !strings.HasPrefix(s[i:], token) {
			continue
		}
		rest := s[i+len(token):]
		if rest == "" || rest[0] == '\n' || rest[0] == ' ' || rest == "." || strings.HasPrefix(rest, ".\n") {
			return true
		}
	}
	return false
}

func (fx spolTFixture) relocateAll(t *testing.T, list []string) []string {
	t.Helper()
	if list == nil {
		return nil
	}
	// Keep the capacity, so that a row can hand Assemble a slice with spare room.
	out := make([]string, len(list), cap(list))
	for i, s := range list {
		out[i] = fx.relocate(t, s)
	}
	return out
}

func (fx spolTFixture) resources(t *testing.T, r Resources) Resources {
	t.Helper()
	return Resources{
		StateRoot:    fx.relocate(t, r.StateRoot),
		DefaultRoot:  fx.relocate(t, r.DefaultRoot),
		ProxyDial:    fx.relocate(t, r.ProxyDial),
		Writes:       fx.relocateAll(t, r.Writes),
		Binds:        fx.relocateAll(t, r.Binds),
		Dials:        fx.relocateAll(t, r.Dials),
		Upstreams:    fx.relocateAll(t, r.Upstreams),
		DefaultBinds: fx.relocateAll(t, r.DefaultBinds),
	}
}

func spolTMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func spolTFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func spolTSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// spolTEntry is what a snapshot keeps of one fixture entry or declared path.
type spolTEntry struct {
	info    fs.FileInfo // Nil for a declared path that does not exist.
	content string      // The bytes of a regular file or the target of a link.
}

// spolTSnapshot records every entry of the fixture tree, and every absolute path of inventory, with
// its identity, mode, size, modification time and, for a file or a link, its bytes or target.
// Comparing two snapshots detects the differences between those two observations: an entry that
// appeared, disappeared, or differs in one of the recorded properties. It cannot see a change that
// was undone before the second observation, a read, or a socket or network call.
func spolTSnapshot(t *testing.T, fx spolTFixture, inventory Resources) map[string]spolTEntry {
	t.Helper()
	snap := map[string]spolTEntry{}
	err := filepath.WalkDir(fx.base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		snap[path] = spolTEntry{info: info, content: spolTContent(t, path, info)}
		return nil
	})
	if err != nil {
		t.Errorf("snapshot of the fixture: %v", err)
	}
	for _, path := range append([]string{inventory.StateRoot, inventory.DefaultRoot}, inventory.Writes...) {
		if !filepath.IsAbs(path) {
			continue
		}
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			snap["declared "+path] = spolTEntry{info: info, content: spolTContent(t, path, info)}
		case errors.Is(err, fs.ErrNotExist):
			snap["declared "+path] = spolTEntry{}
		default:
			// Neither present nor absent: a snapshot that guessed would hide a change.
			t.Errorf("snapshot of declared path %q: %v", path, err)
		}
	}
	return snap
}

func spolTContent(t *testing.T, path string, info fs.FileInfo) string {
	t.Helper()
	var content string
	var err error
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		content, err = os.Readlink(path)
	case info.Mode().IsRegular():
		var data []byte
		data, err = os.ReadFile(path)
		content = string(data)
	}
	if err != nil {
		t.Errorf("snapshot of %q: %v", path, err)
	}
	return content
}

// spolTCompare fails the row for every entry that appeared, disappeared or changed between two snapshots.
func spolTCompare(t *testing.T, before, after map[string]spolTEntry) {
	t.Helper()
	for path, b := range before {
		a, ok := after[path]
		switch {
		case !ok:
			t.Errorf("Preflight removed %q", path)
		case (b.info == nil) != (a.info == nil):
			t.Errorf("Preflight changed whether %q exists", path)
		case b.info == nil:
		case !os.SameFile(b.info, a.info) || b.info.Mode() != a.info.Mode() || b.info.Size() != a.info.Size() ||
			!b.info.ModTime().Equal(a.info.ModTime()) || b.content != a.content:
			t.Errorf("Preflight changed %q: mode %v to %v, size %d to %d, modified %v to %v, same file %t",
				path, b.info.Mode(), a.info.Mode(), b.info.Size(), a.info.Size(), b.info.ModTime(), a.info.ModTime(), os.SameFile(b.info, a.info))
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("Preflight created %q", path)
		}
	}
}

// spolTRefused is the refusal line of the preflight for one resource.
func spolTRefused(resource string) string {
	return "model-gateway: shadow preflight refused: " + resource + ".\n"
}

// spolTListing is the preflight listing of the valid temporary inventory used by several rows.
const spolTListing = "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n"

func TestPreflight(t *testing.T) {
	t.Run("Preflight", func(t *testing.T) {
		tests := map[string]struct {
			res        Resources
			setup      func(t *testing.T, fx spolTFixture)
			wantStdout string
			// altStdout lists further accepted listings, for writes or Unix dials whose declared and
			// resolved spellings differ, where the contract fixes neither for a listing line.
			altStdout []string
			// wantStderr is the refusal line. Where the contract fixes the selected resource but
			// leaves its displayed name open between the declared and the resolved spelling, it
			// lists both.
			wantStderr []string
			wantCode   int
			// wantTokens are path tokens that every nonempty expected literal must contain; the
			// contract's literal oracles use them, so their bytes change only by relocation.
			wantTokens []string
			// peer, when set, is a path compared with the normal root by file identity in the
			// fixture: if both name the same directory, peerStderr lists the accepted refusals.
			peer       string
			peerStderr []string
			// chdir makes the fixture's base the working directory during the call.
			chdir bool
		}{
			// The package oracle, byte for byte. Catches an unsorted or undeduplicated listing, a
			// wrong order of the four kinds, and a listing that omits the proxy or upstream dial.
			"success: oracle inventory is listed sorted and deduplicated": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs", "/s/logs"},
					Binds:     []string{"127.0.0.1:21002", "127.0.0.1:21001"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStdout: "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n",
				wantTokens: []string{"/s", "/s/logs"},
			},
			// The oracle's refusal: writes under the normal root outrank a forbidden bind, and the
			// first write in byte order is named. Catches a check that stops at the first input
			// element, a check that ignores the normal root, and binds checked before writes.
			"error: oracle write under the normal root outranks a forbidden bind": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs", "/s/logs", "/n/z", "/n/a"},
					Binds:     []string{"127.0.0.1:21002", "127.0.0.1:21001", "127.0.0.1:80"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write /n/a")},
				wantCode:   1,
				wantTokens: []string{"/n/a"},
			},
			// The oracle's credential-bearing upstream. Catches an implementation that echoes the
			// input, and with it the secret, in the diagnostic.
			"error: oracle credential-bearing upstream renders invalid": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"user:hunter2@api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial <invalid>")},
				wantCode:   1,
			},

			// Order of the kinds and order within a kind.
			// Catches an implementation that checks writes before the state root.
			"error: state outranks write": {
				res: Resources{
					StateRoot: "/n/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/n/a"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("state /n/s")},
				wantCode:   1,
			},
			// Catches an implementation that checks dials before binds.
			"error: bind outranks dial": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:18764", "127.0.0.1:21002"},
					Upstreams: []string{"user:hunter2@api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:18764")},
				wantCode:   1,
			},
			// Raw bytes order upper case before lower case. Catches a case-folding or locale sort.
			"error: writes are ordered by raw bytes": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/n/a", "/n/B"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write /n/B")},
				wantCode:   1,
			},
			// The undisplayable write sorts first by its raw bytes, although its rendering sorts
			// after /n/a. Catches an implementation that sorts the rendered names.
			"error: an undisplayable write sorts by its raw bytes": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/n/a", "/a\x01b"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write <invalid>")},
				wantCode:   1,
			},
			// Managed, proxy and upstream dials form one kind. The upstream sorts before the managed
			// dial. Catches an implementation that checks managed dials as a kind of their own first.
			"error: managed and upstream dials share one byte order": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:       []string{"/s/logs"},
					Binds:        []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:        []string{"127.0.0.1:19500"},
					Upstreams:    []string{"127.0.0.1:19000"},
					DefaultBinds: []string{"127.0.0.1:19000", "127.0.0.1:19500"},
				},
				wantStderr: []string{spolTRefused("dial 127.0.0.1:19000")},
				wantCode:   1,
			},

			// The state root against the normal root.
			// Catches a check for "below" only.
			"error: state root equal to the normal root": {
				res: Resources{
					StateRoot: "/n", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/n/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("state /n")},
				wantCode:   1,
			},
			// Catches a check that refuses a root below the normal root but not one above it.
			"error: state root ancestral to the normal root": {
				res: Resources{
					StateRoot: "/x", DefaultRoot: "/x/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/x/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("state /x")},
				wantCode:   1,
			},
			// Catches a check that refuses only an equal root.
			"error: state root below the normal root": {
				res: Resources{
					StateRoot: "/n/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/n/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("state /n/s")},
				wantCode:   1,
			},
			// A sibling whose name starts with the normal root's name is not below it. Catches a
			// string-prefix containment test.
			"success: sibling sharing the normal root's name prefix": {
				res: Resources{
					StateRoot: "/nx", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/nx/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStdout: "state /nx\nwrite /nx/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n",
			},
			// A missing root is a missing value, which renders invalid. Catches an implementation
			// that treats the empty root as the current directory.
			"error: empty state root": {
				res: Resources{
					DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("state <invalid>")},
				wantCode:   1,
			},
			// Catches an implementation that resolves a relative root against the working directory, or
			// that hides the displayable name it was given.
			"error: relative state root": {
				res: Resources{
					StateRoot: "s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				// In the fixture's base, s is the private /s directory, so an implementation that
				// creates its relative destination changes the fixture and fails the snapshot.
				chdir:      true,
				wantStderr: []string{spolTRefused("state s")},
				wantCode:   1,
			},
			// Catches an implementation that prints a control character raw.
			"error: state root with a newline": {
				res: Resources{
					StateRoot: "/s\nstate /x", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("state <invalid>")},
				wantCode:   1,
			},

			// Writes.
			// A relocated request-log override outside the root. Catches a check against the normal
			// root only.
			"error: write outside both roots": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs", "/o/request.log"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write /o/request.log")},
				wantCode:   1,
			},
			// Catches a string-prefix containment test for writes.
			"error: write sharing the root's name prefix": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs", "/sx/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write /sx/logs")},
				wantCode:   1,
			},
			// Catches a containment test on the unresolved spelling.
			"error: dot-dot write leaves the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/../n/a"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write /s/../n/a"), spolTRefused("write /n/a")},
				wantCode:   1,
			},
			// The root itself may be a write when it will be created; the dry run creates nothing.
			// Catches an implementation that requires an existing root or creates it.
			"success: root that does not exist yet": {
				res: Resources{
					StateRoot: "/fresh", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/fresh/logs", "/fresh"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStdout: "state /fresh\nwrite /fresh\nwrite /fresh/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n",
			},
			// Existing and prospective files and parents are listed individually, and nothing is
			// created. Catches a catch-all entry and a dry run that prepares directories.
			"success: existing and prospective destinations are listed individually": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/state.json", "/s/logs/a.log", "/s/logs", "/s/logs/a.log"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTFile(t, filepath.Join(fx.root, "state.json"))
				},
				wantStdout: "state /s\nwrite /s/logs\nwrite /s/logs/a.log\nwrite /s/state.json\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n",
			},
			// Catches an implementation that prints a control character raw.
			"error: write with a newline": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/lo\ngs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write <invalid>")},
				wantCode:   1,
			},
			// Catches an implementation that prints invalid UTF-8 raw.
			"error: write with invalid UTF-8": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/\xff"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write <invalid>")},
				wantCode:   1,
			},
			// An existing parent symlink inside the root points into the normal root. Catches a
			// lexical containment test.
			"error: existing parent symlink into the normal root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs/x.log"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTMkdir(t, filepath.Join(fx.def, "logs"))
					spolTSymlink(t, filepath.Join(fx.def, "logs"), filepath.Join(fx.root, "logs"))
				},
				wantStderr: []string{spolTRefused("write /s/logs/x.log"), spolTRefused("write /n/logs/x.log")},
				wantCode:   1,
			},
			// A prospective write below a symlinked parent with a missing suffix. Catches a resolver
			// that gives up on a missing path and falls back to the declared spelling.
			"error: prospective write below a symlinked parent": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/cache/a/b/c"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTSymlink(t, fx.out, filepath.Join(fx.root, "cache"))
				},
				wantStderr: []string{spolTRefused("write /s/cache/a/b/c"), spolTRefused("write /o/a/b/c")},
				wantCode:   1,
			},
			// Catches a resolver that resolves parents but not the leaf.
			"error: leaf symlink to a file outside the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/key"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTFile(t, filepath.Join(fx.out, "key"))
					spolTSymlink(t, filepath.Join(fx.out, "key"), filepath.Join(fx.root, "key"))
				},
				wantStderr: []string{spolTRefused("write /s/key"), spolTRefused("write /o/key")},
				wantCode:   1,
			},
			// A dangling leaf symlink would create its target outside the root. Catches a resolver
			// that treats a symlink to a missing target as a missing file.
			"error: dangling leaf symlink pointing outside the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/dangle"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTSymlink(t, filepath.Join(fx.out, "missing"), filepath.Join(fx.root, "dangle"))
				},
				wantStderr: []string{spolTRefused("write /s/dangle"), spolTRefused("write /o/missing")},
				wantCode:   1,
			},
			// A second hard link inside the root aliases a writable file outside it. Catches an
			// implementation that resolves symlinks only.
			"error: hard link to a writable file outside the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/alias"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTFile(t, filepath.Join(fx.out, "shared"))
					if err := os.Link(filepath.Join(fx.out, "shared"), filepath.Join(fx.root, "alias")); err != nil {
						t.Fatal(err)
					}
				},
				wantStderr: []string{spolTRefused("write /s/alias")},
				wantCode:   1,
			},
			// The state line names the resolved root. Catches an implementation that prints the
			// declared root, or that does not resolve it before checking writes.
			"success: symlinked state root prints its resolved path": {
				res: Resources{
					StateRoot: "/link", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTSymlink(t, fx.root, filepath.Join(fx.base, "link"))
				},
				wantStdout: spolTListing,
			},
			// Catches a root check on the declared spelling only.
			"error: symlinked state root resolving into the normal root": {
				res: Resources{
					StateRoot: "/sneak", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/sneak/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTMkdir(t, filepath.Join(fx.def, "shadow"))
					spolTSymlink(t, filepath.Join(fx.def, "shadow"), filepath.Join(fx.base, "sneak"))
				},
				wantStderr: []string{spolTRefused("state /sneak"), spolTRefused("state /n/shadow")},
				wantCode:   1,
			},

			// Binds.
			// Catches an implementation that deduplicates binds as it does writes.
			"error: duplicate bind": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21001"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:21001")},
				wantCode:   1,
			},
			// Catches an implementation that accepts a single listener.
			"error: one bind missing": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind <missing>")},
				wantCode:   1,
			},
			// Catches an implementation that validates only the binds it is given.
			"error: no binds": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind <missing>")},
				wantCode:   1,
			},
			// Each fixed exclusion on its own. Catches a list that omits one of them.
			"error: bind on port 80": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:80", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:80")},
				wantCode:   1,
			},
			"error: bind on the default public port": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:18764", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:18764")},
				wantCode:   1,
			},
			// The configured proxy is elsewhere, so only the fixed exclusion refuses 18765.
			"error: bind on the default proxy port with a relocated proxy": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:21999",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:18765"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:18765")},
				wantCode:   1,
			},
			// Catches an implementation that knows only the fixed ports.
			"error: public bind on the configured proxy port": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:21999",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21999", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:21999")},
				wantCode:   1,
			},
			// Catches an implementation that allows an ephemeral port.
			"error: bind on port zero": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:0"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:0")},
				wantCode:   1,
			},
			// Catches an implementation that checks only the port, or that hides the displayable name.
			"error: wildcard bind": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"0.0.0.0:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 0.0.0.0:21001")},
				wantCode:   1,
			},
			// Catches an implementation that accepts any loopback name rather than the literal address, or
			// that hides the displayable name.
			"error: host name bind": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "localhost:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind localhost:21002")},
				wantCode:   1,
			},
			// Catches an implementation that ignores the normal instance's actual addresses.
			"error: bind used by the normal instance": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:       []string{"/s/logs"},
					Binds:        []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams:    []string{"api.anthropic.com:443"},
					DefaultBinds: []string{"127.0.0.1:21001"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:21001")},
				wantCode:   1,
			},
			// Catches an implementation that echoes userinfo in a bind.
			"error: bind with userinfo": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"u@127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind <invalid>")},
				wantCode:   1,
			},

			// Dials.
			// Catches an implementation that does not require the proxy dial.
			"error: missing proxy dial": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial <missing>")},
				wantCode:   1,
			},
			// Catches an implementation that accepts any proxy address.
			"error: proxy dial off loopback": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "10.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial 10.0.0.1:18765")},
				wantCode:   1,
			},
			// Unexpected dialing of the normal instance's control endpoint.
			"error: managed dial to the default control port": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"127.0.0.1:18764"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial 127.0.0.1:18764")},
				wantCode:   1,
			},
			// Catches an implementation that compares managed dials with the fixed ports only.
			"error: managed dial to a normal-instance address": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:       []string{"/s/logs"},
					Binds:        []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:        []string{"127.0.0.1:19000"},
					Upstreams:    []string{"api.anthropic.com:443"},
					DefaultBinds: []string{"127.0.0.1:19000"},
				},
				wantStderr: []string{spolTRefused("dial 127.0.0.1:19000")},
				wantCode:   1,
			},
			// Catches an implementation that validates managed dials but not routed upstreams.
			"error: upstream to the normal instance's local service": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"127.0.0.1:18764"},
				},
				wantStderr: []string{spolTRefused("dial 127.0.0.1:18764")},
				wantCode:   1,
			},
			// Catches an implementation that lists a URL instead of an authority.
			"error: upstream given as a URL": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"https://api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial <invalid>")},
				wantCode:   1,
			},
			// Catches an implementation that accepts an authority without an explicit port, or that
			// reports a supplied name as missing or hides it.
			"error: upstream without a port": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com"},
				},
				wantStderr: []string{spolTRefused("dial api.anthropic.com")},
				wantCode:   1,
			},
			// A configured external authority is not refused for not being the proxy. Catches an
			// implementation that allows only the proxy dial.
			"success: external routed authority besides the proxy": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"cli-chat-proxy.grok.com:443", "api.anthropic.com:443"},
				},
				wantStdout: "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\ndial cli-chat-proxy.grok.com:443\n",
			},
			// Catches an implementation that prints the host as configured or deduplicates before
			// canonicalising.
			"success: upstream host is lower-cased and deduplicated": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"API.Anthropic.COM:443", "api.anthropic.com:443"},
				},
				wantStdout: "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n",
			},
			// A provider reached through the proxy adds only the proxy authority. Catches a listing
			// that deduplicates within each source but not across the dial kind.
			"success: proxy authority listed once when it is also an upstream": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"127.0.0.1:18765"},
				},
				wantStdout: "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\n",
			},
			// The supervisor dials its own worker. Catches an implementation that refuses a dial to a
			// shadow bind or omits managed dials from the listing.
			"success: managed dial to the shadow's own worker": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStdout: "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial 127.0.0.1:21002\ndial api.anthropic.com:443\n",
			},

			// The default local backend and an isolated one.
			// The default local backend is a shared server of the normal instance even when the
			// proxy is elsewhere. Catches an implementation that admits it.
			"error: upstream to the default local backend": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:21999",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"127.0.0.1:1234"},
				},
				wantStderr: []string{spolTRefused("dial 127.0.0.1:1234")},
				wantCode:   1,
			},
			// An explicitly isolated nondefault local backend is listed beside the proxy. Catches an
			// implementation that refuses every local upstream.
			"success: isolated nondefault local backend beside the proxy": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"127.0.0.1:21234"},
				},
				wantStdout: "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial 127.0.0.1:21234\n",
			},

			// Unix endpoints. No socket is opened.
			// The worker socket under the root, with its path among the writes. Catches an
			// implementation that refuses every Unix endpoint or leaves it out of the listing.
			"success: owned Unix worker endpoint": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs", "/s/worker.sock"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"unix:/s/worker.sock"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStdout: "state /s\nwrite /s/logs\nwrite /s/worker.sock\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\ndial unix:/s/worker.sock\n",
			},
			// The socket's parent /s/sock links to /s/run, a directory inside the root. Catches an
			// implementation that refuses every Unix dial with a symlinked parent.
			"success: Unix worker endpoint through a symlinked parent inside the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs", "/s/sock/worker.sock"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"unix:/s/sock/worker.sock"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTMkdir(t, filepath.Join(fx.root, "run"))
					spolTSymlink(t, filepath.Join(fx.root, "run"), filepath.Join(fx.root, "sock"))
				},
				wantStdout: "state /s\nwrite /s/logs\nwrite /s/sock/worker.sock\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\ndial unix:/s/sock/worker.sock\n",
				// Each of the two socket entries may be declared or resolved on its own.
				altStdout: []string{
					"state /s\nwrite /s/logs\nwrite /s/run/worker.sock\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\ndial unix:/s/run/worker.sock\n",
					"state /s\nwrite /s/logs\nwrite /s/run/worker.sock\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\ndial unix:/s/sock/worker.sock\n",
					"state /s\nwrite /s/logs\nwrite /s/sock/worker.sock\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\ndial unix:/s/run/worker.sock\n",
				},
			},
			// Catches an implementation that ignores Unix endpoints.
			"error: Unix dial outside the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"unix:/o/worker.sock"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial unix:/o/worker.sock")},
				wantCode:   1,
			},
			// The normal instance's own socket. Catches an implementation that checks Unix dials
			// against the state root only lexically or not at all.
			"error: Unix dial to the normal instance's socket": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"unix:/n/worker.sock"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial unix:/n/worker.sock")},
				wantCode:   1,
			},
			// The socket file outside the root is a write, and writes outrank dials. Catches an
			// implementation that reports the Unix dial first.
			"error: outside socket write outranks its dial": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs", "/o/worker.sock"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"unix:/o/worker.sock"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("write /o/worker.sock")},
				wantCode:   1,
			},

			// Resolution against lexical cleaning, safe links and an aliased normal root.
			// /s/up resolves to /o/deep/dir, so /s/up/../x is /o/deep/x on disk although cleaning
			// the spelling gives /s/x. Catches an implementation that cleans before it resolves.
			"error: dot-dot after a symlink is resolved on disk": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/up/../x"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTMkdir(t, filepath.Join(fx.out, "deep", "dir"))
					spolTSymlink(t, filepath.Join(fx.out, "deep", "dir"), filepath.Join(fx.root, "up"))
				},
				wantStderr: []string{spolTRefused("write /s/up/../x"), spolTRefused("write /o/deep/x")},
				wantCode:   1,
			},
			// A linked parent that stays inside the root is safe. Catches an implementation that
			// refuses every linked write parent.
			"success: symlinked write parent inside the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs/a.log"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTMkdir(t, filepath.Join(fx.root, "real"))
					spolTSymlink(t, filepath.Join(fx.root, "real"), filepath.Join(fx.root, "logs"))
				},
				wantStdout: "state /s\nwrite /s/logs/a.log\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n",
				altStdout:  []string{"state /s\nwrite /s/real/a.log\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n"},
			},
			// The normal root is named through a link to the fixture base, which contains the state
			// root. Catches an implementation that resolves only the state root.
			"error: normal root named through a link to an ancestor of the state root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/alias", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTSymlink(t, fx.base, filepath.Join(fx.base, "alias"))
				},
				wantStderr: []string{spolTRefused("state /s")},
				wantCode:   1,
			},
			// Two invalid writes whose declared order and resolved order disagree: /s/a/x is first
			// by its bytes, /s/b/x by its resolved path /o/a/x. Catches an implementation that
			// selects among resolved paths.
			"error: selection follows the declared bytes, not the resolved paths": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/b/x", "/s/a/x"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTMkdir(t, filepath.Join(fx.out, "z"))
					spolTMkdir(t, filepath.Join(fx.out, "a"))
					spolTSymlink(t, filepath.Join(fx.out, "z"), filepath.Join(fx.root, "a"))
					spolTSymlink(t, filepath.Join(fx.out, "a"), filepath.Join(fx.root, "b"))
				},
				wantStderr: []string{spolTRefused("write /s/a/x"), spolTRefused("write /o/z/x")},
				wantCode:   1,
			},
			// Catches a containment test that compares the normal root's spelling as given.
			"error: normal root spelled with a trailing separator": {
				res: Resources{
					StateRoot: "/n/s", DefaultRoot: "/n/", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/n/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("state /n/s")},
				wantCode:   1,
			},
			// A peer that differs from the normal root only in case. Where the fixture's file system
			// makes both names one directory, the root is refused; where they are distinct
			// directories, it is valid. Catches an implementation that folds case on its own, or
			// that compares spellings where the file system does not.
			"success: case-only peer of the normal root follows file identity": {
				res: Resources{
					StateRoot: "/N", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/N/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTMkdir(t, filepath.Join(fx.base, "N"))
				},
				wantStdout: "state /N\nwrite /N/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:18765\ndial api.anthropic.com:443\n",
				peer:       "/N",
				peerStderr: []string{spolTRefused("state /N"), spolTRefused("state /n")},
			},

			// Address classes and names.
			// Catches an implementation that accepts any numeric address.
			"error: bind off loopback": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"10.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 10.0.0.1:21001")},
				wantCode:   1,
			},
			// Only IPv4 loopback is allowed. Catches an implementation that accepts any loopback.
			"error: IPv6 loopback bind": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "[::1]:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind [::1]:21002")},
				wantCode:   1,
			},
			// Catches an implementation that checks digits but not the range.
			"error: bind port out of range": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:65536"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:65536")},
				wantCode:   1,
			},
			// Catches an implementation that parses the port with a base prefix.
			"error: bind port not decimal": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:0x5209", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("bind 127.0.0.1:0x5209")},
				wantCode:   1,
			},
			// Catches an implementation that renders managed dials raw.
			"error: managed dial with userinfo": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"u:hunter2@127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial <invalid>")},
				wantCode:   1,
			},
			// Catches an implementation that renders the proxy dial raw.
			"error: proxy dial given as a URL": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "http://127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{spolTRefused("dial <invalid>")},
				wantCode:   1,
			},
			// Catches an implementation that requires the default proxy port.
			"success: relocated proxy": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:21999",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStdout: "state /s\nwrite /s/logs\nbind 127.0.0.1:21001\nbind 127.0.0.1:21002\ndial 127.0.0.1:21999\ndial api.anthropic.com:443\n",
			},

			// Selection order within binds and dials.
			// By raw bytes 127.0.0.1:18764 sorts before 127.0.0.1:80. Catches an implementation that
			// checks binds in declared order or by port number.
			"error: binds are selected in byte order": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:80", "127.0.0.1:18764"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantStderr: []string{"model-gateway: shadow preflight refused: bind 127.0.0.1:18764.\n"},
				wantCode:   1,
			},
			// The converse of the shared-order row: the managed dial sorts first. Catches an
			// implementation that always checks upstreams first.
			"error: a managed dial that sorts first is selected before an upstream": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:       []string{"/s/logs"},
					Binds:        []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:        []string{"127.0.0.1:19000"},
					Upstreams:    []string{"127.0.0.1:19500"},
					DefaultBinds: []string{"127.0.0.1:19000", "127.0.0.1:19500"},
				},
				wantStderr: []string{spolTRefused("dial 127.0.0.1:19000")},
				wantCode:   1,
			},
			// An invalid proxy whose name sorts after another invalid dial. Catches an implementation
			// that checks the proxy first.
			"error: an invalid proxy that sorts last is not selected first": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "192.168.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"127.0.0.1:18764"},
				},
				wantStderr: []string{spolTRefused("dial 127.0.0.1:18764")},
				wantCode:   1,
			},
			// The converse: the invalid proxy sorts first by its raw bytes. Catches an implementation
			// that always checks the proxy last.
			"error: an invalid proxy that sorts first is selected": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "10.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Upstreams: []string{"127.0.0.1:18764"},
				},
				wantStderr: []string{spolTRefused("dial 10.0.0.1:18765")},
				wantCode:   1,
			},
			// The socket's parent /s/sock links to /o, so the socket is outside the root although its
			// spelling is inside. Catches an implementation that checks Unix dials by string prefix.
			"error: Unix dial through a symlinked parent outside the root": {
				res: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/logs"},
					Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:     []string{"unix:/s/sock/worker.sock"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				setup: func(t *testing.T, fx spolTFixture) {
					spolTSymlink(t, fx.out, filepath.Join(fx.root, "sock"))
				},
				wantStderr: []string{spolTRefused("dial unix:/s/sock/worker.sock"), spolTRefused("dial unix:/o/worker.sock")},
				wantCode:   1,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				fx := spolTNewFixture(t)
				if tt.setup != nil {
					tt.setup(t, fx)
				}
				res := fx.resources(t, tt.res)
				want := func(s string) string {
					if s == "" {
						return ""
					}
					return fx.relocate(t, s, tt.wantTokens...)
				}
				stdouts := []string{want(tt.wantStdout)}
				for _, s := range tt.altStdout {
					stdouts = append(stdouts, want(s))
				}
				stderrs := []string{""}
				if tt.wantStderr != nil {
					stderrs = nil
					for _, s := range tt.wantStderr {
						stderrs = append(stderrs, want(s))
					}
				}
				code := tt.wantCode
				if tt.peer != "" {
					peer, err := os.Stat(fx.relocate(t, tt.peer))
					if err != nil {
						t.Fatal(err)
					}
					normal, err := os.Stat(fx.def)
					if err != nil {
						t.Fatal(err)
					}
					if os.SameFile(peer, normal) {
						stdouts, stderrs, code = []string{""}, nil, 1
						for _, s := range tt.peerStderr {
							stderrs = append(stderrs, want(s))
						}
					}
				}
				// Compare the tree on cleanup, so that a panic of the code under test does not skip it.
				// Both snapshots use an inventory cloned before the call: a Preflight that rewrites its
				// input slices in place must not change which paths the second snapshot looks at.
				inventory := Resources{StateRoot: res.StateRoot, DefaultRoot: res.DefaultRoot, Writes: slices.Clone(res.Writes)}
				before := spolTSnapshot(t, fx, inventory)
				t.Cleanup(func() { spolTCompare(t, before, spolTSnapshot(t, fx, inventory)) })
				if tt.chdir {
					// Relative inputs then resolve inside the fixture, never in the package directory.
					t.Chdir(fx.base)
				}
				var stdout, stderr bytes.Buffer
				var got int
				if !spolTRecover(t, func() { got = Preflight(res, &stdout, &stderr) }) {
					return
				}
				if got != code {
					t.Errorf("Preflight exit code = %d, want %d", got, code)
				}
				if !slices.Contains(stdouts, stdout.String()) {
					t.Errorf("Preflight stdout = %q, want one of %q", stdout.String(), stdouts)
				}
				if !slices.Contains(stderrs, stderr.String()) {
					t.Errorf("Preflight stderr = %q, want one of %q", stderr.String(), stderrs)
				}
			})
		}
	})

	t.Run("Assemble", func(t *testing.T) {
		tests := map[string]struct {
			// base builds a fresh base for each row, so a faulty mutator cannot spill into another row.
			base      func() Resources
			parts     func(t *testing.T, fx spolTFixture, calls *[]string) []func(*Resources) error
			want      Resources
			wantErr   string
			wantCalls []string
		}{
			// Catches an Assemble that requires contributions, changes an assembled base, or returns
			// the base's own slices.
			"success: no parts validates an already assembled base": {
				base: func() Resources {
					return Resources{
						StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
						Writes:       []string{"/s/logs"},
						Binds:        []string{"127.0.0.1:21001", "127.0.0.1:21002"},
						Dials:        []string{"127.0.0.1:21002"},
						Upstreams:    []string{"api.anthropic.com:443"},
						DefaultBinds: []string{"127.0.0.1:19000"},
					}
				},
				parts: func(*testing.T, spolTFixture, *[]string) []func(*Resources) error { return nil },
				want: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:       []string{"/s/logs"},
					Binds:        []string{"127.0.0.1:21001", "127.0.0.1:21002"},
					Dials:        []string{"127.0.0.1:21002"},
					Upstreams:    []string{"api.anthropic.com:443"},
					DefaultBinds: []string{"127.0.0.1:19000"},
				},
			},
			// Catches an Assemble that skips validation without contributions, or returns the
			// partial inventory, or an error other than the refusal line without its newline.
			"error: no parts with an invalid base": {
				base: func() Resources {
					return Resources{
						StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
						Writes:    []string{"/s/logs", "/n/a"},
						Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
						Upstreams: []string{"api.anthropic.com:443"},
					}
				},
				parts:   func(*testing.T, spolTFixture, *[]string) []func(*Resources) error { return nil },
				wantErr: "model-gateway: shadow preflight refused: write /n/a.",
			},
			// Contributions follow the caller's order: the shim's public bind, then the worker's
			// bind, although the public port is the larger number. Catches an Assemble that sorts
			// the binds or runs the callbacks in another order.
			"success: public then worker bind in caller order": {
				base: spolTRoots,
				parts: func(t *testing.T, fx spolTFixture, calls *[]string) []func(*Resources) error {
					return []func(*Resources) error{
						func(r *Resources) error {
							*calls = append(*calls, "shim")
							r.Binds = append(r.Binds, "127.0.0.1:21002")
							r.Writes = append(r.Writes, fx.relocate(t, "/s/a"))
							return nil
						},
						func(r *Resources) error {
							*calls = append(*calls, "worker")
							r.Binds = append(r.Binds, "127.0.0.1:21001")
							r.Writes = append(r.Writes, fx.relocate(t, "/s/b"))
							return nil
						},
					}
				},
				want: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:    []string{"/s/a", "/s/b"},
					Binds:     []string{"127.0.0.1:21002", "127.0.0.1:21001"},
					Upstreams: []string{"api.anthropic.com:443"},
				},
				wantCalls: []string{"shim", "worker"},
			},
			// The invalid worker write outranks the invalid shim bind in either order. Catches an
			// Assemble that validates after each contribution and reports the first contributor.
			"error: invalid worker write outranks invalid shim bind, shim first": {
				base: spolTRoots,
				parts: func(t *testing.T, fx spolTFixture, calls *[]string) []func(*Resources) error {
					return []func(*Resources) error{spolTShim(t, fx, calls), spolTWorker(t, fx, calls)}
				},
				wantErr:   "model-gateway: shadow preflight refused: write /n/usage.",
				wantCalls: []string{"shim", "worker"},
			},
			"error: invalid worker write outranks invalid shim bind, worker first": {
				base: spolTRoots,
				parts: func(t *testing.T, fx spolTFixture, calls *[]string) []func(*Resources) error {
					return []func(*Resources) error{spolTWorker(t, fx, calls), spolTShim(t, fx, calls)}
				},
				wantErr:   "model-gateway: shadow preflight refused: write /n/usage.",
				wantCalls: []string{"worker", "shim"},
			},
			// Catches an Assemble that validates each contribution alone.
			"error: the same bind from both contributors": {
				base: func() Resources {
					return Resources{StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765", Writes: []string{"/s/logs"}}
				},
				parts: func(_ *testing.T, _ spolTFixture, calls *[]string) []func(*Resources) error {
					return []func(*Resources) error{
						func(r *Resources) error {
							*calls = append(*calls, "shim")
							r.Binds = append(r.Binds, "127.0.0.1:21001")
							return nil
						},
						func(r *Resources) error {
							*calls = append(*calls, "worker")
							r.Binds = append(r.Binds, "127.0.0.1:21001")
							return nil
						},
					}
				},
				wantErr:   "model-gateway: shadow preflight refused: bind 127.0.0.1:21001.",
				wantCalls: []string{"shim", "worker"},
			},
			// A failed contribution rejects the plan, the next contributor still runs exactly once,
			// and the callback's own error text is never shown. Catches an Assemble that stops at
			// the first error or wraps it.
			"error: failed contribution still runs the next one": {
				base: func() Resources {
					return Resources{
						StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
						Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
						Upstreams: []string{"api.anthropic.com:443"},
					}
				},
				parts: func(t *testing.T, fx spolTFixture, calls *[]string) []func(*Resources) error {
					return []func(*Resources) error{
						func(*Resources) error {
							*calls = append(*calls, "first")
							return errors.New("contributor detail hunter2")
						},
						func(r *Resources) error {
							*calls = append(*calls, "second")
							r.Writes = append(r.Writes, fx.relocate(t, "/s/logs"))
							return nil
						},
					}
				},
				wantErr:   "model-gateway: shadow preflight refused: state <invalid>.",
				wantCalls: []string{"first", "second"},
			},
			// A nil callback is a missing contributor; every other callback still runs once.
			// Catches an Assemble that skips nil silently or stops at it.
			"error: nil callback rejects the plan": {
				base: func() Resources {
					return Resources{
						StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
						Writes:    []string{"/s/logs"},
						Binds:     []string{"127.0.0.1:21001", "127.0.0.1:21002"},
						Upstreams: []string{"api.anthropic.com:443"},
					}
				},
				parts: func(_ *testing.T, _ spolTFixture, calls *[]string) []func(*Resources) error {
					return []func(*Resources) error{
						nil,
						func(*Resources) error {
							*calls = append(*calls, "second")
							return nil
						},
					}
				},
				wantErr:   "model-gateway: shadow preflight refused: state <invalid>.",
				wantCalls: []string{"second"},
			},
			// Contributors work on copies of all five base slices. A contributor that appends into
			// spare capacity or overwrites an element must not reach the caller's base. Catches an
			// Assemble that passes any of the base's slices through.
			"success: contributors cannot reach the caller's base": {
				base: func() Resources {
					return Resources{
						StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
						Writes:       append(make([]string, 0, 4), "/s/a"),
						Binds:        append(make([]string, 0, 4), "127.0.0.1:21001", "127.0.0.1:21002"),
						Dials:        append(make([]string, 0, 4), "127.0.0.1:21002"),
						Upstreams:    append(make([]string, 0, 4), "api.anthropic.com:443"),
						DefaultBinds: append(make([]string, 0, 4), "127.0.0.1:19000"),
					}
				},
				parts: func(t *testing.T, fx spolTFixture, calls *[]string) []func(*Resources) error {
					return []func(*Resources) error{
						func(r *Resources) error {
							*calls = append(*calls, "append")
							r.Writes = append(r.Writes, fx.relocate(t, "/s/b"))
							r.Dials = append(r.Dials, "127.0.0.1:21004")
							r.Upstreams = append(r.Upstreams, "api.x.ai:443")
							r.DefaultBinds = append(r.DefaultBinds, "127.0.0.1:19001")
							return nil
						},
						func(r *Resources) error {
							*calls = append(*calls, "overwrite")
							r.Writes[0] = fx.relocate(t, "/s/c")
							r.Binds[1] = "127.0.0.1:21003"
							// The first contributor's dial must still be there before it is dropped.
							if !slices.Contains(r.Dials, "127.0.0.1:21004") {
								t.Errorf("the second contributor does not see the first contributor's dial: %q", r.Dials)
							}
							// Drop the temporary dial; only the worker dial remains.
							r.Dials = r.Dials[:1]
							r.Dials[0] = "127.0.0.1:21003"
							r.Upstreams[0] = "cli-chat-proxy.grok.com:443"
							r.DefaultBinds[0] = "127.0.0.1:19002"
							return nil
						},
					}
				},
				want: Resources{
					StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765",
					Writes:       []string{"/s/c", "/s/b"},
					Binds:        []string{"127.0.0.1:21001", "127.0.0.1:21003"},
					Dials:        []string{"127.0.0.1:21003"},
					Upstreams:    []string{"cli-chat-proxy.grok.com:443", "api.x.ai:443"},
					DefaultBinds: []string{"127.0.0.1:19002", "127.0.0.1:19001"},
				},
				wantCalls: []string{"append", "overwrite"},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				fx := spolTNewFixture(t)
				base := fx.resources(t, tt.base())
				before := spolTBacking(base)
				var calls []string
				parts := tt.parts(t, fx, &calls)
				var got Resources
				var err error
				if !spolTRecover(t, func() { got, err = Assemble(base, parts...) }) {
					return
				}
				if tt.wantErr == "" {
					if err != nil {
						t.Errorf("Assemble error = %q, want nil", err)
					}
					want := fx.resources(t, tt.want)
					if diff := gocmp.Diff(spolTOrderFree(want), spolTOrderFree(got), cmpopts.EquateEmpty()); diff != "" {
						t.Errorf("Assemble resources (-want +got):\n%s", diff)
					}
					// The result must own its slices: the caller changing it must not reach the base.
					for _, list := range [][]string{got.Writes, got.Binds, got.Dials, got.Upstreams, got.DefaultBinds} {
						for i := range list {
							list[i] = "changed by the caller"
						}
					}
				} else {
					if wantErr := fx.relocate(t, tt.wantErr); err == nil || err.Error() != wantErr {
						t.Errorf("Assemble error = %v, want %q", err, wantErr)
					}
					// On failure the result is exactly the zero value, not a partial inventory.
					if diff := gocmp.Diff(Resources{}, got); diff != "" {
						t.Errorf("Assemble resources on failure (-want zero +got):\n%s", diff)
					}
				}
				if diff := gocmp.Diff(tt.wantCalls, calls, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("callbacks run (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(before, spolTBacking(base)); diff != "" {
					t.Errorf("Assemble let a contributor or the caller change the base (-before +after):\n%s", diff)
				}
			})
		}
	})
}

// spolTRoots is a base before contributions: the two roots, the proxy and the upstream.
func spolTRoots() Resources {
	return Resources{StateRoot: "/s", DefaultRoot: "/n", ProxyDial: "127.0.0.1:18765", Upstreams: []string{"api.anthropic.com:443"}}
}

// spolTShim declares a public bind on the default public port, which is invalid, and a valid log.
func spolTShim(t *testing.T, fx spolTFixture, calls *[]string) func(*Resources) error {
	return func(r *Resources) error {
		*calls = append(*calls, "shim")
		r.Binds = append(r.Binds, "127.0.0.1:18764")
		r.Writes = append(r.Writes, fx.relocate(t, "/s/supervisor.log"))
		return nil
	}
}

// spolTWorker declares a valid worker bind and a usage spool under the normal root, which is invalid.
func spolTWorker(t *testing.T, fx spolTFixture, calls *[]string) func(*Resources) error {
	return func(r *Resources) error {
		*calls = append(*calls, "worker")
		r.Binds = append(r.Binds, "127.0.0.1:21002")
		r.Writes = append(r.Writes, fx.relocate(t, "/n/usage"))
		return nil
	}
}

// spolTOrderFree sorts the lists whose order the contract leaves open. Binds keep their order:
// the public bind comes before the worker bind.
func spolTOrderFree(r Resources) Resources {
	for _, list := range []*[]string{&r.Writes, &r.Dials, &r.Upstreams, &r.DefaultBinds} {
		*list = slices.Sorted(slices.Values(*list))
	}
	return r
}

// spolTBacking copies every slice of r up to its capacity, so that a write into spare capacity of
// the caller's arrays shows up as a difference.
func spolTBacking(r Resources) [][]string {
	var out [][]string
	for _, list := range [][]string{r.Writes, r.Binds, r.Dials, r.Upstreams, r.DefaultBinds} {
		out = append(out, slices.Clone(list[:cap(list)]))
	}
	return out
}
