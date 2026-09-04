# Decision 0006: closed DNS and locally trusted TLS

Status: accepted

## Context

Mapping declared names through the container hosts file made HTTP simulation
usable but did not exercise or record DNS behavior. Plaintext-only synthetic
services also forced target-specific URL changes and could not represent MCP,
identity, fixture, or attacker origins that require HTTPS. General container
networking must remain disabled, and local TLS must not require a customer's
enterprise certificate authority.

## Decision

Interceptor provides an optional authoritative DNS server inside each session.
It is the container resolver, answers declared names with loopback `A` records,
never forwards upstream, and deterministically rejects unknown names. DNS query
evidence has an explicit session ceiling. A client that addresses an external
resolver directly is blocked by the existing network namespace rather than
being granted packet-redirection privileges.

Optional shared HTTP and HTTPS front doors route exact Blueprint hostnames to
the already managed handlers. HTTPS terminates TLS 1.2 or newer and rejects an
SNI/Host mismatch. It does not proxy requests to another process or network.
MCP-over-HTTPS is therefore transport reuse, not a second MCP service.

`interceptor app build` generates a private CA and one SAN-constrained server
certificate. The public CA may enter the derived wrapper image. The CA signing
key is discarded; the server keypair stays in private host application state,
is bound by digest in the application lock, and is mounted read-only for the
supervisor. The application chooses sealed trust mode `auto`, `bundle`, or
`application-managed`. Custom application-managed trust must pass its declared
probe against the live front door before the application starts.

## Consequences

Declared names and HTTPS origins can match realistic application configuration
without creating egress. DNS misses and front-door misses become explicit
coverage evidence. Certificate lifecycle remains local to a wrapper build and
does not depend on enterprise PKI.

Images with custom or pinned trust stores need a small explicit integration and
probe. Standard DNS-over-TLS and DNS-over-HTTPS are not implied. Internal
service listeners remain available on their sealed loopback ports for backward
compatibility, while the shared front doors own normal ports 80 and 443.
