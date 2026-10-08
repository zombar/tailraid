#!/usr/bin/env python3
"""Delete invariant/lifecycle stubs in test/invariants whose ID is now cited by a real test."""
import re, glob, subprocess
real = set()
for f in glob.glob('**/*_test.go', recursive=True):
    if f.startswith('test/invariants/'):
        continue
    for line in open(f):
        if re.match(r'\s*//\s*(Invariant|Lifecycle|Regression):', line):
            real |= set(re.findall(r'\b[IL]-\d+\b', line))
removed = []
for f in glob.glob('test/invariants/*_test.go'):
    src = open(f).read()
    blocks = re.split(r'(?=\n// (?:Invariant|Lifecycle): )', src)
    keep = [blocks[0]]
    for b in blocks[1:]:
        ids = set(re.findall(r'\b[IL]-\d+\b', b.split('\n', 1)[0] if b.startswith('\n//') else b.split('\n', 2)[1]))
        if ids and ids <= real:
            removed += sorted(ids)
        else:
            keep.append(b)
    open(f, 'w').write(''.join(keep))
subprocess.run(['gofmt', '-w'] + glob.glob('test/invariants/*_test.go'))
print('pruned:', ' '.join(removed) if removed else 'nothing')
