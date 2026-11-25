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

// Command lintvenue checks test entry points without loading or running packages.
package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const venuePath = "github.com/zchee/claude-code-model-gateway/internal/venue"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: lintvenue directory")
		os.Exit(2)
	}
	if err := checkTree(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func checkTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (d.Name() == "testdata" || d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
				names = append(names, e.Name())
			}
		}
		if len(names) == 0 {
			return nil
		}
		var issues []error
		for _, tags := range [][]string{nil, {"process"}} {
			label := "default"
			if len(tags) != 0 {
				label = "process"
			}
		platforms:
			for _, platform := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}} {
				ctx := build.Default
				ctx.GOOS, ctx.GOARCH, ctx.BuildTags = platform[0], platform[1], tags
				for _, cgo := range []bool{false, true} {
					ctx.CgoEnabled = cgo
					if err := checkPackage(ctx, path, names); err != nil {
						issues = append(issues, fmt.Errorf("%s/%s cgo=%t %s: %w", ctx.GOOS, ctx.GOARCH, cgo, label, err))
						break platforms
					}
				}
			}
		}
		return errors.Join(issues...)
	})
}

func checkPackage(ctx build.Context, dir string, names []string) error {
	var first, guard string
	for _, name := range names {
		// MatchFile uses go/build/constraint for directives as well as Go's
		// filename, platform, compiler and release-tag selection rules.
		ok, err := ctx.MatchFile(dir, name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		path := filepath.Join(dir, name)
		if first == "" {
			first = path
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" {
				continue
			}
			// Go treats TestMain(*testing.T) as an ordinary test, not as the
			// entry point of the internal and external test packages' binary.
			params := fn.Type.Params
			if params.NumFields() != 1 {
				continue
			}
			p, ok := params.List[0].Type.(*ast.StarExpr)
			if !ok || !imported(f, p.X, "testing", "M") {
				continue
			}
			if guard != "" {
				return fmt.Errorf("%s: duplicate TestMain (also in %s)", path, guard)
			}
			if !guardFirst(f, fn) {
				return fmt.Errorf("%s: TestMain must begin with venue.Require()", path)
			}
			guard = path
		}
	}
	if first != "" && guard == "" {
		return fmt.Errorf("%s: missing guarded TestMain", first)
	}
	return nil
}

func guardFirst(f *ast.File, fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	stmt, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	return ok && len(call.Args) == 0 && imported(f, call.Fun, venuePath, "Require")
}

func imported(f *ast.File, expr ast.Expr, importPath, member string) bool {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			id, ok := expr.(*ast.Ident)
			return ok && id.Name == member
		}
		s, ok := expr.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != member {
			continue
		}
		id, ok := s.X.(*ast.Ident)
		if ok && id.Name == name {
			return true
		}
	}
	return false
}
