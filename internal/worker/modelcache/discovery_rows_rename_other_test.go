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

//go:build !linux

package modelcache_test

import "testing"

// discTRenameWatch has no way to pair the halves of a rename outside Linux, whose inotify
// reports them with a shared cookie.
type discTRenameWatch struct{}

// discTWatchRenames reports the observation as unavailable; the caller then checks the
// directories on disk only.
func discTWatchRenames(*testing.T, string, string) (*discTRenameWatch, bool) { return nil, false }

func (*discTRenameWatch) moves(*testing.T) []discTMove { return nil }
