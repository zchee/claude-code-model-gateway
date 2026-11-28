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

package migrate

// JournalState describes the replayed prefix without performing recovery.
type JournalState struct {
	Phase, EndState, ProjectDir, PendingOp string
	PendingItem                            uint64
	ValidBytes                             int
	Torn                                   bool
}

// InspectJournal validates the manifest and complete events preceding a possible torn tail.
func InspectJournal(manifest, events []byte) (JournalState, error) { panic("not implemented") }

// PublisherAllowed inspects legacy state and journal history without changing either.
func PublisherAllowed(stateDir string, legacy bool) (bool, error) { panic("not implemented") }
