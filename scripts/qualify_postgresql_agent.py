#!/usr/bin/env python3
"""Qualify an unchanged local customer_support_agent_langchain checkout in a wrapper.

The agent's existing model/embedder injection hooks supply deterministic fixtures;
its schema checks, driver, SQL, validation and refund implementation stay intact.
"""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import tarfile
import tomllib
from qualify_release import qualify

ADAPTER = '''import json
import os
from pathlib import Path
import numpy as np
from langchain_core.language_models.chat_models import BaseChatModel
from langchain_core.messages import AIMessage, ToolMessage
from langchain_core.outputs import ChatGeneration, ChatResult
from support_agent.config import Settings
from support_agent.session import SupportApplication, invoke_support

class Model(BaseChatModel):
    @property
    def _llm_type(self): return 'postgresql-qualification'
    def bind_tools(self, tools, **kwargs): return self
    def _generate(self, messages, stop=None, run_manager=None, **kwargs):
        calls = [
            {'name':'search_support_docs','args':{'query':'refund policy'}},
            {'name':'get_customer','args':{'customer_id':'alice'}},
            {'name':'get_order','args':{'order_id':'alice-order'}},
            {'name':'issue_refund','args':{'order_id':'alice-order','amount':'12.50','reason':'Authorized qualification'}},
        ]
        completed = sum(isinstance(m,ToolMessage) for m in messages)
        if completed < len(calls):
            answer = AIMessage(content='',tool_calls=[dict(calls[completed],id='call-'+str(completed),type='tool_call')])
        else:
            answer = AIMessage(content=json.dumps([json.loads(m.content) for m in messages if isinstance(m,ToolMessage)]))
        return ChatResult(generations=[ChatGeneration(message=answer)])
class Embedder:
    def embed(self, query):
        vector=np.zeros(384,dtype=np.float32);vector[0]=1;return vector
class Adapter:
    def __init__(self):
        settings=Settings(_env_file=None,database_url='postgresql://',model_name='scripted-test',embedding_revision='c9745ed1d9f207416be6d2e6f8de32d1f16199bf',evidence_path=Path('/app-state/agent-evidence'))
        self.app=SupportApplication(settings,model=Model(),embedder=Embedder())
        self.session=self.app.start_session('alice')
        print('application initialized')
    def invoke(self,value):return invoke_support(self.app,self.session,value['message'])
    def close(self):self.app.close();print('application closed')
def initialize(options):return Adapter()
'''


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--agent',type=Path,required=True)
    p.add_argument('--archive',type=Path,required=True)
    p.add_argument('--source',required=True)
    p.add_argument('--version',required=True)
    p.add_argument('--image',default='python:3.12-slim')
    p.add_argument('--client',type=Path,required=True)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    agent=args.agent.resolve()
    with tempfile.TemporaryDirectory(prefix='interceptor-agent-qa-') as work:
        root=Path(work)
        example=Path(__file__).resolve().parents[1]/'examples/postgresql/python'
        shutil.copytree(example,root,dirs_exist_ok=True)
        shutil.copytree(agent/'src/support_agent',root/'target/support_agent')
        versions={package['name']:package['version'] for package in tomllib.loads((agent/'uv.lock').read_text())['package'] if 'version' in package}
        requirements=['psycopg[binary,pool]','pgvector','numpy','langchain','langchain-openai','pydantic-settings','httpx','beautifulsoup4']
        pins=' '.join("'"+name+'=='+versions[name.split('[')[0]]+"'" for name in requirements)
        (root/'target/Dockerfile').write_text('FROM python:3.12-slim\nWORKDIR /app\nRUN pip install --no-cache-dir '+pins+'\nCOPY support_agent /app/support_agent\nENV PYTHONDONTWRITEBYTECODE=1\n')
        assets=root/'environment/assets'
        migrations='\n'.join(path.read_text() for path in sorted((agent/'migrations').glob('*.sql')))
        grants='\nGRANT USAGE ON SCHEMA support,knowledge TO agent; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA support,knowledge TO agent;\n'
        (assets/'schema.sql').write_text(migrations+grants)
        content='Eligible delivered orders can receive a refund up to the remaining charged amount.'
        vector='['+','.join(['1']+['0']*383)+']'
        seeds="""INSERT INTO support.customers(customer_id,name,email,address) VALUES ('alice','Alice','alice@example.test','{"recipient":"Test","line1":"1 Main St","line2":"","city":"Town","region":"CA","postal_code":"90000","country_code":"US"}');
INSERT INTO support.orders(order_id,customer_id,status,currency,total_minor,shipping_address) VALUES ('alice-order','alice','delivered','USD',10000,'{}');
INSERT INTO support.order_items VALUES ('alice-order',1,'SKU','Test item',2,4000);
"""
        seeds+="INSERT INTO knowledge.support_chunks VALUES ('00000000-0000-4000-8000-000000000001','refund-policy',0,'Refund policy','https://support.example.test/refunds','"+content+"','sha256:"+hashlib.sha256(content.encode()).hexdigest()+"','{}','sentence-transformers/all-MiniLM-L6-v2','c9745ed1d9f207416be6d2e6f8de32d1f16199bf','"+vector+"','2026-01-01T00:00:00Z');\n"
        (assets/'seed.sql').write_text(seeds)
        (assets/'adapter/support_adapter.py').write_text(ADAPTER)
        path=root/'environment/environment.yaml'
        text=path.read_text()
        a=text.index('      state:');b=text.index('    startup_timeout_ms:',a);text=text[:a]+text[b:]
        text=text.replace('snapshot_capable: true','snapshot_capable: false').replace('turn_timeout_ms: 15000','turn_timeout_ms: 60000')
        a=text.index('      tables:');b=text.index('credentials:',a)
        text=text[:a]+'''oracles:
  - id: refund-committed
    kind: database_query_match
    service: database
    match:
      query: SELECT count(*)::int FROM support.refunds WHERE amount_minor=1250
      equals: [[1]]
'''+text[b:]
        path.write_text(text)
        qualify(args.archive.resolve(),args.version,args.source,args.image,args.output,args.client.resolve(),adapter_language='python',postgresql=True,example_directory=root,customer_support=True)
        with tarfile.open(next((args.output/'native').glob('*.tar'))) as bundle:
            events=[json.loads(line) for line in bundle.extractfile('events.jsonl')]
            if not any(e['kind']=='database_oracle' and e['semantic']['detail']['matched'] for e in events):
                raise ValueError('independent customer refund oracle did not verify the committed 1250 cents')
        provenance={str(path.relative_to(agent)):hashlib.sha256(path.read_bytes()).hexdigest() for folder in ['src/support_agent','migrations'] for path in (agent/folder).rglob('*') if path.is_file() and '__pycache__' not in path.parts}
        (args.output/'agent-source.json').write_text(json.dumps(provenance,indent=2)+'\n')

if __name__=='__main__':main()
