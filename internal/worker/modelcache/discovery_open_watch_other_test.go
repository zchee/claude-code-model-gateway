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

// discoveryOpenWatch has no way to observe file opens outside Linux, whose inotify reports them.
type discoveryOpenWatch struct{}

// watchDiscoveryInputOpens reports the observation of opens as unavailable; the caller then asserts
// no opens at all, rather than asserting on an observation that cannot be made.
func watchDiscoveryInputOpens(*testing.T, ...string) (*discoveryOpenWatch, bool) {
	return nil, false
}

func (*discoveryOpenWatch) opened(*testing.T) []string { return nil }
