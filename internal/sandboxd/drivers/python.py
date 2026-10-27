import asyncio
import importlib
import inspect
import json
import os
from pathlib import Path
import sys

# Keep native fd-1 writes as well as print() away from the protocol. In the
# sandbox the protocol is a dedicated inherited pipe; fd 1 supports driver tests.
protocol = os.fdopen(os.dup(int(os.environ.get('INTERCEPTOR_PROTOCOL_FD', '1'))), 'w', buffering=1)
os.dup2(2, 1)


def send(kind, **fields):
    protocol.write(json.dumps(dict(protocol='interceptor.application/v1', type=kind, **fields), default=str) + '\n')
    protocol.flush()


async def resolve(value):
    return await value if inspect.isawaitable(value) else value


async def main():
    cfg = json.loads(os.environ['INTERCEPTOR_CODE_CONFIG'])
    close = None
    stage = 'import ' + cfg['module']
    try:
        if cfg.get('adapter_asset'):
            root = Path.cwd() / '.interceptor-adapter'
            sys.path.insert(0, str(root))
        mod = importlib.import_module(cfg['module'])
        if cfg.get('adapter_asset') and (not getattr(mod, '__file__', None) or root not in Path(mod.__file__).resolve().parents):
            raise ValueError('entry module must come from the installed adapter; reseal and rebuild')
        target = mod
        if cfg.get('initialize'):
            stage = 'initialize ' + cfg['initialize']
            target = await resolve(getattr(mod, cfg['initialize'])(cfg.get('init_arguments') or {}))
        stage = 'validate invocation methods'
        if cfg.get('close'):
            close = getattr(target, cfg['close'])
            if not callable(close):
                raise TypeError('configured close method is not callable')
        invoke = getattr(target, cfg['invoke'])
        if not callable(invoke):
            raise TypeError('configured invoke method is not callable')
        send('ready')
        while True:
            # The event loop stays alive while waiting, including background
            # tasks belonging to clients created by an async initializer.
            line = await asyncio.get_running_loop().run_in_executor(None, sys.stdin.readline)
            if not line:
                break
            frame = json.loads(line)
            if frame.get('type') == 'close':
                break
            if frame.get('type') != 'invoke':
                continue
            try:
                value = frame.get('input')
                result = await resolve(invoke(**value) if cfg.get('input_mode') == 'kwargs' else invoke(value))
                send('result', id=frame['id'], output=result)
            except Exception as error:
                send('result', id=frame.get('id'), error=str(error))
    except Exception as error:
        send('error', error=stage + ': ' + str(error))
        raise
    finally:
        try:
            if callable(close):
                await resolve(close())
            send('closed')
        except Exception as error:
            send('closed', error='adapter cleanup: ' + str(error))
            raise


asyncio.run(main())
