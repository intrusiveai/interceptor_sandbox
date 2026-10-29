#!/usr/bin/env python3
"""Exercise an extracted release with Docker, outside the checkout and without Go.

Uses the documented command application interface with /bin/cat as the target.
The supplied image must already exist locally and match the host architecture.
Only uniquely named test images/containers and an isolated data root are removed.
"""

import argparse
from contextlib import nullcontext
from embedding_fixture import EmbeddingFixture
import python_ca_fixture
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import uuid

from publish_release import read_archive
from build_release import ARCHIVE_ROOT


def qualify(archive, version, source, base_image, output, client, adapter_language=None, embeddings=False, python_ca=False, state_snapshots=False, evidence_failure=False, postgresql=False, example_directory=None, customer_support=False, database_test=None):
    if evidence_failure and not (state_snapshots or postgresql):
        raise ValueError("evidence failure qualification requires state snapshots")
    if (state_snapshots or postgresql) and (not adapter_language or embeddings or python_ca):
        raise ValueError("state snapshots require an adapter language and no other qualification mode")
    if python_ca and (adapter_language != "python" or embeddings):
        raise ValueError("Python CA qualification requires --adapter-language python and no --embeddings")
    if embeddings and adapter_language != "python":
        raise ValueError("embeddings qualification requires --adapter-language python")
    metadata = read_archive(archive)
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    host = platform.system().lower() + "/" + str(arch)
    if metadata["platform"] != host or metadata["version"] != version or metadata["source_commit"] != source:
        raise ValueError("archive must match the native host, requested source and release version")
    docker = shutil.which("docker")
    if not docker:
        raise ValueError("Docker must be installed and running")
    if output.exists():
        raise ValueError("output must be a new directory")
    output.mkdir(parents=True)
    output = output.resolve()
    suffix = uuid.uuid4().hex
    target, wrapper = "interceptor-release-target:"+suffix, "interceptor-release-wrapper:"+suffix
    inspection = "interceptor-release-check-"+suffix
    with (EmbeddingFixture() if (embeddings or state_snapshots or postgresql) else nullcontext()) as fixture, tempfile.TemporaryDirectory(prefix="interceptor-release-qa-") as temporary:
        work = Path(temporary).resolve()
        install, outside, bin_dir = (work / name for name in ("install", "outside", "bin"))
        for directory in (install, outside, bin_dir): directory.mkdir()
        with tarfile.open(archive) as bundle:
            bundle.extractall(install, filter="data")  # read_archive checked the exact regular-file inventory.
        install = install / ARCHIVE_ROOT
        (bin_dir / "docker").symlink_to(Path(docker).resolve())
        env = dict(os.environ, PATH=str(bin_dir), INTERCEPTOR_DATA_DIR=str(work / "data"))
        if shutil.which("go", path=env["PATH"]) is not None:
            raise ValueError("qualification must not expose Go to the CLI")
        cli = str(install / "interceptor")
        def run(*args):
            result = subprocess.run(args, cwd=outside, env=env, text=True, capture_output=True, timeout=900 if "build" in args else 180)
            if result.returncode:
                raise RuntimeError(f"{args}: {result.stdout}\n{result.stderr}")
            return result.stdout.strip()
        if run(cli, "version") != version:
            raise ValueError("extracted CLI version differs")
        daemon = json.loads(run(docker, "info", "--format", "{{json .}}"))
        daemon_arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(daemon["Architecture"])
        if daemon["OSType"] != "linux" or daemon_arch != arch:
            raise ValueError("Docker daemon must run Linux with the same architecture as the CLI")
        base = json.loads(run(docker, "image", "inspect", base_image))[0]
        if base["Os"] != "linux" or base["Architecture"] != arch:
            raise ValueError("target image must be native Linux; cross-architecture execution is not qualified")
        # Reference only the inspected local image ID, never fetch application code.
        # A tag lets BuildKit resolve the local source after its identity is checked.
        run(docker, "tag", base["Id"], target)
        process = None
        try:
            target_id = base["Id"]
            example = Path(__file__).resolve().parents[1] / ("examples/postgresql" if postgresql else "examples/application-state-snapshots" if state_snapshots else "examples/python-ca" if python_ca else "examples/remote-embeddings" if embeddings else "examples/invocation-adapters") / (adapter_language or "")
            if example_directory: example = Path(example_directory).resolve()
            if adapter_language:
                context = work / "target"
                shutil.copytree(example / "target", context)
                dockerfile = context / "Dockerfile"
                dockerfile.write_text("FROM " + base_image + "\n" + dockerfile.read_text().split("\n", 1)[1])
                run(docker, "build", "--pull=false", "--tag", target, str(context))
                target_id = run(docker, "image", "inspect", "--format", "{{.Id}}", target)
                run(docker, "create", "--name", inspection, target)
                absent = subprocess.run([docker, "cp", inspection+":/app/.interceptor-adapter", str(work / "unexpected-adapter")], capture_output=True, timeout=30)
                if absent.returncode == 0:
                    raise ValueError("target already contains the adapter")
                run(docker, "rm", inspection)
            blueprint = outside / "environment"
            run(cli, "environment", "init", str(blueprint))
            record = {
                "api_version": "interceptor.dev/v1alpha1", "kind": "Environment",
                "metadata": {"name": "release-qualification", "version": version,
                             "reviewed_by": "release-qualification", "reviewed_at": "2026-01-01T00:00:00Z"},
                "application": {"image": target+"@"+base["Id"],
                    "launch": {"command": ["/bin/cat"], "workdir": "/tmp",
                               "environment": {"MODEL_BASE_URL": "http://127.0.0.1:18080/v1"}},
                    "interface": {"kind": "command", "operations": [{"id": "echo"}],
                        "command": {"stdin": "raw", "output_media_type": "text/plain", "success_exit_codes": [0], "concurrency": 1},
                        "startup_timeout_ms": 30000, "turn_timeout_ms": 30000},
                    "persistence": {"snapshot_capable": False, "quiesce": {"kind": "none"}}},
                "time": {"base": "2026-01-01T00:00:00Z", "mode": "fixed"},
                "limits": {"max_turns": 10, "max_tokens": 10000, "max_wall_seconds": 120,
                           "max_snapshots": 1, "max_blob_bytes": 10485760},
            }
            if adapter_language:
                shutil.copytree(example / "environment", blueprint, dirs_exist_ok=True)
                yaml = blueprint / "environment.yaml"
                placeholder = "interceptor-adapter-"+adapter_language+":local@sha256:"+"0"*64
                if placeholder not in yaml.read_text(): raise ValueError("example image placeholder is missing")
                yaml.write_text(yaml.read_text().replace(placeholder, target+"@"+target_id))
            else:
                (blueprint / "environment.yaml").write_text(json.dumps(record))
            run(cli, "environment", "validate", str(blueprint))
            run(cli, "environment", "seal", str(blueprint))
            lock = json.loads(run(cli, "app", "build", "--environment", str(blueprint), "--tag", wrapper, "--json"))
            run(cli, "app", "verify", "--environment", str(blueprint))
            trust_checks = None
            if python_ca:
                tls_directory = work / "data/apps/python-ca/1/tls"
                trust_checks = python_ca_fixture.inspect_images(run, docker, target, wrapper, tls_directory)
            packaged = install / ("interceptor-linux" if host.startswith("darwin/") else "interceptor")
            expected = hashlib.sha256(packaged.read_bytes()).hexdigest()
            if lock["interceptor_binary_digest"] != "sha256:"+expected or lock["interceptor_version"] != version:
                raise ValueError("wrapper lock does not identify the packaged executable")
            run(docker, "create", "--name", inspection, wrapper, "version")
            if postgresql:
                licenses = work / 'database-licenses'
                run(docker, 'cp', inspection+':/opt/interceptor/postgresql/licenses', str(licenses))
                required = ['INDEX','PostgreSQL.txt','pgvector.txt','musl.txt','GPL3.txt','GCC-runtime.txt','postgresql-18.6-source.tar.bz2','pgvector-0.8.7-source.tar.gz','build-packages.txt']
                if any(not (licenses / name).is_file() or not (licenses / name).stat().st_size for name in required):
                    raise ValueError('packaged database license inventory is incomplete')
                if '18.6' not in (licenses / 'INDEX').read_text() or '0.8.7' not in (licenses / 'INDEX').read_text():
                    raise ValueError('packaged database version notices differ')
                expected_sources = {'postgresql-18.6-source.tar.bz2':'555610c24d53e4316da5b7d3fc25c279d96856d5e0e23ee308c328c5fa881d9f', 'pgvector-0.8.7-source.tar.gz':'cac0b10c360f05b2d521200105ba3697e773d4cd3731f5a915a7e37ebe0bea85'}
                for name, expected_source in expected_sources.items():
                    if hashlib.sha256((licenses/name).read_bytes()).hexdigest() != expected_source:
                        raise ValueError('packaged upstream source/notice archive differs: '+name)
                for source_archive, member, notice in [('postgresql-18.6-source.tar.bz2','postgresql-18.6/COPYRIGHT','PostgreSQL.txt'),('pgvector-0.8.7-source.tar.gz','pgvector-0.8.7/LICENSE','pgvector.txt')]:
                    with tarfile.open(licenses/source_archive) as upstream:
                        if upstream.extractfile(member).read() != (licenses/notice).read_bytes():
                            raise ValueError('upstream notice changed: '+notice)
                for name, embedded in [('musl.txt','postgresql-musl.LICENSE'),('GPL3.txt','postgresql-GPL3.LICENSE'),('GCC-runtime.txt','postgresql-GCC-runtime.LICENSE')]:
                    if (licenses/name).read_bytes() != (Path(__file__).resolve().parents[1]/'internal/appimage'/embedded).read_bytes():
                        raise ValueError('runtime license changed: '+name)
                if run(docker,'run','--rm','--network','none','--entrypoint','/opt/interceptor/postgresql/bin/postgres',wrapper,'--version') != 'postgres (PostgreSQL) 18.6':
                    raise ValueError('packaged PostgreSQL executable version differs')
                shutil.copytree(licenses, output / 'database-licenses')
                if database_test:
                    native_checks = run(docker, 'run', '--rm', '--network', 'none', '--user', '65534:65534',
                        '--entrypoint', '/database-test', '-e', 'INTERCEPTOR_TEST_POSTGRES_BIN=/opt/interceptor/postgresql/bin',
                        '-v', str(Path(database_test).resolve())+':/database-test:ro', wrapper,
                        '-test.v', '-test.run', 'TestNativePostgreSQL', '-test.timeout', '90s')
                    (output/'database-protocol.log').write_text(native_checks+'\n')
            run(docker,'cp',inspection+':/usr/local/share/interceptor/licenses/PostgreSQL-client.txt',str(work/'postgresql-client.LICENSE'))
            client_notices=(Path(__file__).resolve().parents[1]/'internal/appimage/postgresql-go.LICENSE').read_bytes()
            if (work/'postgresql-client.LICENSE').read_bytes()!=client_notices or client_notices not in (install/'LICENSE').read_bytes():
                raise ValueError('protocol-client dependency notices missing or changed')
            adapter_digests = {}
            if adapter_language:
                copied = work / "copied-adapter"
                run(docker, "cp", inspection+":/app/.interceptor-adapter", str(copied))
                for original in sorted((blueprint / "assets/adapter").rglob("*")):
                    if not original.is_file(): continue
                    relative = original.relative_to(blueprint / "assets/adapter")
                    if original.read_bytes() != (copied / relative).read_bytes():
                        raise ValueError("installed adapter differs from sealed source")
                    adapter_digests[str(relative)] = hashlib.sha256(original.read_bytes()).hexdigest()
                if run(docker, "image", "inspect", "--format", "{{.Id}}", target) != target_id:
                    raise ValueError("target image changed during wrapper construction")
            run(docker, "cp", inspection+":/usr/local/bin/interceptor", str(work / "container-interceptor"))
            if (work / "container-interceptor").read_bytes() != packaged.read_bytes():
                raise ValueError("wrapper contains different executable bytes")
            if run(docker, "start", "--attach", inspection) != version:
                raise ValueError("in-container executable version differs")
            provider_args = ["--model-provider", "litellm", "--provider-auth", "none",
                             "--provider-base-url", "http://127.0.0.1:9/v1", "--allow-model", "unused"]
            if fixture:
                env.update(QA_GENERATION_KEY=fixture.keys['generation'], QA_EMBEDDINGS_KEY=fixture.keys['embeddings'])
                provider_args = ["--model-provider", "openai-chat", "--provider-auth", "api-key-env",
                    "--model-api-key-env", "QA_GENERATION_KEY", "--provider-base-url", fixture.urls['generation'],
                    "--allow-model", "chat-fixture", "--embeddings-provider", "openai-compatible",
                    "--embeddings-auth", "api-key-env", "--embeddings-api-key-env", "QA_EMBEDDINGS_KEY",
                    "--embeddings-base-url", fixture.urls['embeddings'],
                    "--allow-embedding-model", "text-embedding-3-small", "--allow-embedding-model", "embed-error",
                    "--allow-embedding-model", "embed-timeout"]
            with (output / "run.log").open("w") as log:
                process = subprocess.Popen([cli, "run", "--environment", str(blueprint),
                    *provider_args, *(["--feedback-profile", "oracle-assisted"] if postgresql and not customer_support else [])],
                    cwd=outside, env=env, stdout=log, stderr=subprocess.STDOUT)
                deadline = time.monotonic() + 60
                session = None
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        raise RuntimeError("release exited before readiness: " + (output / "run.log").read_text())
                    sessions = json.loads(run(cli, "list", "--json"))["sessions"] or []
                    session = next((item for item in sessions if item.get("phase") == "running"), None)
                    if session and "Interceptor local API:" in (output / "run.log").read_text(): break
                    time.sleep(0.25)
                if not session: raise RuntimeError("release readiness timed out")
                if adapter_language:
                    # The Blueprint is mounted for verification, as for every
                    # interface. No separate adapter/application-source mount is
                    # allowed; the entry module is loaded from the wrapper.
                    active = run(docker, "ps", "--filter", "ancestor="+wrapper, "--format", "{{.ID}}").splitlines()
                    if len(active) != 1: raise ValueError("expected one qualification container")
                    mounts = json.loads(run(docker, "inspect", "--format", "{{json .Mounts}}", active[0]))
                    if any(m["Destination"] == "/app" or m["Destination"].startswith("/app/") for m in mounts):
                        raise ValueError("application/adapter source is mounted instead of installed")
                    (output / "mounts.json").write_text(json.dumps(mounts, indent=2)+"\n")
                    if postgresql:
                        run(docker, 'exec', '--user', str(os.getuid()), active[0], '/bin/sh', '-c',
                            'test -s /opt/interceptor/environment/environment.yaml && test -s /run/interceptor-tls/server-key.pem && test -d /run/interceptor-session/pg/database/data')
                        run(docker, 'exec', '--user', '20000:20000', active[0], '/bin/sh', '-c',
                            'for path in /opt/interceptor/environment/environment.yaml /run/interceptor-tls/server-key.pem; do if head -c 1 "$path" >/dev/null 2>&1; then id; ls -ld /opt/interceptor/environment; echo "application can open $path"; exit 1; fi; done; if test -x /run/interceptor-session/pg/database/data; then echo "application can enter database data"; exit 1; fi; set -- /tmp/interceptor-pg-*; if test "$1" = "/tmp/interceptor-pg-*"; then echo "private socket directory missing"; ls -ld /tmp /tmp/*; exit 1; fi; for path do if test -x "$path"; then echo "application can enter $path"; exit 1; fi; done')
                        (output / 'database-isolation.json').write_text(json.dumps({'application_uid':20000,'private_blueprint':True,'private_database':True,'private_sockets':True,'private_tls_key':True})+'\n')
                run(str(client), "--customer-support" if customer_support else "--postgresql" if postgresql else "--state-snapshots" if state_snapshots else "--adapter-only" if adapter_language else "--command-only", "--output", str(output / "native"), *(["--evidence-failure", "--data-root", str(work / "data")] if evidence_failure else []))
                native = json.loads((output / "native/result.json").read_text())
                if not native["passed"] or native["session_id"] != session["id"]:
                    raise ValueError("native qualification did not pass for this session")
                process.send_signal(signal.SIGINT)
                if process.wait(timeout=30) != 0 and not evidence_failure:
                    raise RuntimeError("release shutdown failed: " + (output / "run.log").read_text())
                process = None
            for verified_session in native.get("sessions", [session["id"]]):
                run(cli, "logs", "verify", verified_session)
            result = {"platform": host, "source_commit": source, "version": version,
                      "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                      "container_binary_sha256": expected, "go_available": False,
                      "target_output": native["target_output"].strip(), "session_id": session["id"]}
            if adapter_language:
                logs = list((work / "data").rglob("application.*.log"))
                diagnostics = "\n".join(p.read_text() for p in logs)
                expected_processes = 1 if customer_support else 2 if postgresql else 3 if state_snapshots else 1
                closed = diagnostics.count("application closed")
                minimum_closed = expected_processes - int(postgresql and evidence_failure)
                if diagnostics.count("application initialized") != expected_processes or not minimum_closed <= closed <= expected_processes:
                    raise ValueError("application did not initialize/close exactly once: " + diagnostics)
                (output / "application.log").write_text(diagnostics)
                runtime = "/usr/local/bin/python3" if adapter_language == "python" else "/usr/local/bin/node"
                result.update(adapter_language=adapter_language, adapter_sha256=adapter_digests,
                              runtime=run(docker, "run", "--rm", "--network", "none", "--entrypoint", runtime, target, "--version"),
                              target_image=target_id, wrapper_image=lock["wrapper_digest"])
            if python_ca:
                result['python_ca'] = dict(images=trust_checks, **python_ca_fixture.verify_evidence(output, diagnostics))
            if customer_support:
                result['customer_support'] = {'scripted_model':True, 'fixed_test_embeddings':True, 'source_unchanged':True}
            elif postgresql:
                if fixture.failures: raise ValueError(fixture.failures)
                if not any('smoke payload' in json.dumps(c['request']) for c in fixture.calls):
                    raise ValueError('database payload did not reach outbound model request')
                (output / 'model-requests.json').write_text(json.dumps(fixture.calls, indent=2)+'\n')
                result['postgresql'] = verify_postgresql_evidence(output, evidence_failure)
            elif state_snapshots:
                if fixture.failures: raise ValueError(fixture.failures)
                requests = [c['request'] for c in fixture.calls]
                if len(requests) != 9 or requests[2] != requests[4] or requests[3] != requests[5]:
                    raise ValueError('restored model requests differ from uninterrupted context')
                if len(requests[-1]['messages']) <= len(requests[4]['messages']):
                    raise ValueError('conversation did not continue after restore')
                (output / 'model-requests.json').write_text(json.dumps(requests, indent=2)+'\n')
                result['state_snapshots'] = {'model_requests': len(requests), 'identical_continuation': True, 'restores': 2,
                                             'sessions': native['sessions'], 'live_provider': False}
                result['observed_evidence'] = verify_observed_evidence(output, evidence_failure)
            elif fixture:
                result['embeddings'] = fixture.verify(output)
            (output / "result.json").write_text(json.dumps(result, indent=2)+"\n")
            print(json.dumps(result))
        finally:
            if process is not None and process.poll() is None:
                process.send_signal(signal.SIGINT)
                try: process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill(); process.wait()
            # This newly created data root contains only this test's sessions.
            if (work / "data").exists():
                run(cli, "purge", "--all")
            subprocess.run([docker, "rm", "-f", inspection], capture_output=True, timeout=30)
            for image in (wrapper, target):
                subprocess.run([docker, "image", "rm", image], capture_output=True, timeout=30)


def verify_observed_evidence(output, failure):
    events = {}
    incomplete = 0
    journals = []
    for path in (output / 'native').glob('*.tar'):
        with tarfile.open(path) as bundle:
            for line in bundle.extractfile('events.jsonl'):
                event = json.loads(line)
                events[event['hash']] = event
            state = json.load(bundle.extractfile('evidence-bundle.json'))
            incomplete += state['completeness']['state'] == 'partial'
            journals.extend(json.loads(line) for line in bundle.extractfile('state.jsonl'))
    observed = [e for e in events.values() if e['kind'] in ('model_exchange', 'stateful_read', 'stateful_mutation')]
    if not observed or any(not e.get('turn_id') or e.get('attribution', {}).get('window') != 'observed' or not e['attribution'].get('observed_attempt_id') for e in observed):
        raise ValueError('native observation window or attempt context missing')
    if incomplete != int(failure):
        raise ValueError('unexpected evidence completeness')
    if failure:
        # The final service mutation occurred before the model request's failed
        # evidence write. Preserve it independently; do not claim rollback.
        mutations = [op for tx in journals for op in tx['operations'] if op.get('service') == 'records' and isinstance(op.get('value'), dict)]
        if not any(op['value'].get('count') == 4 for op in mutations):
            raise ValueError('committed pre-failure service effect is missing')
    return {'observations': len(observed), 'all_observed_during_turn': True,
            'incomplete_sessions': incomplete, 'failure_blocks_next_turn': failure}


def verify_postgresql_evidence(output, failure):
    events = []
    partial = 0
    for path in (output / 'native').glob('*.tar'):
        with tarfile.open(path) as bundle:
            events.extend(json.loads(line) for line in bundle.extractfile('events.jsonl'))
            partial += json.load(bundle.extractfile('evidence-bundle.json'))['completeness']['state'] == 'partial'
    kinds = {e['kind'] for e in events}
    for required in ('database_request','database_result','database_response_rewritten','database_state_injection_committed','database_oracle'):
        if required not in kinds: raise ValueError('missing database evidence: '+required)
    delivered = {e.get('injection_id') for e in events if e['kind']=='injection_delivered'}
    if not {'db-response', 'db-state'} <= delivered:
        raise ValueError('native model-delivery confirmation missing for database injections')
    first_session = json.loads((output/'native/result.json').read_text())['session_id']
    original = [e for e in events if e['session_id']==first_session]
    rolled_turns = {e.get('turn_id') for e in original if e['kind']=='database_request'
                    and e['semantic']['detail'].get('query','').strip().upper()=='ROLLBACK'} - {None}
    oracles = [e for e in original if e['kind']=='database_oracle']
    rolled_oracles = [e for e in oracles if e.get('turn_id') in rolled_turns]
    if not rolled_oracles or any(e['semantic']['detail']['matched'] for e in rolled_oracles):
        raise ValueError('rollback produced a missing or positive oracle result')
    if not any(e['semantic']['detail']['matched'] for e in oracles):
        raise ValueError('committed refund oracle did not fire')
    if any(e['semantic']['detail']['matched'] for e in oracles
           if e['semantic']['detail']['source']=='harness'):
        raise ValueError('harness mutation incorrectly established an application outcome')
    requests = {(e['session_id'], e['semantic']['detail']['connection'], e['semantic']['detail']['execution']):e
                for e in events if e['kind']=='database_request'}
    for e in events:
        if e['kind'] not in ('database_result', 'database_response_rewritten'): continue
        detail = e['semantic']['detail']
        request = requests[(e['session_id'], detail['connection'], detail['execution'])]
        if e.get('turn_id')!=request.get('turn_id') or e.get('attribution')!=request.get('attribution'):
            raise ValueError('database result lost its immutable request observation')
    if partial != int(failure): raise ValueError('unexpected database evidence completeness')
    views = json.loads((output/'native/database-live-feedback.json').read_text())
    if len(views) != 5: raise ValueError('missing live database feedback checks')
    for view in views:
        observed = [e for e in events if e['session_id'] == view['session_id']
                    and e['seq'] <= view['through_event_seq'] and e['kind'] == 'database_oracle'
                    and e.get('turn_id') == view['turn_id']
                    and e.get('attribution', {}).get('window') == 'observed'
                    and e['attribution'].get('observed_attempt_id') == view['attempt_id']
                    and e['semantic']['detail']['source'] == 'application']
        if not observed: raise ValueError('live verdict has no recorded turn-bound database evidence')
        expected = {e['semantic']['detail']['oracle'] for e in observed if e['semantic']['detail']['matched']}
        actual = {o['value']['oracle_id'] for o in view['observations'] if o['kind'] == 'oracle_outcome' and o['value']['fired']}
        if expected != actual: raise ValueError('live and retained database oracle evidence differ')
    return {'engine':'18.6','pgvector':'0.8.7','evidence_events':len(events),'incomplete_sessions':partial,
            'restores':1,'both_injections_delivery_confirmed':True,'rollback_oracle_negative':True,
            'request_observation_preserved':True,'live_oracle_feedback_matches_evidence':True}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--image", required=True, help="existing native Linux image containing /bin/cat")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--client", type=Path, required=True, help="prebuilt examples/local-api-smoke qualification client")
    parser.add_argument("--adapter-language", choices=("python", "node"), help="qualify the separately installed adapter example using the supplied runtime image")
    parser.add_argument("--embeddings", action="store_true", help="qualify remote embeddings with pinned OpenAI/LangChain clients and separate host fixtures")
    parser.add_argument("--python-ca", action="store_true", help="qualify declared Python Certifi stores with unchanged HTTPX clients")
    parser.add_argument("--evidence-failure", action="store_true", help="qualify evidence failure using test-only blob directory permissions")
    parser.add_argument("--postgresql", action="store_true", help="qualify packaged PostgreSQL and pgvector with real drivers")
    parser.add_argument("--database-test", type=Path, help="prebuilt Linux internal/postgresql test binary matching the wrapper architecture")
    parser.add_argument("--state-snapshots", action="store_true", help="qualify application context and sandbox restore with packaged Python or Node adapters")
    args = parser.parse_args()
    if args.database_test and not args.postgresql:
        parser.error('--database-test requires --postgresql')
    qualify(args.archive.resolve(), args.version, args.source, args.image, args.output, args.client.resolve(), args.adapter_language, args.embeddings, args.python_ca, args.state_snapshots, args.evidence_failure, args.postgresql, database_test=args.database_test)
