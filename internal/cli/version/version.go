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

// Package version implements the model-gateway version command.
package version

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// releaseVersion can be set with -ldflags -X github.com/zchee/claude-code-model-gateway/internal/cli/version.releaseVersion=<version>.
var releaseVersion string

// A pseudo-version ends in a timestamp and revision after either vN.0.0- or a prerelease's 0. segment.
var pseudoVersion = regexp.MustCompile(`^v\d+\.(?:0\.0-|\d+\.\d+-(?:[^+]+\.)?0\.)\d{14}-[a-zA-Z0-9]+$`)

// NewCommand returns a command that prints the build version.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "version",
		Short:        "Print the build version",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), String())
			return err
		},
	}
}

// String returns the build version that the version command prints, without a line end.
func String() string {
	info, _ := debug.ReadBuildInfo()
	return resolve(releaseVersion, info)
}

// resolve returns a non-empty linker version verbatim. Otherwise it returns
// the Go module's tagged version with one leading v removed, unless the build
// is marked dirty. A pseudo-version, (devel), empty version, dirty tag, or missing
// build info yields 0.0.0-dev with +vcs.revision appended if a revision is available.
func resolve(linkerVersion string, info *debug.BuildInfo) string {
	if linkerVersion != "" {
		return linkerVersion
	}
	const dev = "0.0.0-dev"
	if info == nil {
		return dev
	}
	version := info.Main.Version
	base, build, _ := strings.Cut(version, "+")
	if version != "" && version != "(devel)" && build != "dirty" && build != "incompatible.dirty" && !pseudoVersion.MatchString(base) {
		return strings.TrimPrefix(version, "v")
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return dev + "+" + setting.Value
		}
	}
	return dev
}
