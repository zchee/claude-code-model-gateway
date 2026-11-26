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

//go:build !unix

package wiring

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

const maxSnapshotBytes = 16 << 20

var errSnapshotTooLarge = errors.New("file exceeds snapshot size limit")

// readRegularFile opens path, verifies on the opened descriptor that the target is a regular
// file, and reads it within the size bound. Symlinks to regular files are followed.
func readRegularFile(path string) ([]byte, fs.FileInfo, error) {
	return readRegularFileLimit(path, maxSnapshotBytes)
}

func readRegularFileLimit(path string, limit int64) ([]byte, fs.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, &atomicfile.NonRegularError{Path: path}
	}
	if info.Size() > limit {
		return nil, nil, fmt.Errorf("%s: %w", path, errSnapshotTooLarge)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%s: %w", path, errSnapshotTooLarge)
	}
	return data, info, nil
}
