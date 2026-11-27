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

package ownership

import (
	"path/filepath"
	"regexp"
	"slices"

	"github.com/zchee/claude-code-model-gateway/internal/install"
)

// nodeScript is the pathname of a Node gateway's script: an argument that is, as a whole, a path whose directory
// before bin/model-gateway.js is named ending in model-gateway, optionally followed by one version directory of
// three dot-separated numbers, with single separators around that version directory and bin. Letter case does not
// matter. The first group is the install root.
var nodeScript = regexp.MustCompile(`(?is)^(.*model-gateway(?:/[0-9]+\.[0-9]+\.[0-9]+)?)/bin/model-gateway\.js$`)

// NodeGateway returns the Legacy predicate for this plugin's install root current. The predicate looks for one
// argument that is the intended pathname of a Node gateway's script, <root>/bin/model-gateway.js: an absolute path,
// compared without regard to letter case, whose root ends in a directory named ending in model-gateway (a component
// such as foreign-model-gateway qualifies), optionally followed by one version directory of three dot-separated
// numbers, with single separators around that version directory and bin. The argument may hold spaces, and any
// argument may be it, since the Node executable comes first. This follows the intent of the source's pattern
// (lib/process-supervision.js:510-514), which searches the whole command text instead of one argument. The root is
// the directory two levels above the script. The predicate cleans both that root and current before it asks
// internal/install whether the root is current or a sibling version of it in a plugin cache that registryDirs, or a
// parent named plugins, recognize. Any other command line has an empty root.
func NodeGateway(current string, registryDirs []string) func(argv []string) (root, cur string, ours bool) {
	current = filepath.Clean(current)
	// A copy, so that a later change to the caller's slice does not change what the predicate recognizes.
	dirs := slices.Clone(registryDirs)
	return func(argv []string) (string, string, bool) {
		for _, arg := range argv {
			match := nodeScript.FindStringSubmatch(arg)
			if match == nil || !filepath.IsAbs(match[1]) {
				continue
			}
			root := filepath.Clean(match[1])
			return root, current, belongsToThisPlugin(root, current, dirs)
		}
		return "", current, false
	}
}

// belongsToThisPlugin reports whether the install root root is this plugin's install current or a sibling version of
// it in the same plugin cache (lib/process-supervision.js:536-542). Both roots are cleaned first, so a spelling with
// a final separator or a dot segment is the same root. A root that internal/install normalizes to the same path as
// current belongs, as the source compares its normalized physical paths (physical path, folded separators, lower
// case); otherwise both must have the same cache identity, which internal/install derives the same way.
func belongsToThisPlugin(root, current string, registryDirs []string) bool {
	root, current = filepath.Clean(root), filepath.Clean(current)
	if !filepath.IsAbs(root) || !filepath.IsAbs(current) {
		return false
	}
	if install.SameInstallRoot(root, current) {
		return true
	}
	identity := install.CacheIdentity(root, registryDirs)
	return identity != "" && identity == install.CacheIdentity(current, registryDirs)
}
