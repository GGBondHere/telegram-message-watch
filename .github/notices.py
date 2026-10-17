"""Generate notices for both shipped Linux targets; no build or tests are run."""

import argparse
import fnmatch
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.request

ROOT = Path(__file__).resolve().parent.parent
LEGAL = re.compile(r"^(?:licen[cs]e|copying|copyright|notice|patents)(?:[._-].*)?$", re.I)
COMMENTS = re.compile(r"/\*.*?\*/|(?m:^[ \t]*//[^\n]*(?:\n[ \t]*//[^\n]*)*)", re.S)
EXCLUDED = {
    "modernc.org/sqlite": {"LICENSE-3RD-PARTY.md", "LICENSE-SQLITE_VEC"},
    "modernc.org/memory": {"LICENSE-LOGO"},
}
MPL_URL = "https://www.mozilla.org/media/MPL/2.0/index.txt"
MPL_SHA256 = "3f3d9e0024b1921b067d6f7f88deb4a60cbe7a78e76c64e3f1d7fc3b779b9d04"
NSS_URL = "https://raw.githubusercontent.com/nss-dev/nss/fc168131f8a629e30c38b372e08848cabd443194/lib/ckfw/builtins/certdata.txt"


def go(*args, arch="amd64"):
    env = dict(os.environ, GOOS="linux", GOARCH=arch, CGO_ENABLED="0", GOAMD64="v1", GOARM64="v8.0")
    return subprocess.check_output(["go", *args], cwd=ROOT, env=env, text=True, encoding="utf-8")


def objects(raw):
    decoder = json.JSONDecoder()
    result = []
    while raw.strip():
        item, end = decoder.raw_decode(raw.lstrip())
        result.append(item)
        raw = raw.lstrip()[end:]
    return result


def fetch(url):
    request = urllib.request.Request(url, headers={"User-Agent": "tgw-notices/1"})
    with urllib.request.urlopen(request, timeout=40) as response:
        return response.read()


def canonical(text):
    return " ".join(text.split())


def archive_url(module):
    path = re.sub("[A-Z]", lambda m: "!" + m[0].lower(), module["Path"])
    return f"https://proxy.golang.org/{path}/@v/{module['Version']}.zip"


def fence(text):
    marker = "`" * max(3, 1 + max((len(x) for x in re.findall(r"`+", text)), default=0))
    return f"{marker}text\n{text.expandtabs(4).strip()}\n{marker}"


def comment_text(raw):
    text = re.sub(r"(?m)^\s*// ?", "", raw) if raw.lstrip().startswith("//") else re.sub(r"(?m)^\s*\* ?", "", raw[2:-2])
    if not re.search(r"(?im)^\s*(?:copyright\b|permission is hereby|redistribution and use|SPDX-License-Identifier|this source code form|this (?:file|code|software) is.*public domain)", text):
        return ""
    # Collapse standard Go header scaffolding; keep every distinct copyright line.
    lines = text.strip().splitlines()
    if all(not line.strip() or re.match(r"Copyright .*|Use of this source code is governed by a BSD-style|license that can be found in the LICENSE file\.", line.strip()) for line in lines):
        return "\n".join(line for line in lines if line.strip().startswith("Copyright"))
    return text.strip()


def legal_files(base, packages, module):
    files, sources = set(), set()
    for package in packages:
        directory = Path(package["Dir"])
        if not directory.is_relative_to(base):
            continue
        current = directory
        while current.is_relative_to(base):
            files.update(p for p in current.iterdir() if p.is_file() and LEGAL.match(p.name))
            if current == base:
                break
            current = current.parent
        for kind in ("GoFiles", "SFiles", "CgoFiles", "CFiles", "HFiles"):
            sources.update(directory / name for name in package.get(kind, []))
    result = []
    for path in sorted(files):
        name = path.relative_to(base).as_posix()
        if name in EXCLUDED.get(module, set()):
            if name != "LICENSE-SQLITE_VEC" or not any(p.get("ImportPath", "").endswith("/sqlite/vec") for p in packages):
                continue
        text = path.read_text(encoding="utf-8-sig")
        if module == "github.com/klauspost/compress" and name == "LICENSE":
            # Upstream explicitly scopes these extra licenses with Files: globs.
            compiled = [p.relative_to(base).as_posix() for p in sources]
            blocks = re.split(r"\n-{3,}\n", text)
            text = "\n\n".join(block for block in blocks if not re.search(r"(?m)^Files:", block) or any(
                fnmatch.fnmatchcase(p, pattern.strip())
                for pattern in re.findall(r"(?m)^Files:\s*(.+)", block) for p in compiled))
        if module == "modernc.org/libc" and name == "LICENSE-3RD-PARTY.md":
            # Nix configuration is not compiled into the executable.
            text = text.split("## NixOS/nixpkgs")[0].rstrip("\n- ")
        if path.name.lower().startswith(("license", "copying")) and not any(term in text.lower() for term in (
            "redistribution and use", "permission is hereby granted", "permission to use, copy, modify", "apache license", "public domain", "mozilla public license")):
            raise RuntimeError(f"Unrecognized license requires review: {module}/{name}")
        if re.search(r"GNU (?:LESSER |AFFERO )?GENERAL PUBLIC LICENSE|Eclipse Public License|Common Development and Distribution License", text, re.I):
            raise RuntimeError(f"Additional source/license obligations require review: {module}/{name}")
        result.append(text.strip())
    if not result:
        raise RuntimeError(f"No license found for {module}")
    headers = {comment_text(match[0]) for p in sources for match in COMMENTS.finditer(p.read_text(encoding="utf-8-sig"))}
    covered = [canonical(text) for text in result]
    result.extend(text for text in sorted(headers) if text and not any(canonical(text) in full for full in covered))
    return result


def mozilla_sources(module):
    # Preserve a reviewed source snapshot; new roots require source review.
    source = fetch(NSS_URL)
    certs = {hashlib.sha256(bytes(int(x, 8) for x in re.findall(r"\\([0-7]{3})", value))).hexdigest()
             for value in re.findall(r"CKA_VALUE MULTILINE_OCTAL\n(.*?)\nEND", source.decode(), re.S)}
    base = Path(module["Dir"])
    index = (base / "bundle/bundle.go").read_text()
    raw = (base / "bundle/bundle.der").read_bytes()
    roots = re.findall(r'sha256Hash:\s*"([0-9a-f]+)".*?certStartOff:\s*(\d+),.*?certLength:\s*(\d+),', index, re.S)
    if not roots or any(h not in certs or hashlib.sha256(raw[int(off):int(off)+int(size)]).hexdigest() != h for h, off, size in roots):
        raise RuntimeError("NSS source does not cover the shipped certificate bundle; review its provenance before release")
    mpl = fetch(MPL_URL)
    if hashlib.sha256(mpl).hexdigest() != MPL_SHA256:
        raise RuntimeError("MPL text changed; review before release")
    return mpl.decode(), hashlib.sha256(raw).hexdigest()


def render_notices(components):
    result = ["# 第三方版权与许可证声明\n",
              "本文件列出 Linux amd64/arm64 发行程序使用的第三方组件。各组件保留其版权、许可条款及免责声明。再分发程序时，请同时提供本声明。\n",
              "## 组件清单\n", "点击组件名称查看说明，展开对应条目可阅读完整许可文本。\n",
              "| 组件 | 版本 | 源码 |", "| --- | --- | --- |"]
    for number, component in enumerate(components, 1):
        result.append(f"| [{component['name']}](#component-{number}) | `{component['version']}` | [获取源码]({component['source']}) |")
    for number, component in enumerate(components, 1):
        result += ["", f'<a id="component-{number}"></a>\n', f"## {component['name']}\n",
                   f"版本：`{component['version']}` · [源码]({component['source']})\n"]
        if component.get("description"):
            result.append(component["description"].strip() + "\n")
        unique = {canonical(text): text for text in component["texts"]}
        result += ["<details>", "<summary>版权、许可条款与免责声明</summary>\n",
                   fence("\n\n".join(unique.values())), "\n</details>\n"]
    return "\n".join(result).rstrip() + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=ROOT / "THIRD_PARTY_NOTICES.md")
    parser.add_argument("--binary", type=Path, help="cross-check the built binary's module versions without executing it")
    args = parser.parse_args()
    packages = []
    for arch in ("amd64", "arm64"):
        packages.extend(objects(go("list", "-mod=readonly", "-deps", "-json", "./src", arch=arch)))
    modules = {p["Module"]["Path"]: p["Module"] for p in packages if p.get("Module") and not p["Module"].get("Main")}
    if any(module.get("Replace") for module in modules.values()):
        raise RuntimeError("Replaced modules require a source/license review")
    if args.binary:
        for line in go("version", "-m", str(args.binary.resolve())).splitlines():
            parts = line.split()
            if parts and parts[0] == "dep" and (parts[1] not in modules or modules[parts[1]]["Version"] != parts[2]):
                raise RuntimeError("Binary dependencies differ from the collected notices")
    goversion = go("env", "GOVERSION").strip()
    goroot = Path(go("env", "GOROOT").strip())
    standard = [p for p in packages if p.get("Standard")]
    components = [{"name": "Go runtime / 标准库", "version": goversion,
                   "source": f"https://go.dev/dl/{goversion}.src.tar.gz",
                   "texts": legal_files(goroot, standard, "Go")}]
    for name, module in sorted(modules.items()):
        selected = [p for p in packages if p.get("Module", {}).get("Path") == name]
        components.append({"name": name, "version": module["Version"], "source": archive_url(module),
                           "texts": legal_files(Path(module["Dir"]), selected, name)})
    fallback = modules.get("golang.org/x/crypto/x509roots/fallback")
    if fallback:
        mpl, bundle_hash = mozilla_sources(fallback)
        extra = ["证书数据适用 MPL-2.0，Go 读取代码适用其 BSD 许可。\n",
                 f"Source Code Form：[NSS certdata.txt]({NSS_URL})；[所用 fallback 模块源码与证书数据]({archive_url(fallback)})。",
                 "本项目未修改上述证书数据；接收者可依 MPL-2.0 获取、修改和再分发受覆盖数据。",
                 "上游未记录生成时使用的 NSS 提交；这里提供同期包含全部所用证书的固定 NSS 快照，并逐证书核对 DER，不将它冒称为上游生成时的原始提交。",
                 f"所分发 `bundle.der` 的 SHA-256：`{bundle_hash}`。\n"]
        components.append({"name": "Mozilla NSS 根证书数据", "version": "随 fallback 模块分发", "source": NSS_URL,
                           "texts": [mpl], "description": "\n".join(extra)})
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(render_notices(components), encoding="utf-8", newline="\n")
    print(f"{len(modules)} third-party modules + {goversion}; component-based notices written to {args.output}")


if __name__ == "__main__":
    main()
