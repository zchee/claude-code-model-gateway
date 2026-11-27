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

package ownership

import "github.com/zchee/claude-code-model-gateway/internal/procinfo"

// Store publishes, reads and conditionally removes the process records of one state root. Its operations exclude
// one another across processes, not only within one.
type Store struct {
	// Root is the clean absolute state root.
	Root string
	// beforeRemove, when the package's own tests set it on their store, runs inside RemoveIf after the stored record
	// has matched the expected one and before anything is unlinked, while the store's exclusion is held.
	beforeRemove func()
}

// Publish replaces the record name with r. A record without a start token is refused and nothing is written.
func (Store) Publish(name string, r procinfo.Record) error {
	panic("not implemented")
}

// Read returns the record name and repairs a stale or missing decimal mirror; a missing record is an error matching
// fs.ErrNotExist.
func (Store) Read(name string) (procinfo.Record, error) {
	panic("not implemented")
}

// RemoveIf removes the record name only while it names the expected process number, generation and start token,
// and reports whether it removed it.
func (Store) RemoveIf(name string, expected procinfo.Record) (bool, error) {
	panic("not implemented")
}
