"""Disposable snapshot qualification target; all operations use the application uid."""
import json
import os
from pathlib import Path
import stat
import sys

root = Path('/state/app')
private = root / 'evidence' / 'nested'
file = private / 'state.txt'
command = sys.stdin.read().strip()
if command == 'create':
    (root / 'evidence').mkdir(mode=0o700)
    private.mkdir(mode=0o700)
    fd = os.open(file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as out:
        out.write('snapshot-private-content')
elif command == 'update':
    with file.open('a') as out:
        out.write('-updated')
elif command != 'read':
    raise ValueError(command)

entries = {}
for path in (root, root / 'seed.txt', root / 'evidence', private, file):
    if path.exists():
        info = path.stat()
        entries[str(path)] = {'uid': info.st_uid, 'gid': info.st_gid,
                              'mode': stat.S_IMODE(info.st_mode)}
# Verify that temporary bootstrap privileges did not reach the application or
# remain in the non-root supervisor/helper processes.
process_caps = []
for status in Path('/proc').glob('[0-9]*/status'):
    try:
        fields = dict(line.split(':', 1) for line in status.read_text().splitlines() if ':' in line)
        uid = int(fields['Uid'].split()[0])
        if uid in (os.getuid(), (root / 'seed.txt').stat().st_uid):
            process_caps.append({'uid': uid, 'effective': int(fields['CapEff'], 16),
                                 'permitted': int(fields['CapPrm'], 16)})
    except (FileNotFoundError, PermissionError, ProcessLookupError):
        continue
print(json.dumps({'process_caps': process_caps, 'content': file.read_text() if file.exists() else None,
                  'seed': (root / 'seed.txt').read_text(), 'entries': entries,
                  'uid': os.getuid(), 'gid': os.getgid()}))
