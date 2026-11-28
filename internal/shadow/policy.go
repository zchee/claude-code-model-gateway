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

package shadow

import (
	"io"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

// Resources lists the actual prospective destinations of a shadow service graph.
type Resources struct {
	StateRoot, DefaultRoot, ProxyDial             string
	Writes, Binds, Dials, Upstreams, DefaultBinds []string
}

// Assemble combines pure resource declarations before service construction.
func Assemble(base Resources, parts ...func(*Resources) error) (Resources, error) {
	panic("not implemented")
}

// Enabled accepts only the explicit shadow mode value.
func Enabled(lookup config.LookupEnv) bool { panic("not implemented") }

// Allowed validates the complete command vector after the executable name.
func Allowed(argv []string) bool { panic("not implemented") }

// Preflight validates the resource inventory and prints its sorted report.
func Preflight(resources Resources, stdout, stderr io.Writer) int { panic("not implemented") }
