from support_app import SupportApplication, invoke_support
from helpers import message_from


class Adapter:
    def __init__(self, options):
        self.app = SupportApplication()
        self.session = self.app.start_session(options['customer_id'])

    async def invoke(self, value):
        return await invoke_support(self.app, self.session, message_from(value))

    async def close(self):
        await self.app.close()


async def initialize(options):
    return Adapter(options)
