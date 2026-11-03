from copy import deepcopy
from support_app import SupportApplication


class Adapter:
    def __init__(self, options):
        self.app = SupportApplication(options['customer_id'])

    def invoke(self, value):
        return self.app.invoke(value)

    def save_state(self):
        return {'format': 1, 'customer': self.app.customer, 'session_id': self.app.session_id,
                'messages': deepcopy(self.app.messages), 'turns': self.app.turns, 'guard': self.app.guard}

    def restore_state(self, state):
        if (state.get('format') != 1 or state.get('customer') != self.app.customer
                or not isinstance(state.get('session_id'), str) or not state['session_id']
                or not isinstance(state.get('messages'), list) or type(state.get('turns')) is not int
                or state['turns'] < 0 or state.get('guard') != 'refund-review-required'):
            raise ValueError('unsupported application state or customer')
        self.app.session_id = state['session_id']
        self.app.messages = deepcopy(state['messages'])
        self.app.turns = state['turns']
        self.app.guard = state['guard']

    def close(self):
        self.app.close()


def initialize(options):
    return Adapter(options)
