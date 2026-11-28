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

import "errors"

// ErrCandidate means a restart request did not name an acceptable worker executable.
//
// Every refusal of ParseRestart, RestartRule.Judge and RestartRule.AcceptVersion matches it, together with zero
// values for every other result; a cause may be wrapped beside it.
var ErrCandidate = errors.New("invalid restart candidate")

// ParseRestart reads one restart request body and returns the candidate path it names, or "" when it names none.
//
// The body must be exactly one JSON object, optionally surrounded by JSON white space, with valid UTF-8, no escaped
// lone surrogate and no two members of one object, at any depth, whose decoded names are equal. Only the members
// script and binary are read; each is an alias of the other, and when both are present their decoded strings must be
// equal. A candidate must be a non-empty absolute path without a NUL byte; it is returned verbatim. ParseRestart
// applies no size limit.
func ParseRestart(body []byte) (string, error) {
	panic("not implemented")
}

// RestartRule is what a restart candidate is judged against at one moment.
type RestartRule struct {
	// Configured is the launch choice of the worker executable the supervisor was started with.
	Configured Launch
	// Current is the last accepted choice: the next worker start launches it, and its version is the downgrade baseline.
	Current Launch
	// Stable is the stable binary as inspected for this request; an empty Image.Path means none is installed.
	Stable Executable
	// UID is the owner a stable candidate must have.
	UID int
}

// Judge decides a resolved candidate; when it reports that the version is needed, AcceptVersion completes the decision.
//
// Every candidate is refused while r.Current is not a valid launch with a valid version text. Otherwise a candidate that is the current choice's file returns the current choice itself, with its recorded path, and a
// candidate that is the configured executable's file returns the configured launch, when the predicate of that
// launch's authorisation and the version baseline allow it. A candidate that is the inspected stable binary, with the
// required owner and no group or other write permission, returns needVersion true and the zero Launch. Every other
// candidate is refused.
func (r RestartRule) Judge(c Executable) (l Launch, needVersion bool, err error) {
	panic("not implemented")
}

// AcceptVersion decides a stable candidate from its version output and a second inspection taken after the command ran.
//
// before must be a candidate for which Judge reports that the version is needed, after must equal before in every
// field, and output must be at most 4096 bytes holding exactly one valid version text followed by one line feed,
// not older than the current choice's version. The result is then a valid launch of before.Image with that text,
// authorised as a stable binary owned by r.UID.
func (r RestartRule) AcceptVersion(before Executable, output []byte, after Executable) (Launch, error) {
	panic("not implemented")
}
