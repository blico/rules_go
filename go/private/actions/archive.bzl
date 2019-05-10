# Copyright 2014 The Bazel Authors. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

load(
    "@io_bazel_rules_go//go/private:skylib/lib/sets.bzl",
    "sets",
)
load(
    "@io_bazel_rules_go//go/private:common.bzl",
    "as_tuple",
    "split_srcs",
)
load(
    "@io_bazel_rules_go//go/private:mode.bzl",
    "mode_string",
)
load(
    "@io_bazel_rules_go//go/private:providers.bzl",
    "GoArchive",
    "GoArchiveData",
    "effective_importpath_pkgpath",
    "get_archive",
)
load(
    "@io_bazel_rules_go//go/private:rules/cgo.bzl",
    "cgo_configure",
)
load(
    "@io_bazel_rules_go//go/private:actions/compilepkg.bzl",
    "emit_compilepkg",
)

def emit_archive(go, source = None):
    """See go/toolchains.rst#archive for full documentation."""

    if source == None:
        fail("source is a required parameter")

    split = split_srcs(source.srcs)
    lib_name = source.library.importmap + ".a"
    out_lib = go.declare_file(go, path = lib_name)
    if go.nogo:
        # TODO(#1847): write nogo data into a new section in the .a file instead
        # of writing a separate file.
        out_export = go.declare_file(go, path = lib_name[:-len(".a")] + ".x")
    else:
        out_export = None
    searchpath = out_lib.path[:-len(lib_name)]
    testfilter = getattr(source.library, "testfilter", None)

    direct = [get_archive(dep) for dep in source.deps]
    runfiles = source.runfiles
    data_files = runfiles.files
    for a in direct:
        runfiles = runfiles.merge(a.runfiles)
        if a.source.mode != go.mode:
            fail("Archive mode does not match {} is {} expected {}".format(a.data.label, mode_string(a.source.mode), mode_string(go.mode)))

    if not source.cgo_archives:
        # TODO(jayconrod): We still need to support the legacy cgo path when
        # Objective C sources are present, since the Objective C toolchain
        # isn't exposed to Starlark. The legacy path runs cgo as a separate
        # action, then builds generated code with cc_library / objc_library.
        # A go_library compiles generated Go code and packs the other
        # objects from cgo_archives. "cgo_archives" is not supported on
        # the new path because we do all that in one action.

        # TODO(jayconrod): do we need to do full Bourne tokenization here?
        cppopts = [f for fs in source.cppopts for f in fs.split(" ")]
        copts = [f for fs in source.copts for f in fs.split(" ")]
        cxxopts = [f for fs in source.cxxopts for f in fs.split(" ")]
        clinkopts = [f for fs in source.clinkopts for f in fs.split(" ")]

        cgo_inputs = depset()
        cgo_deps = depset()
        if source.cgo:
            cgo = cgo_configure(
                go,
                srcs = split.go + split.c + split.asm + split.cxx + split.headers,
                cdeps = source.cdeps,
                cppopts = cppopts,
                copts = copts,
                cxxopts = cxxopts,
                clinkopts = clinkopts,
            )
            runfiles = runfiles.merge(cgo.runfiles)
            cgo_inputs = cgo.inputs
            cgo_deps = cgo.deps
            cppopts = cgo.cppopts
            copts = cgo.copts
            cxxopts = cgo.cxxopts
            clinkopts = cgo.clinkopts

        emit_compilepkg(
            go,
            sources = split.go + split.c + split.asm + split.cxx + split.headers,
            cover = source.cover,
            importpath = effective_importpath_pkgpath(source.library)[0],
            importmap = source.library.importmap,
            archives = direct,
            out_lib = out_lib,
            out_export = out_export,
            gc_goopts = source.gc_goopts,
            cgo = source.cgo,
            cgo_inputs = cgo_inputs,
            cppopts = cppopts,
            copts = copts,
            cxxopts = cxxopts,
            clinkopts = clinkopts,
            cgo_archives = source.cgo_archives,
            testfilter = testfilter,
        )
    else:
        cgo_deps = source.cgo_deps

        if bool(go.cover and go.coverdata and source.cover):
            source = go.cover(go, source)
            direct.append(go.coverdata)

        asmhdr = None
        if split.asm:
            asmhdr = go.declare_file(go, "go_asm.h")

        if len(split.asm) == 0 and not source.cgo_archives:
            go.compile(
                go,
                sources = split.go,
                importpath = source.library.importmap,
                archives = direct,
                out_lib = out_lib,
                out_export = out_export,
                gc_goopts = source.gc_goopts,
                testfilter = testfilter,
            )
        else:
            # Assembly files must be passed to the compiler as sources. We need
            # to run the assembler to produce a symabis file that gets passed to
            # the compiler. The compiler builder does all this so it doesn't
            # need to be a separate action (but individual .o files are still
            # produced with separate actions).
            partial_lib = go.declare_file(go, path = lib_name + "~partial", ext = ".a")
            go.compile(
                go,
                sources = split.go + split.asm + split.headers,
                importpath = source.library.importmap,
                archives = direct,
                out_lib = partial_lib,
                out_export = out_export,
                gc_goopts = source.gc_goopts,
                testfilter = testfilter,
                asmhdr = asmhdr,
            )

            # include other .s as inputs, since they may be #included.
            # This may result in multiple copies of symbols defined in included
            # files, but go build allows it, so we do, too.
            asm_headers = split.headers + split.asm + [asmhdr]
            extra_objects = []
            for src in split.asm:
                extra_objects.append(go.asm(go, source = src, hdrs = asm_headers))
            go.pack(
                go,
                in_lib = partial_lib,
                out_lib = out_lib,
                objects = extra_objects,
                archives = source.cgo_archives,
            )

    data = GoArchiveData(
        name = source.library.name,
        label = source.library.label,
        importpath = source.library.importpath,
        importmap = source.library.importmap,
        pathtype = source.library.pathtype,
        file = out_lib,
        export_file = out_export,
        srcs = as_tuple(source.srcs),
        orig_srcs = as_tuple(source.orig_srcs),
        data_files = as_tuple(data_files),
        searchpath = searchpath,
    )
    x_defs = dict(source.x_defs)
    for a in direct:
        x_defs.update(a.x_defs)
    return GoArchive(
        source = source,
        data = data,
        direct = direct,
        searchpaths = sets.union([searchpath], *[a.searchpaths for a in direct]),
        libs = sets.union([out_lib], *[a.libs for a in direct]),
        transitive = sets.union([data], *[a.transitive for a in direct]),
        x_defs = x_defs,
        cgo_deps = sets.union(cgo_deps, *[a.cgo_deps for a in direct]),
        cgo_exports = sets.union(source.cgo_exports, *[a.cgo_exports for a in direct]),
        runfiles = runfiles,
        mode = go.mode,
    )
