import {nextTurn} from 'session-math';

export class SupportApplication {
    constructor() { console.log('application initialized'); }
    startSession(customer) { return {customer, turns: 0}; }
    async close() { console.log('application closed'); }
}

export async function invokeSupport(app, session, message) {
    if (message === 'slow') await new Promise(resolve => setTimeout(resolve, 20000));
    if (message === 'error') throw new Error('requested application error');
    session.turns = nextTurn(session.turns);
    console.log('application invoked');
    return {customer: session.customer, turn: session.turns, message};
}
