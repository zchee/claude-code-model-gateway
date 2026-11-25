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

//go:build retain_dependencies

// Package dependencies keeps approved dependencies in go.mod until real packages use them.
// Normal builds never set this tag. Delete each blank import when a real package starts using its module.
package dependencies

import (
	_ "github.com/andybalholm/brotli"
	_ "github.com/dlclark/regexp2"
	_ "github.com/google/renameio/v2/maybe"
	_ "github.com/miekg/dns"
	_ "github.com/rogpeppe/go-internal/testscript"
	_ "github.com/shirou/gopsutil/v4/process"
	_ "go.uber.org/goleak"
	_ "golang.org/x/sync/singleflight"
	_ "golang.org/x/sys/unix"
	_ "golang.org/x/text/collate"
)
