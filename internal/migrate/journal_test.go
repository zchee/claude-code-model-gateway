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

package migrate_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/migrate"
)

// Digests of the synthetic images. The reader validates syntax and
// transitions only, so these need not be the digests of real files.
var (
	jrnlTStableBefore    = strings.Repeat("a", 64)
	jrnlTStableAfter     = strings.Repeat("b", 64)
	jrnlTLauncherBefore  = strings.Repeat("c", 64)
	jrnlTLauncherAfter   = strings.Repeat("d", 64)
	jrnlTUpdaterAfter    = strings.Repeat("e", 64)
	jrnlTSettingsBefore  = strings.Repeat("f", 64)
	jrnlTSettingsAfter   = strings.Repeat("1", 64)
	jrnlTBreadcrumbAfter = strings.Repeat("5", 64)
	// Digests observed or planned during a restore that differ from both
	// manifest images.
	jrnlTObserved      = strings.Repeat("2", 64)
	jrnlTPlannedInput  = strings.Repeat("3", 64)
	jrnlTPlannedOutput = strings.Repeat("4", 64)
)

// jrnlTEmptyDigest is the SHA-256 of zero bytes: an empty regular file.
const jrnlTEmptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// jrnlTGeneration is a lowercase RFC 4122 version-4 UUID.
const jrnlTGeneration = "0b6f3c1e-8d2a-4f5b-9c7d-2e1a3b4c5d6e"

// jrnlTVersionOneGeneration is a well-formed UUID of version 1, not 4.
const jrnlTVersionOneGeneration = "0b6f3c1e-8d2a-1f5b-9c7d-2e1a3b4c5d6e"

// SHA-256 of "old stable binary\n", "new stable binary\n" and
// "foreign stable binary\n".
const (
	jrnlTOldBinaryDigest     = "5a4264a492a09a3f36bdd8acbb710dec64da2a68014f16119086c11286e59acf"
	jrnlTNewBinaryDigest     = "d2cd1e3c19e93cb7ce60b9c3ecf614e780346265d9c992410d2b8965ab2fd949"
	jrnlTForeignBinaryDigest = "6416afd8f89cbdf73eba5aaf8ac8a571ea94cbaf9e45344fb9f1aae0188a0d91"
)

// Item descriptors without their id, in the exact journal encoding. The
// stable binary is the contract's Home/.claude/model-gateway/bin/model-gateway.
var (
	jrnlTItemStable        = `"role":"stable","path":"/home/u/.claude/model-gateway/bin/model-gateway","target":"/home/u/.claude/model-gateway/bin/model-gateway","link":null,"aliases":[],"linkGroup":0,"before":{"kind":"regular","mode":493,"sha256":"` + jrnlTStableBefore + `"},"after":{"kind":"regular","mode":493,"sha256":"` + jrnlTStableAfter + `"}`
	jrnlTItemLauncher      = `"role":"launcher","path":"/home/u/.claude/model-gateway/model-gateway.js","target":"/home/u/.claude/model-gateway/model-gateway.js","link":null,"aliases":[],"linkGroup":0,"before":{"kind":"regular","mode":384,"sha256":"` + jrnlTLauncherBefore + `"},"after":{"kind":"regular","mode":384,"sha256":"` + jrnlTLauncherAfter + `"}`
	jrnlTItemUpdater       = `"role":"updater","path":"/home/u/.claude/model-gateway/update.js","target":"/home/u/.claude/model-gateway/update.js","link":null,"aliases":[],"linkGroup":0,"before":{"kind":"absent","mode":0,"sha256":null},"after":{"kind":"regular","mode":384,"sha256":"` + jrnlTUpdaterAfter + `"}`
	jrnlTItemBreadcrumb    = `"role":"breadcrumb","path":"/home/u/.claude/toolshed/registry/model-gateway.json","target":"/home/u/.claude/toolshed/registry/model-gateway.json","link":null,"aliases":[],"linkGroup":0,"before":{"kind":"absent","mode":0,"sha256":null},"after":{"kind":"regular","mode":384,"sha256":"` + jrnlTBreadcrumbAfter + `"}`
	jrnlTItemSettings      = `"role":"settings","path":"/home/u/.claude/settings.json","target":"/home/u/dotfiles/claude/settings.json","link":"../dotfiles/claude/settings.json","aliases":[],"linkGroup":0,"before":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsBefore + `"},"after":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsAfter + `"}`
	jrnlTItemMarker        = `"role":"marker","path":"/home/u/.local/state/model-gateway/intentionally-stopped.json","target":"/home/u/.local/state/model-gateway/intentionally-stopped.json","link":null,"aliases":[],"linkGroup":0,"before":{"kind":"regular","mode":384,"sha256":"` + jrnlTEmptyDigest + `"},"after":{"kind":"regular","mode":384,"sha256":"` + jrnlTEmptyDigest + `"}`
	jrnlTItemMarkerRemoved = `"role":"marker","path":"/home/u/.local/state/model-gateway/intentionally-stopped.json","target":"/home/u/.local/state/model-gateway/intentionally-stopped.json","link":null,"aliases":[],"linkGroup":0,"before":{"kind":"regular","mode":384,"sha256":"` + jrnlTEmptyDigest + `"},"after":{"kind":"absent","mode":0,"sha256":null}`
)

// jrnlTManifestOf encodes a manifest whose items are numbered from one in
// the given order.
func jrnlTManifestOf(endState string, items ...string) string {
	var b strings.Builder
	b.WriteString(`{"schema":1,"endState":"` + endState + `","oldCLI":"/home/u/.npm-global/lib/node_modules/model-gateway/cli.js","projectDir":"/home/u/work/project","items":[`)
	for i, item := range items {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,%s}`, i+1, item)
	}
	b.WriteString("]}\n")
	return b.String()
}

// jrnlTKeepManifest is a keep-stopped manifest: a stable binary replaced in
// place, a launcher rewritten, an updater created where none existed, a
// settings file reached through a symlink, and an empty stop marker kept as
// it was.
var jrnlTKeepManifest = jrnlTManifestOf("keep-stopped", jrnlTItemStable, jrnlTItemLauncher, jrnlTItemUpdater, jrnlTItemSettings, jrnlTItemMarker)

// jrnlTStartManifest is the same home migrated with --start: the stop marker
// is removed.
var jrnlTStartManifest = jrnlTManifestOf("start", jrnlTItemStable, jrnlTItemLauncher, jrnlTItemUpdater, jrnlTItemSettings, jrnlTItemMarkerRemoved)

// jrnlTEvent is one events.jsonl record; an empty digest or generation is
// written as null.
type jrnlTEvent struct {
	seq        int
	action, op string
	item       int
	pre, post  string
	generation string
}

func jrnlTNullable(s string) string {
	if s == "" {
		return "null"
	}
	return `"` + s + `"`
}

func jrnlTEvents(events ...jrnlTEvent) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, `{"seq":%d,"action":"%s","op":"%s","item":%d,"pre":%s,"post":%s,"generation":%s}`+"\n",
			e.seq, e.action, e.op, e.item, jrnlTNullable(e.pre), jrnlTNullable(e.post), jrnlTNullable(e.generation))
	}
	return b.String()
}

// jrnlTKeepForward is a complete keep-stopped forward migration: every item
// in manifest order, then the commit.
var jrnlTKeepForward = []jrnlTEvent{
	{1, "intent", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
	{2, "done", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
	{3, "intent", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
	{4, "done", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
	{5, "intent", "publish", 3, "", jrnlTUpdaterAfter, ""},
	{6, "done", "publish", 3, "", jrnlTUpdaterAfter, ""},
	{7, "intent", "settings", 4, jrnlTSettingsBefore, jrnlTSettingsAfter, ""},
	{8, "done", "settings", 4, jrnlTSettingsBefore, jrnlTSettingsAfter, ""},
	{9, "intent", "marker", 5, jrnlTEmptyDigest, jrnlTEmptyDigest, ""},
	{10, "done", "marker", 5, jrnlTEmptyDigest, jrnlTEmptyDigest, ""},
	{11, "intent", "commit", 0, "", "", ""},
	{12, "done", "commit", 0, "", "", ""},
}

// jrnlTStartForward is a complete --start forward migration: the marker is
// removed, the start pair carries its generation, then the commit.
var jrnlTStartForward = slices.Concat(jrnlTKeepForward[:8], []jrnlTEvent{
	{9, "intent", "marker", 5, jrnlTEmptyDigest, "", ""},
	{10, "done", "marker", 5, jrnlTEmptyDigest, "", ""},
	{11, "intent", "start", 0, "", "", jrnlTGeneration},
	{12, "done", "start", 0, "", "", jrnlTGeneration},
	{13, "intent", "commit", 0, "", "", ""},
	{14, "done", "commit", 0, "", "", ""},
})

// jrnlTKeepRollback continues jrnlTKeepForward with an operator reverse: one
// restore per effected item in reverse order, the no-op marker included, then
// the terminal pair. A keep-stopped journal has no stop effect.
var jrnlTKeepRollback = []jrnlTEvent{
	{13, "intent", "reverse", 0, "", "", ""},
	{14, "done", "reverse", 0, "", "", ""},
	{15, "intent", "restore", 5, jrnlTEmptyDigest, jrnlTEmptyDigest, ""},
	{16, "done", "restore", 5, jrnlTEmptyDigest, jrnlTEmptyDigest, ""},
	{17, "intent", "restore", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
	{18, "done", "restore", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
	{19, "intent", "restore", 3, jrnlTUpdaterAfter, "", ""},
	{20, "done", "restore", 3, jrnlTUpdaterAfter, "", ""},
	{21, "intent", "restore", 2, jrnlTLauncherAfter, jrnlTLauncherBefore, ""},
	{22, "done", "restore", 2, jrnlTLauncherAfter, jrnlTLauncherBefore, ""},
	{23, "intent", "restore", 1, jrnlTStableAfter, jrnlTStableBefore, ""},
	{24, "done", "restore", 1, jrnlTStableAfter, jrnlTStableBefore, ""},
	{25, "intent", "rolled-back", 0, "", "", ""},
	{26, "done", "rolled-back", 0, "", "", ""},
}

// jrnlTAutoReverse is a failed forward migration compensated automatically:
// the launcher intent is open when the reverse is recorded, its effect is
// then found applied, and both effected items are restored.
var jrnlTAutoReverse = []jrnlTEvent{
	{1, "intent", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
	{2, "done", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
	{3, "intent", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
	{4, "intent", "reverse-auto", 0, "", "", ""},
	{5, "done", "reverse-auto", 0, "", "", ""},
	{6, "done", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
	{7, "intent", "restore", 2, jrnlTLauncherAfter, jrnlTLauncherBefore, ""},
	{8, "done", "restore", 2, jrnlTLauncherAfter, jrnlTLauncherBefore, ""},
	{9, "intent", "restore", 1, jrnlTStableAfter, jrnlTStableBefore, ""},
	{10, "done", "restore", 1, jrnlTStableAfter, jrnlTStableBefore, ""},
	{11, "intent", "rolled-back", 0, "", "", ""},
	{12, "done", "rolled-back", 0, "", "", ""},
}

// jrnlTOpenCommitReverse reverses a forward migration whose commit intent is
// still open: the reverse pair is finished first, then the commit is skipped.
var jrnlTOpenCommitReverse = slices.Concat(jrnlTKeepForward[:11], []jrnlTEvent{
	{12, "intent", "reverse", 0, "", "", ""},
	{13, "done", "reverse", 0, "", "", ""},
	{14, "done", "skip", 0, "", "", ""},
})

// jrnlTOtherGeneration is a valid generation that no start intent records.
const jrnlTOtherGeneration = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

// jrnlTStartOpenReversed is a --start migration interrupted after its start
// intent, then reversed automatically: the start intent is still open after
// the reverse pair.
var jrnlTStartOpenReversed = slices.Concat(jrnlTStartForward[:11], []jrnlTEvent{
	{12, "intent", "reverse-auto", 0, "", "", ""},
	{13, "done", "reverse-auto", 0, "", "", ""},
})

// jrnlTStartRestores restores every item of the --start manifest in reverse
// order from seq on, then appends the terminal pair.
func jrnlTStartRestores(seq int) []jrnlTEvent {
	images := []struct {
		item      int
		pre, post string
	}{
		{5, "", jrnlTEmptyDigest},
		{4, jrnlTSettingsAfter, jrnlTSettingsBefore},
		{3, jrnlTUpdaterAfter, ""},
		{2, jrnlTLauncherAfter, jrnlTLauncherBefore},
		{1, jrnlTStableAfter, jrnlTStableBefore},
	}
	var events []jrnlTEvent
	for _, im := range images {
		events = append(events,
			jrnlTEvent{seq, "intent", "restore", im.item, im.pre, im.post, ""},
			jrnlTEvent{seq + 1, "done", "restore", im.item, im.pre, im.post, ""})
		seq += 2
	}
	return append(events,
		jrnlTEvent{seq, "intent", "rolled-back", 0, "", "", ""},
		jrnlTEvent{seq + 1, "done", "rolled-back", 0, "", "", ""})
}

// jrnlTFrom renumbers copies of events consecutively from seq.
func jrnlTFrom(seq int, events ...jrnlTEvent) []jrnlTEvent {
	out := slices.Clone(events)
	for i := range out {
		out[i].seq = seq + i
	}
	return out
}

// jrnlTItemSettingsProjectSameTarget is the project's shared settings file
// linked to the same dotfiles copy as the user settings.
var jrnlTItemSettingsProjectSameTarget = `"role":"settings","path":"/home/u/work/project/.claude/settings.json","target":"/home/u/dotfiles/claude/settings.json","link":"/home/u/dotfiles/claude/settings.json","aliases":[],"linkGroup":0,"before":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsBefore + `"},"after":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsAfter + `"}`

// Hard-linked settings entries recorded in one positive link group.
var (
	jrnlTItemSettingsGroupUser    = `"role":"settings","path":"/home/u/.claude/settings.json","target":"/home/u/.claude/settings.json","link":null,"aliases":[],"linkGroup":1,"before":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsBefore + `"},"after":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsAfter + `"}`
	jrnlTItemSettingsGroupProject = `"role":"settings","path":"/home/u/work/project/.claude/settings.local.json","target":"/home/u/work/project/.claude/settings.local.json","link":null,"aliases":[],"linkGroup":1,"before":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsBefore + `"},"after":{"kind":"regular","mode":420,"sha256":"` + jrnlTSettingsAfter + `"}`
)

// jrnlTEdit replaces the single occurrence of old in s, so that an error
// fixture differs from an accepted one in exactly one place.
func jrnlTEdit(t *testing.T, s, old, replacement string) string {
	t.Helper()
	if n := strings.Count(s, old); n != 1 {
		t.Fatalf("fixture edit: %q occurs %d times, want exactly once", old, n)
	}
	return strings.Replace(s, old, replacement, 1)
}

// jrnlTInspect calls InspectJournal and turns a panic into a failure of the
// calling row only.
func jrnlTInspect(t *testing.T, manifest, events []byte) (state migrate.JournalState, ok bool, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("InspectJournal panicked: %v", r)
			ok = false
		}
	}()
	state, err = migrate.InspectJournal(manifest, events)
	return state, true, err
}

func TestJournalReplay(t *testing.T) {
	keep := jrnlTKeepManifest
	start := jrnlTStartManifest
	forward := func(n int) string { return jrnlTEvents(jrnlTKeepForward[:n]...) }
	committed := jrnlTEvents(jrnlTKeepForward...)
	rolledBack := jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback)...)
	fresh := migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project"}

	tests := map[string]struct {
		manifest string
		events   string
		want     migrate.JournalState
		wantErr  bool
	}{
		// Catches a reader that requires at least one event, or that reports a
		// pending operation for a journal that has not begun.
		"success: manifest without events is a forward journal with nothing pending": {
			manifest: keep,
			events:   "",
			want:     fresh,
		},
		// Catches a reader that forgets an intent with no result yet.
		"success: an intent without its result is pending": {
			manifest: keep,
			events:   forward(1),
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "install", PendingItem: 1, ValidBytes: 217},
		},
		// Catches a reader that keeps an intent pending after its matching done.
		"success: a matching done resolves its intent": {
			manifest: keep,
			events:   forward(2),
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 432},
		},
		// Catches a reader that names the first open intent instead of the
		// last one, or that loses the item number of a later item.
		"success: the open launcher intent after a finished install is pending": {
			manifest: keep,
			events:   forward(3),
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "publish", PendingItem: 2, ValidBytes: 649},
		},
		// Catches a reader that treats a commit intent as terminal.
		"success: a commit intent alone keeps the journal forward": {
			manifest: keep,
			events:   forward(11),
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "commit", PendingItem: 0, ValidBytes: 2130},
		},
		// Catches a reader that rejects an absent preimage, an empty regular
		// file, a settings symlink or a keep-stopped end state, or that does
		// not report the matched commit as terminal.
		"success: a matched commit is committed": {
			manifest: keep,
			events:   committed,
			want:     migrate.JournalState{Phase: "committed", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 2221},
		},
		// Catches a reader that rejects a generation on a start record or
		// misreports the start end state.
		"success: a start pair with its generation precedes the commit": {
			manifest: start,
			events:   jrnlTEvents(jrnlTStartForward...),
			want:     migrate.JournalState{Phase: "committed", EndState: "start", ProjectDir: "/home/u/work/project", ValidBytes: 2347},
		},
		// Catches a reader that rejects a journal whose only bytes are an
		// unterminated line, or counts those bytes as valid.
		"success: an unterminated first line is a torn tail": {
			manifest: keep,
			events:   `{"seq":1,"action":"int`,
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", Torn: true},
		},
		// Catches a reader that counts the torn suffix in the valid prefix or
		// that reports it as an error.
		"success: a torn tail after a resolved intent is excluded from the valid prefix": {
			manifest: keep,
			events:   forward(2) + `{"seq":3,"action":"intent","op":"pub`,
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 432, Torn: true},
		},
		// Catches a reader that parses the unacknowledged suffix: arbitrary
		// bytes after the last LF are not validated.
		"success: a torn suffix that is not JSON leaves the pending intent": {
			manifest: keep,
			events:   forward(3) + "\x00\xffnot json",
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "publish", PendingItem: 2, ValidBytes: 649, Torn: true},
		},
		// Catches a reader that drops the committed phase when a suffix
		// follows, or hides the suffix that must suppress publication.
		"success: a torn tail after the commit is committed and torn": {
			manifest: keep,
			events:   committed + `{"seq":13,"action":"intent","op":"rev`,
			want:     migrate.JournalState{Phase: "committed", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 2221, Torn: true},
		},
		// Catches a reader that refuses an operator reverse of a committed
		// journal or keeps reporting it as committed.
		"success: an operator reverse after the commit switches to rollback": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTKeepRollback[0]),
			want:     migrate.JournalState{Phase: "rollback", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "reverse", PendingItem: 0, ValidBytes: 2315},
		},
		// Catches a reader that waits for the reverse done before switching
		// direction, or that names the earlier open intent instead of the
		// later reverse intent.
		"success: an automatic reverse intent with a forward intent open is rollback": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTAutoReverse[:4]...),
			want:     migrate.JournalState{Phase: "rollback", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "reverse-auto", PendingItem: 0, ValidBytes: 747},
		},
		// Catches a reader that forgets the forward intent that was still open
		// when the reverse pair was finished.
		"success: the forward intent left open by a finished reverse stays pending": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTAutoReverse[:5]...),
			want:     migrate.JournalState{Phase: "rollback", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "publish", PendingItem: 2, ValidBytes: 843},
		},
		// Catches a reader that rejects a skip result for an intent whose
		// effect was proven not to have happened.
		"success: a skip resolves an open forward intent after the reverse": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTAutoReverse[:5], []jrnlTEvent{
				{6, "done", "skip", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
			})...),
			want: migrate.JournalState{Phase: "rollback", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 1055},
		},
		// Catches a reader that refuses to reverse an open commit, or that
		// requires a commit done before the skip.
		"success: a reverse of an open commit skips the commit": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTOpenCommitReverse...),
			want:     migrate.JournalState{Phase: "rollback", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 2405},
		},
		// Catches a reader that does not report the matched terminal pair of a
		// complete operator rollback.
		"success: every effected item restored in reverse order is rolled back": {
			manifest: keep,
			events:   rolledBack,
			want:     migrate.JournalState{Phase: "rolled-back", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 4647},
		},
		// Catches a reader that cannot finish an automatic reverse.
		"success: a completed automatic reverse is rolled back": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTAutoReverse...),
			want:     migrate.JournalState{Phase: "rolled-back", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 2117},
		},
		// Catches a reader that treats a rolled-back intent as terminal.
		"success: a rolled-back intent without its done is still rollback": {
			manifest: keep,
			events:   jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:13])...),
			want:     migrate.JournalState{Phase: "rollback", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", PendingOp: "rolled-back", PendingItem: 0, ValidBytes: 4551},
		},

		// The rows below resolve a start intent left open when the reverse
		// pair was recorded. Whether a saved owner exists is not in the
		// events, so a skipped start replays both with and without a stop
		// pair; every start, skip and stop record keeps the start's
		// generation.

		// Catches a reader that drops or resolves an unresolved start when
		// the direction switches.
		"success: a start left open by an automatic reverse stays pending": {
			manifest: start,
			events:   jrnlTEvents(jrnlTStartOpenReversed...),
			want:     migrate.JournalState{Phase: "rollback", EndState: "start", ProjectDir: "/home/u/work/project", PendingOp: "start", PendingItem: 0, ValidBytes: 2235},
		},
		// Catches a reader that refuses an operator reverse while a start
		// intent is open.
		"success: an operator reverse of an open start leaves the start pending": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:11], []jrnlTEvent{
				{12, "intent", "reverse", 0, "", "", ""},
				{13, "done", "reverse", 0, "", "", ""},
			})...),
			want: migrate.JournalState{Phase: "rollback", EndState: "start", ProjectDir: "/home/u/work/project", PendingOp: "start", PendingItem: 0, ValidBytes: 2225},
		},
		// Catches a reader that rejects a start done after the reverse when
		// serving was proven, or that loses the stop intent that follows it.
		"success: the stop intent after a served start is pending": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "start", 0, "", "", jrnlTGeneration},
				{15, "intent", "stop", 0, "", "", jrnlTGeneration},
			})...),
			want: migrate.JournalState{Phase: "rollback", EndState: "start", ProjectDir: "/home/u/work/project", PendingOp: "stop", PendingItem: 0, ValidBytes: 2484},
		},
		// Catches a reader that cannot complete a rollback whose start was
		// served: start done, the stop pair, then every restore.
		"success: a served start is stopped before the restores": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "start", 0, "", "", jrnlTGeneration},
				{15, "intent", "stop", 0, "", "", jrnlTGeneration},
				{16, "done", "stop", 0, "", "", jrnlTGeneration},
			}, jrnlTStartRestores(17))...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "start", ProjectDir: "/home/u/work/project", ValidBytes: 4723},
		},
		// Catches a reader that requires a stop pair after every skipped
		// start, or that rejects a skip carrying the start's generation.
		"success: a start skipped without a saved owner needs no stop pair": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
			}, jrnlTStartRestores(15))...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "start", ProjectDir: "/home/u/work/project", ValidBytes: 4474},
		},
		// Catches a reader that forbids a stop pair after a skipped start: a
		// saved owner, dead or alive, still requires it.
		"success: a start skipped with a saved owner is followed by the stop pair": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
				{15, "intent", "stop", 0, "", "", jrnlTGeneration},
				{16, "done", "stop", 0, "", "", jrnlTGeneration},
			}, jrnlTStartRestores(17))...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "start", ProjectDir: "/home/u/work/project", ValidBytes: 4722},
		},
		// Catches a reader that restores a committed start migration without
		// stopping the supervisor it started.
		"success: a committed start is stopped before the restores": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward, []jrnlTEvent{
				{15, "intent", "reverse", 0, "", "", ""},
				{16, "done", "reverse", 0, "", "", ""},
				{17, "intent", "stop", 0, "", "", jrnlTGeneration},
				{18, "done", "stop", 0, "", "", jrnlTGeneration},
			}, jrnlTStartRestores(19))...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "start", ProjectDir: "/home/u/work/project", ValidBytes: 4897},
		},

		// Catches a reader that counts a skipped item as effected and so
		// requires its restore. ValidBytes: 1680 = the ten records: 217+215+217+98+96+212+217+215+97+96.
		"success: a skipped launcher leaves only the stable binary to restore": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTAutoReverse[:5], []jrnlTEvent{
				{6, "done", "skip", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
				{7, "intent", "restore", 1, jrnlTStableAfter, jrnlTStableBefore, ""},
				{8, "done", "restore", 1, jrnlTStableAfter, jrnlTStableBefore, ""},
				{9, "intent", "rolled-back", 0, "", "", ""},
				{10, "done", "rolled-back", 0, "", "", ""},
			})...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 1680},
		},
		// Catches a reader that accepts only exact restore results: a no-write
		// conflict with equal observed halves is the marker's result.
		// ValidBytes: 4648 = 4647 for the complete rollback - 218 - 216 for the marker restore pair + 218 + 217 for the conflict pair.
		"success: a marker conflict with equal observed halves completes the rollback": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:2], []jrnlTEvent{
				{15, "intent", "restore", 5, jrnlTObserved, jrnlTObserved, ""},
				{16, "done", "conflict", 5, jrnlTObserved, jrnlTObserved, ""},
			}, jrnlTKeepRollback[4:])...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 4648},
		},
		// Catches a reader that rejects the null-image receipt of a
		// nonregular replacement. ValidBytes: 4400 = 4647 for the complete rollback - 218 - 216 for the marker restore pair + 94 + 93 for the null conflict pair.
		"success: a nonregular conflict receipt with null halves completes the rollback": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:2], []jrnlTEvent{
				{15, "intent", "restore", 5, "", "", ""},
				{16, "done", "conflict", 5, "", "", ""},
			}, jrnlTKeepRollback[4:])...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 4400},
		},
		// Catches a reader that rejects an owned-hook merge result, or that
		// requires restore digests to equal the manifest images.
		// ValidBytes: 4649 = 4647 for the complete rollback - 218 - 216 for the settings restore pair + 218 + 218 for the preserved pair.
		"success: a preserved settings merge completes the rollback": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:4], []jrnlTEvent{
				{17, "intent", "restore", 4, jrnlTPlannedInput, jrnlTPlannedOutput, ""},
				{18, "done", "preserved", 4, jrnlTPlannedInput, jrnlTPlannedOutput, ""},
			}, jrnlTKeepRollback[6:])...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 4649},
		},
		// Catches a reader that counts a superseded plan as the item's result
		// or leaves it pending. ValidBytes: 3278 = 2221 for the commit + 620 for the reverse and marker pairs + 218 + 219 for the superseded pair.
		"success: a superseded settings restore leaves no result for its item": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:4], []jrnlTEvent{
				{17, "intent", "restore", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
				{18, "done", "superseded", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
			})...),
			want: migrate.JournalState{Phase: "rollback", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 3278},
		},
		// Catches a reader that refuses a replacement plan for the same item
		// after a superseded one: the replacement is a merge against the
		// current document, so its result is preserved. ValidBytes: 5086 = 3278 for the superseded journal + 218 + 218 for the preserved retry pair + 1372 for the last three restore pairs and the terminal pair numbered from 21.
		"success: the retried settings restore completes the rollback": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:4], []jrnlTEvent{
				{17, "intent", "restore", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
				{18, "done", "superseded", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
				{19, "intent", "restore", 4, jrnlTPlannedInput, jrnlTPlannedOutput, ""},
				{20, "done", "preserved", 4, jrnlTPlannedInput, jrnlTPlannedOutput, ""},
			}, jrnlTFrom(21, jrnlTKeepRollback[6:]...))...),
			want: migrate.JournalState{Phase: "rolled-back", EndState: "keep-stopped", ProjectDir: "/home/u/work/project", ValidBytes: 5086},
		},
		// Catches a reader that reports a constant or truncated project
		// directory.
		"success: the project directory comes from the manifest": {
			manifest: jrnlTEdit(t, keep, `"projectDir":"/home/u/work/project"`, `"projectDir":"/home/u/work/second project"`),
			events:   "",
			want:     migrate.JournalState{Phase: "forward", EndState: "keep-stopped", ProjectDir: "/home/u/work/second project"},
		},
		// Catches a reader that rejects every alias, or an absolute link text:
		// the project's local settings file links to the same dotfiles copy.
		"success: a settings alias to the same destination is accepted": {
			manifest: jrnlTEdit(t, keep, `"link":"../dotfiles/claude/settings.json","aliases":[]`, `"link":"../dotfiles/claude/settings.json","aliases":[{"path":"/home/u/work/project/.claude/settings.local.json","link":"/home/u/dotfiles/claude/settings.json"}]`),
			events:   "",
			want:     fresh,
		},
		// Catches a reader that rejects every positive link group.
		"success: hard-linked settings entries in one positive group are accepted": {
			manifest: jrnlTManifestOf("keep-stopped", jrnlTItemStable, jrnlTItemLauncher, jrnlTItemUpdater, jrnlTItemSettingsGroupUser, jrnlTItemSettingsGroupProject, jrnlTItemMarker),
			events:   "",
			want:     fresh,
		},
		// Catches a reader that rejects the optional breadcrumb in its
		// publication place.
		"success: a breadcrumb between the updater and the settings is accepted": {
			manifest: jrnlTManifestOf("keep-stopped", jrnlTItemStable, jrnlTItemLauncher, jrnlTItemUpdater, jrnlTItemBreadcrumb, jrnlTItemSettings, jrnlTItemMarker),
			events:   "",
			want:     fresh,
		},
		// Manifest errors: each differs from the accepted manifest in one place.

		// Catches a reader that ignores the schema number.
		"error: manifest of an unknown schema": {
			manifest: jrnlTEdit(t, keep, `"schema":1`, `"schema":2`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts any end state string.
		"error: manifest with an unknown end state": {
			manifest: jrnlTEdit(t, keep, `"endState":"keep-stopped"`, `"endState":"stopped"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that does not check the project directory.
		"error: manifest with a relative project directory": {
			manifest: jrnlTEdit(t, keep, `"projectDir":"/home/u/work/project"`, `"projectDir":"work/project"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts an absolute but unclean project
		// directory.
		"error: manifest with an unclean project directory": {
			manifest: jrnlTEdit(t, keep, `"projectDir":"/home/u/work/project"`, `"projectDir":"/home/u/work//project"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that does not check the legacy CLI path.
		"error: manifest with a relative legacy CLI": {
			manifest: jrnlTEdit(t, keep, `"oldCLI":"/home/u/.npm-global/lib/node_modules/model-gateway/cli.js"`, `"oldCLI":"lib/node_modules/model-gateway/cli.js"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts any item role.
		"error: manifest item with an unknown role": {
			manifest: jrnlTEdit(t, keep, `"role":"updater"`, `"role":"binary"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that does not check item paths.
		"error: manifest item with a relative path": {
			manifest: jrnlTEdit(t, keep, `"path":"/home/u/.claude/model-gateway/update.js"`, `"path":".claude/model-gateway/update.js"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts an image kind other than absent or
		// regular.
		"error: manifest image of an unknown kind": {
			manifest: jrnlTEdit(t, keep, `{"kind":"absent","mode":0,"sha256":null}`, `{"kind":"symlink","mode":0,"sha256":null}`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts an absent image with permission bits.
		"error: absent image with a mode": {
			manifest: jrnlTEdit(t, keep, `{"kind":"absent","mode":0,"sha256":null}`, `{"kind":"absent","mode":384,"sha256":null}`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that confuses an absent image with an empty file.
		"error: absent image with the digest of an empty file": {
			manifest: jrnlTEdit(t, keep, `{"kind":"absent","mode":0,"sha256":null}`, `{"kind":"absent","mode":0,"sha256":"`+jrnlTEmptyDigest+`"}`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts a regular image without its digest.
		"error: regular image without a digest": {
			manifest: jrnlTEdit(t, keep, `"before":{"kind":"regular","mode":384,"sha256":"`+jrnlTLauncherBefore+`"}`, `"before":{"kind":"regular","mode":384,"sha256":null}`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts uppercase hexadecimal digests.
		"error: digest in uppercase": {
			manifest: jrnlTEdit(t, keep, `"sha256":"`+jrnlTLauncherBefore+`"`, `"sha256":"`+strings.ToUpper(jrnlTLauncherBefore)+`"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that does not check the digest length.
		"error: digest one character short": {
			manifest: jrnlTEdit(t, keep, `"sha256":"`+jrnlTLauncherBefore+`"`, `"sha256":"`+jrnlTLauncherBefore[1:]+`"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that ignores members it does not know.
		"error: manifest with an unknown member": {
			manifest: jrnlTEdit(t, keep, `"schema":1,`, `"schema":1,"note":"x",`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that lets a repeated member override the first.
		"error: manifest with a duplicate member": {
			manifest: jrnlTEdit(t, keep, `"schema":1,`, `"schema":1,"schema":1,`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that defaults an omitted member.
		"error: manifest item without its link group": {
			manifest: jrnlTEdit(t, keep, `"aliases":[],"linkGroup":0,"before":{"kind":"absent"`, `"aliases":[],"before":{"kind":"absent"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts two items with one id.
		"error: manifest items with a duplicate id": {
			manifest: jrnlTEdit(t, keep, `"id":3,`, `"id":2,`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts a manifest missing its final LF.
		"error: manifest without its final LF": {
			manifest: strings.TrimSuffix(keep, "\n"),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts a truncated manifest.
		"error: truncated manifest": {
			manifest: keep[:len(keep)/2],
			events:   "",
			wantErr:  true,
		},

		// Catches a reader that does not check item targets.
		"error: manifest item with a relative target": {
			manifest: jrnlTEdit(t, keep, `"target":"/home/u/.claude/model-gateway/update.js"`, `"target":".claude/model-gateway/update.js"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that does not check alias paths.
		"error: settings alias with a relative path": {
			manifest: jrnlTEdit(t, keep, `"link":"../dotfiles/claude/settings.json","aliases":[]`, `"link":"../dotfiles/claude/settings.json","aliases":[{"path":"work/project/.claude/settings.local.json","link":"/home/u/dotfiles/claude/settings.json"}]`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts a followed symlink outside the
		// settings files; the target is the sibling the link names, so the
		// descriptor is otherwise consistent.
		"error: symlink recorded for the launcher": {
			manifest: jrnlTEdit(t, keep, `"target":"/home/u/.claude/model-gateway/model-gateway.js","link":null`, `"target":"/home/u/.claude/model-gateway/model-gateway.real.js","link":"model-gateway.real.js"`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts two logical items for one resolved
		// destination: two valid, ordered settings entries whose links reach
		// the same dotfiles copy.
		"error: two items with one target": {
			manifest: jrnlTManifestOf("keep-stopped", jrnlTItemStable, jrnlTItemLauncher, jrnlTItemUpdater, jrnlTItemSettings, jrnlTItemSettingsProjectSameTarget, jrnlTItemMarker),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that reads a null mode as zero.
		"error: regular image with a null mode": {
			manifest: jrnlTEdit(t, keep, `"before":{"kind":"regular","mode":384,"sha256":"`+jrnlTLauncherBefore+`"}`, `"before":{"kind":"regular","mode":null,"sha256":"`+jrnlTLauncherBefore+`"}`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts negative permission bits.
		"error: regular image with a negative mode": {
			manifest: jrnlTEdit(t, keep, `"before":{"kind":"regular","mode":384,"sha256":"`+jrnlTLauncherBefore+`"}`, `"before":{"kind":"regular","mode":-1,"sha256":"`+jrnlTLauncherBefore+`"}`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts a mode beyond the permission bits.
		"error: regular image with a mode beyond the permission bits": {
			manifest: jrnlTEdit(t, keep, `"before":{"kind":"regular","mode":384,"sha256":"`+jrnlTLauncherBefore+`"}`, `"before":{"kind":"regular","mode":1048576,"sha256":"`+jrnlTLauncherBefore+`"}`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts a negative link group.
		"error: negative link group": {
			manifest: jrnlTEdit(t, keep, `"aliases":[],"linkGroup":0,"before":{"kind":"regular","mode":420`, `"aliases":[],"linkGroup":-1,"before":{"kind":"regular","mode":420`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts an item id of zero, which names the
		// global operations.
		"error: manifest item with id zero": {
			manifest: jrnlTEdit(t, keep, `{"id":1,`, `{"id":0,`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that does not check the effect order of the
		// descriptors.
		"error: launcher descriptor before the stable binary": {
			manifest: jrnlTManifestOf("keep-stopped", jrnlTItemLauncher, jrnlTItemStable, jrnlTItemUpdater, jrnlTItemSettings, jrnlTItemMarker),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts members out of the fixed order.
		"error: manifest members out of order": {
			manifest: jrnlTEdit(t, keep, `{"schema":1,"endState":"keep-stopped",`, `{"endState":"keep-stopped","schema":1,`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts insignificant whitespace.
		"error: manifest with an insignificant space": {
			manifest: jrnlTEdit(t, keep, `"schema":1,`, `"schema": 1,`),
			events:   "",
			wantErr:  true,
		},
		// Catches a reader that accepts invalid UTF-8 inside a string.
		"error: manifest with invalid UTF-8 in a path": {
			manifest: jrnlTEdit(t, keep, `"oldCLI":"/home/u/.npm-global/lib/node_modules/model-gateway/cli.js"`, "\"oldCLI\":\"/home/u/.npm-global/lib/node_modules/model-gateway/cli\xff.js\""),
			events:   "",
			wantErr:  true,
		},

		// Event errors: each differs from an accepted event sequence in one
		// place.

		// Catches a reader that accepts a sequence not starting at one.
		"error: first event is not number one": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTEvent{2, "intent", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
				jrnlTEvent{3, "done", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts a gap in the sequence.
		"error: event numbers with a gap": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0],
				jrnlTEvent{3, "done", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that treats any malformed line as a torn tail, even
		// one that later complete lines follow.
		"error: damaged earlier line before valid lines": {
			manifest: keep,
			events:   forward(1) + `{"seq":2,"action":"done","op":"inst` + "\n" + jrnlTEvents(jrnlTKeepForward[2:4]...),
			wantErr:  true,
		},
		// Catches a reader that treats a malformed but LF-terminated last line
		// as torn.
		"error: damaged complete last line": {
			manifest: keep,
			events:   forward(2) + "not json\n",
			wantErr:  true,
		},
		// Catches a reader that records a result with no durable intent.
		"error: done without an intent": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTEvent{1, "done", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""}),
			wantErr:  true,
		},
		// Catches a reader that matches a done by operation and item only.
		"error: done contradicting its intent": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0],
				jrnlTEvent{2, "done", "install", 1, jrnlTStableBefore, jrnlTLauncherAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts two results for one intent.
		"error: duplicate done": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0], jrnlTKeepForward[1],
				jrnlTEvent{3, "done", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that allows two forward intents open at once.
		"error: second forward intent while one is open": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0],
				jrnlTEvent{2, "intent", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that does not follow manifest order.
		"error: forward intent before the first manifest item": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTEvent{1, "intent", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""}),
			wantErr:  true,
		},
		// Catches a reader that allows a manifest item to be skipped.
		"error: forward intent skipping a manifest item": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0], jrnlTKeepForward[1],
				jrnlTEvent{3, "intent", "publish", 3, "", jrnlTUpdaterAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts a commit before every item has its
		// result.
		"error: commit before every item is done": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0], jrnlTKeepForward[1],
				jrnlTEvent{3, "intent", "commit", 0, "", "", ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts any operation name.
		"error: event with an unknown operation": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTEvent{1, "intent", "delete", 1, jrnlTStableBefore, jrnlTStableAfter, ""}),
			wantErr:  true,
		},
		// Catches a reader that accepts any action name.
		"error: event with an unknown action": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTEvent{1, "begin", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""}),
			wantErr:  true,
		},
		// Catches a reader that ignores unknown members of a record.
		"error: event with an unknown member": {
			manifest: keep,
			events:   `{"seq":1,"action":"intent","op":"install","item":1,"pre":"` + jrnlTStableBefore + `","post":"` + jrnlTStableAfter + `","generation":null,"note":"x"}` + "\n",
			wantErr:  true,
		},
		// Catches a reader that lets a repeated member override the first.
		"error: event with a duplicate member": {
			manifest: keep,
			events:   `{"seq":1,"seq":1,"action":"intent","op":"install","item":1,"pre":"` + jrnlTStableBefore + `","post":"` + jrnlTStableAfter + `","generation":null}` + "\n",
			wantErr:  true,
		},
		// Catches a reader that accepts a start record with no generation.
		"error: start pair without a generation": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:10], []jrnlTEvent{
				{11, "intent", "start", 0, "", "", ""},
				{12, "done", "start", 0, "", "", ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a reversal bound to an item.
		"error: reverse intent naming an item": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTEvent{13, "intent", "reverse", 1, "", "", ""}),
			wantErr:  true,
		},
		// Catches a reader that matches a reversal done by kind only.
		"error: reverse intent with an automatic reverse done": {
			manifest: keep,
			events: committed + jrnlTEvents(
				jrnlTKeepRollback[0],
				jrnlTEvent{14, "done", "reverse-auto", 0, "", "", ""},
			),
			wantErr: true,
		},
		// Catches a reader that allows an automatic reverse of a committed
		// journal.
		"error: automatic reverse after the commit": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTEvent{13, "intent", "reverse-auto", 0, "", "", ""}),
			wantErr:  true,
		},
		// Catches a reader that allows both reversal directions in one journal.
		"error: operator reverse after an automatic reverse": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTAutoReverse[:6]...) + jrnlTEvents(jrnlTEvent{7, "intent", "reverse", 0, "", "", ""}),
			wantErr:  true,
		},
		// Catches a reader that allows forward effects after a reverse.
		"error: forward intent after the reverse": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTAutoReverse[:2]...) + jrnlTEvents(jrnlTEvent{3, "intent", "reverse-auto", 0, "", "", ""}, jrnlTEvent{4, "done", "reverse-auto", 0, "", "", ""}, jrnlTEvent{5, "intent", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""}),
			wantErr:  true,
		},
		// Catches a reader that lets an open commit complete after the reverse.
		"error: commit done after the reverse": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTOpenCommitReverse[:13], []jrnlTEvent{
				{14, "done", "commit", 0, "", "", ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that allows a forward effect after the commit.
		"error: forward intent after the commit": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTEvent{13, "intent", "install", 1, jrnlTStableBefore, jrnlTStableAfter, ""}),
			wantErr:  true,
		},
		// Catches a reader that accepts records other than an operator
		// reverse after the commit: a second commit would otherwise satisfy
		// every other rule.
		"error: second commit after the commit": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTEvent{13, "intent", "commit", 0, "", "", ""}),
			wantErr:  true,
		},
		// Catches a reader that records a stop before the open start intent
		// is resolved.
		"error: stop intent while the start is unresolved": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "intent", "stop", 0, "", "", jrnlTGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that restores files while the start's identity is
		// unresolved.
		"error: restore while the start is unresolved": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "intent", "restore", 5, "", jrnlTEmptyDigest, ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that restores files after a served start without
		// the stop pair.
		"error: restore after a served start without the stop pair": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "start", 0, "", "", jrnlTGeneration},
				{15, "intent", "restore", 5, "", jrnlTEmptyDigest, ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a skip that does not retain the
		// start's generation.
		"error: start skipped under another generation": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTOtherGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a stop for a generation other than
		// the journaled start's.
		"error: stop under another generation": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
				{15, "intent", "stop", 0, "", "", jrnlTOtherGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a second stop pair.
		"error: second stop after the stop pair": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
				{15, "intent", "stop", 0, "", "", jrnlTGeneration},
				{16, "done", "stop", 0, "", "", jrnlTGeneration},
				{17, "intent", "stop", 0, "", "", jrnlTGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that restores while the stop intent is open.
		"error: restore while the stop is open": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
				{15, "intent", "stop", 0, "", "", jrnlTGeneration},
				{16, "intent", "restore", 5, "", jrnlTEmptyDigest, ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that allows a relaunch after the reverse.
		"error: second start intent after the reverse": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
				{15, "intent", "start", 0, "", "", jrnlTOtherGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a stop outside a rollback.
		"error: stop before any reverse": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:12], []jrnlTEvent{
				{13, "intent", "stop", 0, "", "", jrnlTGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a stop in a journal with no start:
		// a committed keep-stopped journal has no stop effect.
		"error: stop without a journaled start": {
			manifest: keep,
			events: committed + jrnlTEvents(
				jrnlTKeepRollback[0], jrnlTKeepRollback[1],
				jrnlTEvent{15, "intent", "stop", 0, "", "", jrnlTGeneration},
			),
			wantErr: true,
		},
		// Catches a reader that commits a --start journal whose start never
		// ran.
		"error: start-mode commit without a start": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:10], []jrnlTEvent{
				{11, "intent", "commit", 0, "", "", ""},
				{12, "done", "commit", 0, "", "", ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that does not compare a start done's generation.
		"error: start done under another generation": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:11], []jrnlTEvent{
				{12, "done", "start", 0, "", "", jrnlTOtherGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that does not compare a stop done's generation.
		"error: stop done under another generation": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
				{15, "intent", "stop", 0, "", "", jrnlTGeneration},
				{16, "done", "stop", 0, "", "", jrnlTOtherGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a generation that is not a version-4
		// UUID.
		"error: start pair with a version-1 generation": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:10], []jrnlTEvent{
				{11, "intent", "start", 0, "", "", jrnlTVersionOneGeneration},
				{12, "done", "start", 0, "", "", jrnlTVersionOneGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a start before the marker effect.
		"error: start before the marker": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:8], []jrnlTEvent{
				{9, "intent", "start", 0, "", "", jrnlTGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a second forward start, including one
		// that rejects only a reused generation: the second start is fresh.
		"error: second start after the start pair": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartForward[:12], []jrnlTEvent{
				{13, "intent", "start", 0, "", "", jrnlTOtherGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a start in a keep-stopped journal.
		"error: start in a keep-stopped journal": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward[:10], []jrnlTEvent{
				{11, "intent", "start", 0, "", "", jrnlTGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that does not pair the opcode with the item's role.
		"error: stable binary recorded under the publish opcode": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat([]jrnlTEvent{
				{1, "intent", "publish", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
				{2, "done", "publish", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
			}, jrnlTKeepForward[2:])...),
			wantErr: true,
		},
		// Catches a reader that does not compare forward digests with the
		// item's manifest images.
		"error: install records naming the launcher's post image": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat([]jrnlTEvent{
				{1, "intent", "install", 1, jrnlTStableBefore, jrnlTLauncherAfter, ""},
				{2, "done", "install", 1, jrnlTStableBefore, jrnlTLauncherAfter, ""},
			}, jrnlTKeepForward[2:])...),
			wantErr: true,
		},
		// Catches a reader that does not compare a done's pre image.
		"error: done changing only its pre image": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0],
				jrnlTEvent{2, "done", "install", 1, jrnlTLauncherBefore, jrnlTStableAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that does not compare a done's item.
		"error: done naming another item": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0],
				jrnlTEvent{2, "done", "install", 2, jrnlTStableBefore, jrnlTStableAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that does not compare a done's opcode.
		"error: done with another opcode": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0],
				jrnlTEvent{2, "done", "publish", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that lets another result in before the reverse
		// pair is finished.
		"error: forward done between the reverse intent and its done": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTAutoReverse[:3], []jrnlTEvent{
				{4, "intent", "reverse", 0, "", "", ""},
				{5, "done", "publish", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that restores while an interrupted file intent is
		// still unresolved.
		"error: restore while an interrupted file intent is open": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTAutoReverse[:5], []jrnlTEvent{
				{6, "intent", "restore", 1, jrnlTStableAfter, jrnlTStableBefore, ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a skip result outside a rollback.
		"error: skip before any reverse": {
			manifest: keep,
			events: jrnlTEvents(
				jrnlTKeepForward[0],
				jrnlTEvent{2, "done", "skip", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts the same reversal twice.
		"error: second automatic reverse": {
			manifest: keep,
			events:   jrnlTEvents(jrnlTAutoReverse[:6]...) + jrnlTEvents(jrnlTEvent{7, "intent", "reverse-auto", 0, "", "", ""}),
			wantErr:  true,
		},
		// The next three rows each catch a reader that accepts a reversal
		// intent with a non-null field; the contract names all three null.
		"error: reverse intent with a pre image": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTEvent{13, "intent", "reverse", 0, jrnlTStableAfter, "", ""}),
			wantErr:  true,
		},
		"error: reverse intent with a post image": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTEvent{13, "intent", "reverse", 0, "", jrnlTStableBefore, ""}),
			wantErr:  true,
		},
		"error: reverse intent with a generation": {
			manifest: keep,
			events:   committed + jrnlTEvents(jrnlTEvent{13, "intent", "reverse", 0, "", "", jrnlTGeneration}),
			wantErr:  true,
		},
		// Catches a reader that accepts a stop after the restores began.
		"error: stop after the first restore": {
			manifest: start,
			events: jrnlTEvents(slices.Concat(jrnlTStartOpenReversed, []jrnlTEvent{
				{14, "done", "skip", 0, "", "", jrnlTGeneration},
			}, jrnlTStartRestores(15)[:2], []jrnlTEvent{
				{17, "intent", "stop", 0, "", "", jrnlTGeneration},
			})...),
			wantErr: true,
		},
		// Catches a reader that restores an item whose effect was skipped.
		"error: restore of a skipped launcher": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTAutoReverse[:5], []jrnlTEvent{
				{6, "done", "skip", 2, jrnlTLauncherBefore, jrnlTLauncherAfter, ""},
				{7, "intent", "restore", 2, jrnlTLauncherAfter, jrnlTLauncherBefore, ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts a conflict that plans a write.
		"error: conflict with unequal halves": {
			manifest: keep,
			events: committed + jrnlTEvents(
				jrnlTKeepRollback[0], jrnlTKeepRollback[1],
				jrnlTEvent{15, "intent", "restore", 5, jrnlTObserved, jrnlTEmptyDigest, ""},
				jrnlTEvent{16, "done", "conflict", 5, jrnlTObserved, jrnlTEmptyDigest, ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts a conflict for an item other than the
		// outstanding restore.
		"error: conflict naming another item": {
			manifest: keep,
			events: committed + jrnlTEvents(
				jrnlTKeepRollback[0], jrnlTKeepRollback[1], jrnlTKeepRollback[2],
				jrnlTEvent{16, "done", "conflict", 4, jrnlTEmptyDigest, jrnlTEmptyDigest, ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts a conflict receipt with no
		// outstanding restore.
		"error: conflict receipt without an outstanding restore": {
			manifest: keep,
			events: committed + jrnlTEvents(
				jrnlTKeepRollback[0], jrnlTKeepRollback[1],
				jrnlTEvent{15, "done", "conflict", 5, jrnlTEmptyDigest, jrnlTEmptyDigest, ""},
			),
			wantErr: true,
		},
		// Catches a reader that advances to the next item after a superseded
		// plan instead of retrying the same item.
		"error: next item restored after a superseded plan": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:4], []jrnlTEvent{
				{17, "intent", "restore", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
				{18, "done", "superseded", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
				{19, "intent", "restore", 3, jrnlTUpdaterAfter, "", ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts the terminal pair while an item has
		// only a superseded plan.
		"error: rolled back after a superseded plan": {
			manifest: keep,
			events: jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback[:4], []jrnlTEvent{
				{17, "intent", "restore", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
				{18, "done", "superseded", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
				{19, "intent", "rolled-back", 0, "", "", ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts event members out of the fixed order.
		"error: event members out of order": {
			manifest: keep,
			events:   jrnlTEdit(t, forward(1), `{"seq":1,"action":"intent",`, `{"action":"intent","seq":1,`),
			wantErr:  true,
		},
		// Catches a reader that accepts insignificant whitespace in a record.
		"error: event with an insignificant space": {
			manifest: keep,
			events:   jrnlTEdit(t, forward(1), `{"seq":1,`, `{"seq": 1,`),
			wantErr:  true,
		},
		// Catches a reader that defaults an omitted nullable member.
		"error: complete event without its generation member": {
			manifest: keep,
			events:   jrnlTEdit(t, forward(1), `,"generation":null}`, `}`),
			wantErr:  true,
		},
		// Catches a reader that does not restore in reverse effect order.
		"error: restore out of reverse order": {
			manifest: keep,
			events: committed + jrnlTEvents(
				jrnlTKeepRollback[0], jrnlTKeepRollback[1],
				jrnlTEvent{15, "intent", "restore", 4, jrnlTSettingsAfter, jrnlTSettingsBefore, ""},
			),
			wantErr: true,
		},
		// Catches a reader that accepts the terminal pair before every
		// effected item has its restore result.
		"error: rolled back before every effected item is restored": {
			manifest: keep,
			events: committed + jrnlTEvents(slices.Concat(jrnlTKeepRollback[:10], []jrnlTEvent{
				{23, "intent", "rolled-back", 0, "", "", ""},
				{24, "done", "rolled-back", 0, "", "", ""},
			})...),
			wantErr: true,
		},
		// Catches a reader that accepts records after the terminal result: a
		// second terminal intent would otherwise satisfy every other rule.
		"error: event after the rolled-back done": {
			manifest: keep,
			events:   rolledBack + jrnlTEvents(jrnlTEvent{27, "intent", "rolled-back", 0, "", "", ""}),
			wantErr:  true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			manifest, events := []byte(tc.manifest), []byte(tc.events)
			got, ok, err := jrnlTInspect(t, manifest, events)
			if !ok {
				return
			}
			if tc.wantErr {
				if err == nil {
					t.Errorf("InspectJournal() error = nil, want an error; state = %+v", got)
				}
				if diff := gocmp.Diff(migrate.JournalState{}, got); diff != "" {
					t.Errorf("InspectJournal() state with an error is not the zero value (-want +got):\n%s", diff)
				}
			} else {
				if err != nil {
					t.Errorf("InspectJournal() error = %v, want nil", err)
				}
				if diff := gocmp.Diff(tc.want, got); diff != "" {
					t.Errorf("InspectJournal() state mismatch (-want +got):\n%s", diff)
				}
			}
			if diff := gocmp.Diff(tc.manifest, string(manifest)); diff != "" {
				t.Errorf("InspectJournal() edited the manifest bytes (-before +after):\n%s", diff)
			}
			if diff := gocmp.Diff(tc.events, string(events)); diff != "" {
				t.Errorf("InspectJournal() edited the event bytes (-before +after):\n%s", diff)
			}
		})
	}
}

// Published journal names, oldest first.
const (
	jrnlTOlder  = "20261101T000000.000000001Z"
	jrnlTLatest = "20261128T060000.000000001Z"
	jrnlTNewest = "20261128T070000.000000001Z"
)

// jrnlTJournal writes a published journal of ordinary private files.
func jrnlTJournal(t *testing.T, migrationRoot, name, manifest, events string) {
	t.Helper()
	dir := filepath.Join(migrationRoot, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jrnlTMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func jrnlTWrite(t *testing.T, path, data string) {
	t.Helper()
	jrnlTMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jrnlTChmod(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func jrnlTChtimes(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func jrnlTSymlink(t *testing.T, target, link string) {
	t.Helper()
	jrnlTMkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// jrnlTEntry is what an observer can see of one directory entry without
// following a symlink. Access times are left out: reading may change them.
type jrnlTEntry struct {
	Type    fs.FileMode
	Perm    fs.FileMode
	ModTime time.Time
	Link    string
	Data    string
}

// jrnlTTree records every entry under root, root included.
func jrnlTTree(t *testing.T, root string) map[string]jrnlTEntry {
	t.Helper()
	tree := make(map[string]jrnlTEntry)
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		e := jrnlTEntry{Type: info.Mode().Type(), Perm: info.Mode().Perm(), ModTime: info.ModTime()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if e.Link, err = os.Readlink(path); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			e.Data = string(data)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		tree[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// jrnlTPublisher calls PublisherAllowed and turns a panic into a failure of
// the calling row only.
func jrnlTPublisher(t *testing.T, stateDir string, legacy bool) (allowed, ok bool, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("PublisherAllowed panicked: %v", r)
			ok = false
		}
	}()
	allowed, err = migrate.PublisherAllowed(stateDir, legacy)
	return allowed, true, err
}

// jrnlTHistory builds the home of one case: root is a private directory that
// holds the state directory and anything else the case needs.
type jrnlTHistory func(t *testing.T, root, stateDir string)

func TestPublisherAllowed(t *testing.T) {
	committed := jrnlTEvents(jrnlTKeepForward...)
	rolledBack := jrnlTEvents(slices.Concat(jrnlTKeepForward, jrnlTKeepRollback)...)
	migration := func(stateDir string) string { return filepath.Join(stateDir, "migration") }
	olderCommit := func(t *testing.T, stateDir string) {
		jrnlTJournal(t, migration(stateDir), jrnlTOlder, jrnlTKeepManifest, committed)
	}
	// pendingInstall writes a latest journal whose manifest names a stable
	// binary inside the case's own home, with its install intent open, and
	// puts the given bytes at that path.
	pendingInstall := func(binary string) jrnlTHistory {
		return func(t *testing.T, root, stateDir string) {
			home := filepath.Join(root, "home")
			manifest := strings.ReplaceAll(jrnlTKeepManifest, "/home/u", home)
			manifest = jrnlTEdit(t, manifest, jrnlTStableBefore, jrnlTOldBinaryDigest)
			manifest = jrnlTEdit(t, manifest, jrnlTStableAfter, jrnlTNewBinaryDigest)
			olderCommit(t, stateDir)
			jrnlTJournal(t, migration(stateDir), jrnlTLatest, manifest,
				jrnlTEvents(jrnlTEvent{1, "intent", "install", 1, jrnlTOldBinaryDigest, jrnlTNewBinaryDigest, ""}))
			stable := filepath.Join(home, ".claude", "model-gateway", "bin", "model-gateway")
			jrnlTWrite(t, stable, binary)
			jrnlTChmod(t, stable, 0o755)
			// An explicitly old mtime: a rewrite with the same bytes within the
			// filesystem's coarse timestamp tick would otherwise leave it as it was.
			jrnlTChtimes(t, stable, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC))
		}
	}
	// startWithoutStart is a --start journal committed with no start pair.
	startWithoutStart := jrnlTEvents(slices.Concat(jrnlTStartForward[:10], []jrnlTEvent{
		{11, "intent", "commit", 0, "", "", ""},
		{12, "done", "commit", 0, "", "", ""},
	})...)
	// stableAsPublish is a commit whose stable binary is recorded under the
	// launcher's opcode.
	stableAsPublish := jrnlTEvents(slices.Concat([]jrnlTEvent{
		{1, "intent", "publish", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
		{2, "done", "publish", 1, jrnlTStableBefore, jrnlTStableAfter, ""},
	}, jrnlTKeepForward[2:])...)
	// installWrongDigest is a commit whose install records name the
	// launcher's post image instead of the stable binary's.
	installWrongDigest := jrnlTEvents(slices.Concat([]jrnlTEvent{
		{1, "intent", "install", 1, jrnlTStableBefore, jrnlTLauncherAfter, ""},
		{2, "done", "install", 1, jrnlTStableBefore, jrnlTLauncherAfter, ""},
	}, jrnlTKeepForward[2:])...)
	// Directory mtimes that disagree with the order of the directory names.
	earlier := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		histories map[string]jrnlTHistory
		// want maps the legacy argument to the expected permission.
		want    map[bool]bool
		wantErr bool
	}{
		"error: history cannot be classified": {
			histories: map[string]jrnlTHistory{
				// Catches a predicate that skips an unreadable older journal
				// and trusts the latest commit.
				"older manifest unreadable beside a latest commit": func(t *testing.T, _, stateDir string) {
					jrnlTMkdir(t, filepath.Join(migration(stateDir), jrnlTOlder, "manifest.json"))
					jrnlTWrite(t, filepath.Join(migration(stateDir), jrnlTOlder, "events.jsonl"), "")
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed)
				},
				// Catches a predicate that falls back to an older commit when
				// the latest journal cannot be read.
				"latest manifest unreadable after an older commit": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTMkdir(t, filepath.Join(migration(stateDir), jrnlTLatest, "manifest.json"))
					jrnlTWrite(t, filepath.Join(migration(stateDir), jrnlTLatest, "events.jsonl"), "")
				},
				// Catches a predicate that inspects only the latest journal.
				"older journal with a damaged event line beside a latest commit": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTOlder, jrnlTKeepManifest,
						jrnlTEvents(jrnlTKeepForward[0])+"not json\n"+jrnlTEvents(jrnlTKeepForward[2]))
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed)
				},
				// Catches a predicate that treats a corrupt latest journal as
				// absent and uses the older commit.
				"latest manifest corrupt after an older commit": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest[:len(jrnlTKeepManifest)/2], "")
				},
				// Catches a predicate that picks one of several active
				// journals instead of failing closed.
				"two active journals after an older commit": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, jrnlTEvents(jrnlTKeepForward[0]))
					jrnlTJournal(t, migration(stateDir), jrnlTNewest, jrnlTKeepManifest, "")
				},
				// Catches a predicate that treats a first journal it cannot
				// parse as a fresh home.
				"sole journal with a truncated manifest": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest[:len(jrnlTKeepManifest)/2], "")
				},
				// Catches a predicate that reads a missing events file as
				// empty.
				"latest journal without events.jsonl after an older commit": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTWrite(t, filepath.Join(migration(stateDir), jrnlTLatest, "manifest.json"), jrnlTKeepManifest)
				},
				// Catches a predicate that skips a published directory whose
				// manifest is missing.
				"latest journal without manifest.json after an older commit": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTWrite(t, filepath.Join(migration(stateDir), jrnlTLatest, "events.jsonl"), committed)
				},
				// Catches a predicate that follows a symlinked manifest to
				// valid bytes.
				"latest manifest.json is a symlink to valid bytes": func(t *testing.T, root, stateDir string) {
					olderCommit(t, stateDir)
					elsewhere := filepath.Join(root, "elsewhere", "manifest.json")
					jrnlTWrite(t, elsewhere, jrnlTKeepManifest)
					jrnlTSymlink(t, elsewhere, filepath.Join(migration(stateDir), jrnlTLatest, "manifest.json"))
					jrnlTWrite(t, filepath.Join(migration(stateDir), jrnlTLatest, "events.jsonl"), committed)
				},
				// Catches a predicate that follows a symlinked events file to
				// valid bytes.
				"latest events.jsonl is a symlink to valid bytes": func(t *testing.T, root, stateDir string) {
					olderCommit(t, stateDir)
					elsewhere := filepath.Join(root, "elsewhere", "events.jsonl")
					jrnlTWrite(t, elsewhere, committed)
					jrnlTWrite(t, filepath.Join(migration(stateDir), jrnlTLatest, "manifest.json"), jrnlTKeepManifest)
					jrnlTSymlink(t, elsewhere, filepath.Join(migration(stateDir), jrnlTLatest, "events.jsonl"))
				},
				// Catches a predicate that accepts a journal name that is not a
				// real UTC time.
				"commit-shaped journal with an impossible timestamp beside a commit": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), "20261399T250000.000000001Z", jrnlTKeepManifest, committed)
				},
				// Catches a predicate that accepts journal files that are not
				// private.
				"committed journal whose events file is mode 0644": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed)
					jrnlTChmod(t, filepath.Join(migration(stateDir), jrnlTLatest, "events.jsonl"), 0o644)
				},
				// Catches a predicate that accepts a --start commit whose start
				// never ran.
				"sole start commit without a start": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTStartManifest, startWithoutStart)
				},
				// Catches a predicate that accepts the stable binary under the
				// launcher's opcode.
				"sole commit recording the stable binary under the publish opcode": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, stableAsPublish)
				},
				// Catches a predicate that accepts a commit whose install
				// records disagree with the manifest's stable images.
				"sole commit whose install records name the launcher's post image": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, installWrongDigest)
				},
				// Catches a predicate that treats a migration root it cannot
				// list as no history.
				"migration root is a regular file": func(t *testing.T, _, stateDir string) {
					jrnlTWrite(t, migration(stateDir), "not a directory\n")
				},
				// Catches a predicate that follows a substituted migration
				// root to a committed history.
				"migration root is a symlink to a committed history": func(t *testing.T, root, stateDir string) {
					elsewhere := filepath.Join(root, "elsewhere", "migration")
					jrnlTJournal(t, elsewhere, jrnlTLatest, jrnlTKeepManifest, committed)
					jrnlTMkdir(t, stateDir)
					jrnlTSymlink(t, elsewhere, migration(stateDir))
				},
				// Catches a predicate that follows a substituted journal
				// directory to a commit.
				"journal directory is a symlink to a committed journal": func(t *testing.T, root, stateDir string) {
					elsewhere := filepath.Join(root, "elsewhere")
					jrnlTJournal(t, elsewhere, jrnlTLatest, jrnlTKeepManifest, committed)
					jrnlTSymlink(t, filepath.Join(elsewhere, jrnlTLatest), filepath.Join(migration(stateDir), jrnlTLatest))
				},
			},
			want:    map[bool]bool{true: false, false: false},
			wantErr: true,
		},
		"success: latest commit permits a retained legacy install": {
			histories: map[string]jrnlTHistory{
				// Catches a predicate that consults the legacy install even
				// when the latest journal is committed.
				"one committed keep-stopped journal": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed)
				},
				// Catches a predicate that rejects a committed start journal.
				"one committed start journal": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTStartManifest, jrnlTEvents(jrnlTStartForward...))
				},
				// Catches a predicate that lets an older rollback veto a later
				// migration.
				"latest commit after an older rollback": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTOlder, jrnlTKeepManifest, rolledBack)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed)
				},
				// Catches a predicate that orders journals by mtime instead of
				// by name.
				"latest commit created first, older rollback holding the later mtime": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed)
					jrnlTJournal(t, migration(stateDir), jrnlTOlder, jrnlTKeepManifest, rolledBack)
					jrnlTChtimes(t, filepath.Join(migration(stateDir), jrnlTLatest), earlier)
					jrnlTChtimes(t, filepath.Join(migration(stateDir), jrnlTOlder), later)
				},
				// Catches a predicate that counts an unpublished preparation
				// as a journal.
				"latest commit beside a preparation leftover": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed)
					jrnlTWrite(t, filepath.Join(migration(stateDir), ".preparing-1", "manifest.json"), "{")
				},
			},
			want: map[bool]bool{true: true, false: true},
		},
		"success: latest active or rolled-back journal refuses": {
			histories: map[string]jrnlTHistory{
				// Catches a predicate that lets an older commit authorize
				// publication during a later forward migration.
				"latest forward journal with an open intent": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, jrnlTEvents(jrnlTKeepForward[:3]...))
				},
				// Catches a predicate that treats a journal with no events as
				// no history.
				"latest forward journal without events": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, "")
				},
				// Catches a predicate that ignores a rollback intent after the
				// commit.
				"latest commit followed by an operator reverse": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed+jrnlTEvents(jrnlTKeepRollback[:4]...))
				},
				// Catches a predicate that permits publication while an
				// automatic reverse is in progress.
				"latest journal in automatic reverse": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, jrnlTEvents(jrnlTAutoReverse[:8]...))
				},
				// Catches a predicate that reads an open commit intent as a
				// commit once it has been reversed.
				"latest journal reversing an open commit": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, jrnlTEvents(jrnlTOpenCommitReverse...))
				},
				// Catches a predicate that lets an older commit outlive a
				// later rollback.
				"latest journal rolled back": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, rolledBack)
				},
				// Catches a predicate that publishes over a torn tail after the
				// commit, or truncates it.
				"latest commit with a torn tail": func(t *testing.T, _, stateDir string) {
					olderCommit(t, stateDir)
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed+`{"seq":13,"action":"intent","op":"rev`)
				},
				// The sole-journal histories catch a predicate that treats a
				// first migration that is not committed as a fresh home.
				"sole journal with an open install": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, jrnlTEvents(jrnlTKeepForward[0]))
				},
				"sole journal with only a commit intent": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, jrnlTEvents(jrnlTKeepForward[:11]...))
				},
				"sole rolled-back journal": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, rolledBack)
				},
				"sole commit with a torn tail": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed+`{"seq":13,"action":"intent","op":"rev`)
				},
				// Catches a predicate that waits for the reverse done before
				// suppressing publication.
				"sole commit followed only by an operator reverse intent": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, committed+jrnlTEvents(jrnlTKeepRollback[0]))
				},
				// Catches a predicate that orders journals by mtime instead of
				// by name.
				"latest rollback created first, older commit holding the later mtime": func(t *testing.T, _, stateDir string) {
					jrnlTJournal(t, migration(stateDir), jrnlTLatest, jrnlTKeepManifest, rolledBack)
					jrnlTJournal(t, migration(stateDir), jrnlTOlder, jrnlTKeepManifest, committed)
					jrnlTChtimes(t, filepath.Join(migration(stateDir), jrnlTLatest), earlier)
					jrnlTChtimes(t, filepath.Join(migration(stateDir), jrnlTOlder), later)
				},
				// The three pending-install histories catch a predicate whose
				// answer depends on, or that repairs, the live image of a
				// pending effect: old, new and foreign bytes all refuse and
				// stay as they are.
				"pending install with the stable binary in its old image":   pendingInstall("old stable binary\n"),
				"pending install with the stable binary in its new image":   pendingInstall("new stable binary\n"),
				"pending install with the stable binary in a foreign image": pendingInstall("foreign stable binary\n"),
			},
			want: map[bool]bool{true: false, false: false},
		},
		"success: no history consults legacy installation": {
			histories: map[string]jrnlTHistory{
				// Catches a predicate that errors on a home that has no state
				// directory yet, or creates one.
				"state directory absent": func(*testing.T, string, string) {},
				// Catches a predicate that creates the migration root.
				"state directory without a migration root": func(t *testing.T, _, stateDir string) {
					jrnlTMkdir(t, stateDir)
				},
				// Catches a predicate that treats an empty migration root as
				// unclassifiable.
				"empty migration root": func(t *testing.T, _, stateDir string) {
					jrnlTMkdir(t, migration(stateDir))
				},
				// Catches a predicate that counts an interrupted preparation
				// as a published journal, or removes it.
				"migration root with only a preparation leftover": func(t *testing.T, _, stateDir string) {
					jrnlTWrite(t, filepath.Join(migration(stateDir), ".preparing-1", "manifest.json"), "{")
				},
			},
			want: map[bool]bool{true: false, false: true},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			for historyName, history := range tc.histories {
				t.Run(historyName, func(t *testing.T) {
					for _, legacy := range []bool{true, false} {
						t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
							root := t.TempDir()
							stateDir := filepath.Join(root, "state")
							history(t, root, stateDir)
							before := jrnlTTree(t, root)
							allowed, ok, err := jrnlTPublisher(t, stateDir, legacy)
							if ok {
								if tc.wantErr && err == nil {
									t.Errorf("PublisherAllowed(%q, %t) error = nil, want an error", stateDir, legacy)
								}
								if !tc.wantErr && err != nil {
									t.Errorf("PublisherAllowed(%q, %t) error = %v, want nil", stateDir, legacy, err)
								}
								if want := tc.want[legacy]; allowed != want {
									t.Errorf("PublisherAllowed(%q, %t) = %t, want %t", stateDir, legacy, allowed, want)
								}
							}
							if diff := gocmp.Diff(before, jrnlTTree(t, root)); diff != "" {
								t.Errorf("PublisherAllowed changed the filesystem (-before +after):\n%s", diff)
							}
						})
					}
				})
			}
		})
	}
}
