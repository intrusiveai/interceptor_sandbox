"""An ordinary in-memory agent. No checkpoint or Interceptor integration code."""
import json
import os
from pathlib import Path
from urllib.request import Request, urlopen
from uuid import uuid4


def request(url, method='GET', value=None):
    body = None if value is None else json.dumps(value).encode()
    with urlopen(Request(url, data=body, method=method, headers={'Content-Type': 'application/json'}), timeout=10) as response:
        return json.load(response)


class SupportApplication:
    def __init__(self, customer):
        print('application initialized')
        self.customer = customer
        self.session_id = str(uuid4())
        self.messages = []
        self.turns = 0
        self.guard = 'refund-review-required'

    def invoke(self, value):
        message = value['message']
        self.turns += 1
        self.messages.append({'role': 'user', 'content': message})
        service = os.environ['SERVICE_URL']
        record = request(service)
        if message == 'change':
            record = request(service, 'PATCH', {'count': record['count'] + 1})
            Path('/app-state/count.txt').write_text(str(record['count']))
            call_id = 'change-' + str(self.turns)
            self.messages.extend([
                {'role': 'assistant', 'content': None, 'tool_calls': [{'id': call_id, 'type': 'function', 'function': {'name': 'change_record', 'arguments': '{}'}}]},
                {'role': 'tool', 'tool_call_id': call_id, 'content': json.dumps(record, sort_keys=True, separators=(',', ':'))},
            ])
        response = request(os.environ['MODEL_BASE_URL'] + '/chat/completions', 'POST', {
            'model': 'chat-fixture', 'user': self.customer,
            'messages': [{'role': 'system', 'content': 'Assist ' + self.customer + '; session ' + self.session_id + '; ' + self.guard}] + self.messages,
        })
        self.messages.append(response['choices'][0]['message'])
        return {'customer': self.customer, 'session_id': self.session_id, 'turn': self.turns,
                'guard': self.guard, 'service_count': record['count'],
                'file_count': int(Path('/app-state/count.txt').read_text()), 'messages': len(self.messages)}

    def close(self):
        print('application closed')
