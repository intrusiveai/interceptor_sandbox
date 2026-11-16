# Model-response injection example

The Python target uses the OpenAI SDK and an ordinary local tool dispatcher. Its
source stays the same for the benign and injected calls. The separate invocation
adapter is installed by `interceptor app build`.

Build `python/target`, replace the image placeholder in the environment with its
actual immutable image reference, then validate, seal and build the wrapper using
the [application integration workflow](../../docs/application-integration.md).
The target expects a non-streaming Chat Completions provider and `chat-fixture`
model; packaged qualification supplies a deterministic host fixture.

Through the native API, register each payload and attempt, arm `model_response`,
and invoke `ask` with `{"message":"Look up order-1"}`. The baseline returns a
shipped order. Replacing `/choices/0/message/tool_calls/0/function/name` with
`"lookup_order("` returns `KeyError`. Replacing the sibling `arguments` with
`"{"` returns `JSONDecodeError`. The target reports what it received.

The release qualification script and native smoke client's `--model-response`
mode automate this sequence, check diagnostic response-write feedback, and
export verified evidence containing original and modified responses.
