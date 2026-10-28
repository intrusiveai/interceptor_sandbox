// An ordinary in-memory agent, with no checkpoint or Interceptor integration.
import fs from 'node:fs';
import {randomUUID} from 'node:crypto';
async function request(url, method = 'GET', value) {
    const response = await fetch(url, {method, headers: {'Content-Type':'application/json'}, body: value === undefined ? undefined : JSON.stringify(value)});
    if (!response.ok) throw new Error('HTTP ' + response.status);
    return response.json();
}
export class SupportApplication {
    constructor(customer) {
        console.log('application initialized');
        this.customer = customer; this.session_id = randomUUID(); this.messages = [];
        this.turns = 0; this.guard = 'refund-review-required';
    }
    async invoke(value) {
        this.turns++;
        this.messages.push({role:'user', content:value.message});
        let record = await request(process.env.SERVICE_URL);
        if (value.message === 'change') {
            record = await request(process.env.SERVICE_URL, 'PATCH', {count: record.count + 1});
            fs.writeFileSync('/app-state/count.txt', String(record.count));
            const callID = 'change-' + this.turns;
            this.messages.push({role:'assistant', content:null, tool_calls:[{id:callID, type:'function', function:{name:'change_record', arguments:'{}'}}]},
                {role:'tool', tool_call_id:callID, content:JSON.stringify(record)});
        }
        const response = await request(process.env.MODEL_BASE_URL + '/chat/completions', 'POST', {
            model:'chat-fixture', user:this.customer,
            messages:[{role:'system', content:'Assist '+this.customer+'; session '+this.session_id+'; '+this.guard}, ...this.messages]
        });
        this.messages.push(response.choices[0].message);
        return {customer:this.customer, session_id:this.session_id, turn:this.turns, guard:this.guard,
            service_count:record.count, file_count:Number(fs.readFileSync('/app-state/count.txt', 'utf8')), messages:this.messages.length};
    }
    close() { console.log('application closed'); }
}
