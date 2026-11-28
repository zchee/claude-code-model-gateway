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

package modelcache_test

import (
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
)

const (
	discTBase      = "http://127.0.0.1:19999"
	discTFetchedAt = int64(1700000000123)
	discTFile      = "gateway-models.json"
	// discTReadBound is the size bound of the existing read of a discovery cache, 16 MiB.
	discTReadBound = 16 << 20

	// discTPairRows gives two gateway rows, one named and one not.
	discTPairRows = `[{"id":"zeta-claude-two","display_name":"Zeta"},{"id":"Vendor-CLAUDE-one"}]`
	// discTPairFile is the discovery cache for discTPairRows at discTBase and discTFetchedAt.
	discTPairFile = `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "zeta-claude-two",
      "display_name": "Zeta"
    },
    {
      "id": "Vendor-CLAUDE-one"
    }
  ]
}
`
	// discTPairCompact is a cache with the same IDs and names as discTPairFile, written compactly.
	discTPairCompact = `{"baseUrl":"http://127.0.0.1:19999","fetchedAt":1,"models":[{"id":"zeta-claude-two","display_name":"Zeta"},{"id":"Vendor-CLAUDE-one"}]}`
	// discTEmptyFile is the discovery cache for no gateway rows at discTBase and discTFetchedAt.
	discTEmptyFile = `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": []
}
`
)

// discTParent is the state of the cache file's directory before a write.
type discTParent int

const (
	// discTPrepared is an existing directory holding the private staging subdirectory.
	discTPrepared discTParent = iota
	// discTUnprepared is an existing directory without the private staging subdirectory.
	discTUnprepared
	// discTMissing is a directory that does not exist, nor does its parent.
	discTMissing
	// discTSymlinkStaging is an existing directory whose staging name is a symlink to a private
	// directory elsewhere.
	discTSymlinkStaging
	// discTGroupStaging is an existing directory whose staging subdirectory grants its group
	// read and search access.
	discTGroupStaging
)

// discTMove is one rename a discTRenameWatch saw: From is the name that left the staging
// directory and To the name that arrived in the destination directory, paired by the rename's
// cookie; a half without its partner leaves the other side empty.
type discTMove struct {
	From string
	To   string
}

// discTCappedResolver caps Codex at the unknown-window size, so its client IDs carry no [1m]
// suffix; the zero resolver advertises them with it.
func discTCappedResolver() policy.Resolver {
	return policy.NewResolver(policy.ResolverInputs{Windows: policy.Windows{"codex": {Value: config.ContextWindow{Tokens: 200000}, Source: "saved"}}})
}

// TestDiscoveryWriterParity decides the raw discovery cache writer: which precomposed rows it
// keeps and how it spells them, the exact bytes of the file, when an existing cache counts as
// unchanged, force, the bounded read of the existing cache, the result it returns, and where it
// may stage: a missing directory is created with its private staging subdirectory, an existing
// directory without one, or with a symlinked or group-accessible one, fails any write it needs
// without gaining an entry, and a write moves its file from that subdirectory into place. Modes
// are compared with the creation modes this process measures, so they hold under any umask. It
// decides no wiring, health or stand-in policy, which belong to the writer's callers. Every
// expected file is an independent byte image of the source writer's output,
// JSON.stringify(cache, null, 2) followed by LF.
func TestDiscoveryWriterParity(t *testing.T) {
	tests := map[string]struct {
		parent discTParent
		// emptyPath calls the writer with an empty cache path; name, when set, is the cache
		// file's name instead of discTFile.
		emptyPath bool
		name      string
		// existing, when set, is the cache an earlier run left, mode 0644; existingPadTo, when
		// set, pads it with spaces to that many bytes. existingDir instead puts a directory
		// holding one file at the cache path.
		existing      string
		existingPadTo int
		existingDir   bool
		rows          string
		resolver      policy.Resolver
		// baseURL and fetchedAt, when nil, are discTBase and discTFetchedAt.
		baseURL   *string
		fetchedAt *int64
		force     bool
		// want is the result of a call that succeeds; its CachePath is the path the call names.
		want modelcache.DiscoverySyncResult
		// wantFailure marks a call that must fail; wantErrText, when set, is its exact message.
		wantFailure bool
		wantErrText string
		// wantFile, when set, is the exact cache after the call; otherwise the call must leave
		// the existing cache, or its absence, as it was.
		wantFile string
	}{
		// Catches a writer that keeps every string ID, sorts the rows, keeps rows whose ID is
		// not a string, keeps unknown row members, or folds case beyond ASCII: Go's
		// strings.ToLower turns U+0130 into "i" and strings.ToUpper turns U+0131 into "I",
		// while the source's /(claude|anthropic)/i pattern without the u flag folds ASCII
		// letters only.
		"success: rows naming claude or anthropic in any ASCII case are kept in order with only id and display_name": {
			rows: `[{"id":"zeta-claude-two","display_name":"Zeta"},{"id":"gpt-5.6-sol","display_name":"Sol"},{"id":"Vendor-CLAUDE-one"},{"id":"x-AnThRoPiC-y","display_name":"Mixed"},{"id":42,"display_name":"claude number"},{"display_name":"claude without id"},null,"claude-bare-string",["claude-in-array"],{"id":"ANTHROPİC-dotted-capital","display_name":"Dotted"},{"id":"anthropıc-dotless","display_name":"Dotless"},{"id":"alpha-anthropic","display_name":"Alpha","contextWindow":1000000,"provider":"codex"}]`,
			want: modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 4},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "zeta-claude-two",
      "display_name": "Zeta"
    },
    {
      "id": "Vendor-CLAUDE-one"
    },
    {
      "id": "x-AnThRoPiC-y",
      "display_name": "Mixed"
    },
    {
      "id": "alpha-anthropic",
      "display_name": "Alpha"
    }
  ]
}
`,
		},
		// Catches a writer that drops an empty name, writes a name that is not a string, or
		// decodes a name to a Go string, which turns each unpaired surrogate into U+FFFD; and an
		// encoder that escapes HTML characters or non-ASCII text the source writes as they are.
		"success: a display name is written iff it is a string, empty and unpaired surrogates included": {
			rows: `[{"id":"names-claude-empty","display_name":""},{"id":"names-claude-surrogates","display_name":"\ud800x\udfff"},{"id":"names-claude-null","display_name":null},{"id":"names-claude-number","display_name":7},{"id":"names-claude-object","display_name":{"text":"no"}},{"id":"names-claude-text","display_name":"é東京😀 <b>&amp; \"q\" \\ /"}]`,
			want: modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 6},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "names-claude-empty",
      "display_name": ""
    },
    {
      "id": "names-claude-surrogates",
      "display_name": "\ud800x\udfff"
    },
    {
      "id": "names-claude-null"
    },
    {
      "id": "names-claude-number"
    },
    {
      "id": "names-claude-object"
    },
    {
      "id": "names-claude-text",
      "display_name": "é東京😀 <b>&amp; \"q\" \\ /"
    }
  ]
}
`,
		},
		// Catches a writer that writes the row's ID as given instead of the supplied resolver's
		// client ID.
		"success: each ID is written as the client ID of the zero resolver": {
			rows: `[{"id":"claude-gpt-5.6-sol","display_name":"Sol"}]`,
			want: modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 1},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "claude-gpt-5.6-sol[1m]",
      "display_name": "Sol"
    }
  ]
}
`,
		},
		// Catches a writer that resolves client IDs with a resolver of its own instead of the
		// one it is given.
		"success: each ID is written as the client ID of the supplied resolver": {
			rows:     `[{"id":"claude-gpt-5.6-sol","display_name":"Sol"}]`,
			resolver: discTCappedResolver(),
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 1},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "claude-gpt-5.6-sol",
      "display_name": "Sol"
    }
  ]
}
`,
		},
		// Catches a writer that checks the base URL against the gateway's wiring or replaces
		// it: the raw writer writes the URL its caller chose.
		"success: the base URL is written as given": {
			rows:    `[]`,
			baseURL: new("https://gateway.example.invalid/v1"),
			want:    modelcache.DiscoverySyncResult{State: "wrote"},
			wantFile: `{
  "baseUrl": "https://gateway.example.invalid/v1",
  "fetchedAt": 1700000000123,
  "models": []
}
`,
		},
		// Catches a writer that compares the base URL, fetchedAt, unknown members or the
		// serialized bytes instead of each model's ID and display name by index.
		"success: a cache with the same IDs and names under another baseUrl and fetchedAt is unchanged": {
			existing: `{"other":true,"models":[{"id":"zeta-claude-two","extra":[1],"display_name":"Zeta"},{"id":"Vendor-CLAUDE-one","contextWindow":5}],"fetchedAt":5,"baseUrl":"http://old.invalid"}`,
			rows:     discTPairRows,
			want:     modelcache.DiscoverySyncResult{State: "unchanged", ModelCount: 2},
		},
		// Catches a writer that ignores force: an equal cache is rewritten with the new
		// fetchedAt.
		"success: force rewrites a cache with the same IDs and names": {
			existing: discTPairCompact,
			rows:     discTPairRows,
			force:    true,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: discTPairFile,
		},
		// Catches a writer that compares the models as a set.
		"success: a cache with the same models in another order is rewritten": {
			existing: `{"baseUrl":"http://127.0.0.1:19999","fetchedAt":1,"models":[{"id":"Vendor-CLAUDE-one"},{"id":"zeta-claude-two","display_name":"Zeta"}]}`,
			rows:     discTPairRows,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: discTPairFile,
		},
		// Catches a writer that compares only the leading models.
		"success: a cache with one more model is rewritten": {
			existing: `{"baseUrl":"http://127.0.0.1:19999","fetchedAt":1,"models":[{"id":"zeta-claude-two","display_name":"Zeta"},{"id":"Vendor-CLAUDE-one"},{"id":"third-claude"}]}`,
			rows:     discTPairRows,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: discTPairFile,
		},
		// Catches a writer that treats a null display name as an absent one: the source
		// compares null with undefined strictly.
		"success: a cached null display name differs from no display name": {
			existing: `{"baseUrl":"http://127.0.0.1:19999","fetchedAt":1,"models":[{"id":"zeta-claude-two","display_name":"Zeta"},{"id":"Vendor-CLAUDE-one","display_name":null}]}`,
			rows:     discTPairRows,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: discTPairFile,
		},
		// Catches a writer that treats an empty display name as an absent one.
		"success: a cached absent display name differs from an empty one": {
			existing: `{"models":[{"id":"names-claude-empty"}]}`,
			rows:     `[{"id":"names-claude-empty","display_name":""}]`,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 1},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "names-claude-empty",
      "display_name": ""
    }
  ]
}
`,
		},
		// Catches a writer that compares names as Go strings, in which an unpaired surrogate
		// and U+FFFD are the same text.
		"success: a cached U+FFFD differs from an unpaired surrogate": {
			existing: `{"models":[{"id":"names-claude-lone","display_name":"�"}]}`,
			rows:     `[{"id":"names-claude-lone","display_name":"\ud800"}]`,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 1},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "names-claude-lone",
      "display_name": "\ud800"
    }
  ]
}
`,
		},
		// Catches a writer that cannot match a cached unpaired surrogate and rewrites it.
		"success: a cached unpaired surrogate equals the same surrogate": {
			existing: `{"models":[{"id":"names-claude-lone","display_name":"\ud800"}]}`,
			rows:     `[{"id":"names-claude-lone","display_name":"\ud800"}]`,
			want:     modelcache.DiscoverySyncResult{State: "unchanged", ModelCount: 1},
		},
		// Catches a writer that compares the rows before it turns their IDs into client IDs.
		"success: the cache is compared with the client IDs, not the row IDs": {
			existing: `{"models":[{"id":"claude-gpt-5.6-sol","display_name":"Sol"}]}`,
			rows:     `[{"id":"claude-gpt-5.6-sol","display_name":"Sol"}]`,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 1},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "claude-gpt-5.6-sol[1m]",
      "display_name": "Sol"
    }
  ]
}
`,
		},
		// The control of the row above: the cache that holds the client ID is current.
		"success: a cache holding the client IDs is unchanged": {
			existing: `{"models":[{"id":"claude-gpt-5.6-sol[1m]","display_name":"Sol"}]}`,
			rows:     `[{"id":"claude-gpt-5.6-sol","display_name":"Sol"}]`,
			want:     modelcache.DiscoverySyncResult{State: "unchanged", ModelCount: 1},
		},
		// Catches a writer that fails, or keeps the old file, when the existing cache does not
		// parse.
		"success: an unparseable cache is replaced": {
			existing: `{"models":[`,
			rows:     discTPairRows,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: discTPairFile,
		},
		// Catches a writer that takes a cache whose models member is not an array for an empty
		// list.
		"success: a cache whose models is an object is replaced even with no rows": {
			existing: `{"models":{}}`,
			rows:     `[]`,
			want:     modelcache.DiscoverySyncResult{State: "wrote"},
			wantFile: discTEmptyFile,
		},
		// Catches a writer that takes a cache without a models member for an empty list.
		"success: a cache without models is replaced even with no rows": {
			existing: `{"baseUrl":"http://127.0.0.1:19999","fetchedAt":1}`,
			rows:     `[]`,
			want:     modelcache.DiscoverySyncResult{State: "wrote"},
			wantFile: discTEmptyFile,
		},
		// The control of the two rows above.
		"success: an empty cache with no rows is unchanged": {
			existing: `{"models":[]}`,
			rows:     `[]`,
			want:     modelcache.DiscoverySyncResult{State: "unchanged"},
		},
		// With the next row, catches a writer that reads the existing cache without its bound:
		// a cache at the bound is read and found equal.
		"success: an equal cache of exactly the read bound is unchanged": {
			existing:      discTPairCompact,
			existingPadTo: discTReadBound,
			rows:          discTPairRows,
			want:          modelcache.DiscoverySyncResult{State: "unchanged", ModelCount: 2},
		},
		// Catches a writer that reads the existing cache without its bound, which finds this
		// cache equal: a cache one byte over the bound is not read, so it is not current and is
		// replaced.
		"success: an equal cache one byte over the read bound is replaced": {
			existing:      discTPairCompact,
			existingPadTo: discTReadBound + 1,
			rows:          discTPairRows,
			want:          modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile:      discTPairFile,
		},
		// Catches a writer that creates the directory without its private staging
		// subdirectory, or that cannot write into a directory it created.
		"success: a missing directory is created with its staging subdirectory": {
			parent:   discTMissing,
			rows:     discTPairRows,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: discTPairFile,
		},
		// Catches a writer that stages beside the file, or creates the staging subdirectory
		// in a directory a reader may already watch.
		"error: a needed write into an existing directory without its staging subdirectory fails": {
			parent:      discTUnprepared,
			rows:        discTPairRows,
			wantFailure: true,
		},
		// Catches a writer that stages beside the file it replaces, or creates the staging
		// subdirectory first, when an older cache is already in the directory.
		"error: a needed replacement in an existing directory without its staging subdirectory fails": {
			parent:      discTUnprepared,
			existing:    `{"models":[{"id":"Vendor-CLAUDE-one"}]}`,
			rows:        discTPairRows,
			wantFailure: true,
		},
		// Catches a writer that refuses an unprepared directory before it knows a write is
		// needed.
		"success: an equal cache in an existing directory without its staging subdirectory is unchanged": {
			parent:   discTUnprepared,
			existing: discTPairCompact,
			rows:     discTPairRows,
			want:     modelcache.DiscoverySyncResult{State: "unchanged", ModelCount: 2},
		},
		// Catches a writer that answers a forced write with unchanged.
		"error: a forced write into an existing directory without its staging subdirectory fails": {
			parent:      discTUnprepared,
			existing:    discTPairCompact,
			rows:        discTPairRows,
			force:       true,
			wantFailure: true,
		},
		// Catches a writer that words the refusal of an empty path differently, or writes
		// somewhere for it.
		"error: an empty cache path is refused": {
			emptyPath:   true,
			rows:        discTPairRows,
			wantFailure: true,
			wantErrText: "modelcache: the discovery cache path is unavailable",
		},
		// Catches a writer that writes a fixed file name, or a timestamp of its own, instead of
		// the path and fetchedAt it is given.
		"success: a forced write uses the given file name and timestamp": {
			name:      "picker-rows.json",
			existing:  discTPairCompact,
			rows:      discTPairRows,
			fetchedAt: new(int64(1700000009999)),
			force:     true,
			want:      modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000009999,
  "models": [
    {
      "id": "zeta-claude-two",
      "display_name": "Zeta"
    },
    {
      "id": "Vendor-CLAUDE-one"
    }
  ]
}
`,
		},
		// Catches a writer that keeps a wiring gate for an empty URL: the raw writer writes the
		// URL its caller chose, even an empty one.
		"success: an explicitly empty base URL is written as given": {
			rows:    `[]`,
			baseURL: new(""),
			want:    modelcache.DiscoverySyncResult{State: "wrote"},
			wantFile: `{
  "baseUrl": "",
  "fetchedAt": 1700000000123,
  "models": []
}
`,
		},
		// Catches a writer that keeps the source's skip of the retired compatibility URL, which
		// belongs to its callers.
		"success: the retired compatibility URL is written as given": {
			rows:    `[]`,
			baseURL: new("http://api.anthropic.com"),
			want:    modelcache.DiscoverySyncResult{State: "wrote"},
			wantFile: `{
  "baseUrl": "http://api.anthropic.com",
  "fetchedAt": 1700000000123,
  "models": []
}
`,
		},
		// Catches a writer that removes rows with an ID it already wrote.
		"success: two rows with one ID are both kept with their names": {
			rows: `[{"id":"dup-claude","display_name":"First"},{"id":"dup-claude","display_name":"Second"}]`,
			want: modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "dup-claude",
      "display_name": "First"
    },
    {
      "id": "dup-claude",
      "display_name": "Second"
    }
  ]
}
`,
		},
		// Catches a writer that compares names without case.
		"success: a cached name that differs only in case is rewritten": {
			existing: `{"models":[{"id":"case-claude","display_name":"Name"}]}`,
			rows:     `[{"id":"case-claude","display_name":"NAME"}]`,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 1},
			wantFile: `{
  "baseUrl": "http://127.0.0.1:19999",
  "fetchedAt": 1700000000123,
  "models": [
    {
      "id": "case-claude",
      "display_name": "NAME"
    }
  ]
}
`,
		},
		// Catches a writer that compares the cached JSON spelling instead of the decoded ID and
		// name: the cached ID and name spell one letter each as a \u escape, which decodes
		// to the same text as the plain incoming row.
		"success: a cache spelling the same ID and name with escapes is unchanged": {
			existing: `{"models":[{"id":"case-\u0063laude","display_name":"\u004eame"}]}`,
			rows:     `[{"id":"case-claude","display_name":"Name"}]`,
			want:     modelcache.DiscoverySyncResult{State: "unchanged", ModelCount: 1},
		},
		// Catches a writer that answers force with wrote but keeps a cache whose bytes already
		// equal its output: the file is replaced by a new one.
		"success: force replaces a cache that already holds the exact output": {
			existing: discTPairFile,
			rows:     discTPairRows,
			force:    true,
			want:     modelcache.DiscoverySyncResult{State: "wrote", ModelCount: 2},
			wantFile: discTPairFile,
		},
		// Catches a writer that takes rows it cannot parse for an empty list and replaces the
		// cache with it.
		"error: malformed rows fail and keep the cache": {
			existing:    discTPairCompact,
			rows:        `[{"id":"broken-claude"`,
			wantFailure: true,
		},
		// Catches a writer that reports a publication it could not make: the rename onto a
		// nonempty directory fails, which must be returned with the directory kept and nothing
		// left in staging.
		"error: a directory at the cache path fails the write and keeps its contents": {
			existingDir: true,
			rows:        discTPairRows,
			wantFailure: true,
		},
		// Catches a writer that follows a symlink at the staging name: the staging subdirectory
		// must be a real directory.
		"error: a symlinked staging directory fails a needed write": {
			parent:      discTSymlinkStaging,
			existing:    `{"models":[{"id":"Vendor-CLAUDE-one"}]}`,
			rows:        discTPairRows,
			wantFailure: true,
		},
		// Catches a writer that stages in a staging subdirectory its group can read.
		"error: a group-accessible staging directory fails a needed write": {
			parent:      discTGroupStaging,
			existing:    `{"models":[{"id":"Vendor-CLAUDE-one"}]}`,
			rows:        discTPairRows,
			wantFailure: true,
		},
	}
	noteUnobservable := sync.OnceFunc(func() {
		t.Log("the changes of a directory's entries are not observed on this system: observing them needs Linux inotify")
	})
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			fileMode, dirMode := discTCreationModes(t, root)
			// The fixtures need the owner's own bits; a umask that removes them is not supported.
			if fileMode&0o600 != 0o600 || dirMode&0o700 != 0o700 {
				t.Fatalf("fixture: unsupported umask: a new 0600 file gets %v and a new 0777 directory %v", fileMode, dirMode)
			}
			dir := filepath.Join(root, "cache")
			if tt.parent == discTMissing {
				dir = filepath.Join(root, "home", "cache")
			}
			staging := filepath.Join(dir, atomicfile.StagingDirName)
			// elsewhere is the private directory a symlinked staging name points to.
			elsewhere := filepath.Join(root, "elsewhere")
			if tt.parent != discTMissing {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			switch tt.parent {
			case discTPrepared:
				if err := atomicfile.CreateStagingDir(dir, 0o777); err != nil {
					t.Fatalf("prepare the staging subdirectory of %s: %v", dir, err)
				}
			case discTSymlinkStaging:
				if err := os.Mkdir(elsewhere, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(elsewhere, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, staging); err != nil {
					t.Fatal(err)
				}
			case discTGroupStaging:
				if err := os.Mkdir(staging, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(staging, 0o750); err != nil {
					t.Fatal(err)
				}
				if info, err := os.Lstat(staging); err != nil {
					t.Fatal(err)
				} else if info.Mode().Perm()&0o070 != 0o050 {
					t.Fatalf("fixture: the filesystem ignored chmod: %s has mode %v after chmod 0750", staging, info.Mode().Perm())
				}
			}
			fileName := cmp.Or(tt.name, discTFile)
			path := filepath.Join(dir, fileName)
			var existing []byte
			// old holds the earlier cache open, so its identity stays allocated and a
			// replacement is told from a rewrite in place on every system.
			var old *os.File
			// oldDir holds the directory at the cache path open for the same reason.
			var oldDir *os.File
			switch {
			case tt.existingDir:
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "keep"), []byte("kept"), 0o600); err != nil {
					t.Fatal(err)
				}
				d, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = d.Close() })
				oldDir = d
			case tt.existing != "":
				existing = []byte(tt.existing)
				if pad := tt.existingPadTo - len(existing); pad > 0 {
					existing = append(existing, bytes.Repeat([]byte(" "), pad)...)
				}
				if err := os.WriteFile(path, existing, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				old = f
			}
			before := discTEntries(t, dir)
			var watch *cacheDirWatch
			var renames *discTRenameWatch
			observable, renamesObservable := false, false
			if tt.parent != discTMissing {
				watch, observable = watchCacheDir(t, dir)
			}
			if tt.parent == discTPrepared {
				renames, renamesObservable = discTWatchRenames(t, staging, dir)
			}
			callPath := path
			if tt.emptyPath {
				callPath = ""
			}
			baseURL, fetchedAt := discTBase, discTFetchedAt
			if tt.baseURL != nil {
				baseURL = *tt.baseURL
			}
			if tt.fetchedAt != nil {
				fetchedAt = *tt.fetchedAt
			}

			got, completed, err := discTWrite(t, callPath, baseURL, jsontext.Value(tt.rows), tt.resolver, fetchedAt, tt.force)
			if !completed {
				return
			}

			if tt.wantFailure {
				switch {
				case err == nil:
					t.Errorf("WriteDiscoveryRows = %+v, want an error", got)
				case tt.wantErrText != "" && err.Error() != tt.wantErrText:
					t.Errorf("WriteDiscoveryRows error = %q, want %q", err.Error(), tt.wantErrText)
				}
				if got.State == "wrote" || got.State == "unchanged" {
					t.Errorf("WriteDiscoveryRows failed with state %q, want no success state", got.State)
				}
				if got.CachePath != callPath {
					t.Errorf("WriteDiscoveryRows failed with CachePath %q, want %q", got.CachePath, callPath)
				}
			} else {
				if err != nil {
					t.Errorf("WriteDiscoveryRows: %v", err)
				}
				want := tt.want
				want.CachePath = callPath
				if d := gocmp.Diff(want, got); d != "" {
					t.Errorf("WriteDiscoveryRows result (-want +got):\n%s", d)
				}
			}

			// The watches are drained before the test touches anything, so every change they
			// report is the writer's.
			switch {
			case tt.parent == discTMissing:
				// The directory did not exist when the call began, so nothing watched it.
			case !observable:
				noteUnobservable()
			default:
				// A reader watching the directory sees, for a write, only the finished file
				// renamed into place: no staging name, no staging directory created, no change
				// after the rename such as a mode change. A call that writes nothing changes
				// nothing.
				var wantEvents []cacheDirEvent
				if tt.wantFile != "" {
					wantEvents = arrivedWhole(fileName)
				}
				if d := gocmp.Diff(wantEvents, watch.events(t)); d != "" {
					t.Errorf("changes a watcher of %s saw (-want +got):\n%s", dir, d)
				}
			}
			if renamesObservable {
				// The file that arrives must be the one that left the private staging
				// subdirectory, by the same rename: a writer that stages anywhere else makes
				// the arrival without a matching departure. The staging name is not fixed.
				moves := renames.moves(t)
				switch {
				case tt.wantFile == "" && len(moves) != 0:
					t.Errorf("renames between the staging subdirectory and %s for a call that wrote nothing: %+v, want none", dir, moves)
				case tt.wantFile != "" && (len(moves) != 1 || moves[0].From == "" || moves[0].To != fileName):
					t.Errorf("renames between the staging subdirectory and %s: %+v, want exactly one rename from a staging name to %s", dir, moves, fileName)
				}
			}

			switch {
			case tt.wantFile != "":
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("stat the written cache: %v", err)
				}
				// Anything but a regular file fails the row before it is changed or read: a
				// FIFO would block the read.
				if !info.Mode().IsRegular() {
					t.Fatalf("the written cache is %v, want a regular file", info.Mode())
				}
				// The mode is the one this process gives a new 0600 file, so it holds under
				// any umask; the bytes are checked apart from it.
				if info.Mode().Perm() != fileMode {
					t.Errorf("written cache mode %v, want the creation mode %v of a new 0600 file", info.Mode().Perm(), fileMode)
				}
				if old != nil {
					if oldInfo, err := old.Stat(); err != nil {
						t.Errorf("stat the earlier cache: %v", err)
					} else if os.SameFile(oldInfo, info) {
						t.Errorf("the earlier cache is still the file at %s: the write did not replace it", path)
					}
				}
				// A wrong writer can leave the file without the owner's read bit; the mode check
				// above has already failed the row then, and the test grants itself read access
				// so that the byte check still reports on its own.
				if perm := info.Mode().Perm(); perm&0o400 == 0 {
					if err := os.Chmod(path, perm|0o400); err != nil {
						t.Fatal(err)
					}
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read the written cache: %v", err)
				}
				if d := gocmp.Diff(tt.wantFile, string(raw)); d != "" {
					t.Errorf("written cache bytes (-want +got):\n%s", d)
				}
				// The write leaves the directory with its staging subdirectory, private and
				// empty, and the cache file, and nothing else.
				if d := gocmp.Diff([]string{atomicfile.StagingDirName, fileName}, discTEntries(t, dir)); d != "" {
					t.Errorf("entries of %s after the write (-want +got):\n%s", dir, d)
				}
				if info, err := os.Lstat(staging); err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
					t.Errorf("staging subdirectory %s: %v, want a directory that grants nothing to group or others (error %v)", staging, info, err)
				}
				if d := gocmp.Diff([]string{}, discTEntries(t, staging)); d != "" {
					t.Errorf("entries of the staging subdirectory after the write (-want +got):\n%s", d)
				}
				if tt.parent == discTMissing {
					// Both missing directories get the mode this process gives a new 0777
					// directory. Where that measured mode is itself 0700, a writer that creates
					// them private cannot be told apart here; a controlled umask is a matter for
					// a child process.
					for _, created := range []string{filepath.Dir(dir), dir} {
						if info, err := os.Lstat(created); err != nil || !info.IsDir() || info.Mode().Perm() != dirMode {
							t.Errorf("created directory %s: %v, want a directory with the creation mode %v of a new 0777 directory (error %v)", created, info, dirMode, err)
						}
					}
				}
			case tt.existingDir:
				// The original directory is still at the path, with its one file and that
				// file's bytes.
				if info, err := os.Lstat(path); err != nil {
					t.Fatalf("stat the cache path: %v", err)
				} else if oldInfo, err := oldDir.Stat(); err != nil {
					t.Fatalf("stat the earlier directory: %v", err)
				} else if !os.SameFile(oldInfo, info) {
					t.Fatalf("the directory at %s was replaced, want the original kept", path)
				}
				if d := gocmp.Diff([]string{"keep"}, discTEntries(t, path)); d != "" {
					t.Errorf("entries of the directory at the cache path (-want +got):\n%s", d)
				}
				// The file is checked before it is read: a symlink, even to the same bytes, or a
				// FIFO fails the row at once.
				sentinel := filepath.Join(path, "keep")
				if info, err := os.Lstat(sentinel); err != nil {
					t.Fatalf("stat the file in the directory at the cache path: %v", err)
				} else if !info.Mode().IsRegular() {
					t.Fatalf("the file in the directory at the cache path is now %v, want the original regular file", info.Mode())
				}
				if raw, err := os.ReadFile(sentinel); err != nil {
					t.Errorf("read the file in the directory at the cache path: %v", err)
				} else if d := gocmp.Diff("kept", string(raw)); d != "" {
					t.Errorf("bytes of the file in the directory at the cache path (-want +got):\n%s", d)
				}
			case existing != nil:
				// The leaf is checked before it is read: anything but the original regular file
				// fails the row at once.
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("stat the existing cache: %v", err)
				}
				if !info.Mode().IsRegular() {
					t.Fatalf("the existing cache is now %v, want the original regular file", info.Mode())
				}
				if oldInfo, err := old.Stat(); err != nil {
					t.Fatalf("stat the earlier cache: %v", err)
				} else if !os.SameFile(oldInfo, info) {
					t.Fatalf("the cache at %s was replaced, want it kept", path)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read the existing cache: %v", err)
				}
				if !bytes.Equal(existing, raw) {
					t.Errorf("the existing cache changed:\n%s\nwant it kept:\n%s", discTHead(raw), discTHead(existing))
				}
			default:
				// Absence is checked without opening the path, so a FIFO left there fails the
				// row instead of blocking it.
				if info, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("cache after the call: %v (error %v), want it absent", info, err)
				}
			}
			if tt.wantFile == "" {
				if d := gocmp.Diff(before, discTEntries(t, dir)); d != "" {
					t.Errorf("entries of %s changed by a call that wrote nothing (-before +after):\n%s", dir, d)
				}
				// A failed or unneeded write leaves nothing staged.
				stage := staging
				if tt.parent == discTSymlinkStaging {
					stage = elsewhere
				}
				if tt.parent != discTMissing && tt.parent != discTUnprepared {
					if d := gocmp.Diff([]string{}, discTEntries(t, stage)); d != "" {
						t.Errorf("entries of %s after a call that wrote nothing (-want +got):\n%s", stage, d)
					}
				}
			}
		})
	}
}

// discTCreationModes measures, with probes in root, the modes this process gives a new 0600
// file and a new 0777 directory, so a row's expectations follow the runner's umask without
// changing it.
func discTCreationModes(t *testing.T, root string) (file, dir fs.FileMode) {
	t.Helper()
	probe := filepath.Join(root, "probe-file")
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err = errors.Join(err, f.Close(), os.Remove(probe)); err != nil {
		t.Fatal(err)
	}
	file = info.Mode().Perm()
	probe = filepath.Join(root, "probe-dir")
	if err := os.Mkdir(probe, 0o777); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(probe)
	if err = errors.Join(err, os.Remove(probe)); err != nil {
		t.Fatal(err)
	}
	return file, info.Mode().Perm()
}

// discTWrite calls WriteDiscoveryRows. A panic fails the calling row and returns completed
// false, apart from any error the writer returns, so each row fails on its own while the writer
// is not implemented and the package's other tests still run.
func discTWrite(t *testing.T, path, baseURL string, rows jsontext.Value, resolver policy.Resolver, fetchedAt int64, force bool) (result modelcache.DiscoverySyncResult, completed bool, err error) {
	t.Helper()
	defer func() {
		if v := recover(); v != nil {
			t.Errorf("WriteDiscoveryRows panicked: %v", v)
			completed = false
		}
	}()
	result, err = modelcache.WriteDiscoveryRows(path, baseURL, rows, resolver, fetchedAt, force)
	return result, true, err
}

// discTEntries returns the names in dir, which os.ReadDir sorts, or nil when dir does not exist.
func discTEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// discTHead returns at most the first 512 bytes of data, for a failure message about a file that
// may be as large as the read bound.
func discTHead(data []byte) string {
	return string(data[:min(len(data), 512)])
}
