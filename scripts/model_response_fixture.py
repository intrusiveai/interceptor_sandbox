"""Inspect exported native evidence for the SDK model-response qualification."""
import hashlib
import json
import tarfile


def verify(fixture, output):
    if fixture.failures or len(fixture.calls) != 3:
        raise ValueError('incorrect provider calls: '+repr(fixture.calls))
    archives = list((output / 'native').glob('*.tar'))
    if len(archives) != 1:
        raise ValueError('expected one native export')
    with tarfile.open(archives[0]) as archive:
        files = {m.name: archive.extractfile(m).read() for m in archive if m.isfile()}
    events = [json.loads(line) for line in files['events.jsonl'].splitlines()]
    exchanges = [e['semantic'] for e in events if e['kind'] == 'model_exchange']
    if len(exchanges) != 3:
        raise ValueError('expected baseline and two injected exchanges')
    def blob(ref):
        # Match the exact content-addressed object, independent of store fanout.
        matches = [data for name, data in files.items() if name.startswith('blobs/') and 'sha256:'+hashlib.sha256(data).hexdigest() == ref]
        if len(matches) != 1:
            raise ValueError('missing/ambiguous retained blob '+ref)
        return matches[0]
    for index, exchange in enumerate(exchanges):
        original = json.loads(blob(exchange['response_ref']))
        function = original['choices'][0]['message']['tool_calls'][0]['function']
        if function['name'] != 'lookup_order' or json.loads(function['arguments']) != {'order_id': 'order-1'}:
            raise ValueError('original model response was overwritten')
        if index == 0:
            if exchange.get('model_response_injection_ids'):
                raise ValueError('baseline was injected')
            continue
        modified = blob(exchange['delivered_response_ref'])
        if exchange['delivered_response_digest'] != 'sha256:'+hashlib.sha256(modified).hexdigest():
            raise ValueError('delivered digest mismatch')
        function = json.loads(modified)['choices'][0]['message']['tool_calls'][0]['function']
        if (index == 1 and function['name'] != 'lookup_order(') or (index == 2 and function['arguments'] != '{'):
            raise ValueError('wrong delivered mutation')
        if exchange['usage']['total_tokens'] != 5:
            raise ValueError('accounting changed')
    writes = [e for e in events if e['kind'] == 'model_response_write']
    if len(writes) != 2 or any(e['semantic']['write_state'] != 'response_written' for e in writes):
        raise ValueError('missing response-write evidence')
    if any(key.encode() in data for key in fixture.keys.values() for data in files.values()):
        raise ValueError('host credentials leaked into evidence')
    (output/'provider-calls.json').write_text(json.dumps(fixture.calls, indent=2)+'\n')
    return {'baseline': True, 'malformed_name': True, 'malformed_arguments': True,
            'original_and_delivered_evidence': True, 'live_provider': False}
