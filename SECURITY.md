# Security policy

## Supported versions

The most recent minor release gets security fixes. This project is small and
has no long-term support branches.

| Version | Supported |
| ------- | --------- |
| 1.1.x   | Yes       |
| < 1.1   | No        |

## Report a vulnerability

Report vulnerabilities privately through
[GitHub Security Advisories](https://github.com/NavistAu/traefik-leaf-provider/security/advisories/new).
Do not open a public issue for a vulnerability.

Include the plugin version, the Traefik version, a configuration that shows the
problem, and the effect you can demonstrate. Expect an acknowledgement within
seven days.

## What is in scope

This provider sits at an edge and decides which middlewares attach to
aggregated routes. Treat anything that removes a gate as a security issue, not
a bug. Examples of in-scope reports:

- A configuration where a `routeMiddlewares` entry or an endpoint-level
  `middlewares` list does not attach to the route it names.
- A leaf response that makes the provider emit a router with fewer middlewares
  than the configuration specifies.
- A leaf response that makes the provider emit a router that shadows or
  replaces a router from another provider.
- A crash or a hang in the poll loop that a leaf can trigger.

## What is out of scope

- **A leaf you do not trust.** The provider copies router rules from each
  configured leaf. Anyone who can write router rules on a leaf can create
  routes on the main instance. This is what aggregation means. Only configure
  leaves you control.
- **Plaintext leaf traffic.** The second hop goes to the leaf's web entrypoint
  over whatever scheme you configure in `webUrl`. Protecting that hop is a
  deployment decision.
- **An exposed leaf API.** The provider reads `/api/rawdata` from each leaf.
  Restrict that API to the main instance at the network level.
- Vulnerabilities in Traefik itself. Report those to
  [the Traefik project](https://github.com/traefik/traefik/security).
