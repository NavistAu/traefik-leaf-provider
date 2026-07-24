// Package traefik_leaf_provider aggregates HTTP router rules from
// downstream "leaf" traefik instances into this (main) traefik, using
// the two-hop model: main matches the leaf's Host rules and forwards
// the whole request to the leaf's web entrypoint; the leaf does its own
// final routing. Backend/service definitions are deliberately NOT
// copied from leaves — their backend URLs are container-network
// addresses that are unreachable from here, and interpreting a proxy's
// internals to route around it is the design error this plugin exists
// to avoid.
//
// Hard-won constraints (each broke a prior candidate plugin — do not
// "clean up" without understanding them):
//   - Init() must do NO network I/O: traefik treats provider init
//     failure as fatal, so a blocking probe here turns "one leaf down
//     during a restart" into "every route in the estate down".
//   - Endpoints are polled sequentially with per-endpoint error
//     isolation and per-endpoint last-known-good: one dead leaf must
//     never block or blank the others.
//   - Loops over endpoints use explicit indexing: this code runs under
//     Yaegi (traefik's interpreter), whose loop-variable capture
//     semantics have diverged from compiled Go and shipped exactly
//     that bug in a prior plugin.
package traefik_leaf_provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/traefik/genconf/dynamic"
)

// Endpoint is one leaf traefik instance.
type Endpoint struct {
	// Name namespaces the emitted routers (router "grafana@docker" on
	// leaf "argus" becomes "grafana-argus"). Required, unique.
	Name string `json:"name" yaml:"name"`
	// APIURL is the base URL of the leaf's traefik API, e.g.
	// "http://10.0.0.8:8080". "/api/rawdata" is appended.
	APIURL string `json:"apiUrl" yaml:"apiUrl"`
	// WebURL is where matched traffic is forwarded — the leaf's web
	// entrypoint, e.g. "http://10.0.0.8:80".
	WebURL string `json:"webUrl" yaml:"webUrl"`
	// Middlewares are attached to every router emitted for this
	// endpoint, referenced with their full provider suffix, e.g.
	// "authentik-forwardauth@file". Optional.
	Middlewares []string `json:"middlewares" yaml:"middlewares"`
	// RouteMiddlewares overrides Middlewares per router, keyed by the
	// leaf router's base name (rawdata key with its "@provider" suffix
	// stripped, e.g. "grafana-leaf" for rawdata key
	// "grafana-leaf@docker" — the same base name used to build the
	// emitted router name). A present key REPLACES Middlewares for that
	// router, including an explicitly-empty list (no middlewares). A
	// router with no key here falls back to Middlewares. Optional.
	RouteMiddlewares map[string][]string `json:"routeMiddlewares" yaml:"routeMiddlewares"`
}

// Config is the plugin configuration.
type Config struct {
	// PollInterval between refresh cycles. Default "5s".
	PollInterval string `json:"pollInterval" yaml:"pollInterval"`
	// RequestTimeout for each leaf API call. Default "3s".
	RequestTimeout string     `json:"requestTimeout" yaml:"requestTimeout"`
	Endpoints      []Endpoint `json:"endpoints" yaml:"endpoints"`
}

// CreateConfig is required by traefik's plugin loader.
func CreateConfig() *Config {
	return &Config{PollInterval: "5s", RequestTimeout: "3s"}
}

// Provider implements traefik's plugin provider interface.
type Provider struct {
	name         string
	pollInterval time.Duration
	client       *http.Client
	endpoints    []Endpoint

	// lastGood holds the most recent successfully-built config per
	// endpoint index; on poll failure the stale copy keeps serving
	// (stale beats blank: the routes usually still work, and blanking
	// them would hard-404 the estate on a transient API hiccup).
	lastGood []*dynamic.HTTPConfiguration

	// lastUnmatched holds, per endpoint index, the sorted set of
	// routeMiddlewares keys that matched no leaf router as of the last
	// successful poll — so the "typo'd gate" warning logs on change,
	// not on every poll.
	lastUnmatched [][]string

	cancel context.CancelFunc
	done   chan struct{}
}

// New is called by traefik with the parsed config. No I/O here.
func New(ctx context.Context, config *Config, name string) (*Provider, error) {
	if config == nil || len(config.Endpoints) == 0 {
		return nil, errors.New("at least one endpoint is required")
	}

	poll, err := time.ParseDuration(config.PollInterval)
	if err != nil || poll <= 0 {
		return nil, fmt.Errorf("invalid pollInterval %q", config.PollInterval)
	}

	reqTimeout, err := time.ParseDuration(config.RequestTimeout)
	if err != nil || reqTimeout <= 0 {
		return nil, fmt.Errorf("invalid requestTimeout %q", config.RequestTimeout)
	}

	seen := map[string]bool{}
	for i := range config.Endpoints {
		e := config.Endpoints[i]
		if e.Name == "" || e.APIURL == "" || e.WebURL == "" {
			return nil, fmt.Errorf("endpoint %d: name, apiUrl and webUrl are all required", i)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("duplicate endpoint name %q", e.Name)
		}
		seen[e.Name] = true
	}

	return &Provider{
		name:          name,
		pollInterval:  poll,
		client:        &http.Client{Timeout: reqTimeout},
		endpoints:     config.Endpoints,
		lastGood:      make([]*dynamic.HTTPConfiguration, len(config.Endpoints)),
		lastUnmatched: make([][]string, len(config.Endpoints)),
		done:          make(chan struct{}),
	}, nil
}

// Init must stay free of network I/O — see package comment.
func (p *Provider) Init() error {
	return nil
}

// Provide starts the poll loop. Returning promptly is required;
// polling happens in the goroutine.
func (p *Provider) Provide(cfgChan chan<- json.Marshaler) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go func() {
		defer close(p.done)

		ticker := time.NewTicker(p.pollInterval)
		defer ticker.Stop()

		p.refresh(ctx, cfgChan)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refresh(ctx, cfgChan)
			}
		}
	}()

	return nil
}

// Stop terminates the poll loop.
func (p *Provider) Stop() error {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
	return nil
}

// refresh polls every endpoint (sequentially — two leaves at a 5s
// interval does not need concurrency, and concurrency is where the
// prior candidates' endpoint bugs lived) and emits the merged config.
func (p *Provider) refresh(ctx context.Context, cfgChan chan<- json.Marshaler) {
	merged := &dynamic.HTTPConfiguration{
		Routers:  map[string]*dynamic.Router{},
		Services: map[string]*dynamic.Service{},
	}

	for i := range p.endpoints {
		cfg, err := p.fetchEndpoint(ctx, i)
		if err != nil {
			log.Printf("leaf-provider %s: endpoint %q: %s (serving last known good)", p.name, p.endpoints[i].Name, err)
			cfg = p.lastGood[i]
		} else {
			p.lastGood[i] = cfg
		}
		if cfg == nil {
			continue
		}
		for name, r := range cfg.Routers {
			merged.Routers[name] = r
		}
		for name, s := range cfg.Services {
			merged.Services[name] = s
		}
	}

	if ctx.Err() != nil {
		return
	}
	// Plain send, NOT a select-case send: under Yaegi, routing an
	// interpreted JSONPayload through reflect.Select panics with
	// "not assignable to type json.Marshaler" (hit 2026-07-23, e2e).
	// A direct send is handled fine; blocking is acceptable because
	// traefik always consumes provider channels.
	cfgChan <- dynamic.JSONPayload{Configuration: &dynamic.Configuration{HTTP: merged}}
}

// fetchEndpoint reads one leaf's rawdata and builds this endpoint's
// slice of the config: one shared service pointing at the leaf's web
// entrypoint, plus one router per aggregated leaf router.
func (p *Provider) fetchEndpoint(ctx context.Context, idx int) (*dynamic.HTTPConfiguration, error) {
	e := p.endpoints[idx]

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(e.APIURL, "/")+"/api/rawdata", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	res, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch: HTTP %d", res.StatusCode)
	}

	// rawdata's top level is a flat {"routers": ..., "services": ...}
	// whose keys match HTTPConfiguration's json tags, so it decodes
	// directly; unknown runtime fields (status, using) are ignored.
	var raw dynamic.HTTPConfiguration
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	out := &dynamic.HTTPConfiguration{
		Routers:  map[string]*dynamic.Router{},
		Services: map[string]*dynamic.Service{},
	}

	serviceName := e.Name + "-leaf"
	matched := map[string]bool{}
	for key, r := range raw.Routers {
		if strings.HasSuffix(key, "@internal") || r == nil || r.Rule == "" {
			continue
		}
		base := strings.Split(key, "@")[0]
		matched[base] = true

		mw := e.Middlewares
		if v, ok := e.RouteMiddlewares[base]; ok {
			mw = v
		}

		out.Routers[base+"-"+e.Name] = &dynamic.Router{
			Rule:        r.Rule,
			Priority:    r.Priority,
			Service:     serviceName,
			Middlewares: mw,
		}
	}

	if len(out.Routers) > 0 {
		out.Services[serviceName] = &dynamic.Service{
			LoadBalancer: &dynamic.ServersLoadBalancer{
				Servers: []dynamic.Server{{URL: e.WebURL}},
			},
		}
	}

	// Security-relevant: a routeMiddlewares key matching no live leaf
	// router is a typo'd gate — that route would silently serve
	// ungated. Log on change, not every poll (see lastUnmatched).
	unmatched := []string{}
	for k := range e.RouteMiddlewares {
		if !matched[k] {
			unmatched = append(unmatched, k)
		}
	}
	sort.Strings(unmatched)
	if len(unmatched) > 0 && !sameStrings(unmatched, p.lastUnmatched[idx]) {
		log.Printf("leaf-provider %s: endpoint %q: routeMiddlewares keys matching no leaf router: %v", p.name, e.Name, unmatched)
	}
	p.lastUnmatched[idx] = unmatched

	return out, nil
}

// sameStrings reports whether two (already-sorted) slices hold the same
// elements in the same order. Index-based loop — see package comment.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
