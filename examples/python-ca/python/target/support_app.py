import asyncio
import subprocess


class SupportApplication:
    def __init__(self):
        self.loop = asyncio.get_running_loop()
        print('application initialized')

    def start_session(self, customer):
        return {'customer': customer, 'turns': 0}

    async def close(self):
        assert self.loop is asyncio.get_running_loop()
        print('application closed')


async def invoke_support(app, session, message):
    assert app.loop is asyncio.get_running_loop()
    if message == 'error':
        raise ValueError('requested application error')
    for python in ['/app/.venv/bin/python', '/opt/worker-venv/bin/python']:
        result = subprocess.run([python, '/app/web_client.py', 'https://web.sandbox.test/page'],
                                cwd='/', capture_output=True, text=True, timeout=10, check=True)
        print('python trust checked: ' + result.stdout.strip())
    session['turns'] += 1
    return {'customer': session['customer'], 'turn': session['turns'], 'message': message}
