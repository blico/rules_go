// Copyright 2017 The Bazel Authors. All rights reserved.
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

// compile compiles .go files with "go tool compile". It is invoked by the
// Go rules as an action.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func compile(args []string) error {
	// Parse arguments.
	args, err := readParamsFiles(args)
	if err != nil {
		return err
	}
	builderArgs, toolArgs := splitArgs(args)
	flags := flag.NewFlagSet("GoCompile", flag.ExitOnError)
	unfiltered := multiFlag{}
	archives := compileArchiveMultiFlag{}
	goenv := envFlags(flags)
	packagePath := flags.String("p", "", "The package path (importmap) of the package being compiled")
	flags.Var(&unfiltered, "src", "A source file to be filtered and compiled")
	flags.Var(&archives, "arc", "Import path, package path, and file name of a direct dependency, separated by '='")
	nogo := flags.String("nogo", "", "The nogo binary")
	outExport := flags.String("x", "", "Path to nogo that should be written")
	output := flags.String("o", "", "The output object file to write")
	asmhdr := flags.String("asmhdr", "", "Path to assembly header file to write")
	packageList := flags.String("package_list", "", "The file containing the list of standard library packages")
	testfilter := flags.String("testfilter", "off", "Controls test package filtering")
	if err := flags.Parse(builderArgs); err != nil {
		return err
	}
	if err := goenv.checkFlags(); err != nil {
		return err
	}
	*output = abs(*output)
	if *asmhdr != "" {
		*asmhdr = abs(*asmhdr)
	}

	// Filter sources using build constraints.
	all, err := filterAndSplitFiles(unfiltered)
	if err != nil {
		return err
	}
	goFiles, sFiles, hFiles := all.goSrcs, all.sSrcs, all.hSrcs
	if len(all.cSrcs) > 0 {
		return fmt.Errorf("unexpected C file: %s", all.cSrcs[0].filename)
	}
	if len(all.cxxSrcs) > 0 {
		return fmt.Errorf("unexpected C++ file: %s", all.cxxSrcs[0].filename)
	}
	switch *testfilter {
	case "off":
	case "only":
		testFiles := make([]fileInfo, 0, len(goFiles))
		for _, f := range goFiles {
			if strings.HasSuffix(f.pkg, "_test") {
				testFiles = append(testFiles, f)
			}
		}
		goFiles = testFiles
	case "exclude":
		libFiles := make([]fileInfo, 0, len(goFiles))
		for _, f := range goFiles {
			if !strings.HasSuffix(f.pkg, "_test") {
				libFiles = append(libFiles, f)
			}
		}
		goFiles = libFiles
	default:
		return fmt.Errorf("invalid test filter %q", *testfilter)
	}
	if len(goFiles) == 0 {
		// We need to run the compiler to create a valid archive, even if there's
		// nothing in it. GoPack will complain if we try to add assembly or cgo
		// objects.
		emptyPath := filepath.Join(filepath.Dir(*output), "_empty.go")
		if err := ioutil.WriteFile(emptyPath, []byte("package empty\n"), 0666); err != nil {
			return err
		}
		goFiles = append(goFiles, fileInfo{filename: emptyPath, pkg: "empty"})
	}

	if *packagePath == "" {
		*packagePath = goFiles[0].pkg
	}

	// Check that the filtered sources don't import anything outside of
	// the standard library and the direct dependencies.
	depImports, stdImports, err := checkDirectDeps(goFiles, archives, *packageList)
	if err != nil {
		return err
	}

	// Build an importcfg file for the compiler.
	importcfgName, err := buildImportcfgFileForCompile(depImports, stdImports, goenv.installSuffix, filepath.Dir(*output))
	if err != nil {
		return err
	}
	defer os.Remove(importcfgName)

	// If there are assembly files, and this is go1.12+, generate symbol ABIs.
	symabisName, err := buildSymabisFile(goenv, sFiles, hFiles, *asmhdr)
	if symabisName != "" {
		defer os.Remove(symabisName)
	}
	if err != nil {
		return err
	}

	// Compile the filtered files.
	goargs := goenv.goTool("compile")
	goargs = append(goargs, "-p", *packagePath)
	goargs = append(goargs, "-importcfg", importcfgName)
	goargs = append(goargs, "-pack", "-o", *output)
	if symabisName != "" {
		goargs = append(goargs, "-symabis", symabisName)
	}
	if *asmhdr != "" {
		goargs = append(goargs, "-asmhdr", *asmhdr)
	}
	goargs = append(goargs, toolArgs...)
	goargs = append(goargs, "--")
	filenames := make([]string, 0, len(goFiles))
	for _, f := range goFiles {
		filenames = append(filenames, f.filename)
	}
	goargs = append(goargs, filenames...)
	absArgs(goargs, []string{"-I", "-o", "-trimpath", "-importcfg"})
	cmd := exec.Command(goargs[0], goargs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting compiler: %v", err)
	}

	// Run nogo concurrently.
	var nogoOutput bytes.Buffer
	nogoFailed := false
	if *nogo != "" {
		var nogoargs []string
		nogoargs = append(nogoargs, "-p", *packagePath)
		nogoargs = append(nogoargs, "-importcfg", importcfgName)
		for _, imp := range stdImports {
			nogoargs = append(nogoargs, "-stdimport", imp)
		}
		for _, arc := range archives {
			if arc.xFile != "" {
				nogoargs = append(nogoargs, "-fact", fmt.Sprintf("%s=%s", arc.importPath, arc.xFile))
			}
		}
		nogoargs = append(nogoargs, "-x", *outExport)
		nogoargs = append(nogoargs, filenames...)
		nogoCmd := exec.Command(*nogo, nogoargs...)
		nogoCmd.Stdout, nogoCmd.Stderr = &nogoOutput, &nogoOutput
		if err := nogoCmd.Run(); err != nil {
			if _, ok := err.(*exec.ExitError); ok {
				// Only fail the build if nogo runs and finds errors in source code.
				nogoFailed = true
			} else {
				// All errors related to running nogo will merely be printed.
				nogoOutput.WriteString(fmt.Sprintf("error running nogo: %v\n", err))
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("error running compiler: %v", err)
	}
	// Only print the output of nogo if compilation succeeds.
	if nogoFailed {
		return fmt.Errorf("%s", nogoOutput.String())
	}
	if nogoOutput.Len() != 0 {
		fmt.Fprintln(os.Stderr, nogoOutput.String())
	}
	return nil
}

func checkDirectDeps(files []fileInfo, archives []archive, packageList string) (depImports, stdImports []string, err error) {
	packagesTxt, err := ioutil.ReadFile(packageList)
	if err != nil {
		log.Fatal(err)
	}
	stdlibSet := map[string]bool{}
	for _, line := range strings.Split(string(packagesTxt), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			stdlibSet[line] = true
		}
	}

	depImports = map[string]archive{}
	depSet := map[string]archive{}
	depList := make([]string, len(archives))
	for i, arc := range archives {
		depSet[arc.importPath] = arc
		depList[i] = arc.importPath
	}

	derr := depsError{known: depList}
	for _, f := range files {
		for _, path := range f.imports {
			if path == "C" || isRelative(path) {
				// TODO(#1645): Support local (relative) import paths. We don't emit
				// errors for them here, but they will probably break something else.
				continue
			}
			if stdlibSet[path] {
				stdImports = append(stdImports, path)
				continue
			}
			if arc, ok := depSet[path]; ok {
				depImports[path] = arc
				continue
			}
			if vN := modMajorRex.FindString(path); vN != "" {
				newPath := strings.Replace(path, vN, "", 1)
				if arc, ok := depSet[newPath]; ok {
					depImports[path] = arc
					continue
				}
			}
			derr.missing = append(derr.missing, missingDep{f.filename, path})
		}
	}
	if len(derr.missing) > 0 {
		return nil, nil, derr
	}
	return depImports, stdImports, nil
}

type depsError struct {
	missing []missingDep
	known   []string
}

type missingDep struct {
	filename, imp string
}

var _ error = depsError{}

func (e depsError) Error() string {
	buf := bytes.NewBuffer(nil)
	fmt.Fprintf(buf, "missing strict dependencies:\n")
	for _, dep := range e.missing {
		fmt.Fprintf(buf, "\t%s: import of %q\n", dep.filename, dep.imp)
	}
	if len(e.known) == 0 {
		fmt.Fprintln(buf, "No dependencies were provided.")
	} else {
		fmt.Fprintln(buf, "Known dependencies are:")
		for _, imp := range e.known {
			fmt.Fprintf(buf, "\t%s\n", imp)
		}
	}
	fmt.Fprint(buf, "Check that imports in Go sources match importpath attributes in deps.")
	return buf.String()
}

func isRelative(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../")
}
