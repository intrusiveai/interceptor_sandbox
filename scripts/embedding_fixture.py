"""Deterministic host-only providers for packaged SDK qualification.

Never used by the product. Separate listeners and generated keys demonstrate
independent routing without spending against a commercial model account.
"""
import base64
from contextlib import AbstractContextManager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import struct
import tarfile
import threading
import time
import uuid


class EmbeddingFixture(AbstractContextManager):
    def __init__(self):
        self.calls = []
        self.failures = []
        self.servers = []
        self.threads = []
        self.keys = {kind: uuid.uuid4().hex for kind in ('generation', 'embeddings')}
        self.urls = {}

    def __enter__(self):
        for kind in self.keys:
            self.start(kind)
        return self

    def start(self, kind):
        fixture = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                expected = '/v1/embeddings' if kind == 'embeddings' else '/v1/chat/completions'
                if self.path != expected or self.headers.get('Authorization') != 'Bearer '+fixture.keys[kind]:
                    fixture.failures.append('incorrect route or host credential')
                    self.send_error(403)
                    return
                size = int(self.headers.get('Content-Length', '0'))
                if not 0 < size <= 4*1024*1024:
                    self.send_error(413)
                    return
                request = json.loads(self.rfile.read(size))
                model = request['model']
                fixture.calls.append({'provider': kind, 'model': model,
                                      'request': request, 'input': request.get('input'), 'encoding_format': request.get('encoding_format'),
                                      'credential_matched': True})
                status = 200
                if kind == 'generation':
                    response = {'id': 'fixture-chat', 'object': 'chat.completion', 'created': 0,
                                'model': model, 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'generation fixture OK'}, 'finish_reason': 'stop'}],
                                'usage': {'prompt_tokens': 4, 'completion_tokens': 1, 'total_tokens': 5}}
                elif model == 'embed-error':
                    status, response = 429, {'error': {'message': 'fixture rate limit', 'type': 'rate_limit_error'}}
                else:
                    if model == 'embed-timeout':
                        time.sleep(1)
                    inputs = request['input']
                    count = 1 if isinstance(inputs, str) or isinstance(inputs[0], int) else len(inputs)
                    data = []
                    for index in range(count):
                        vector = [index + 0.25, -0.5]
                        if request.get('encoding_format') == 'base64':
                            vector = base64.b64encode(struct.pack('<2f', *vector)).decode()
                        data.append({'object': 'embedding', 'index': index, 'embedding': vector})
                    response = {'object': 'list', 'model': 'fixture-embedding-model', 'data': data,
                                'usage': {'prompt_tokens': count*3, 'total_tokens': count*3}}
                encoded = json.dumps(response).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(encoded)))
                if status == 429:
                    self.send_header('Retry-After', '2')
                self.end_headers()
                try:
                    self.wfile.write(encoded)
                except (BrokenPipeError, ConnectionResetError):
                    pass  # Expected for the explicit client cancellation check.
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.servers.append(server)
        self.threads.append(thread)
        self.urls[kind] = f'http://127.0.0.1:{server.server_port}/v1'

    def verify(self, output):
        if self.failures:
            raise ValueError(self.failures)
        models = [call['model'] for call in self.calls]
        if models.count('chat-fixture') != 2 or models.count('text-embedding-3-small') != 5 or models.count('embed-error') != 1 or models.count('embed-timeout') != 1 or 'forbidden-model' in models:
            raise ValueError('unexpected fixture calls: '+repr(self.calls))
        first = self.calls[0]
        if not isinstance(first['input'][0], list) or len(first['input']) != 2:
            raise ValueError('LangChain did not send the tokenized startup batch')
        if not any(c['encoding_format'] == 'base64' for c in self.calls) or not any(c['encoding_format'] == 'float' for c in self.calls):
            raise ValueError('both SDK vector encodings must be exercised')
        archives = list((output / 'native').glob('*.tar'))
        if len(archives) != 1:
            raise ValueError('missing native evidence')
        with tarfile.open(archives[0]) as archive:
            events = [json.loads(line) for line in archive.extractfile('events.jsonl') if line.strip()]
            # Inspect all retained evidence, including private blobs, for host keys.
            for member in archive:
                if member.isfile():
                    content = archive.extractfile(member).read()
                    if any(key.encode() in content for key in self.keys.values()):
                        raise ValueError('host credential leaked into evidence')
        exchanges = [e for e in events if e['kind'] == 'model_exchange']
        embeddings = [e for e in exchanges if e['semantic'].get('codec') == 'openai_embeddings']
        successful = [e for e in embeddings if e['semantic']['status'] == 200]
        if len(successful) != 5 or successful[0].get('turn_id') or successful[0]['semantic']['input_count'] != 2:
            raise ValueError('startup/turn embeddings evidence missing')
        if sum(e['semantic']['usage']['total_tokens'] for e in successful) != 18:
            raise ValueError('incorrect embeddings token accounting')
        if any(e['semantic'].get('dimensions') != 2 or e['semantic'].get('injection_ids') for e in successful):
            raise ValueError('incorrect embeddings dimensions or generation attribution')
        if not any(e['semantic']['status'] == 429 for e in embeddings):
            raise ValueError('provider error evidence missing')
        (output / 'provider-calls.json').write_text(json.dumps(self.calls, indent=2)+'\n')
        return {'provider': 'deterministic host fixture', 'live_provider': False,
                'startup_batch': True, 'generation_calls': 2, 'embedding_exchanges': 5,
                'embedding_tokens': 18, 'encodings': ['base64', 'float'], 'separate_credentials': True}

    def __exit__(self, *args):
        for server in self.servers:
            server.shutdown()
            server.server_close()
        for thread in self.threads:
            thread.join(timeout=5)
