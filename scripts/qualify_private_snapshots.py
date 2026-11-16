#!/usr/bin/env python3
"""Exercise native private-file snapshots using a local CLI and Docker.

Accepts an extracted release CLI or a development CLI (run from the checkout).
Evidence stays in --output. Only isolated test state/images are cleaned up.
"""
import argparse
import json
import hashlib
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
import uuid


def qualify(cli, client, output, image, transport, oracles_only=False):
    if os.getuid() == 0:
        raise ValueError('qualification requires a non-root supervisor host identity')
    output.mkdir(parents=True, exist_ok=False)
    tag = 'interceptor-private-snapshots:' + uuid.uuid4().hex
    with tempfile.TemporaryDirectory(prefix='interceptor-private-qa-') as temp:
        work = Path(temp).resolve()
        env = dict(os.environ, INTERCEPTOR_DATA_DIR=str(work / 'data'))
        def run(*args, timeout=180):
            result = subprocess.run(args, env=env, text=True, capture_output=True, timeout=timeout)
            if result.returncode:
                raise RuntimeError(f'{args}: {result.stdout}\n{result.stderr}')
            return result.stdout.strip()
        image_id = run('docker', 'image', 'inspect', '--format', '{{.Id}}', image)
        blueprint = work / 'environment'
        (blueprint / 'assets/seed').mkdir(parents=True)
        (blueprint / 'assets/probe').mkdir()
        shutil.copyfile(Path(__file__).resolve().parents[1] / 'examples/private-file-snapshots/probe.py', blueprint / 'assets/probe/probe.py')
        (blueprint / 'assets/seed/seed.txt').write_text('supervisor-seed')
        (blueprint / 'assets/seed/seed.txt').chmod(0o600)
        record = {
            'api_version': 'interceptor.dev/v1alpha1', 'kind': 'Environment',
            'metadata': {'name':'private-snapshot-qa', 'version':'1', 'reviewed_by':'qualification', 'reviewed_at':'2026-11-10T00:00:00Z'},
            'application': {'image': image + '@' + image_id,
                'launch': {'command':['/usr/local/bin/python3','/environment/probe/probe.py'], 'workdir':'/tmp',
                          'environment':{'MODEL_BASE_URL':'http://127.0.0.1:18080/v1','PYTHONDONTWRITEBYTECODE':'1'}},
                'interface': {'kind':'command','operations':[{'id':'probe'}],
                    'command':{'stdin':'raw','output_media_type':'application/json','success_exit_codes':[0],'concurrency':1},
                    'startup_timeout_ms':30000,'turn_timeout_ms':30000},
                'persistence':{'snapshot_capable':not oracles_only,'mutable_roots':['/state/app'],
                               'quiesce':{'kind':'command','command':['/bin/true'],'timeout_ms':5000}}},
            'filesystem':{'seed':[{'source':'assets/probe','target':'/environment/probe','read_only':True},
                                  {'source':'assets/seed','target':'/state/app','read_only':False}]},
            'oracles':[
                {'id':'private-exists','kind':'file_state_match','match':{'path':'/state/app/evidence/nested/state.txt','exists':True}},
                {'id':'private-content','kind':'file_state_match','match':{'path':'/state/app/evidence/nested/state.txt','content_contains':'snapshot-private-content'}},
                {'id':'private-digest','kind':'file_state_match','match':{'path':'/state/app/evidence/nested/state.txt','digest':'sha256:'+hashlib.sha256(b'snapshot-private-content').hexdigest()}},
                {'id':'private-updated','kind':'file_state_match','match':{'path':'/state/app/evidence/nested/state.txt','content_contains':'-updated'}},
                {'id':'nested-missing','kind':'file_state_match','match':{'path':'/state/app/evidence/nested/missing.txt','absent':True}}],
            'time':{'base':'2026-01-01T00:00:00Z','mode':'fixed'},
            'limits':{'max_turns':30,'max_snapshots':10,'max_blob_bytes':10485760,'max_wall_seconds':600,'max_tokens':10000}}
        (blueprint/'environment.yaml').write_text(json.dumps(record))
        process = None
        try:
            run(str(cli),'environment','seal',str(blueprint))
            run(str(cli),'app','build','--environment',str(blueprint),'--tag',tag,timeout=900)
            with (output/'run.log').open('w') as log:
                process = subprocess.Popen([str(cli),'run','--environment',str(blueprint),
                    '--model-provider','litellm','--provider-auth','none','--provider-base-url','http://127.0.0.1:9/v1',
                    '--allow-model','unused','--bridge-transport',transport],env=env,stdout=log,stderr=subprocess.STDOUT)
                deadline = time.monotonic()+60
                while time.monotonic()<deadline:
                    if process.poll() is not None:
                        raise RuntimeError((output/'run.log').read_text())
                    records=json.loads(run(str(cli),'list','--json'))['sessions'] or []
                    if any(r.get('phase')=='running' for r in records) and 'Interceptor local API:' in (output/'run.log').read_text(): break
                    time.sleep(.2)
                else: raise TimeoutError('wrapper did not become ready')
                active = next(r for r in records if r.get('phase')=='running')
                diagnostic = "import os,json; p='/state/app'; f=os.open(p,os.O_RDONLY|os.O_DIRECTORY); print(json.dumps({'path':list(os.stat(p)), 'fd':list(os.fstat(f)), 'dot':list(os.stat('.',dir_fd=f)), 'mounts':open('/proc/self/mountinfo').read()}))"
                (output/'identity.json').write_text(run('docker','exec','--user',str(os.getuid())+':'+str(os.getgid()),active['container_id'],'python3','-c',diagnostic))
                native = run(str(client),'--private-file-oracles' if oracles_only else '--private-files','--output',str(output/'native'),'--data-root',str(work/'data'),timeout=360)
                (output/'client.log').write_text(native+'\n')
                process.send_signal(signal.SIGINT)
                process.wait(timeout=45)
                if process.returncode: raise RuntimeError((output/'run.log').read_text())
                process=None
            print((output/'native/result.json').read_text())
        finally:
            if process is not None and process.poll() is None:
                process.send_signal(signal.SIGINT)
                try: process.wait(timeout=45)
                except subprocess.TimeoutExpired: process.kill();process.wait()
            # Exercise normal product purge; no permission repair is permitted.
            if (work/'data').exists():
                volumes=[]
                for binding in (work/'data/container-bindings').glob('*.json'):
                    volumes.extend(json.loads(binding.read_text()).get('mutable_volumes',[]))
                result_file=output/'native/result.json'
                if result_file.exists() and len(volumes)!=len(json.loads(result_file.read_text())['sessions']):
                    raise RuntimeError('expected one managed mutable volume per qualification session')
                run(str(cli),'purge','--all')
                remaining=set(run('docker','volume','ls','--format','{{.Name}}').splitlines())
                if remaining.intersection(volumes):
                    raise RuntimeError('qualification left managed mutable volumes behind')
                (output/'cleanup.json').write_text(json.dumps({'purged':True,'mutable_volumes_removed':volumes})+'\n')
            subprocess.run(['docker','image','rm',tag],capture_output=True,timeout=30)

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cli',type=Path,required=True)
    parser.add_argument('--client',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--image',default='python:3.12-slim')
    parser.add_argument('--transport',choices=['spool','unix'],default='spool')
    parser.add_argument('--oracles-only',action='store_true',help='check private-file oracles with snapshots disabled')
    args=parser.parse_args()
    qualify(args.cli.resolve(),args.client.resolve(),args.output.resolve(),args.image,args.transport,args.oracles_only)
