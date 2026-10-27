import {SupportApplication, invokeSupport} from '/app/support_app.mjs';
import {nextTurn} from 'session-math';
import {messageFrom} from './helpers.mjs';

export async function initialize(options) {
    const app = new SupportApplication();
    return {
        app, session: app.startSession(options.customer_id),
        async invoke(value) {
            // Exercises application node_modules resolution from the adapter.
            if (nextTurn(this.session.turns) < 1) throw new Error('invalid state');
            return invokeSupport(this.app, this.session, messageFrom(value));
        },
        async close() { await this.app.close(); }
    };
}
