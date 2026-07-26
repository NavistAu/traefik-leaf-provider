# traefik-leaf-provider

[![CI](https://github.com/NavistAu/traefik-leaf-provider/actions/workflows/ci.yml/badge.svg)](https://github.com/NavistAu/traefik-leaf-provider/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/NavistAu/traefik-leaf-provider.svg)](https://pkg.go.dev/github.com/NavistAu/traefik-leaf-provider)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A Traefik provider plugin. It reads the router rules from one or more
downstream Traefik instances. It then makes the same rules available on the
Traefik instance at your edge.

This lets one edge instance serve the routes of many downstream instances. You
do not have to copy each route by hand.

## Contents

- [Terms](#terms)
- [How it operates](#how-it-operates)
- [When to use this plugin](#when-to-use-this-plugin)
- [Requirements](#requirements)
- [Installation](#installation)
- [Configuration](#configuration)
- [Names of the emitted routers and services](#names-of-the-emitted-routers-and-services)
- [Middlewares](#middlewares)
- [Behaviour when a leaf instance fails](#behaviour-when-a-leaf-instance-fails)
- [Log messages](#log-messages)
- [Troubleshooting](#troubleshooting)
- [Design constraints](#design-constraints)
- [Tests](#tests)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Terms

This document uses these terms with one meaning each.

| Term | Meaning |
| --- | --- |
| **main instance** | The Traefik instance at your edge. This plugin operates in the main instance. |
| **leaf instance** | A Traefik instance downstream of the main instance. |
| **endpoint** | One leaf instance, as configured in this plugin. |
| **to aggregate** | To read the router rules from a leaf instance and make them available on the main instance. |
| **poll cycle** | One pass in which the plugin reads each endpoint in turn. |
| **base name** | A leaf router key with the `@provider` part removed. The key `grafana@docker` has the base name `grafana`. |
| **emitted router** | A router that this plugin makes on the main instance. |
| **two hops** | The path of a request. The client to the main instance is hop 1. The main instance to the leaf instance is hop 2. |

## How it operates

The plugin reads the API of each leaf instance at a regular interval. It reads
the router rules, and it makes an equivalent router on the main instance. Each
emitted router sends its traffic to the web entrypoint of that leaf instance.
The leaf instance then does its own final routing.

```
                       client
                          |
                          | Host: grafana.example.com
                          v
              +-------------------------+
     hop 1    |      main instance      |
              |  (this plugin operates  |
              |        in here)         |
              +-------------------------+
                    |               ^
                    |               |  poll: GET /api/rawdata
                    |               |  every pollInterval
                    v               |
              +-------------------------+
     hop 2    |      leaf instance      |
              +-------------------------+
                          |
                          | the leaf instance does its own routing
                          v
                    +-----------+
                    |  grafana  |
                    +-----------+
```

The plugin does not copy the backend definitions from a leaf instance. The
backends of a leaf instance have container-network addresses. The main instance
cannot reach those addresses. The plugin makes one service for each endpoint
instead. That service points at the web entrypoint of the leaf instance.

## When to use this plugin

Use this plugin when all of these are true.

- You have more than one Traefik instance.
- One instance is at your edge, and it holds your certificates and your
  authentication middlewares.
- The other instances are on networks that the edge can reach, but their
  backends are not.
- You do not want to write a route on the edge each time a service starts
  downstream.

Do not use this plugin in these cases.

- **You do not control a leaf instance.** The plugin copies router rules from
  each endpoint you configure. Any person who can write a router rule on a leaf
  instance can make a route on your edge. Configure only leaf instances that
  you control.
- **You want one hop, not two.** Every request goes through both instances.
  This adds a proxy hop, and the leaf instance sees the traffic.
- **You have one Traefik instance.** Use the standard providers instead.

## Requirements

| Item | Value |
| --- | --- |
| Traefik | v3. The E2E suite tests against v3.7. Earlier versions are not tested. |
| Leaf instance API | The `/api/rawdata` route must be reachable from the main instance. |
| Network | The main instance must reach the API URL and the web URL of each endpoint. |
| Go | Only to build or test the plugin. Traefik interprets the source at runtime. |

**Restrict the leaf instance API to the main instance.** The `/api/rawdata`
route shows your full routing table. Use a firewall rule, a private network, or
a middleware to control access to it.

## Installation

There are two methods. Both are supported. Read the difference before you
choose.

### Method 1: local plugin (recommended)

With this method Traefik reads the plugin from your disk. Traefik does not
contact the plugin registry at any time, including at startup.

Choose this method for a production edge. A plugin on disk cannot fail to
download.

1. Get the source. Use one of these two commands.

   ```sh
   # From a release. Replace the version with the one you want.
   curl -fsSL -o plugin.tar.gz \
     https://github.com/NavistAu/traefik-leaf-provider/releases/download/v1.1.0/traefik-leaf-provider-v1.1.0.tar.gz
   ```

   ```sh
   # Or from Git, if you prefer to track a branch.
   git clone https://github.com/NavistAu/traefik-leaf-provider.git
   ```

2. Put the source at this path, relative to the working directory of Traefik.

   ```
   plugins-local/src/github.com/NavistAu/traefik-leaf-provider/
   ```

   The `vendor/` directory must be present. It is part of the repository and
   part of the release tarball.

3. Declare the plugin in your static configuration.

   ```yaml
   experimental:
     localPlugins:
       leafprovider:
         moduleName: github.com/NavistAu/traefik-leaf-provider
   ```

For Docker, mount the source into the container.

```yaml
services:
  traefik:
    image: traefik:v3.7
    volumes:
      - ./traefik-leaf-provider:/plugins-local/src/github.com/NavistAu/traefik-leaf-provider:ro
```

### Method 2: plugin catalog

With this method Traefik downloads the plugin from the plugin catalog.

```yaml
experimental:
  plugins:
    leafprovider:
      moduleName: github.com/NavistAu/traefik-leaf-provider
      version: v1.1.0
```

**Know the cost of this method.** Traefik contacts `plugins.traefik.io` at
startup. If that download fails, Traefik does not start. Your edge then serves
no routes at all. Method 1 has no such dependency.

## Configuration

Declare the provider in your static configuration.

```yaml
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
          routeMiddlewares:
            grafana-leaf: []
            status-leaf:
              - authentik-forwardauth@file
```

The key `leafprovider` must match the key you used in `experimental`.

### Top-level options

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `pollInterval` | duration | `5s` | The time between poll cycles. Must be more than zero. |
| `requestTimeout` | duration | `3s` | The timeout for one API call to one leaf instance. Must be more than zero. |
| `endpoints` | list | — | The leaf instances. You must give at least one. |

A duration is a Go duration string, such as `5s`, `500ms`, or `1m`.

Set `pollInterval` to a value that suits your rate of change. A short interval
finds new routes sooner. It also makes more requests to each leaf instance.

Keep `requestTimeout` well below `pollInterval`. The plugin reads the endpoints
one after the other, not at the same time. If every endpoint times out, one
poll cycle takes `requestTimeout` multiplied by the number of endpoints.

### Endpoint options

| Option | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | Yes | Identifies the endpoint. Must be unique. The plugin puts this name into each emitted router name. |
| `apiUrl` | string | Yes | The base URL of the leaf instance API. The plugin adds `/api/rawdata` to it. |
| `webUrl` | string | Yes | The web entrypoint of the leaf instance. The plugin sends matched traffic here. |
| `middlewares` | list of string | No | Middlewares to attach to every router emitted for this endpoint. |
| `routeMiddlewares` | map of string to list of string | No | Middlewares for named routes. Replaces `middlewares` for those routes. See [Middlewares](#middlewares). |

Give each middleware its full name, with the provider part. Write
`authentik-forwardauth@file`, not `authentik-forwardauth`. The middleware must
exist on the **main** instance. The plugin does not copy middlewares from a
leaf instance.

### Configuration errors

Traefik does not start if the configuration is invalid. The plugin rejects
these cases with a clear message.

| Message | Cause |
| --- | --- |
| `at least one endpoint is required` | The `endpoints` list is absent or empty. |
| `invalid pollInterval "…"` | The value does not parse, or it is not more than zero. |
| `invalid requestTimeout "…"` | The value does not parse, or it is not more than zero. |
| `endpoint N: name, apiUrl and webUrl are all required` | Endpoint number N has an empty required field. N starts at 0. |
| `duplicate endpoint name "…"` | Two endpoints have the same `name`. |

## Names of the emitted routers and services

The plugin builds each emitted router name from two parts.

```
<base name of the leaf router>-<endpoint name>
```

The service name for an endpoint is:

```
<endpoint name>-leaf
```

For example, the leaf router `grafana@docker` on the endpoint `argus` becomes
the router `grafana-argus`. That router uses the service `argus-leaf`.

Traefik adds its own provider part to these names. In the Traefik API and
dashboard you see `grafana-argus@plugin-leafprovider`.

The plugin copies the `rule` and the `priority` of each leaf router. It sets
the service and the middlewares itself.

**Keep your endpoint names short and distinct.** The endpoint name is part of
every router name that comes from that endpoint.

### Routers that the plugin ignores

The plugin does not emit a router in these cases.

- The leaf router key ends with `@internal`. These are the internal routers of
  the leaf instance, such as its API and its dashboard.
- The leaf router has an empty rule.

If an endpoint gives no usable routers, the plugin makes no service for it
either.

## Middlewares

There are two levels. The per-route level replaces the endpoint level. It does
not add to it.

### Endpoint level

`middlewares` attaches to **every** router emitted for that endpoint. Use this
for a concern that applies to the whole leaf instance, such as forward
authentication.

```yaml
- name: argus
  apiUrl: http://10.0.0.8:8080
  webUrl: http://10.0.0.8:80
  middlewares:
    - authentik-forwardauth@file
```

Every route from `argus` now goes through `authentik-forwardauth@file`.

### Per-route level

`routeMiddlewares` sets the middlewares for named routes. The keys are **base
names** of leaf routers. The rawdata key `grafana-leaf@docker` has the base
name `grafana-leaf`.

```yaml
- name: argus
  apiUrl: http://10.0.0.8:8080
  webUrl: http://10.0.0.8:80
  middlewares:
    - authentik-forwardauth@file
  routeMiddlewares:
    grafana-leaf: []
    status-leaf:
      - authentik-forwardauth@file
      - ratelimit@file
```

This configuration gives three different results.

| Leaf router | Middlewares attached | Reason |
| --- | --- | --- |
| `grafana-leaf@docker` | none | The key is present with an empty list. |
| `status-leaf@docker` | `authentik-forwardauth@file`, `ratelimit@file` | The key is present with a list. That list replaces the endpoint list. |
| `prometheus@docker` | `authentik-forwardauth@file` | No key is present. The endpoint list applies. |

Read these rules again, because the behaviour is deliberate.

1. A key that is present **replaces** the endpoint list for that route. The
   plugin does not merge the two lists.
2. A key with an explicitly empty list (`[]`) attaches **no** middlewares. This
   is how you exempt one route from an otherwise gated endpoint.
3. A route with no key falls back to the endpoint list.

**Check the log after you add or rename a key.** A key with a typographical
error matches no route. The route you meant to gate then serves with the
endpoint middlewares, or with none. The plugin reports unmatched keys. See the
next section.

## Behaviour when a leaf instance fails

The plugin keeps each endpoint separate. A failure at one endpoint does not
affect another.

| Event | What the plugin does |
| --- | --- |
| Traefik starts and a leaf instance is down | Traefik starts normally. The plugin makes no network call during initialisation. |
| A poll of one endpoint fails | The plugin logs the error. It serves the last good configuration for that endpoint. Other endpoints are unaffected. |
| A poll fails and there is no last good configuration | The plugin emits no routers for that endpoint. Other endpoints are unaffected. |
| A poll succeeds again | The plugin replaces the last good configuration with the new one. |
| A route disappears from a leaf instance | The plugin removes the emitted router at the next successful poll of that endpoint. |

The rule behind this table is simple. **Stale routes are better than no
routes.** A stale route usually still works, because the backend is still
there. An empty configuration returns HTTP 404 for every aggregated host.

## Log messages

Every message from this plugin starts with `leaf-provider`, followed by the
provider name you configured.

### An endpoint failed

```
leaf-provider leafprovider: endpoint "argus": fetch: dial tcp 10.0.0.8:8080: connect: connection refused (serving last known good)
```

The plugin could not read this endpoint. It logs this on every failed poll
cycle. The text after `endpoint "argus":` states the cause.

| Text | Cause |
| --- | --- |
| `fetch: …` | The main instance could not reach the leaf instance API. Check the network and `apiUrl`. |
| `fetch: HTTP 404` | The URL is wrong. Give the base URL, not the `/api/rawdata` path. |
| `fetch: HTTP 401` or `HTTP 403` | The leaf instance API needs authentication. This plugin cannot supply it. |
| `decode: …` | The response is not the expected rawdata format. Check that `apiUrl` points at a Traefik API. |

### A routeMiddlewares key matched no route

```
leaf-provider leafprovider: endpoint "argus": routeMiddlewares keys matching no leaf router: [staus-leaf]
```

One or more keys in `routeMiddlewares` match no router on that leaf instance.
The usual cause is a typographical error in the key, as in the example above.

The plugin logs this when the set of unmatched keys changes. It does not log it
on every poll cycle. Look for this message after you add or rename a key.

**Treat this message as a security problem, not as a warning.** The route you
meant to gate is not gated the way you intended.

## Troubleshooting

### Traefik does not start

Read the Traefik log. The plugin reports every configuration error with a clear
message. See [Configuration errors](#configuration-errors).

If the message is about the plugin itself, and not about the configuration,
check that the `vendor/` directory is present at the install path.

### No routes appear

Do these checks in order.

1. Confirm that the provider loaded. Look for `plugin-leafprovider` in the
   Traefik dashboard, or in `/api/rawdata` on the main instance.
2. Look for a `leaf-provider` line in the log. A failed endpoint always logs.
3. Confirm that the main instance can reach the leaf instance API.

   ```sh
   curl -s http://10.0.0.8:8080/api/rawdata | head -c 200
   ```

4. Confirm that the leaf instance has routers with rules.

   ```sh
   curl -s http://10.0.0.8:8080/api/rawdata | jq '.routers | keys'
   ```

   The plugin ignores routers that end with `@internal`. This is intended.

5. Wait for one `pollInterval`. The first poll happens immediately. A route
   that appears downstream needs the next cycle.

### A route appears but returns an error

The router exists on the main instance, so the aggregation works. The problem
is at hop 2.

Test the leaf instance directly. Use the `Host` header of the route.

```sh
curl -i -H 'Host: grafana.example.com' http://10.0.0.8:80/
```

If this fails, the problem is in the leaf instance or in its backend, not in
this plugin. If it succeeds, check `webUrl` and check the middlewares on the
main instance.

### A middleware does not apply

1. Read `/api/rawdata` on the **main** instance, and find the emitted router.

   ```sh
   curl -s http://127.0.0.1:8080/api/rawdata \
     | jq '.routers["grafana-argus@plugin-leafprovider"].middlewares'
   ```

2. If the list is wrong, look for a `routeMiddlewares` key with that base name.
   A key that is present replaces the endpoint list.
3. If the list is empty and you did not intend that, look for the unmatched-key
   message in the log.
4. If the list is correct but the middleware does nothing, confirm that the
   middleware exists on the main instance. It must have that exact name,
   including the provider part.

### Routes disappear and return

The endpoint fails some polls and succeeds at others. Look for `serving last
known good` lines in the log. Raise `requestTimeout`, or find why the leaf
instance API is slow.

## Design constraints

These constraints explain why the code has its present shape. Each one records
a failure in an earlier plugin that this one replaced. Read
[`provider.go`](provider.go) before you change any of them.

- **`Init()` makes no network call.** Traefik treats a provider initialisation
  failure as fatal. A probe in `Init()` would turn "one leaf instance is down
  during a restart" into "every route in the estate is down".
- **The plugin reads endpoints one after the other.** It isolates errors per
  endpoint, and it keeps a last good configuration per endpoint. One dead leaf
  instance must never block or blank another. Concurrency is where the earlier
  plugins had their endpoint bugs.
- **The plugin never copies a backend from a leaf instance.** Leaf backend URLs
  are container-network addresses. The main instance cannot reach them. To
  interpret the internals of a proxy, and then to route around it, is the
  design error that this plugin exists to avoid.
- **The code uses idioms that are safe under Yaegi.** Traefik interprets
  plugins with [Yaegi](https://github.com/traefik/yaegi). Yaegi is not the Go
  compiler, and the two have disagreed about this code twice.
  - Loops over endpoints use an explicit index. The capture of a loop variable
    behaves differently under Yaegi. That difference shipped as a bug in an
    earlier plugin.
  - `refresh` sends to the provider channel directly. A `select` case that
    sends an interpreted value panics inside `reflect.Select`.

This plugin replaced three earlier aggregator plugins:
`im-kulikov/traefik-provider`, `MadddinTribleD/traefikaggregator`, and
`Beanow/traefik-plugin-rawdata`. Each one was unmaintained, or failed in a way
that matters at a production edge: a crash at startup when an endpoint was
unreachable, incorrect handling of more than one endpoint, or silent loss of
routes. This plugin is the small subset that does only what is needed, and it
takes those failures as its design constraints.

## Tests

```sh
go test ./...          # compiled unit tests
./test/e2e/run.sh      # real Traefik, real Yaegi
```

The compiled unit tests cover the transform logic and the merge logic. They are
fast. They are also insufficient by their nature, because Traefik does not use
the Go compiler to run this plugin.

**Run the E2E suite before you trust any change.** It starts a real Traefik
instance that loads this repository as a local plugin. It uses one live leaf
instance and one dead endpoint. It then asserts these things.

- Startup survives the dead endpoint.
- Aggregation works, and the emitted names are correct.
- A request goes through both hops.
- The emitted routers have the correct provider namespace.
- The plugin logs the dead endpoint and isolates it.
- The four `routeMiddlewares` behaviours hold.

You need Docker with the Compose plugin.

## Contributing

Read [`CONTRIBUTING.md`](CONTRIBUTING.md). It covers the branch model, the two
test suites, and the Yaegi constraints that reviewers check.

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## Security

Report a vulnerability privately. Follow [`SECURITY.md`](SECURITY.md). Do not
open a public issue for a vulnerability.

## License

MIT. See [`LICENSE`](LICENSE).
