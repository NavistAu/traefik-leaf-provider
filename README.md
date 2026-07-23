# traefik-leaf-provider

A Traefik provider plugin that aggregates HTTP router rules from
downstream "leaf" Traefik instances, two-hop style: this (main)
instance matches the leaf's Host rules and forwards matched traffic to
the leaf's web entrypoint; the leaf does its own final routing.

Purpose-built after evaluating the existing aggregator plugins
(im-kulikov/traefik-provider, MadddinTribleD/traefikaggregator,
Beanow/traefik-plugin-rawdata) and finding each unmaintained or broken
in ways that matter for a production edge: startup crashes on
unreachable endpoints, multi-endpoint handling bugs, silent route
breakage. This is the ~230-line subset that does only what is needed,
with those failure modes as explicit design constraints.

## Design constraints (why the code looks like it does)

- **No network I/O in `Init()`** — provider init failure is fatal to
  Traefik; a blocking probe there turns "one leaf down during a
  restart" into "every route down".
- **Sequential polling, per-endpoint error isolation, per-endpoint
  last-known-good** — one dead leaf never blocks or blanks another;
  transient API failures serve stale rather than empty.
- **Backends are never copied from leaves** — leaf backend URLs are
  container-network addresses, unreachable from the aggregator. One
  service per leaf pointing at its web entrypoint.
- **Yaegi-safe idioms** — this runs under Traefik's Go interpreter,
  not the compiler: index-based loops (loop-var capture diverges),
  plain channel sends into the provider channel (select-case send of
  an interpreted value panics in `reflect.Select` — found empirically,
  see provider.go). Compiled unit tests are necessary but NOT
  sufficient; `test/e2e/run.sh` (real Traefik, local-plugin mount) is
  the authoritative gate.

## Config

```yaml
experimental:
  localPlugins:
    leafprovider:
      moduleName: github.com/jhogendorn/traefik-leaf-provider

providers:
  plugin:
    leafprovider:
      pollInterval: 5s
      requestTimeout: 3s
      endpoints:
        - name: argus
          apiUrl: http://10.0.0.8:8080
          webUrl: http://10.0.0.8:80
          middlewares:
            - authentik-forwardauth@file
```

Install as a local plugin: the repo (including `vendor/`) mounted at
`plugins-local/src/github.com/jhogendorn/traefik-leaf-provider`
relative to Traefik's working directory. No plugin-registry access is
needed at any point, including startup.

Emitted naming: leaf router `grafana@docker` on endpoint `argus`
becomes router `grafana-argus` backed by service `argus-leaf`.
`middlewares` are attached to every router emitted for that endpoint
(this is where an edge concern like forward-auth gets applied to
aggregated routes).

## Tests

- `go test ./...` — transform/merge logic, compiled. Fast, incomplete
  by nature (see Yaegi note above).
- `test/e2e/run.sh` — dockerised Traefik loading this repo as a local
  plugin against one live leaf + one dead endpoint. Asserts: startup
  survives the dead endpoint, aggregation + naming, end-to-end two-hop
  routing, provider-namespace provenance, dead-endpoint isolation
  logging. Run this before trusting any change.
