"""SDK interoperability fixture; vectors are synthetic, not a quality benchmark."""
import asyncio
import importlib.metadata
import math
import os

from langchain_openai import OpenAIEmbeddings
from openai import OpenAI, APIStatusError, APITimeoutError


class SupportApplication:
    def __init__(self):
        self.loop = asyncio.get_running_loop()
        base = os.environ['MODEL_BASE_URL']
        key = os.environ['OPENAI_API_KEY']  # Declared synthetic Blueprint credential.
        self.client = OpenAI(base_url=base, api_key=key, max_retries=0, timeout=5)
        self.embeddings = OpenAIEmbeddings(
            model='text-embedding-3-small', dimensions=2, chunk_size=128,
            base_url=base, api_key=key, max_retries=0, request_timeout=5)
        self.documents = self.embeddings.embed_documents(['account help', 'billing help'])
        assert self.documents == [[0.25, -0.5], [1.25, -0.5]]
        for name in ('openai', 'langchain-openai', 'tiktoken'):
            print(f'{name}={importlib.metadata.version(name)}')
        print('application initialized')

    def start_session(self, customer):
        return {'customer': customer, 'turns': 0}

    async def close(self):
        self.client.close()
        print('application closed')


async def invoke_support(app, session, message):
    assert app.loop is asyncio.get_running_loop()
    if message == 'error':
        raise ValueError('requested application error')
    vector = app.embeddings.embed_query(message)
    assert vector == [0.25, -0.5]
    # Explicit float encoding as well as the SDK's normal base64 decoding.
    direct = app.client.embeddings.create(model='text-embedding-3-small', input=[1, 2], dimensions=2, encoding_format='float')
    assert direct.data[0].embedding == vector
    assert all(math.isfinite(v) for v in vector)
    generation = app.client.chat.completions.create(model='chat-fixture', messages=[{'role': 'user', 'content': message}])
    assert generation.choices[0].message.content == 'generation fixture OK'
    if session['turns'] == 0:
        for model, status in [('forbidden-model', 502), ('embed-error', 429)]:
            try:
                app.client.embeddings.create(model=model, input='test')
                raise AssertionError('expected a model error')
            except APIStatusError as error:
                assert error.status_code == status
                if status == 429:
                    assert error.response.headers['retry-after'] == '2'
        try:
            app.client.with_options(timeout=0.2).embeddings.create(model='embed-timeout', input='test')
            raise AssertionError('expected a timeout')
        except APITimeoutError:
            pass
    session['turns'] += 1
    print('application invoked with embeddings and generation')
    return {'customer': session['customer'], 'turn': session['turns'], 'message': message}
