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
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// ErrCandidate means a restart request did not name an acceptable worker executable.
//
// Every refusal of ParseRestart, RestartRule.Judge and RestartRule.AcceptVersion matches it, together with zero
// values for every other result; a cause may be wrapped beside it.
var ErrCandidate = errors.New("invalid restart candidate")

// maxVersionOutput is the most standard output, line end included, that the version command of a stable candidate
// may print.
const maxVersionOutput = 4096

// refuseCandidate returns an error matching ErrCandidate that says why a candidate was refused.
func refuseCandidate(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCandidate, fmt.Sprintf(format, args...))
}

// ParseRestart reads one restart request body and returns the candidate path it names, or "" when it names none.
//
// The body must be exactly one JSON object, optionally surrounded by JSON white space, with valid UTF-8, no escaped
// lone surrogate and no two members of one object, at any depth, whose decoded names are equal. Only the members
// script and binary are read; each is an alias of the other, and when both are present their decoded strings must be
// equal. A candidate must be a non-empty absolute path without a NUL byte; it is returned verbatim. ParseRestart
// applies no size limit. The nesting limit of the JSON decoder applies: a body nested deeper than it is refused as
// malformed.
func ParseRestart(body []byte) (string, error) {
	// The decoder's own checks refuse invalid UTF-8, an escaped lone surrogate and a repeated decoded member name in
	// any object of the body; both options are their defaults and are spelled out because the request depends on them.
	// A bytes.Buffer is parsed in place, without a copy, and the decoder does not write to its bytes.
	dec := jsontext.NewDecoder(bytes.NewBuffer(body), jsontext.AllowDuplicateNames(false), jsontext.AllowInvalidUTF8(false))
	start, err := dec.ReadToken()
	switch {
	case errors.Is(err, io.EOF):
		return "", refuseCandidate("the body holds no JSON value")
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrCandidate, err)
	case start.Kind() != '{':
		return "", refuseCandidate("the body is a JSON %v, not an object", start.Kind())
	}
	var script, binary string
	var hasScript, hasBinary bool
	for dec.PeekKind() != '}' {
		nameToken, err := dec.ReadToken()
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrCandidate, err)
		}
		name := nameToken.String()
		if name != "script" && name != "binary" {
			if err := dec.SkipValue(); err != nil {
				return "", fmt.Errorf("%w: %w", ErrCandidate, err)
			}
			continue
		}
		if kind := dec.PeekKind(); kind != '"' {
			return "", refuseCandidate("member %q is not a JSON string (kind %v)", name, kind)
		}
		valueToken, err := dec.ReadToken()
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrCandidate, err)
		}
		if name == "script" {
			script, hasScript = valueToken.String(), true
		} else {
			binary, hasBinary = valueToken.String(), true
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrCandidate, err)
	}
	if extra, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrCandidate, err)
		}
		return "", refuseCandidate("a JSON %v follows the object", extra.Kind())
	}
	switch {
	case !hasScript && !hasBinary:
		return "", nil
	case !hasScript:
		script = binary
	case hasBinary && binary != script:
		return "", refuseCandidate("script %q and binary %q name different candidates", script, binary)
	}
	if !filepath.IsAbs(script) || strings.ContainsRune(script, 0) {
		return "", refuseCandidate("candidate %q is not an absolute path without a NUL byte", script)
	}
	return script, nil
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
	baseline, ok := parseVersion(r.Current.version)
	if !r.Current.valid() || !ok {
		return Launch{}, false, refuseCandidate("the current choice is not a valid launch with a valid version (version %q)", r.Current.version)
	}
	if !validPath(c.Image.Path) || c.Image.Inode == 0 || !c.Mode.IsRegular() {
		return Launch{}, false, refuseCandidate("candidate %q (inode %d, mode %v) is not a regular file at an absolute, clean path", c.Image.Path, c.Image.Inode, c.Mode)
	}
	switch {
	case sameFile(c, r.Current.image):
		// The current choice is returned with its recorded path, so another name of the same file does not replace it.
		if !r.Current.permits(c) {
			return Launch{}, false, refuseCandidate("candidate %q is the current choice but has owner %d and mode %v", c.Image.Path, c.UID, c.Mode)
		}
		return r.Current, false, nil
	case sameFile(c, r.Configured.image):
		configured, ok := parseVersion(r.Configured.version)
		switch {
		case r.Configured.auth != authConfigured || !r.Configured.valid():
			return Launch{}, false, refuseCandidate("candidate %q is the file of a configured launch that is not authorised as the configured worker executable", c.Image.Path)
		case !ok:
			return Launch{}, false, refuseCandidate("candidate %q is the configured worker executable, whose version %q is not valid", c.Image.Path, r.Configured.version)
		case configured.olderThan(baseline):
			return Launch{}, false, refuseCandidate("candidate %q is the configured worker executable at version %q, older than the current %q", c.Image.Path, r.Configured.version, r.Current.version)
		}
		return r.Configured, false, nil
	case r.Stable.Image.Path != "" && c.Image == r.Stable.Image:
		if c.UID != r.UID || c.Mode.Perm()&0o022 != 0 {
			return Launch{}, false, refuseCandidate("stable binary %q has owner %d and mode %v, want owner %d without group or other write permission", c.Image.Path, c.UID, c.Mode, r.UID)
		}
		return Launch{}, true, nil
	}
	return Launch{}, false, refuseCandidate("candidate %q is neither the current choice, the configured worker executable nor the stable binary", c.Image.Path)
}

// sameFile reports whether c shows the device and inode of image.
func sameFile(c Executable, image procinfo.Image) bool {
	return c.Image.Device == image.Device && c.Image.Inode == image.Inode
}

// AcceptVersion decides a stable candidate from its version output and a second inspection taken after the command ran.
//
// before must be a candidate for which Judge reports that the version is needed, after must equal before in every
// field, and output must be at most 4096 bytes holding exactly one valid version text followed by one line feed,
// not older than the current choice's version. The result is then a valid launch of before.Image with that text,
// authorised as a stable binary owned by r.UID.
func (r RestartRule) AcceptVersion(before Executable, output []byte, after Executable) (Launch, error) {
	_, needVersion, err := r.Judge(before)
	if err != nil {
		return Launch{}, err
	}
	if !needVersion {
		return Launch{}, refuseCandidate("candidate %q is decided without its version", before.Image.Path)
	}
	if after != before {
		return Launch{}, refuseCandidate("stable binary %q (device %d, inode %d, mode %v, owner %d) changed while its version command ran to %q (device %d, inode %d, mode %v, owner %d)",
			before.Image.Path, before.Image.Device, before.Image.Inode, before.Mode, before.UID,
			after.Image.Path, after.Image.Device, after.Image.Inode, after.Mode, after.UID)
	}
	if len(output) > maxVersionOutput {
		return Launch{}, refuseCandidate("the version output of %q has %d bytes, more than %d", before.Image.Path, len(output), maxVersionOutput)
	}
	line, ok := bytes.CutSuffix(output, []byte("\n"))
	// string copies the bytes, so a caller that reuses output does not change the accepted version.
	text := string(line)
	numbers, valid := parseVersion(text)
	if !ok || !valid {
		return Launch{}, refuseCandidate("the version output %q of %q is not one valid version line", output, before.Image.Path)
	}
	baseline, _ := parseVersion(r.Current.version) // valid: Judge refused every candidate otherwise.
	if numbers.olderThan(baseline) {
		return Launch{}, refuseCandidate("stable binary %q at version %q is older than the current %q", before.Image.Path, text, r.Current.version)
	}
	return Launch{image: before.Image, version: text, auth: authStable, owner: r.UID}, nil
}
