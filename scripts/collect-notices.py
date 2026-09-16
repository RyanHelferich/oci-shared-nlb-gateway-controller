#!/usr/bin/env python3
"""Collect complete upstream notice files for the Linux controller import graph.

Requires the pinned Go toolchain and its dependency cache. Does not scan unrelated
modules, include source excerpts, or publish local filesystem paths. --check
compares a freshly resolved bundle without writing it.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

ROOT=Path(__file__).resolve().parents[1]
LEGAL=re.compile(r'^(licen[cs]e|notice|copying|copyright|patents?)(?:[._-].*)?$',re.I)


def digest(data):return hashlib.sha256(data).hexdigest()


def canonical_text(data):
    """Return UTF-8 text with LF endings so Windows and Linux builds agree."""
    return data.decode('utf-8').replace('\r\n','\n').replace('\r','\n')


def mismatch_summary(expected,actual):
    """Describe notice drift without printing license bodies or local paths."""
    lines=[]
    for field in ('format','goVersion','sourceInputs','target'):
        if expected.get(field)!=actual.get(field):lines.append(f'{field}: committed={expected.get(field)!r} generated={actual.get(field)!r}')
    def keyed(bundle):return {(x['module'],x['version']):x for x in bundle.get('components',[])}
    old,new=keyed(expected),keyed(actual)
    if old.keys()!=new.keys():
        lines.append(f'components removed={sorted(old.keys()-new.keys())!r} added={sorted(new.keys()-old.keys())!r}')
    for key in sorted(old.keys()&new.keys()):
        for field in ('importedPackages','notices'):
            if old[key].get(field)!=new[key].get(field):
                before=set(json.dumps(x,sort_keys=True) for x in old[key].get(field,[]));after=set(json.dumps(x,sort_keys=True) for x in new[key].get(field,[]))
                lines.append(f'{key!r} {field}: removed={sorted(before-after)!r} added={sorted(after-before)!r}')
    old_texts=set(expected.get('texts',{}));new_texts=set(actual.get('texts',{}))
    if old_texts!=new_texts:lines.append(f'text hashes removed={sorted(old_texts-new_texts)!r} added={sorted(new_texts-old_texts)!r}')
    return '\n'.join(lines) or 'serialized form differs despite equivalent summarized fields'


def objects(text):
    decoder=json.JSONDecoder();result=[];text=text.lstrip('\ufeff')
    while text.strip():
        text=text.lstrip();item,end=decoder.raw_decode(text);result.append(item);text=text[end:]
    return result


def legal_files(root,package_dirs):
    """Only legal files on ancestry paths of actually imported packages."""
    root=root.resolve();directories={root}
    for folder in package_dirs:
        folder=Path(folder).resolve()
        if not folder.is_relative_to(root):raise ValueError('Package directory outside its selected module')
        directories.update([folder,*[x for x in folder.parents if x.is_relative_to(root)]])
    selected=[]
    for directory in sorted(directories):
        for path in sorted(directory.iterdir()):
            if path.is_file() and LEGAL.fullmatch(path.name):
                if path.is_symlink():raise ValueError('Notice symlinks require explicit review')
                selected.append(path)
    return sorted(set(selected))


def collect(packages,goroot,go_version,source_inputs):
    components={};texts={}
    for pkg in packages:
        if pkg.get('Error') or pkg.get('DepsErrors'):raise ValueError('Incomplete dependency graph')
        module=pkg.get('Module')
        if pkg.get('Standard'):
            key=('go',go_version);root=Path(goroot)
        elif module and not module.get('Main'):
            if module.get('Replace'):raise ValueError('Replaced dependency requires explicit notice-source review')
            if not module.get('Version') or not module.get('Dir'):raise ValueError('Unpinned/unavailable dependency')
            key=(module['Path'],module['Version']);root=Path(module['Dir'])
        else:continue
        component=components.setdefault(key,{'module':key[0],'version':key[1],'root':root,'packages':[],'directories':[]})
        if component['root'].resolve()!=root.resolve():raise ValueError('Ambiguous module root')
        component['packages'].append(pkg['ImportPath']);component['directories'].append(pkg['Dir'])
    result=[]
    for key,component in sorted(components.items()):
        notices=[];root=component['root'].resolve()
        for path in legal_files(root,component['directories']):
            text=canonical_text(path.read_bytes());sha=digest(text.encode('utf-8'))
            texts[sha]={'text':text,'normalizedTextSHA256':sha}
            notices.append({'path':path.relative_to(root).as_posix(),'sha256':sha})
        notices.sort(key=lambda item:item['path'])
        if not any(re.match(r'^(licen[cs]e|copying)',Path(n['path']).name,re.I) for n in notices):
            raise ValueError('Selected module has no license file: '+key[0])
        result.append({'module':key[0],'version':key[1],'importedPackages':sorted(set(component['packages'])),'notices':notices})
    return {'format':2,'scope':'Complete LICENSE/NOTICE/COPYING/COPYRIGHT/PATENT files from module roots and ancestors of packages in the controller import graph; UTF-8 text and source-input hashes use canonical LF line endings; no test-only or unrelated cache modules. The graph can include code removed by the linker.',
        'target':{'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0','entrypoint':'./cmd/controller'},'goVersion':go_version,
        'sourceInputs':source_inputs,'components':result,'texts':{k:texts[k] for k in sorted(texts)}}


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--go',default='go');p.add_argument('--output',default=str(ROOT/'THIRD_PARTY_NOTICES.json'));p.add_argument('--check',action='store_true')
    a=p.parse_args();env=os.environ.copy();env.update(GOOS='linux',GOARCH='amd64',CGO_ENABLED='0')
    def go(*args):
        r=subprocess.run([a.go,*args],cwd=ROOT,env=env,capture_output=True,text=True,encoding='utf-8',timeout=300)
        if r.returncode:raise ValueError('Go dependency inventory failed; use the pinned toolchain and populated cache: '+r.stderr[-1000:])
        return r.stdout.strip()
    expected=re.search(r'^go\s+([0-9.]+)$',(ROOT/'go.mod').read_text(),re.M)[1]
    version=go('version');match=re.search(r'\bgo([0-9.]+)\b',version)
    if not match or match[1]!=expected:raise ValueError('Go toolchain differs from pinned go.mod version')
    packages=objects(go('list','-buildvcs=false','-deps','-json','./cmd/controller'))
    bundle=collect(packages,go('env','GOROOT'),expected,{n:digest(canonical_text((ROOT/n).read_bytes()).encode('utf-8')) for n in ['go.mod','go.sum']})
    data=(json.dumps(bundle,indent=2,ensure_ascii=True)+'\n').encode();path=Path(a.output)
    if a.check:
        committed=path.read_bytes()
        if committed!=data:
            detail=mismatch_summary(json.loads(committed),bundle)
            raise ValueError('Notice bundle differs from exact current target dependencies; regenerate and review\n'+detail)
    else:path.write_bytes(data)
    print(json.dumps({'checked':a.check,'components':len(bundle['components']),'noticeFiles':sum(len(c['notices']) for c in bundle['components']),'uniqueTexts':len(bundle['texts']),'sha256':digest(data)}))


if __name__=='__main__':main()
