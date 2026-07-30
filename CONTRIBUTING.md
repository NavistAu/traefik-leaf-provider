# Contributing

Thanks for your interest. Bug reports, documentation fixes, and small focused
changes are all welcome.

Read [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) first. To report a
vulnerability, follow [`SECURITY.md`](SECURITY.md) instead of opening an issue.

## Before you start

This plugin is deliberately small — roughly 300 lines of Go, under nearly as
many lines of comment, that aggregate router rules and forward to a leaf's web
entrypoint. It replaced three larger
aggregator plugins that each failed in a way that took down a production edge.
The constraints in [`provider.go`](provider.go)'s package comment are not
stylistic preferences; each one records a specific outage. Read them before you
change the poll loop.

If your change adds configuration surface, open an issue first. The bar for new
options is "Traefik's own configuration cannot express this", not "this would be
convenient".

## Set up

```sh
git clone https://github.com/NavistAu/traefik-leaf-provider.git
cd traefik-leaf-provider
mise install     # installs the Go toolchain pinned in mise.toml
go build ./...
```

You need Docker with the Compose plugin for the E2E suite.

## The two test suites, and why one of them decides

```sh
go test ./...          # compiled unit tests: transform and merge logic
./test/e2e/run.sh      # real Traefik, real Yaegi — the gate that counts
```

Traefik runs plugins under [Yaegi](https://github.com/traefik/yaegi), an
interpreter, not the Go compiler. Yaegi and the compiler have disagreed about
this code twice already:

- **Loop-variable capture.** A closure over a range variable behaved
  differently under Yaegi and shipped exactly that bug in a prior plugin. Every
  loop over endpoints uses explicit indexing for this reason.
- **`select`-case sends into the provider channel.** Routing an interpreted
  `dynamic.JSONPayload` through `reflect.Select` panics with "not assignable to
  type json.Marshaler". `refresh` uses a plain channel send instead. This was
  found empirically, in E2E, on 2026-07-23.

`crypto/tls`, `crypto/x509` and `os.ReadFile` were spiked against Traefik v3.7
on 2026-07-30 and all resolve under Yaegi, which is what makes the endpoint
`tls` block possible at all. Do not assume the next stdlib package you reach
for is available — spike it in the E2E harness first, the same way.

A green `go test ./...` therefore proves very little about whether the plugin
works. **Run the E2E suite before you open a PR**, and again after any review
change that touches `provider.go`. CI runs it on every pull request, but a
local run is much faster than a round trip.

The E2E suite starts a main Traefik instance that loads this repository as a
local plugin, one live leaf with two `whoami` backends, one deliberately dead
endpoint, and an nginx gate that fronts the leaf API with `ssl_verify_client
on`. It asserts thirteen things: startup survives the dead endpoint,
aggregation and naming work, a request two-hops end to end, routers land in the
plugin's provider namespace, the dead endpoint is logged and isolated, the four
`routeMiddlewares` behaviours hold, an endpoint aggregates through the mTLS
gate, an endpoint with no client keypair is rejected by it, the generated
serversTransport resolves, and the plain routes still work alongside all of it.

The suite generates its own certificates on first run into `test/e2e/certs/`,
which is gitignored. No key material is committed and nothing expires.

If you add a behaviour, add an assertion to `test/e2e/run.sh`. A behaviour that
only the compiled suite covers is a behaviour nobody has actually tested.

## Before you open a PR

```sh
gofmt -l . | grep -v '^vendor/'   # must print nothing
go vet ./...
go mod tidy && go mod vendor      # must produce no diff
go test ./...
./test/e2e/run.sh
```

`vendor/` is committed on purpose. The Traefik Plugin Catalog requires it, and
the local-plugin install path loads dependencies straight out of it. CI fails
any PR where `vendor/` has drifted from `go.mod`.

Add an entry to `CHANGELOG.md` under `## [Unreleased]`.

## Branches and releases

```
feature branch  ->  develop  ->  main
```

- **Feature branches** start from `develop` and target `develop`. Name them
  `feat/…`, `fix/…`, or `docs/…`.
- **`develop`** is the integration branch. It is always releasable in principle
  but is not released.
- **`main`** only advances through a release PR from `develop`. Every push to
  `main` is a release.

To cut a release, open a PR from `develop` to `main` that:

1. Sets `VERSION` to the new `X.Y.Z`.
2. Renames the `## [Unreleased]` section in `CHANGELOG.md` to that version with
   today's date, and opens a fresh `## [Unreleased]` above it.

When that PR merges, `.github/workflows/release.yml` re-runs the unit and E2E
suites, creates the tag `vX.Y.Z`, publishes a GitHub Release with a
local-plugin tarball, and asks `proxy.golang.org` for the new version so the
Plugin Catalog sees it on its next poll. That last step is skipped while the
repository is private, because the proxy cannot fetch a private module.

Nothing is tagged from a local machine. Do not push tags.

### Versioning

[Semantic versioning](https://semver.org/), read from the operator's side:

- **Major** — an existing configuration stops working, or a route that was
  gated becomes ungated.
- **Minor** — new configuration options, backward compatible.
- **Patch** — fixes that change no configuration.

Treat any change to which middlewares attach to which routes as at least a
minor bump, and say so explicitly in the changelog. Operators depend on that
mapping for access control.

## Documentation style

`README.md` deliberately mixes two registers. Match whichever one you are
editing.

**Reference and procedural sections use [ASD-STE100][ste] simplified technical
English.** This covers Terms, Requirements, Installation, Configuration, the
naming and middleware sections, failure behaviour, log messages, and
troubleshooting. The rules that matter in practice:

- One idea per sentence. Twenty words for an instruction, twenty-five for
  description.
- Active voice, present tense, simple verbs.
- One term for one thing, every time. The Terms table is the authority — if you
  need a new term, add it there.
- Start a warning with the command, not the explanation.
- Keep noun clusters to three words or fewer.

**Argumentative and introductory sections keep an ordinary voice.** This covers
the opening, "When to use this plugin", "Design constraints", and the framing
around the test suites. These sections persuade rather than instruct, and
flattening them costs more than it gains.

The split is not arbitrary. A reader configuring the plugin at 2am wants
unambiguous declaratives, and many of them are not reading in their first
language. A reader deciding whether to adopt it, or about to "simplify" the
poll loop, needs to understand *why*, which takes real prose.

[ste]: https://www.asd-ste100.org/

## Code style

Match the surrounding code. Two conventions are load-bearing rather than
cosmetic:

- **Comment the "why", not the "what".** The existing comments record outages
  and Yaegi findings. Keep that.
- **Index-based loops over endpoints.** See the Yaegi note above. Do not
  "clean these up" into range loops with closures.

Certificate files must never be read during `New()` or `Init()`. Build TLS
material lazily at poll time so an unreadable path degrades one endpoint
instead of aborting provider init. Only reject shape errors that can never
become valid, such as a keypair missing one half.

`Init()` must never do network input or output. Traefik treats a provider init
failure as fatal, so one unreachable leaf during a restart would take down every
route in the estate. This is the single most important rule in the codebase.
