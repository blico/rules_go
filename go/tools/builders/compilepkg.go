// Copyright 2019 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// compilepkg compiles a complete Go package from Go, C, and assembly files.  It
// supports cgo, coverage, and nogo. It is invoked by the Go rules as an action.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func compilePkg(args []string) error {
	// Parse arguments.
	args, err := readParamsFiles(args)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("GoCompilePkg", flag.ExitOnError)
	goenv := envFlags(fs)
	var unfilteredSrcs, coverSrcs, cgoArchivePaths multiFlag
	var deps compileArchiveMultiFlag
	var importPath, packagePath, nogoPath, packageListPath, coverMode string
	var outPath, outFactsPath, cgoExportHPath string
	var testFilter string
	var gcFlags, asmFlags, cppFlags, cFlags, cxxFlags, ldFlags quoteMultiFlag
	fs.Var(&unfilteredSrcs, "src", ".go, .c, or .s file to be filtered and compiled")
	fs.Var(&coverSrcs, "cover", ".go file that should be instrumented for coverage (must also be a -src)")
	fs.Var(&deps, "arc", "Import path, package path, and file name of a direct dependency, separated by '='")
	fs.Var(&cgoArchivePaths, "cgoarc", "Path to a C/C++/ObjC archive to repack into the Go archive. May be repeated.")
	fs.StringVar(&importPath, "importpath", "", "The import path of the package being compiled. Not passed to the compiler, but may be displayed in debug data.")
	fs.StringVar(&packagePath, "p", "", "The package path (importmap) of the package being compiled")
	fs.Var(&gcFlags, "gcflags", "Go compiler flags")
	fs.Var(&asmFlags, "asmflags", "Go assembler flags")
	fs.Var(&cppFlags, "cppflags", "C preprocessor flags")
	fs.Var(&cFlags, "cflags", "C compiler flags")
	fs.Var(&cxxFlags, "cxxflags", "C++ compiler flags")
	fs.Var(&ldFlags, "ldflags", "C linker flags")
	fs.StringVar(&nogoPath, "nogo", "", "The nogo binary. If unset, nogo will not be run.")
	fs.StringVar(&packageListPath, "package_list", "", "The file containing the list of standard library packages")
	fs.StringVar(&coverMode, "cover_mode", "", "The coverage mode to use. Empty if coverage instrumentation should not be added.")
	fs.StringVar(&outPath, "o", "", "The output archive file to write")
	fs.StringVar(&outFactsPath, "x", "", "The nogo facts file to write")
	fs.StringVar(&cgoExportHPath, "cgoexport", "", "The _cgo_exports.h file to write")
	fs.StringVar(&testFilter, "testfilter", "off", "Controls test package filtering")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlags(); err != nil {
		return err
	}
	if importPath == "" {
		importPath = packagePath
	}
	cgoEnabled := os.Getenv("CGO_ENABLED") == "1"
	cc := os.Getenv("CC")
	outPath = abs(outPath)
	for i := range unfilteredSrcs {
		unfilteredSrcs[i] = abs(unfilteredSrcs[i])
	}
	for i := range coverSrcs {
		coverSrcs[i] = abs(coverSrcs[i])
	}

	// Filter sources.
	srcs, err := filterAndSplitFiles(unfilteredSrcs)
	if err != nil {
		return err
	}

	// TODO(jayconrod): remove -testfilter flag. The test action should compile
	// the main, internal, and external packages by calling compileArchive
	// with the correct sources for each.
	switch testFilter {
	case "off":
	case "only":
		testSrcs := make([]fileInfo, 0, len(srcs.goSrcs))
		for _, f := range srcs.goSrcs {
			if strings.HasSuffix(f.pkg, "_test") {
				testSrcs = append(testSrcs, f)
			}
		}
		srcs.goSrcs = testSrcs
	case "exclude":
		libSrcs := make([]fileInfo, 0, len(srcs.goSrcs))
		for _, f := range srcs.goSrcs {
			if !strings.HasSuffix(f.pkg, "_test") {
				libSrcs = append(libSrcs, f)
			}
		}
		srcs.goSrcs = libSrcs
	default:
		return fmt.Errorf("invalid test filter %q", testFilter)
	}

	return compileArchive(
		goenv,
		importPath,
		packagePath,
		srcs,
		deps,
		cgoArchivePaths,
		coverMode,
		coverSrcs,
		cgoEnabled,
		cc,
		gcFlags,
		asmFlags,
		cppFlags,
		cFlags,
		cxxFlags,
		ldFlags,
		nogoPath,
		packageListPath,
		outPath,
		outFactsPath,
		cgoExportHPath)
}

func compileArchive(
	goenv *env,
	importPath string,
	packagePath string,
	srcs archiveSrcs,
	deps []archive,
	cgoArchivePaths []string,
	coverMode string,
	coverSrcs []string,
	cgoEnabled bool,
	cc string,
	gcFlags []string,
	asmFlags []string,
	cppFlags []string,
	cFlags []string,
	cxxFlags []string,
	ldFlags []string,
	nogoPath string,
	packageListPath string,
	outPath string,
	outFactsPath string,
	cgoExportHPath string) error {

	workDir, cleanup, err := goenv.workDir()
	if err != nil {
		return err
	}
	defer cleanup()

	if len(srcs.goSrcs) == 0 {
		emptyPath := filepath.Join(workDir, "_empty.go")
		if err := ioutil.WriteFile(emptyPath, []byte("package empty\n"), 0666); err != nil {
			return err
		}
		srcs.goSrcs = append(srcs.goSrcs, fileInfo{
			filename: emptyPath,
			ext:      goExt,
			matched:  true,
			pkg:      "empty",
		})
		defer os.Remove(emptyPath)
	}
	packageName := srcs.goSrcs[0].pkg
	var goSrcs, cgoSrcs []string
	for _, src := range srcs.goSrcs {
		if src.isCgo {
			cgoSrcs = append(cgoSrcs, src.filename)
		} else {
			goSrcs = append(goSrcs, src.filename)
		}
	}
	cSrcs := make([]string, len(srcs.cSrcs))
	for i, src := range srcs.cSrcs {
		cSrcs[i] = src.filename
	}
	cxxSrcs := make([]string, len(srcs.cxxSrcs))
	for i, src := range srcs.cxxSrcs {
		cxxSrcs[i] = src.filename
	}
	if len(srcs.mSrcs) > 0 {
		return errors.New("Objective C sources not supported by GoCompilePkg")
	}
	sSrcs := make([]string, len(srcs.sSrcs))
	for i, src := range srcs.sSrcs {
		sSrcs[i] = src.filename
	}
	hSrcs := make([]string, len(srcs.hSrcs))
	for i, src := range srcs.hSrcs {
		hSrcs[i] = src.filename
	}
	haveCgo := len(cgoSrcs)+len(cSrcs)+len(cxxSrcs) > 0

	// Instrument source files for coverage.
	if coverMode != "" {
		shouldCover := make(map[string]bool)
		for _, s := range coverSrcs {
			shouldCover[s] = true
		}

		combined := append([]string{}, goSrcs...)
		if cgoEnabled {
			combined = append(combined, cgoSrcs...)
		}
		for i, origSrc := range combined {
			if !shouldCover[origSrc] {
				continue
			}

			srcName := origSrc
			if importPath != "" {
				srcName = path.Join(importPath, filepath.Base(origSrc))
			}

			coverVar := fmt.Sprintf("Cover_%s_%s", sanitizePathForIdentifier(importPath), sanitizePathForIdentifier(origSrc[:len(origSrc)-len(".go")]))
			coverSrc := filepath.Join(workDir, fmt.Sprintf("cover_%d.go", i))
			if err := instrumentForCoverage(goenv, origSrc, srcName, coverVar, coverMode, coverSrc); err != nil {
				return err
			}

			if i < len(goSrcs) {
				goSrcs[i] = coverSrc
			} else {
				cgoSrcs[i-len(goSrcs)] = coverSrc
			}
		}
	}

	// If we have cgo, generate separate C and go files, and compile the
	// C files.
	var objFiles []string
	if cgoEnabled && haveCgo {
		// TODO(#2006): Compile .s and .S files with cgo2, not the Go assembler.
		// If cgo is not enabled or we don't have other cgo sources, don't
		// compile .S files.
		var srcDir string
		srcDir, goSrcs, objFiles, err = cgo2(goenv, goSrcs, cgoSrcs, cSrcs, cxxSrcs, nil, hSrcs, packagePath, packageName, cc, cppFlags, cFlags, cxxFlags, ldFlags, cgoExportHPath)
		if err != nil {
			return err
		}

		gcFlags = append(gcFlags, "-trimpath="+srcDir)
	} else {
		gcFlags = append(gcFlags, "-trimpath=.")
	}

	// Check that the filtered sources don't import anything outside of
	// the standard library and the direct dependencies.
	_, stdImports, err := checkDirectDeps(srcs.goSrcs, deps, packageListPath)
	if err != nil {
		return err
	}
	if cgoEnabled && len(cgoSrcs) != 0 {
		// cgo generated code imports some extra packages.
		cgoStdImports := map[string]struct{}{
			"runtime/cgo": struct{}{},
			"syscall":     struct{}{},
			"unsafe":      struct{}{},
		}
		for _, imp := range stdImports {
			delete(cgoStdImports, imp)
		}
		for imp := range cgoStdImports {
			stdImports = append(stdImports, imp)
		}
	}

	// Build an importcfg file for the compiler.
	importcfgPath, err := buildImportcfgFileForCompile(deps, stdImports, goenv.installSuffix, filepath.Dir(outPath))
	if err != nil {
		return err
	}
	defer os.Remove(importcfgPath)

	// Run nogo concurrently.
	var nogoChan chan error
	if nogoPath != "" {
		ctx, cancel := context.WithCancel(context.Background())
		nogoChan = make(chan error)
		go func() {
			nogoChan <- runNogo(ctx, nogoPath, goSrcs, deps, stdImports, packagePath, importcfgPath, outFactsPath)
		}()
		defer func() {
			if nogoChan != nil {
				cancel()
				<-nogoChan
			}
		}()
	}

	// If there are assembly files, and this is go1.12+, generate symbol ABIs.
	asmHdrPath := ""
	if len(srcs.sSrcs) > 0 {
		asmHdrPath = filepath.Join(workDir, "go_asm.h")
	}
	symabisPath, err := buildSymabisFile(goenv, srcs.sSrcs, srcs.hSrcs, asmHdrPath)
	if symabisPath != "" {
		defer os.Remove(symabisPath)
	}
	if err != nil {
		return err
	}

	// Compile the filtered .go files.
	if err := compileGo(goenv, goSrcs, packagePath, importcfgPath, asmHdrPath, symabisPath, gcFlags, outPath); err != nil {
		return err
	}

	// Compile the .s files.
	if len(srcs.sSrcs) > 0 {
		includeSet := map[string]struct{}{
			filepath.Join(os.Getenv("GOROOT"), "pkg", "include"): struct{}{},
			workDir: struct{}{},
		}
		for _, hdr := range srcs.hSrcs {
			includeSet[filepath.Dir(hdr.filename)] = struct{}{}
		}
		includes := make([]string, len(includeSet))
		for inc := range includeSet {
			includes = append(includes, inc)
		}
		sort.Strings(includes)
		asmFlags := make([]string, 0, len(includeSet)*2)
		for _, inc := range includes {
			asmFlags = append(asmFlags, "-I", inc)
		}
		for i, sSrc := range srcs.sSrcs {
			obj := filepath.Join(workDir, fmt.Sprintf("s%d.o", i))
			if err := asmFile(goenv, sSrc.filename, asmFlags, obj); err != nil {
				return err
			}
			objFiles = append(objFiles, obj)
		}
	}

	// Extract cgo archvies and re-pack them into the archive.
	if len(cgoArchivePaths) > 0 {
		names := map[string]struct{}{}
		for _, cgoArchivePath := range cgoArchivePaths {
			arcObjFiles, err := extractFiles(cgoArchivePath, workDir, names)
			if err != nil {
				return err
			}
			objFiles = append(objFiles, arcObjFiles...)
		}
	}

	// Pack .o files into the archive. These may come from cgo generated code,
	// cgo dependencies (cdeps), or assembly.
	if len(objFiles) > 0 {
		if err := appendFiles(goenv, outPath, objFiles); err != nil {
			return err
		}
	}

	// Check results from nogo.
	if nogoChan != nil {
		err := <-nogoChan
		nogoChan = nil // no cancellation needed
		if err != nil {
			return err
		}
	}

	return nil
}

func compileGo(goenv *env, srcs []string, packagePath, importcfgPath, asmHdrPath, symabisPath string, gcFlags []string, outPath string) error {
	args := goenv.goTool("compile")
	args = append(args, "-p", packagePath, "-importcfg", importcfgPath, "-pack")
	if asmHdrPath != "" {
		args = append(args, "-asmhdr", asmHdrPath)
	}
	if symabisPath != "" {
		args = append(args, "-symabis", symabisPath)
	}
	args = append(args, gcFlags...)
	args = append(args, "-o", outPath)
	args = append(args, "--")
	args = append(args, srcs...)
	absArgs(args, []string{"-I", "-o", "-trimpath", "-importcfg"})
	return goenv.runCommand(args)
}

func runNogo(ctx context.Context, nogoPath string, srcs []string, deps []archive, stdImports []string, packagePath, importcfgPath, outFactsPath string) error {
	args := []string{nogoPath}
	args = append(args, "-p", packagePath)
	args = append(args, "-importcfg", importcfgPath)
	for _, imp := range stdImports {
		args = append(args, "-stdimport", imp)
	}
	for _, dep := range deps {
		if dep.xFile != "" {
			args = append(args, "-fact", fmt.Sprintf("%s=%s", dep.importPath, dep.xFile))
		}
	}
	args = append(args, "-x", outFactsPath)
	args = append(args, srcs...)

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return errors.New(out.String())
		} else {
			if out.Len() != 0 {
				fmt.Fprintln(os.Stderr, out.String())
			}
			return fmt.Errorf("error running nogo: %v", err)
		}
	}
	return nil
}

func sanitizePathForIdentifier(path string) string {
	r := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	return r.Replace(path)
}
