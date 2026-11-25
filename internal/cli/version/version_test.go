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

package version

import (
	"runtime/debug"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestResolve(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	buildInfo := func(version, revision string) *debug.BuildInfo {
		info := &debug.BuildInfo{Main: debug.Module{Version: version}}
		if revision != "" {
			info.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: revision}}
		}
		return info
	}
	tests := map[string]struct {
		linker string
		info   *debug.BuildInfo
		want   string
	}{
		"success: linker value wins verbatim": {
			linker: "v0.57.1",
			info:   buildInfo("v0.57.0", revision),
			want:   "v0.57.1",
		},
		"success: released module version loses leading v": {
			info: buildInfo("v0.57.0", revision),
			want: "0.57.0",
		},
		"success: tagged prerelease is not a pseudo-version": {
			info: buildInfo("v0.57.0-rc.1", ""),
			want: "0.57.0-rc.1",
		},
		"success: pseudo-version without base tag": {
			info: buildInfo("v0.0.0-20200102030405-0123456789ab", revision),
			want: "0.0.0-dev+" + revision,
		},
		"success: pseudo-version following release without build settings": {
			info: buildInfo("v0.57.1-0.20200102030405-0123456789ab", ""),
			want: "0.0.0-dev",
		},
		"success: pseudo-version following prerelease with dirty metadata": {
			info: buildInfo("v0.57.0-rc.1.0.20200102030405-0123456789ab+dirty", revision),
			want: "0.0.0-dev+" + revision,
		},
		"success: dirty build at a tagged commit": {
			info: buildInfo("v0.57.0+dirty", revision),
			want: "0.0.0-dev+" + revision,
		},
		"success: dirty build at an incompatible tagged commit": {
			info: buildInfo("v2.0.0+incompatible.dirty", revision),
			want: "0.0.0-dev+" + revision,
		},
		"success: devel with revision": {
			info: buildInfo("(devel)", revision),
			want: "0.0.0-dev+" + revision,
		},
		"success: devel without revision": {
			info: buildInfo("(devel)", ""),
			want: "0.0.0-dev",
		},
		"success: empty module version": {
			info: buildInfo("", revision),
			want: "0.0.0-dev+" + revision,
		},
		"success: missing build info": {
			want: "0.0.0-dev",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, resolve(tt.linker, tt.info)); diff != "" {
				t.Errorf("resolve() version mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
