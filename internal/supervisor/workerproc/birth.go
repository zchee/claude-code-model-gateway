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

package workerproc

import (
	"errors"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// ErrBirth means the evidence does not make a complete, consistent, storable birth record of a supervised worker.
var ErrBirth = errors.New("incomplete or inconsistent worker birth")

// Birth identifies one supervised worker process, the executable it was launched from and the supervisor that started it.
//
// The encoding of a Birth is exactly what encoding/json/v2 Marshal produces for it with no options: compact, fields
// in declaration order, no trailing newline. It is the only form in which a birth is stored, compared or removed. A
// Birth is valid only when that encoding succeeds and has at most 65,536 bytes.
type Birth struct {
	// SchemaVersion is 1.
	SchemaVersion int `json:"schemaVersion"`
	// PID is the worker's process number.
	PID int `json:"pid"`
	// Argv is the worker's command line as sampled.
	Argv []string `json:"argv"`
	// StartedAt is the worker's birth in the record form procinfo.LegacyStart gives.
	StartedAt string `json:"startedAt"`
	// StartToken is the worker's process-start token.
	StartToken procinfo.StartToken `json:"startToken"`
	// StateRoot is the state root of the supervisor that started the worker.
	StateRoot string `json:"stateRoot"`
	// Generation is the worker's generation.
	Generation string `json:"generation"`
	// Version is the version of the launched executable, as it reported it.
	Version string `json:"version"`
	// Image is the launched executable.
	Image procinfo.Image `json:"image"`
	// Parent is the supervisor that started the worker.
	Parent BirthParent `json:"parent"`
}

// BirthParent identifies the supervisor that started a worker.
type BirthParent struct {
	// PID is the supervisor's process number.
	PID int `json:"pid"`
	// Argv is the supervisor's command line as sampled.
	Argv []string `json:"argv"`
	// StartedAt is the supervisor's birth in the record form procinfo.LegacyStart gives.
	StartedAt string `json:"startedAt"`
	// StartToken is the supervisor's process-start token.
	StartToken procinfo.StartToken `json:"startToken"`
}

// NewBirth builds the birth of worker, a child of parent launched from l, or returns ErrBirth when the evidence does not support one.
//
// Only the current child of the sampled supervisor gets a birth: worker.ParentPID must equal parent.PID. l must be a
// valid launch, both births must be non-zero, and a worker image read directly from the executable must show l's
// device and inode. The result copies both argv slices, so a later change to the caller's slices does not change it,
// and it is returned only when it is a valid birth of stateRoot.
func NewBirth(worker, parent procinfo.Identity, stateRoot, generation string, l Launch) (Birth, error) {
	panic("not implemented")
}
