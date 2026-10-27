const fs = require('node:fs');
const path = require('node:path');
const {pathToFileURL} = require('node:url');
const readline = require('node:readline');
const cfg = JSON.parse(process.env.INTERCEPTOR_CODE_CONFIG);
const protocolFD = Number(process.env.INTERCEPTOR_PROTOCOL_FD || 1);
// Console and direct stream writes are diagnostics. Production uses fd 3 so
// native writes to fd 1 are also independent of the protocol.
process.stdout.write = process.stderr.write.bind(process.stderr);
function send(type, fields = {}) {
    const bytes = Buffer.from(JSON.stringify({protocol: 'interceptor.application/v1', type, ...fields}) + '\n');
    let offset = 0;
    while (offset < bytes.length) offset += fs.writeSync(protocolFD, bytes, offset, bytes.length - offset);
}

(async () => {
    let close;
    let stage = 'import ' + cfg.module;
    const active = new Set();
    try {
        const entry = cfg.adapter_asset ? pathToFileURL(path.resolve('.interceptor-adapter', cfg.module)).href : cfg.module;
        const mod = await import(entry);
        let target = mod;
        if (cfg.initialize) {
            stage = 'initialize ' + cfg.initialize;
            target = await mod[cfg.initialize](cfg.init_arguments || {});
        }
        stage = 'validate invocation methods';
        if (cfg.close) {
            if (typeof target[cfg.close] !== 'function') throw new TypeError('configured close method is not callable');
            close = target[cfg.close].bind(target);
        }
        if (typeof target[cfg.invoke] !== 'function') throw new TypeError('configured invoke method is not callable');
        const invoke = target[cfg.invoke].bind(target);
        const input = readline.createInterface({input: process.stdin});
        send('ready');
        for await (const line of input) {
            const frame = JSON.parse(line);
            if (frame.type === 'close') { input.close(); break; }
            if (frame.type !== 'invoke') continue;
            const task = (async () => {
                try { send('result', {id: frame.id, output: await invoke(frame.input)}); }
                catch (error) { send('result', {id: frame.id, error: String(error.message || error)}); }
            })();
            active.add(task);
            task.finally(() => active.delete(task));
            if (active.size >= (cfg.concurrency || 1)) await Promise.race(active);
        }
        await Promise.all(active);
    } catch (error) {
        send('error', {error: stage + ': ' + String(error.message || error)});
        throw error;
    } finally {
        try {
            if (close) await close();
            send('closed');
        } catch (error) {
            send('closed', {error: 'adapter cleanup: ' + String(error.message || error)});
            throw error;
        }
    }
})().then(() => process.exit(0), error => { console.error(error); process.exit(1); });
