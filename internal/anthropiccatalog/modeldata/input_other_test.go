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

package modeldata_test

import "testing"

// mdatTPlatformRows holds no rows on targets without symbolic links and FIFOs of the Unix kind.
func mdatTPlatformRows() map[string]struct {
	setup func(t *testing.T, dir string) (path string)
	open  func(t *testing.T, dir string, previous mdatTOpen) mdatTOpen
	want  mdatTResult
} {
	return nil
}
