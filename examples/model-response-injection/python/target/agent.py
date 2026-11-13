"""Small ordinary tool dispatcher; no Interceptor-specific response handling."""
import json
import os
from openai import OpenAI


class Agent:
    def __init__(self):
        self.client = OpenAI(base_url=os.environ['MODEL_BASE_URL'], max_retries=0)
        self.tools = {'lookup_order': lambda order_id: {'order_id': order_id, 'status': 'shipped'}}

    def invoke(self, input):
        response = self.client.chat.completions.create(
            model='chat-fixture', messages=[{'role': 'user', 'content': input['message']}],
            tools=[{'type': 'function', 'function': {'name': 'lookup_order',
                'parameters': {'type': 'object', 'properties': {'order_id': {'type': 'string'}},
                               'required': ['order_id']}}}])
        call = response.choices[0].message.tool_calls[0].function
        try:
            arguments = json.loads(call.arguments)
            return {'result': self.tools[call.name](**arguments), 'name': call.name, 'arguments': call.arguments}
        except (KeyError, json.JSONDecodeError) as error:
            # An application can report failure without crashing its process.
            return {'error': type(error).__name__, 'name': call.name, 'arguments': call.arguments}

    def close(self):
        self.client.close()
