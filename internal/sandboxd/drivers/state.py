# Embedded before the Python invocation driver; no application dependency needed.
import math
import stat
import tempfile


def state_json(value, seen=None):
    if seen is None:
        seen = set()
    if value is None or type(value) in (str, bool, int):
        return
    if type(value) is float and math.isfinite(value):
        return
    if type(value) not in (dict, list):
        raise ValueError('state must contain only JSON objects, arrays and finite primitives')
    if id(value) in seen:
        raise ValueError('state contains a cycle')
    seen.add(id(value))
    try:
        if type(value) is dict:
            if any(type(key) is not str for key in value):
                raise ValueError('state object keys must be strings')
            values = value.values()
        else:
            values = value
        for item in values:
            state_json(item, seen)
    finally:
        seen.remove(id(value))


class StateStore:
    def __init__(self, cfg):
        self.path = Path(cfg['path'])
        self.limit = int(os.environ['INTERCEPTOR_STATE_MAX_BYTES'])
        if self.limit <= 0 or not self.path.is_absolute() or '..' in self.path.parts:
            raise ValueError('invalid state path or byte limit')

    def directory(self, create=False):
        # Reject links in every component, including the final file. The adapter
        # is trusted application code; this is not protection from that same UID.
        current = Path(self.path.anchor)
        for part in self.path.parts[1:-1]:
            current /= part
            if create:
                try:
                    current.mkdir(mode=0o750)
                except FileExistsError:
                    pass
            if not stat.S_ISDIR(current.lstat().st_mode):
                raise ValueError('state path parent must be a directory, not a link')
        try:
            info = self.path.lstat()
        except FileNotFoundError:
            return
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise ValueError('state path must be a regular file without links')

    def save(self, value):
        if type(value) is not dict:
            raise ValueError('save method must return a JSON object')
        state_json(value)
        # Iterative encoding bounds output allocation as well as disk writes.
        self.directory(create=True)
        fd, temporary = tempfile.mkstemp(prefix='.state-', dir=self.path.parent)
        try:
            os.fchmod(fd, 0o640)
            with os.fdopen(fd, 'wb') as stream:
                size = 0
                for chunk in json.JSONEncoder(allow_nan=False, ensure_ascii=True).iterencode({'version': 1, 'state': value}):
                    data = chunk.encode('utf-8')
                    size += len(data)
                    if size > self.limit:
                        raise ValueError('state exceeds limits.max_blob_bytes')
                    stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            self.directory()
            os.replace(temporary, self.path)
            directory = os.open(self.path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def load(self):
        self.directory()
        fd = os.open(self.path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > self.limit:
                raise ValueError('invalid or oversized state file')
            data = stream.read(self.limit + 1)
        if len(data) > self.limit:
            raise ValueError('state exceeds limits.max_blob_bytes')
        value = json.loads(data)
        if type(value) is not dict or set(value) != {'version', 'state'} or type(value['version']) is not int or value['version'] != 1 or type(value['state']) is not dict:
            raise ValueError('unsupported state document; expected version 1 and object state')
        state_json(value)
        return value['state']
