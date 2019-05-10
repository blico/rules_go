# Copyright 2019 The Bazel Authors. All rights reserved.
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
    "@io_bazel_rules_go//go/private:mode.bzl",
    "link_mode_args",
)
load(
    "@io_bazel_rules_go//go/private:skylib/lib/shell.bzl",
    "shell",
)

def _archive(v):
    return "{}={}={}={}".format(
        v.data.importpath,
        v.data.importmap,
        v.data.file.path,
        v.data.export_file.path if v.data.export_file else "",
    )

def emit_compilepkg(
        go,
        sources = None,
        cover = None,
        importpath = "",
        importmap = "",
        archives = [],
        cgo = False,
        cgo_inputs = depset(),
        cppopts = [],
        copts = [],
        cxxopts = [],
        clinkopts = [],
        cgo_archives = [],
        out_lib = None,
        out_export = None,
        gc_goopts = [],
        testfilter = None):  # TODO: remove when test action compiles packages
    if sources == None:
        fail("sources is a required parameter")
    if out_lib == None:
        fail("out_lib is a required parameter")

    inputs = (sources + [go.package_list] +
              [archive.data.file for archive in archives] +
              cgo_archives +
              go.sdk.tools + go.sdk.headers + go.stdlib.libs)
    outputs = [out_lib]
    env = go.env

    args = go.builder_args(go, "compilepkg")
    args.add_all(sources, before_each = "-src")
    if cover and go.coverdata:
        inputs.append(go.coverdata.data.file)
        args.add("-arc", _archive(go.coverdata))
        args.add("-cover_mode", "set")
        args.add_all(cover, before_each = "-cover")
    args.add_all(archives, before_each = "-arc", map_each = _archive)
    if importpath:
        args.add("-importpath", importpath)
    if importmap:
        args.add("-p", importmap)
    args.add("-package_list", go.package_list)

    args.add("-o", out_lib)
    if go.nogo:
        args.add("-nogo", go.nogo)
        args.add("-x", out_export)
        inputs.append(go.nogo)
        inputs.extend([archive.data.export_file for archive in archives if archive.data.export_file])
        outputs.append(out_export)
    if testfilter:
        args.add("-testfilter", testfilter)

    gc_flags = [
        go._ctx.expand_make_variables("gc_goopts", f, {})
        for f in gc_goopts
    ]
    asm_flags = []
    if go.mode.race:
        gc_flags.append("-race")
        asm_flags.append("-race")
    if go.mode.msan:
        gc_flags.append("-msan")
        asm_flags.append("-msan")
    if go.mode.debug:
        gc_flags.extend(["-N", "-l"])
        asm_flags.extend(["-N", "-l"])
    gc_flags.extend(go.toolchain.flags.compile)
    gc_flags.extend(link_mode_args(go.mode))
    asm_flags.extend(link_mode_args(go.mode))
    args.add("-gcflags", _quote_opts(gc_flags))
    args.add("-asmflags", _quote_opts(asm_flags))

    env = go.env
    if cgo:
        inputs.extend(cgo_inputs.to_list())  # OPT: don't expand depset
        inputs.extend(go.crosstool)
        env["CC"] = go.cgo_tools.c_compiler_path
        if cppopts:
            args.add("-cppflags", _quote_opts(cppopts))
        if copts:
            args.add("-cflags", _quote_opts(copts))
        if cxxopts:
            args.add("-cxxflags", _quote_opts(cxxopts))
        if clinkopts:
            args.add("-ldflags", _quote_opts(clinkopts))

    go.actions.run(
        inputs = inputs,
        outputs = outputs,
        mnemonic = "GoCompilePkg",
        executable = go.toolchain._builder,
        arguments = [args],
        env = go.env,
    )

def _quote_opts(opts):
    return " ".join([shell.quote(opt) if " " in opt else opt for opt in opts])
