// Embedded before the Node invocation driver. Keep checkpoint values lossless.
function stateJSON(value, seen = new Set()) {
    if (value === null || typeof value === 'string' || typeof value === 'boolean') return;
    if (typeof value === 'number' && Number.isFinite(value) && !Object.is(value, -0)) return;
    if (typeof value !== 'object' || seen.has(value)) throw new Error('state contains a non-JSON value or cycle');
    const array = Array.isArray(value);
    if (!array && Object.getPrototypeOf(value) !== Object.prototype && Object.getPrototypeOf(value) !== null)
        throw new Error('state requires plain JSON objects');
    const keys = Reflect.ownKeys(value).filter(key => !(array && key === 'length'));
    if (array && keys.length !== value.length) throw new Error('state arrays must be dense without extra properties');
    seen.add(value);
    try {
        for (const key of keys) {
            const descriptor = Object.getOwnPropertyDescriptor(value, key);
            if (typeof key !== 'string' || !descriptor.enumerable || !('value' in descriptor) ||
                (array && (!/^(0|[1-9][0-9]*)$/.test(key) || Number(key) >= value.length)))
                throw new Error('state contains non-JSON properties');
            stateJSON(descriptor.value, seen);
        }
    } finally { seen.delete(value); }
}
function stateObject(value) {
    return value !== null && typeof value === 'object' && !Array.isArray(value);
}
function createStateStore(cfg) {
    const filename = cfg.path;
    const limit = Number(process.env.INTERCEPTOR_STATE_MAX_BYTES);
    if (!path.isAbsolute(filename) || path.normalize(filename) !== filename || !Number.isSafeInteger(limit) || limit <= 0)
        throw new Error('invalid state path or byte limit');
    function directory(create = false) {
        let current = '/';
        for (const part of path.dirname(filename).split('/').filter(Boolean)) {
            current = path.join(current, part);
            if (create) { try { fs.mkdirSync(current, {mode: 0o750}); } catch (e) { if (e.code !== 'EEXIST') throw e; } }
            if (!fs.lstatSync(current).isDirectory()) throw new Error('state parent must be a directory, not a link');
        }
        try {
            const info = fs.lstatSync(filename);
            if (!info.isFile() || info.nlink !== 1) throw new Error('state path must be a regular file without links');
        } catch (e) { if (e.code !== 'ENOENT') throw e; }
    }
    return {
        save(value) {
            if (!stateObject(value)) throw new Error('save method must return a JSON object');
            stateJSON(value);
            const data = Buffer.from(JSON.stringify({version: 1, state: value}));
            if (data.length > limit) throw new Error('state exceeds limits.max_blob_bytes');
            directory(true);
            const temporary = path.join(path.dirname(filename), '.state-' + require('node:crypto').randomUUID());
            try {
                const fd = fs.openSync(temporary, 'wx', 0o640);
                try { fs.writeFileSync(fd, data); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
                directory();
                fs.renameSync(temporary, filename);
                const parent = fs.openSync(path.dirname(filename), fs.constants.O_RDONLY | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW);
                try { fs.fsyncSync(parent); } finally { fs.closeSync(parent); }
            } finally { try { fs.unlinkSync(temporary); } catch (e) { if (e.code !== 'ENOENT') throw e; } }
        },
        load() {
            directory();
            const fd = fs.openSync(filename, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
            let data;
            try {
                const info = fs.fstatSync(fd);
                if (!info.isFile() || info.nlink !== 1 || info.size > limit) throw new Error('invalid or oversized state file');
                data = Buffer.alloc(Math.min(info.size + 1, limit + 1));
                let total = 0, read;
                while (total < data.length && (read = fs.readSync(fd, data, total, data.length - total, null)) > 0) total += read;
                if (total > info.size || total > limit) throw new Error('state file changed or exceeds limits.max_blob_bytes');
                data = data.subarray(0, total);
            } finally { fs.closeSync(fd); }
            const value = JSON.parse(data.toString('utf8'));
            if (!stateObject(value) || Object.keys(value).sort().join(',') !== 'state,version' || value.version !== 1 || !stateObject(value.state))
                throw new Error('unsupported state document; expected version 1 and object state');
            stateJSON(value);
            return value.state;
        }
    };
}
