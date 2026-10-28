"""Ordinary pooled PostgreSQL client; no Interceptor imports or interception hooks."""
import json
import os
from pathlib import Path
from urllib.request import Request, urlopen
from uuid import uuid4
import psycopg
from psycopg_pool import ConnectionPool
from pgvector.psycopg import register_vector


def configure(connection):
    register_vector(connection)


class SupportApplication:
    def __init__(self, customer):
        self.pool = ConnectionPool('', min_size=2, max_size=4,
                                   kwargs={'autocommit': True}, configure=configure)
        self.pool.wait(timeout=15)
        self.customer = customer
        self.session_id = str(uuid4())
        self.messages = []
        self.turns = 0
        self.guard = 'refund-review-required'
        print('application initialized')

    def invoke(self, value):
        message = value['message']
        self.turns += 1
        with self.pool.connection() as db:
            if message == 'change':
                with db.transaction():
                    db.execute("UPDATE counters SET count=count+1 WHERE id='counter'")
            if message in ('refund', 'rollback'):
                try:
                    with db.transaction():
                        db.execute('INSERT INTO refunds VALUES (%s,42)', ('refund',))
                        if message == 'rollback':
                            raise ValueError('intentional rollback')
                except ValueError:
                    pass
            body = db.execute("SELECT body FROM documents WHERE id=%s", ('one',), prepare=True).fetchone()[0]
            count = db.execute("SELECT count FROM counters WHERE id='counter'").fetchone()[0]
            nearest = db.execute("SELECT d.id,a.name FROM documents d JOIN authors a ON a.id=d.author_id ORDER BY embedding <-> '[1,0,0]' LIMIT 1").fetchone()
            docs = db.execute('SELECT count(*) FROM documents').fetchone()[0]
            refunds = db.execute('SELECT count(*) FROM refunds').fetchone()[0]
            # Server-side cursor and binary JSONB, including a real vector codec.
            with db.transaction():
                with db.cursor(name='documents', binary=True) as cursor:
                    cursor.execute('SELECT metadata,embedding FROM documents ORDER BY id')
                    assert len(cursor.fetchmany(1)) == 1
                    cursor.fetchall()
        Path('/app-state/count.txt').write_text(str(count))
        self.messages.append({'role': 'user', 'content': message})
        self.messages.append({'role': 'user', 'content': body})
        request = {'model': 'chat-fixture', 'user': self.customer,
                   'messages': [{'role':'system','content':self.customer + ';' + self.session_id}] + self.messages}
        with urlopen(Request(os.environ['MODEL_BASE_URL'] + '/chat/completions', data=json.dumps(request).encode(),
                             headers={'Content-Type':'application/json'}), timeout=10) as response:
            self.messages.append(json.load(response)['choices'][0]['message'])
        return {'customer': self.customer, 'session_id': self.session_id, 'turn': self.turns,
                'guard': self.guard, 'service_count': count, 'file_count': count,
                'messages': len(self.messages), 'body': body, 'documents': docs,
                'refunds': refunds, 'nearest': list(nearest)}

    def close(self):
        self.pool.close()
        print('application closed')
