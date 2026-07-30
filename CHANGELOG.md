# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Any change to which middlewares attach to which routes is called out
explicitly, whatever its size. Operators depend on that mapping for access
control.

## [Unreleased]

Nothing yet.

## [1.2.0] - 2026-07-30

Per-endpoint TLS on both hops, which is what this needs to be usable in a
zero-trust estate rather than only on a trusted network.

### Added

- `tls` on an endpoint: `rootCAs`, `certificates` (client keypairs for mTLS),
  `serverName`, and `insecureSkipVerify`. One block configures **both** hops —
  the plugin's own poll of the leaf API, and Traefik's forward to the leaf's
  web entrypoint. The poll hop had no TLS configuration at all before this, so
  a leaf API behind a private CA or a client-certificate requirement was
  unreachable.
- `serversTransport` on an endpoint: names an existing Traefik serversTransport
  for the traffic hop, overriding the generated one. This is the escape hatch
  for transport options the `tls` block does not model, such as SPIFFE, cipher
  suites, and forwarding timeouts. It also makes the two hops configurable
  separately.
- The plugin now emits a `serversTransport` per TLS-configured endpoint, named
  `<endpoint>-transport`, and points that endpoint's service at it.
- A startup warning when `insecureSkipVerify` is set on an endpoint.
- E2E coverage for all of the above against a real mTLS-gated leaf API,
  including a negative control that proves the gate rejects an endpoint with no
  client keypair.

### Fixed

- The E2E suite pointed two endpoints at the same leaf, so `whoami-leaf1` and
  `whoami-mtlsleaf` carried identical `Host` rules and Traefik chose between
  them arbitrarily. That made the `routeMiddlewares` assertions a coin toss;
  CI lost the toss on the first run, after three clean local runs. The mTLS
  gate now fronts its own leaf, partitioned by a docker label constraint.

### Documentation

- The README states the zero-trust position up front, and links the CoreDNS
  consumer to [coredns-traefik](https://github.com/jhogendorn/coredns-traefik).
- Corrected the stale "~230-line" claim. The plugin is ~300 lines of Go as of
  this release, up from ~170 at 1.0.0.

### Changed

- `serversTransport` on an emitted service was previously always empty, so
  every endpoint used Traefik's global default transport. Endpoints with no
  `tls` and no `serversTransport` still do; only configured endpoints change.

### Security

- Certificate files are read lazily at poll time, never during `New()` or
  `Init()`. A path that is briefly unreadable — a secret still mounting during
  a restart — degrades that one endpoint and retries, rather than failing
  provider init and taking every route in the estate down. A keypair missing
  one half is still rejected at startup, because it can never become valid.

## [1.1.0] - 2026-07-26

First public release. Versions 1.0.0 and 1.1.0 were developed internally; this
is the first tagged, published version, so everything below ships together.

### Added

- Aggregation of HTTP router rules from one or more leaf Traefik instances,
  using the two-hop model: the main instance matches the leaf's `Host` rules
  and forwards matched traffic to the leaf's web entrypoint. The leaf does its
  own final routing.
- Per-endpoint error isolation with a per-endpoint last-known-good
  configuration. One unreachable leaf never blanks or blocks another, and a
  transient API failure serves stale routes rather than no routes.
- No network input or output in `Init()`. Traefik treats provider init failure
  as fatal, so a blocking probe there would turn one leaf being down during a
  restart into every route being down.
- One service per endpoint, pointing at that leaf's web entrypoint. Backend
  definitions are never copied from leaves, because leaf backend URLs are
  container-network addresses that the main instance cannot reach.
- Endpoint-level `middlewares`, attached to every router emitted for that
  endpoint.
- `routeMiddlewares`: per-route middleware attachment, keyed by the leaf
  router's base name. A present key **replaces** the endpoint-level
  `middlewares` list for that router rather than merging with it. An explicitly
  empty list (`[]`) attaches no middlewares, which is the supported way to
  exempt one route from an otherwise gated endpoint.
- A warning when a `routeMiddlewares` key matches no live leaf router. A typo
  in that key would otherwise leave the intended route silently ungated. The
  warning logs when the set of unmatched keys changes, not on every poll.
- E2E suite running the plugin under real Traefik and real Yaegi, covering
  startup with a dead endpoint, aggregation and naming, two-hop routing,
  provider namespace provenance, dead-endpoint isolation logging, and the four
  `routeMiddlewares` behaviours.
- Continuous integration: lint, unit tests, vendor-drift check, plugin manifest
  validation, and the E2E suite on every pull request.
- Release automation on merge to `main`, driven by the `VERSION` file.
- Contributor documentation: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  `SECURITY.md`, issue templates, and a pull request template.
- A full user-facing README: defined terms, both installation methods, a
  complete configuration reference, Mermaid diagrams for the two-hop path and
  for middleware resolution, error and log message tables, and troubleshooting.
  The reference and procedural sections follow ASD-STE100 simplified technical
  English; the introductory and design sections keep an ordinary voice.
  `CONTRIBUTING.md` documents where the boundary falls.
- The README now states the DNS benefit of aggregation: every hostname in the
  estate lands in one routing table, so a CoreDNS plugin that syncs records
  from Traefik resolves downstream services without being pointed at each leaf.

### Changed

- Documentation and test fixtures use neutral example identifiers throughout —
  RFC 5737 documentation addresses, `example.com`, and generic endpoint and
  middleware names. No deployment-specific host names, addresses, or domains
  remain in the published source.

### Fixed

- The E2E suite no longer races the poll loop. The two log assertions — the
  dead-endpoint isolation check and the unmatched-`routeMiddlewares`-key check
  — searched the log once. A dead endpoint only logs after its `requestTimeout`
  elapses, so both could fail spuriously. They now retry.

[Unreleased]: https://github.com/NavistAu/traefik-leaf-provider/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/NavistAu/traefik-leaf-provider/releases/tag/v1.2.0
[1.1.0]: https://github.com/NavistAu/traefik-leaf-provider/releases/tag/v1.1.0
