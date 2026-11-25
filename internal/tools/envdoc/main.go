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

// Command envdoc renders and verifies the environment variable catalog.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

func main() {
	check := flag.Bool("check", false, "compare the generated document with the named file")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: envdoc [-check] <output-file>")
		os.Exit(2)
	}
	path := flag.Arg(0)
	data := render()
	var err error
	if *check {
		var current []byte
		current, err = os.ReadFile(path)
		if err == nil && !bytes.Equal(current, data) {
			err = errors.New("environment catalog is stale; run make env-doc")
		}
	} else {
		err = os.WriteFile(path, data, 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "envdoc: %s: %v\n", path, err)
		os.Exit(1)
	}
}

func render() []byte {
	var out bytes.Buffer
	out.WriteString("<!-- Generated from internal/config by make env-doc; do not edit. -->\n\n# Environment variables\n\n")
	out.WriteString("The readers column identifies the component and line at the pinned source, or the Go reader for a port-only setting. Child processes also inherit environment entries the gateway itself does not interpret.\n\n")
	out.WriteString("| Variable | Default or fallback | Meaning | Parse rule | Readers |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, row := range config.Variables() {
		fmt.Fprintf(&out, "| `%s` | %s | %s | %s | %s |\n", row.Name, cell(row.Default), cell(row.Meaning), cell(row.Rule), cell(row.Readers))
	}
	return out.Bytes()
}

func cell(s string) string {
	return strings.NewReplacer(`\`, `\\`, `|`, `\|`, "\n", " ").Replace(s)
}
