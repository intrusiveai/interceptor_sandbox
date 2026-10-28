import fs from 'node:fs';
import {randomUUID} from 'node:crypto';
import pg from 'pg';
export class SupportApplication {
    constructor(customer) {
        this.pool = new pg.Pool({max:4, ssl:{rejectUnauthorized:true, ca:fs.readFileSync(process.env.PGSSLROOTCERT,'utf8')}});
        this.customer=customer; this.session_id=randomUUID(); this.messages=[];
        this.turns=0; this.guard='refund-review-required';
        console.log('application initialized');
    }
    async invoke(value) {
        this.turns++;
        const db = await this.pool.connect();
        let body,count,nearest,docs,refunds;
        try {
            if (value.message === 'change') await db.query("UPDATE counters SET count=count+1 WHERE id='counter'");
            if (['refund','rollback'].includes(value.message)) {
                await db.query('BEGIN');
                await db.query('INSERT INTO refunds VALUES ($1,42)', ['refund']);
                await db.query(value.message === 'refund' ? 'COMMIT' : 'ROLLBACK');
            }
            body=(await db.query({name:'document',text:'SELECT body FROM documents WHERE id=$1',values:['one']})).rows[0].body;
            count=(await db.query("SELECT count FROM counters WHERE id='counter'")).rows[0].count;
            nearest=(await db.query({text:"SELECT d.id,a.name FROM documents d JOIN authors a ON a.id=d.author_id ORDER BY embedding <-> '[1,0,0]' LIMIT 1",rowMode:'array'})).rows[0];
            docs=Number((await db.query('SELECT count(*) FROM documents')).rows[0].count);
            refunds=Number((await db.query('SELECT count(*) FROM refunds')).rows[0].count);
        } finally { db.release(); }
        fs.writeFileSync('/app-state/count.txt',String(count));
        this.messages.push({role:'user',content:value.message},{role:'user',content:body});
        const response = await fetch(process.env.MODEL_BASE_URL+'/chat/completions',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({model:'chat-fixture',user:this.customer,messages:[{role:'system',content:this.customer+';'+this.session_id},...this.messages]})});
        if (!response.ok) throw new Error('model HTTP '+response.status);
        this.messages.push((await response.json()).choices[0].message);
        return {customer:this.customer, session_id:this.session_id,turn:this.turns,guard:this.guard,service_count:count,file_count:count,messages:this.messages.length,body,documents:docs,refunds,nearest};
    }
    async close() { await this.pool.end(); console.log('application closed'); }
}
