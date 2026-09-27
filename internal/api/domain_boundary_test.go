package api

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDomainAndRuntimePackagesDoNotImportDTOsOrNativeAdapters(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate boundary test source")
	}
	internalDir := filepath.Dir(filepath.Dir(source))
	forbiddenImports := map[string]bool{
		"opensbx/models":                  true,
		"opensbx/internal/docker":         true,
		"opensbx/internal/applecontainer": true,
		"github.com/moby/moby/api":        true,
		"github.com/moby/moby/client":     true,
	}
	for _, pkg := range []string{"sandbox", "service", "runtimeio", "images"} {
		files, err := filepath.Glob(filepath.Join(internalDir, pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			node, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			for _, imp := range node.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("unquote import in %s: %v", file, err)
				}
				if forbiddenImports[path] || strings.HasPrefix(path, "github.com/moby/moby/") {
					t.Errorf("domain/OCI package %s leaks forbidden dependency %q", file, path)
				}
			}
		}
	}
}

func TestOnlyAPIProductionPackageImportsHTTPModelDTOs(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate boundary test source")
	}
	internalDir := filepath.Dir(filepath.Dir(source))
	packages, err := filepath.Glob(filepath.Join(internalDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range packages {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		pkg := filepath.Base(dir)
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			node, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			for _, imp := range node.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if path == "opensbx/models" && pkg != "api" {
					t.Errorf("DTO import escaped API facade into package %s (%s)", pkg, file)
				}
			}
		}
	}
}
