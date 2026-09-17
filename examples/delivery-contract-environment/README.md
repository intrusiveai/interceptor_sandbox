# Delivery-contract authoring example

This is synthetic offline validation/capability data, not a runnable approved
target. The all-zero image digest and review identity are placeholders. Before
running a real target, replace the application image, launch/interface settings,
input contract and review metadata with the actual reviewed application values.

```sh
interceptor environment validate examples/delivery-contract-environment
interceptor environment seal examples/delivery-contract-environment
interceptor capabilities --environment examples/delivery-contract-environment
```

`invoke` accepts the declared JSON query shape and has unknown output
structure. The exported MCP `get_ticket` contract describes synthetic
`structuredContent`; `search_notes` deliberately has no output schema.
Seed/fixture contents are private and are not copied into capabilities.
The authored `delivery_examples` are explicitly public examples.

A response injection can select `tickets` / `get_ticket` and append text at
`/structuredContent/description`, changing only that served representation.
It does not update the source ticket seed, the managed ticket or the MCP
`content[0].text` copy. A text-block injection uses its own pointer instead.

The independent CSV seed is copied to the session's mutable `records`
namespace. Fidelity injection at `relative_path: tickets.csv` replaces or
mutates that session file using existing text placement rules. It is persistent
within the session, not a CSV-cell or per-read service. These CSV records and
MCP tickets are independent stores; no implicit synchronization exists.
