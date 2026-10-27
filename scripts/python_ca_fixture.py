"""Additional checks for the installed-release Python trust qualification."""
import hashlib
import json
import ssl
import tarfile

INTERPRETERS = ('/app/.venv/bin/python', '/opt/worker-venv/bin/python')

BUNDLE_INFO = r'''
import certifi, hashlib, json, ssl
from pathlib import Path
p = Path(certifi.where())
c = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
c.load_verify_locations(cafile=str(p))
print(json.dumps({'bundle': str(p), 'digest': hashlib.sha256(p.read_bytes()).hexdigest(),
 'roots': sorted(hashlib.sha256(x).hexdigest() for x in c.get_ca_certs(binary_form=True))}))
'''

# This endpoint exists only in an isolated qualification container. Its certificate
# is the sandbox leaf: unknown issuer in the original image; wrong IP in wrapper.
REJECTION = r'''
import http.server, json, ssl, threading
import httpx
server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), http.server.BaseHTTPRequestHandler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain('/qa-tls/server.pem', '/qa-tls/server-key.pem')
server.socket = context.wrap_socket(server.socket, server_side=True)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    with httpx.Client(verify=True, trust_env=False) as client:
        client.get('https://127.0.0.1:' + str(server.server_port), timeout=5)
except httpx.ConnectError as error:
    cause = error
    while cause is not None and not isinstance(cause, ssl.SSLCertVerificationError):
        cause = cause.__cause__ or cause.__context__
    if cause is None:
        raise AssertionError('failure was not certificate verification') from error
    print(json.dumps({'verify_code': cause.verify_code, 'reason': cause.verify_message}))
else:
    raise AssertionError('invalid certificate accepted')
finally:
    server.shutdown()
    server.server_close()
    thread.join()
'''


def inspect_images(run, docker, target, wrapper, tls_directory):
    ca = ssl.PEM_cert_to_DER_cert((tls_directory / 'ca.pem').read_text())
    ca_digest = hashlib.sha256(ca).hexdigest()
    checks = []
    for python in INTERPRETERS:
        def info(image):
            return json.loads(run(docker, 'run', '--rm', '--network', 'none',
                                  '--entrypoint', python, image, '-I', '-c', BUNDLE_INFO))
        before, after = info(target), info(wrapper)
        if set(after['roots']) != set(before['roots']) | {ca_digest}:
            raise ValueError('Certifi installation lost existing roots or added unexpected roots')
        # The helper runs again in a throwaway container, with no network. Its
        # unchanged bundle digest proves idempotence against real installed Certifi.
        repeat = "import subprocess; subprocess.run(['/usr/local/bin/interceptor','image-install-python-ca','/opt/interceptor/tls/ca.pem','[]'," + repr(python) + "],check=True,stdout=subprocess.DEVNULL);" + BUNDLE_INFO
        repeated = json.loads(run(docker, 'run', '--rm', '--network', 'none', '--user', '0:0',
                                  '--entrypoint', python, wrapper, '-I', '-c', repeat))
        if repeated != after or info(target) != before:
            raise ValueError('repeated installation or original target bundle changed')
        rejections = []
        for image, codes in ((target, {18, 19, 20, 21}), (wrapper, {62, 64})):
            result = json.loads(run(docker, 'run', '--rm', '--network', 'none', '--user', '0:0',
                                    '--mount', f'type=bind,src={tls_directory},dst=/qa-tls,readonly',
                                    '--entrypoint', python, image, '-I', '-c', REJECTION))
            if result['verify_code'] not in codes:
                raise ValueError('expected issuer/hostname verification rejection: ' + str(result))
            rejections.append(result)
        checks.append({'python': python, 'original_root_count': len(before['roots']),
                       'bundle': after['bundle'], 'bundle_sha256': after['digest'],
                       'original_rejection': rejections[0], 'hostname_rejection': rejections[1],
                       'idempotent': True})
    return checks


def verify_evidence(output, diagnostics):
    lines = [line.split('python trust checked: ', 1)[1] for line in diagnostics.splitlines()
             if 'python trust checked: ' in line]
    checks = [json.loads(line) for line in lines]
    if len(checks) != 4 or {item['python'] for item in checks} != set(INTERPRETERS):
        raise ValueError('both environments must run for both application turns')
    if not all(item['sync_https'] and item['async_https'] and item['undeclared_blocked']
               and item['uid'] != 0 for item in checks):
        raise ValueError('unprivileged HTTPX trust checks failed')
    archives = list((output / 'native').glob('*.tar'))
    if len(archives) != 1:
        raise ValueError('missing complete Interceptor evidence archive')
    with tarfile.open(archives[0]) as archive:
        events = [json.loads(line) for line in archive.extractfile('events.jsonl') if line.strip()]
    # Event structure is checked against the real recorder during qualification.
    requests = [event for event in events if event.get('kind') == 'fixture_hit' and event.get('semantic', {}).get('path') == '/page']
    if len(requests) != 8:
        raise ValueError('evidence must contain eight HTTPS fixture hits (two clients, two environments, two turns)')
    return {'clients': checks, 'service_events': len(requests)}
