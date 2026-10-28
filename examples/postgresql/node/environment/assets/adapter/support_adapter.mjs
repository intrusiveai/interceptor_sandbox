import {SupportApplication} from '/app/support_app.mjs';
export function initialize(options) {
    const app = new SupportApplication(options.customer_id);
    return {
        invoke(value) { return app.invoke(value); },
        async save_state() {
            return {format:1, customer:app.customer, session_id:app.session_id,
                messages:structuredClone(app.messages), turns:app.turns, guard:app.guard};
        },
        async restore_state(state) {
            if (state.format !== 1 || state.customer !== app.customer || typeof state.session_id !== 'string' || !state.session_id ||
                !Array.isArray(state.messages) || !Number.isSafeInteger(state.turns) || state.turns < 0 || state.guard !== 'refund-review-required')
                throw new Error('unsupported application state or customer');
            app.session_id=state.session_id; app.messages=structuredClone(state.messages);
            app.turns=state.turns; app.guard=state.guard;
        },
        async close() { await app.close(); }
    };
}
