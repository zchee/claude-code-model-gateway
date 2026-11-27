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

// NodeGateway returns the Legacy predicate for this plugin's install root current. A Node gateway command line names
// a script <root>/bin/model-gateway.js, matched without regard to letter case, in one argument (which may hold spaces),
// as the source's pattern requires (lib/process-supervision.js:510-514): the text before the separator that precedes
// bin ends in model-gateway (a component such as foreign-model-gateway qualifies), optionally followed by one
// version directory of three dot-separated numbers, and the separators around the version directory and bin are
// single. The root is the directory two levels above the script. The predicate cleans both that root and current
// before it asks internal/install whether the root is current or a sibling version of it in a plugin cache that
// registryDirs, or a parent named plugins, recognize. Any other command line has an empty root.
func NodeGateway(current string, registryDirs []string) func(argv []string) (root, cur string, ours bool) {
	panic("not implemented")
}
