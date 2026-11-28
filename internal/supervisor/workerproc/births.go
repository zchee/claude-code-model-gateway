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

import "context"

// Births stores the worker birth records of one state root in its worker-births directory.
//
// The record of a birth b is named after b.PID in decimal, a hyphen, the lowercase hexadecimal SHA-256 of b.StartToken
// and ".json", and holds exactly the encoding of b. Every method holds an exclusive advisory lock on the directory's
// births.lock file for its file operations. The directory must be owned by the effective user and grant no group or
// other permission; a directory that is not is refused, never repaired. On systems without Unix file operations
// every method returns an error matching errors.ErrUnsupported.
type Births struct {
	// Root is the clean absolute state root.
	Root string
}

// Stored is one entry of the birth directory as Read found it.
type Stored struct {
	// Name is the entry's name in the directory.
	Name string
	// Birth is the decoded record; it is meaningful only when Err is nil.
	Birth Birth
	// Err says why the entry is not a usable birth record, or is nil.
	Err error
}

// Record writes b as a new birth record, creating the private birth directory when it is missing, and reports whether this call committed the record's name.
//
// An existing entry of the record's name is left unchanged and gives an error matching fs.ErrExist with published
// false. published is true when the name was linked, also when a later cleanup, a release of the lock or a close of a
// directory failed; the caller keeps b for Retire exactly when published is true.
func (s Births) Record(ctx context.Context, b Birth) (published bool, err error) {
	panic("not implemented")
}

// Retire removes b's record only while it holds exactly b's encoding, and reports whether it removed it.
//
// A missing record, a record with other bytes and an entry that is not a regular file are left as they are. No other
// entry is ever touched.
func (s Births) Retire(ctx context.Context, b Birth) (bool, error) {
	panic("not implemented")
}

// Read returns every entry of the birth directory, each with its decoded birth or the reason it is not usable.
//
// The entries come in ascending byte order of their names, without the lock file. A missing directory gives no
// entries and no error.
func (s Births) Read(ctx context.Context) ([]Stored, error) {
	panic("not implemented")
}

// Signal calls send once while it holds the birth directory's exclusion, taken without waiting, and b's record still holds exactly b's encoding, and reports whether it called send.
//
// A lock held by another holder, or a record that is missing or changed, gives attempted false and send is not
// called. After an attempted send, err is send's error joined with any failure to release the lock or close a
// directory. The lock is released on every return, and a panic of send propagates after the release. send must
// neither wait nor call a method of Births: the exclusion is not reentrant.
func (s Births) Signal(ctx context.Context, b Birth, send func() error) (attempted bool, err error) {
	panic("not implemented")
}
