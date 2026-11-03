import asyncio
from session_math import next_turn


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
    if message == 'slow':
        await asyncio.sleep(20)
    if message == 'error':
        raise ValueError('requested application error')
    session['turns'] = next_turn(session['turns'])
    print('application invoked')
    return {'customer': session['customer'], 'turn': session['turns'], 'message': message}
