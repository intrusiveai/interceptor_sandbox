"""Native Titan SDK example; fixture checks are explicitly enabled for CI only."""
import asyncio
import importlib.metadata
import json
import math
import os

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError
from langchain_aws import BedrockEmbeddings
from openai import OpenAI

MODEL = 'amazon.titan-embed-text-v2:0'


class SupportApplication:
    def __init__(self):
        self.loop = asyncio.get_running_loop()
        self.fixture = os.environ.get('TITAN_FIXTURE_CHECKS') == '1'
        self.native = boto3.client(
            'bedrock-runtime', endpoint_url=os.environ['AWS_ENDPOINT_URL_BEDROCK_RUNTIME'],
            region_name=os.environ['AWS_REGION'],
            config=Config(retries={'total_max_attempts': 1}, connect_timeout=5, read_timeout=10))
        self.embeddings = BedrockEmbeddings(
            client=self.native, model_id=MODEL,
            model_kwargs={'dimensions': 256, 'normalize': True}, normalize=False)
        self.texts = ['account help', 'billing help']
        self.documents = self.embeddings.embed_documents(self.texts)
        assert len(self.documents) == 2 and all(len(v) == 256 for v in self.documents)
        self.client = OpenAI(base_url=os.environ['MODEL_BASE_URL'],
                             api_key=os.environ['OPENAI_API_KEY'], max_retries=0, timeout=10)
        for name in ('boto3', 'langchain-aws', 'openai'):
            print(f'{name}={importlib.metadata.version(name)}')
        print('application initialized')

    def start_session(self, customer):
        return {'customer': customer, 'turns': 0}

    def embed_native(self, text, **options):
        response = self.native.invoke_model(modelId=MODEL, contentType='application/json',
                                           accept='application/json',
                                           body=json.dumps({'inputText': text, **options}))
        return json.loads(response['body'].read())

    async def close(self):
        self.native.close()
        self.client.close()
        print('application closed')


async def invoke_support(app, session, message):
    assert app.loop is asyncio.get_running_loop()
    if message == 'error':
        raise ValueError('requested application error')
    query = app.embeddings.embed_query(message)
    assert len(query) == 256 and all(math.isfinite(v) for v in query)
    scores = [sum(a*b for a, b in zip(query, document)) for document in app.documents]
    selected = app.texts[max(range(len(scores)), key=scores.__getitem__)]
    if app.fixture:
        assert selected == 'account help'
    # Native SDK response handling, including the absence of the float field.
    binary = app.embed_native(message, dimensions=512, normalize=False, embeddingTypes=['binary'])
    assert 'embedding' not in binary and len(binary['embeddingsByType']['binary']) == 512
    both = app.embed_native(message, embeddingTypes=['float', 'binary'])
    assert len(both['embedding']) == len(both['embeddingsByType']['binary']) == 1024
    assert both['embedding'] == both['embeddingsByType']['float']
    generation = app.client.chat.completions.create(
        model=os.environ['GENERATION_MODEL'],
        messages=[{'role': 'user', 'content': f'{message}; retrieved: {selected}'}])
    assert generation.choices[0].message.content
    if app.fixture and session['turns'] == 0:
        for text, code in [('fixture-throttle', 'ThrottlingException'),
                           ('fixture-denied', 'AccessDeniedException')]:
            try:
                app.embed_native(text)
                raise AssertionError('expected native AWS error')
            except ClientError as error:
                assert error.response['Error']['Code'] == code
                assert error.response['ResponseMetadata']['RequestId']
                assert error.response['ResponseMetadata']['RetryAttempts'] == 0
    session['turns'] += 1
    print('application invoked with Titan embeddings, retrieval and generation')
    return {'customer': session['customer'], 'turn': session['turns'], 'message': message}
