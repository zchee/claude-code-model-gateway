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

// Package venue prevents test binaries from running on gateway machines.
package venue

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// Require exits with status 97 unless the effective account is attested, its
// gateway state is absent, and neither gateway port has a TCP listener.
func Require() {
	if reason := check(homeDir, os.Lstat, listening); reason != "" {
		fmt.Fprintln(os.Stderr, "not a test venue:", reason)
		os.Exit(97)
	}
}

func check(homeDir func() (string, error), lstat func(string) (fs.FileInfo, error), listening func(string) (bool, error)) string {
	home, err := homeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "cannot establish account home"
	}
	info, err := lstat(filepath.Join(home, ".model-gateway-test-venue"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "attestation missing"
	case err != nil:
		return "cannot inspect attestation"
	case !info.Mode().IsRegular():
		return "attestation is not a regular file"
	}
	_, err = lstat(filepath.Join(home, ".claude", "model-gateway"))
	switch {
	case err == nil:
		return "gateway state present"
	case !errors.Is(err, fs.ErrNotExist):
		return "cannot inspect gateway state"
	}
	for _, port := range []int{18764, 18765} {
		p := strconv.Itoa(port)
		busy, err := listening(p)
		if err != nil {
			return "cannot inspect TCP port " + p
		}
		if busy {
			return "TCP port " + p + " has a listener"
		}
	}
	return ""
}
