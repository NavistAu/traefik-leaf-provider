# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Any change to which middlewares attach to which routes is called out
explicitly, whatever its size. Operators depend on that mapping for access
control.

## [Unreleased]

Nothing yet.

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

### Fixed

- The E2E suite no longer races the poll loop. The two log assertions — the
  dead-endpoint isolation check and the unmatched-`routeMiddlewares`-key check
  — searched the log once. A dead endpoint only logs after its `requestTimeout`
  elapses, so both could fail spuriously. They now retry.

[Unreleased]: https://github.com/NavistAu/traefik-leaf-provider/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/NavistAu/traefik-leaf-provider/releases/tag/v1.1.0
