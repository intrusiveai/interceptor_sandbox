"""Host-only, signed InvokeModel fixture for archive qualification; never a provider."""
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import tarfile
import threading
import uuid
from urllib.parse import quote

from embedding_fixture import EmbeddingFixture

MODEL = 'amazon.titan-embed-text-v2:0'


class TitanFixture(EmbeddingFixture):
    def __init__(self):
        super().__init__()
        self.access_key = 'AKIA' + uuid.uuid4().hex[:16].upper()
        self.session_token = uuid.uuid4().hex

    def start(self, kind):
        if kind == 'generation':
            return super().start(kind)
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                size = int(self.headers.get('Content-Length', '0'))
                if not 0 < size <= 4*1024*1024:
                    self.send_error(413)
                    return
                raw = self.rfile.read(size)
                try:
                    fixture.verify_signature(self, raw)
                    request = json.loads(raw)
                    assert self.path == '/model/amazon.titan-embed-text-v2%3A0/invoke', 'unexpected InvokeModel path'
                    assert self.headers.get('Accept') == 'application/json'
                    assert self.headers.get('Content-Type') == 'application/json'
                except Exception as error:
                    fixture.failures.append('native request/signature check failed: '+str(error))
                    self.send_error(403)
                    return
                fixture.calls.append({'provider': 'embeddings', 'model': MODEL,
                                      'request': request, 'credential_matched': True})
                text = request['inputText']
                status, error_type = 200, None
                if text in ('fixture-throttle', 'fixture-denied'):
                    status = 429 if text == 'fixture-throttle' else 403
                    error_type = 'ThrottlingException' if status == 429 else 'AccessDeniedException'
                    # Ensure provider diagnostics cannot leak into native evidence.
                    response = {'message': fixture.session_token}
                else:
                    dimensions = request.get('dimensions', 1024)
                    vector = [0] * dimensions
                    vector[1 if text == 'billing help' else 0] = 1
                    types = request.get('embeddingTypes', ['float'])
                    response = {'inputTextTokenCount': 3,
                                'embeddingsByType': {kind: vector for kind in types}}
                    if 'float' in types:
                        response['embedding'] = vector
                body = json.dumps(response).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(body)))
                self.send_header('X-Amzn-Requestid', 'titan-fixture-request')
                if error_type:
                    self.send_header('X-Amzn-Errortype', error_type)
                self.end_headers()
                self.wfile.write(body)

        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.servers.append(server)
        self.threads.append(thread)
        self.urls[kind] = f'http://127.0.0.1:{server.server_port}'

    def verify_signature(self, handler, body):
        authorization = handler.headers['Authorization']
        algorithm, fields = authorization.split(' ', 1)
        fields = dict(item.split('=', 1) for item in fields.split(', '))
        key, scope = fields['Credential'].split('/', 1)
        date, region, service, end = scope.split('/')
        assert algorithm == 'AWS4-HMAC-SHA256' and key == self.access_key
        assert region == 'us-west-2' and service == 'bedrock' and end == 'aws4_request'
        assert handler.headers['X-Amz-Security-Token'] == self.session_token
        signed = fields['SignedHeaders']
        headers = ''.join(name+':'+ ' '.join(handler.headers[name].split())+'\n'
                          for name in signed.split(';'))
        # SigV4 canonicalizes the escaped wire path again (unlike S3 signing).
        canonical = '\n'.join(['POST', quote(handler.path, safe='/~'), '', headers, signed,
                               hashlib.sha256(body).hexdigest()])
        string_to_sign = '\n'.join([algorithm, handler.headers['X-Amz-Date'], scope,
                                    hashlib.sha256(canonical.encode()).hexdigest()])
        signing_key = ('AWS4'+self.keys['embeddings']).encode()
        for item in (date, region, service, end):
            signing_key = hmac.new(signing_key, item.encode(), hashlib.sha256).digest()
        expected = hmac.new(signing_key, string_to_sign.encode(), hashlib.sha256).hexdigest()
        assert hmac.compare_digest(expected, fields['Signature']), 'signature mismatch'

    def verify(self, output):
        if self.failures:
            raise ValueError(self.failures)
        if len(self.calls) != 12 or sum(c['provider'] == 'generation' for c in self.calls) != 2:
            raise ValueError('unexpected Titan fixture call count (possible retries): '+repr(self.calls))
        archives = list((output / 'native').glob('*.tar'))
        if len(archives) != 1:
            raise ValueError('missing native evidence')
        with tarfile.open(archives[0]) as archive:
            events = [json.loads(line) for line in archive.extractfile('events.jsonl') if line.strip()]
            secrets = [*self.keys.values(), self.access_key, self.session_token]
            for member in archive:
                if member.isfile() and any(secret.encode() in archive.extractfile(member).read()
                                           for secret in secrets):
                    raise ValueError('host credential leaked into evidence')
        exchanges = [e for e in events if e['kind'] == 'model_exchange'
                     and e['semantic'].get('codec') == 'aws_titan_embeddings']
        success = [e for e in exchanges if e['semantic']['status'] == 200]
        if len(success) != 8 or len(exchanges) != 10:
            raise ValueError('missing embeddings evidence')
        if any(e.get('turn_id') for e in success[:2]) or not all(e.get('turn_id') for e in success[2:]):
            raise ValueError('incorrect initialization/turn attribution')
        if sum(e['semantic']['usage']['total_tokens'] for e in success) != 24:
            raise ValueError('incorrect embeddings token accounting')
        if {e['semantic']['dimensions'] for e in success} != {256, 512, 1024}:
            raise ValueError('dimensions not exercised')
        if any(e['semantic'].get('injection_ids') or e['semantic'].get('reported_model') for e in success):
            raise ValueError('invented generation attribution or reported model')
        if any(e['semantic']['provider_metadata'].get('request_id') != 'titan-fixture-request' for e in exchanges):
            raise ValueError('missing AWS request identity')
        (output / 'provider-calls.json').write_text(json.dumps(self.calls, indent=2)+'\n')
        return {'provider': 'signed native Bedrock fixture', 'live_provider': False,
                'startup_documents': 2, 'generation_calls': 2, 'embedding_exchanges': 8,
                'embedding_tokens': 24, 'representations': ['float', 'binary', 'both'],
                'sdk_errors': ['ThrottlingException', 'AccessDeniedException'],
                'separate_credentials': True, 'retrieval': 'account help'}
