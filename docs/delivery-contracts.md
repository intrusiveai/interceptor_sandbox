# Delivery contracts for an external attack harness

A delivery contract tells a harness how to package an application request and
what is known about the response. It does not prescribe an attack payload.
Use Blueprint `interceptor.dev/v1alpha2` for these fields; older Blueprints
and their capability exports remain supported unchanged.

For an application operation, add one local contract reference:

```yaml
application:
  interface:
    kind: http
    operations:
      - id: invoke
        method: POST
        path: /invoke
        delivery_asset: assets/invoke.json
```

The asset contains a complete contract, for example:

```json
{
  "description": "Submit a task to the agent",
  "input": {
    "media_type": "application/json", "encoding": "json",
    "max_bytes": 65536, "basis": "synthetic",
    "schema": {
      "type": "object",
      "properties": {"query": {"type": "string", "description": "Task text"}},
      "required": ["query"], "additionalProperties": false
    }
  },
  "output": {
    "media_type": "application/json", "encoding": "json",
    "max_bytes": 65536, "basis": "unknown"
  },
  "examples": [
    {"id": "benign", "input": {"query": "Summarize ticket T-1"},
     "output": {"answer": "An illustrative answer"}}
  ]
}
```

Replace this illustrative input contract with the application's actual reviewed
interface. You can also put the same object inline as `delivery`.
Output structure may stay unknown; no output schema is guessed from examples.
Inputs are validated before invoking the application. Turn output conformance
is advisory and does not repair or suppress otherwise bounded responses.

For managed MCP/HTTP service context, explicitly set `export_contracts: true`
on the service. MCP descriptions reuse the sealed tools file. HTTP routes and
fixture rules can carry inline `delivery` objects. Export omits private
fixture bodies, matching values and seed records. Public examples are authored
separately, not copied from those protected sources.

Remote MCP metadata can be collected by the user or source-assessment tooling
before sealing, with permission and sanitization. If a remote tool does not
declare an output schema, omit it: protocol text placement can still be tried
when that field exists. A sanitized sample describes an observed response, not
all possible outputs. The local MCP service does not forward to production.

See the [normative fields, bounds and schema profile](../specs/delivery-contracts.md),
[MCP service guide](mcp-services.md), and
[injection lifetime / source isolation](injections-and-evaluation.md).
The [delivery example](../examples/delivery-contract-environment/) contains a
synthetic ticket seed, CSV seed and both known/unknown MCP output descriptions.
