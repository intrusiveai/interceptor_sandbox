"""Local HTTPS/Vault-shaped fixtures for qualification, not a Langfuse deployment."""
import base64
import gzip
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import shutil
import ssl
import subprocess
import tarfile
import tempfile
import threading
import uuid


class HTTPSSeamFixture:
    def __enter__(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='interceptor-https-fixture-')
        root = Path(self.temporary.name)
        self.ca, key = root/'ca.pem', root/'key.pem'
        config = root/'openssl.cnf'
        config.write_text('''[req]
prompt=no
distinguished_name=dn
x509_extensions=extensions
[dn]
CN=localhost
[extensions]
subjectAltName=DNS:localhost,IP:127.0.0.1
basicConstraints=critical,CA:TRUE
keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid:always
''')
        subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','2',
                        '-config',str(config),'-keyout',str(key),'-out',str(self.ca)],
                       check=True, capture_output=True)
        key.chmod(0o600)
        self.secrets = {name: 'qa-'+uuid.uuid4().hex for name in ('public','secret','guardrail')}
        self.calls, self.failures = [], []
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args): pass

            def reply(self, status, value):
                body = json.dumps(value).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                if self.path.split('?', 1)[0] != '/v1/secret/data/qa':
                    self.reply(404, {})
                    return
                self.reply(200, {'data': {'data': fixture.secrets, 'metadata': {'version': 1}}})

            def do_POST(self):
                size = int(self.headers.get('Content-Length', '0'))
                if not 0 < size <= 4*1024*1024:
                    self.reply(413, {})
                    return
                body = self.rfile.read(size)
                if self.path == '/api/public/otel/v1/traces':
                    expected = 'Basic '+base64.b64encode((fixture.secrets['public']+':'+fixture.secrets['secret']).encode()).decode()
                    if self.headers.get('Authorization') != expected:
                        fixture.failures.append('tracing authentication mismatch')
                        self.reply(401, {'error': 'wrong authentication'})
                        return
                    if self.headers.get('Content-Encoding') == 'gzip': body = gzip.decompress(body)
                    # The pinned real SDK emits protobuf. This checks delivery,
                    # not ingestion/persistence by a real Langfuse installation.
                    fixture.calls.append({'kind':'tracing', 'startup': b'https-seam-startup' in body,
                                          'shutdown': b'https-seam-shutdown' in body})
                    self.send_response(200)
                    self.send_header('Content-Type', 'application/x-protobuf')
                    self.send_header('Content-Length', '0')
                    self.end_headers()
                elif self.path == '/check':
                    if self.headers.get('Authorization') != 'Bearer '+fixture.secrets['guardrail']:
                        fixture.failures.append('guardrail authentication mismatch')
                        self.reply(401, {'error':'wrong authentication'})
                        return
                    request = json.loads(body)
                    fixture.calls.append({'kind':'guardrail','message':request['text']})
                    self.reply(200, {'allowed':request['text']=='allow', 'diagnostic':'response-sentinel'})
                else:
                    self.reply(404, {})

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(self.ca, key)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.origin = f'https://localhost:{self.server.server_port}'
        return self

    def __exit__(self, *args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.temporary.cleanup()

    def options(self, work, debug):
        credentials = {'profiles':[{'api_version':'interceptor.dev/secret-store-profile/v1alpha1',
            'kind':'SecretStoreProfile','id':'qa-vault','backend_kind':'hashicorp-vault',
            'vault_url':self.origin,'vault_proxy':True,'vault_ca_certificate':str(self.ca),
            'allowed_locator_prefixes':['vault://secret/qa']}], 'credentials':[]}
        for field in self.secrets:
            credentials['credentials'].append({'api_version':'interceptor.dev/host-credential-ref/v1alpha1',
                'kind':'HostCredentialRef','credential_id':field,'store_profile_id':'qa-vault',
                'locator':{'backend_kind':'hashicorp-vault','mount':'secret','path':'qa','version':'1'},
                'value':{'format':'json-string-field','field':field}})
        stores = work/'credentials.json'
        stores.write_text(json.dumps(credentials)); stores.chmod(0o600)
        config = work/'https-seams.json'
        config.write_text(json.dumps({'seams':[
            {'id':'tracing','origin':self.origin,'ca_file':str(self.ca),
             'auth':{'kind':'basic','username_credential':'public','password_credential':'secret'}},
            {'id':'guardrail','origin':self.origin,'ca_file':str(self.ca),
             'auth':{'kind':'header','header':'Authorization','credential':'guardrail','prefix':'Bearer '},
             'debug':{'enabled':debug}}]})); config.chmod(0o600)
        return ['--https-seam-config',str(config),'--credential-config',str(stores)]

    def verify(self, output, data, debug):
        if self.failures: raise ValueError(self.failures)
        if sum(c['kind']=='tracing' for c in self.calls) != 4:
            raise ValueError('unexpected SDK request count, including pre-request failure')
        stderr = '\n'.join(p.read_text() for p in data.glob('sessions/*/logs/application.stderr.log'))
        if 'Langfuse initialization failed before a request' not in stderr or 'Sample rate must be between' not in stderr:
            raise ValueError('SDK initialization failure not diagnosable through application stderr')
        if not any(c.get('startup') for c in self.calls): raise ValueError('startup trace not delivered')
        if not any(c.get('shutdown') for c in self.calls): raise ValueError('shutdown trace not delivered')
        if [c['message'] for c in self.calls if c['kind']=='guardrail'] != ['allow','deny']:
            raise ValueError('guardrail requests lost or retried')
        for archive in (output/'native').glob('*.tar'):
            with tarfile.open(archive) as tar:
                for item in tar:
                    if not item.isfile(): continue
                    content = tar.extractfile(item).read()
                    if any(s.encode() in content for s in self.secrets.values()): raise ValueError('host credential in exported evidence')
                    if b'request-sentinel' in content or b'response-sentinel' in content:
                        raise ValueError('seam payload in exported evidence')
        captures = list(data.glob('sessions/*/logs/https-seam-*-debug.jsonl'))
        if bool(captures) != debug: raise ValueError('unexpected debug capture state')
        for file in data.glob('sessions/*/logs/*'):
            raw = file.read_bytes()
            if any(s.encode() in raw for s in self.secrets.values()): raise ValueError('host credential in logs')
            if 'debug' not in file.name and (b'request-sentinel' in raw or b'response-sentinel' in raw):
                raise ValueError('payload in ordinary logs')
        if debug:
            records = [json.loads(line) for file in captures for line in file.read_text().splitlines()]
            if not records or not all(b'request-sentinel' in base64.b64decode(r['request_body']) and
                                     b'response-sentinel' in base64.b64decode(r['response_body']) for r in records):
                raise ValueError('missing host-visible diagnostic bodies')
        for file in data.glob('sessions/*/logs/https-seam-*'):
            shutil.copyfile(file,output/file.name)
        return {'pinned_langfuse_sdk':'4.7.0','live_langfuse':False,'upstream':'local HTTPS fixture',
                'private_ca':True,'host_managed_auth':True,'startup_and_shutdown_export':True,
                'guardrail_allow_deny':True,'sdk_pre_request_failure_diagnosed':True,'debug':debug,'payloads_excluded_from_evidence':True}
