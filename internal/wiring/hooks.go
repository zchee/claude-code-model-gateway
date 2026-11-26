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

package wiring

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// readinessEvents are the hook events that run the readiness gate, in the source's order (lib/readiness-hooks.js:40).
func readinessEvents() [2]string { return [...]string{"SessionStart", "UserPromptSubmit"} }

const readinessArg = "readiness-gate"

// hookForms holds the two spellings of the gateway's readiness hook: the stable binary with separate arguments,
// which is written, and the Node launcher command string an earlier install wrote, which is still recognized.
type hookForms struct{ bin, legacy string }

func (s Store) hookForms() (hookForms, error) {
	home, err := s.Paths.GatewayHome()
	if err != nil {
		return hookForms{}, err
	}
	if !utf8.ValidString(home) {
		return hookForms{}, fmt.Errorf("gateway home %q is not valid UTF-8", home)
	}
	return hookForms{
		bin:    filepath.Join(home, "bin", "model-gateway"),
		legacy: `node "` + filepath.Join(home, "model-gateway.js") + `" ` + readinessArg,
	}, nil
}

// owned reports whether a hook is the gateway's readiness gate. The command-string form is owned only without an
// args member, and the binary form only with exactly the readiness-gate argument, so a hook carrying the
// user's own arguments is kept (lib/readiness-hooks.js:9-11).
func (h hookForms) owned(hook jsonx.WireValue) bool {
	kind, ok := member(hook, "type")
	if !ok || !isString(kind, "command") {
		return false
	}
	command, ok := member(hook, "command")
	if !ok || command.Kind != '"' {
		return false
	}
	args, hasArgs := member(hook, "args")
	switch wtf8(command.Text) {
	case h.legacy:
		return !hasArgs
	case h.bin:
		return hasArgs && args.Kind == '[' && len(args.Elements) == 1 && isString(args.Elements[0], readinessArg)
	}
	return false
}

// group is the hook group the gateway installs for event. No onFailure member: the gate blocks a prompt itself,
// and a gate that could not run must not eat the prompt (lib/readiness-hooks.js:22-29).
func (h hookForms) group(event string) jsonx.WireValue {
	hook := object(
		field("type", text("command")),
		field("command", text(h.bin)),
		field("args", jsonx.WireValue{Kind: '[', Elements: []jsonx.WireValue{text(readinessArg)}}),
		field("timeout", jsonx.WireValue{Kind: '0', Number: 35}),
	)
	g := object(field("hooks", jsonx.WireValue{Kind: '[', Elements: []jsonx.WireValue{hook}}))
	if event == "SessionStart" {
		g.Members = append(g.Members, field("matcher", text("startup|resume")))
	}
	return g
}

func sameValue(a, b jsonx.WireValue) bool {
	x, err := jsonx.EncodeWire(a)
	if err != nil {
		return false
	}
	y, err := jsonx.EncodeWire(b)
	return err == nil && bytes.Equal(x, y)
}

// sync returns the edits that remove every owned readiness hook from both events and, when enabled and hooks
// are not disabled, append one fresh group per event (lib/readiness-hooks.js:13-46). An event whose last group
// already is the fresh group keeps it, which yields the same document without rewriting its bytes.
func (h hookForms) sync(root jsonx.WireValue, enabled bool) ([]jsonx.Edit, error) {
	if disabled, ok := member(root, "disableAllHooks"); ok && disabled.Kind == 't' {
		enabled = false
	}
	hooks, present := member(root, "hooks")
	if hooks.Kind != '{' {
		return h.replaceHooks(hooks, present, enabled)
	}
	var edits []jsonx.Edit
	keys := len(hooks.Members)
	for _, event := range readinessEvents() {
		groups, has := member(hooks, event)
		fresh := h.group(event)
		if !has || groups.Kind != '[' {
			// The source reads hooks[event] || [], so only a falsy non-array stands for no groups.
			if has && truthy(groups) {
				return nil, fmt.Errorf("hooks.%s is not an array", event)
			}
			switch {
			case enabled:
				e, err := set(jsonx.WireValue{Kind: '[', Elements: []jsonx.WireValue{fresh}}, "hooks", event)
				if err != nil {
					return nil, err
				}
				edits = append(edits, e)
				if !has {
					keys++
				}
			case has:
				edits = append(edits, remove("hooks", event))
				keys--
			}
			continue
		}
		n := len(groups.Elements)
		reuse := enabled && n > 0 && sameValue(groups.Elements[n-1], fresh)
		eventEdits, kept, err := h.strip(event, groups.Elements, reuse)
		if err != nil {
			return nil, err
		}
		if kept == 0 && !enabled {
			edits = append(edits, remove("hooks", event))
			keys--
			continue
		}
		edits = append(edits, eventEdits...)
		if enabled && !reuse {
			e, err := set(fresh, "hooks", event, "-")
			if err != nil {
				return nil, err
			}
			edits = append(edits, e)
		}
	}
	if keys == 0 {
		return []jsonx.Edit{remove("hooks")}, nil
	}
	return edits, nil
}

// strip removes owned hooks from the groups of one event, highest index first so earlier indexes stay valid. A
// group left without hooks is removed; a group without a hooks array is kept as it is. When reuseLast is set the
// last group is the fresh group and is left alone. It returns the number of groups that remain.
func (h hookForms) strip(event string, groups []jsonx.WireValue, reuseLast bool) ([]jsonx.Edit, int, error) {
	limit := len(groups)
	kept := 0
	if reuseLast {
		limit--
		kept++
	}
	var edits []jsonx.Edit
	for i := limit - 1; i >= 0; i-- {
		group := groups[i]
		if group.Kind == 'n' {
			return nil, 0, fmt.Errorf("hooks.%s[%d] is null", event, i)
		}
		list, ok := member(group, "hooks")
		if !ok || list.Kind != '[' {
			kept++
			continue
		}
		var owned []int
		for j, hook := range list.Elements {
			if h.owned(hook) {
				owned = append(owned, j)
			}
		}
		index := strconv.Itoa(i)
		switch {
		case len(owned) == 0:
			kept++
		case len(owned) == len(list.Elements):
			edits = append(edits, remove("hooks", event, index))
		default:
			kept++
			for _, o := range slices.Backward(owned) {
				edits = append(edits, remove("hooks", event, index, "hooks", strconv.Itoa(o)))
			}
		}
	}
	return edits, kept, nil
}

// replaceHooks handles a hooks member that is absent or not an object. The source spreads it into a new object
// (a string or array contributes its indexes, anything else nothing) and writes that object back, or deletes the
// member when the object is empty (lib/readiness-hooks.js:39-45).
func (h hookForms) replaceHooks(hooks jsonx.WireValue, present, enabled bool) ([]jsonx.Edit, error) {
	var members []jsonx.Member
	switch hooks.Kind {
	case '"':
		for i, unit := range hooks.Text {
			members = append(members, field(strconv.Itoa(i), jsonx.WireValue{Kind: '"', Text: []uint16{unit}}))
		}
	case '[':
		for i, element := range hooks.Elements {
			members = append(members, field(strconv.Itoa(i), element))
		}
	}
	if enabled {
		for _, event := range readinessEvents() {
			members = append(members, field(event, jsonx.WireValue{Kind: '[', Elements: []jsonx.WireValue{h.group(event)}}))
		}
	}
	if len(members) == 0 {
		if present {
			return []jsonx.Edit{remove("hooks")}, nil
		}
		return nil, nil
	}
	e, err := set(object(members...), "hooks")
	if err != nil {
		return nil, err
	}
	return []jsonx.Edit{e}, nil
}

// hasAnyOwned reports whether either readiness hook event contains an owned gateway hook.
func (h hookForms) hasAnyOwned(root jsonx.WireValue) bool {
	hooks, _ := member(root, "hooks")
	for _, event := range readinessEvents() {
		groups, _ := member(hooks, event)
		if groups.Kind == '[' && h.anyGroupOwned(groups.Elements) {
			return true
		}
	}
	return false
}

// present reports whether both events have a group holding an owned readiness hook (lib/readiness-hooks.js:48-51).
// A shape the source cannot traverse counts as absent.
func (h hookForms) present(root jsonx.WireValue) bool {
	hooks, _ := member(root, "hooks")
	for _, event := range readinessEvents() {
		groups, _ := member(hooks, event)
		if groups.Kind != '[' || !h.anyGroupOwned(groups.Elements) {
			return false
		}
	}
	return true
}

func (h hookForms) anyGroupOwned(groups []jsonx.WireValue) bool {
	for _, group := range groups {
		if group.Kind == 'n' {
			return false
		}
		list, ok := member(group, "hooks")
		if !ok || !truthy(list) {
			continue
		}
		if list.Kind != '[' {
			return false
		}
		if slices.ContainsFunc(list.Elements, h.owned) {
			return true
		}
	}
	return false
}
