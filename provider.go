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
//   - TLS material is NEVER read during New()/Init(), only lazily at
//     poll time: a cert path that is momentarily unreadable (a secret
//     still mounting during a restart) must degrade one endpoint, not
//     abort provider init and take the whole estate down with it. Only
//     shape errors that can never resolve — a keypair missing one half
//     — are rejected up front.
//
// crypto/tls, crypto/x509 and os.ReadFile were all confirmed to resolve
// under Yaegi against traefik v3.7 (spiked 2026-07-30); test/e2e drives
// the full mTLS path so a future interpreter change cannot regress it
// silently.
package traefik_leaf_provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/traefik/genconf/dynamic"
	tlstypes "github.com/traefik/genconf/dynamic/tls"
)

// Certificate is one client-certificate keypair, given as file paths.
// Field names mirror traefik's own serversTransport certificates so an
// operator writes the same shape in both places.
type Certificate struct {
	CertFile string `json:"certFile" yaml:"certFile"`
	KeyFile  string `json:"keyFile" yaml:"keyFile"`
}

// ClientTLS configures TLS for BOTH hops of one endpoint:
//
//   - hop 1, this plugin's own poll of the leaf's API. Traefik has no
//     say in this connection — it is a plain net/http client inside the
//     plugin — so without this block there is no way to reach a leaf API
//     behind a private CA or a client-certificate requirement.
//   - hop 2, traefik's forward to the leaf's web entrypoint. The plugin
//     emits an equivalent serversTransport and points the endpoint's
//     service at it, so one block gates both hops the same way.
//
// Set Endpoint.ServersTransport instead to hand hop 2 to a transport you
// define yourself; it takes precedence over the generated one.
type ClientTLS struct {
	// RootCAs are PEM bundle paths, ADDED to the system pool rather than
	// replacing it. Empty means system trust only.
	RootCAs []string `json:"rootCAs" yaml:"rootCAs"`
	// Certificates are client keypairs presented to the leaf (mTLS).
	Certificates []Certificate `json:"certificates" yaml:"certificates"`
	// ServerName overrides SNI and the verified hostname. Use when the
	// endpoint is addressed by IP but the certificate names a host.
	ServerName string `json:"serverName" yaml:"serverName"`
	// InsecureSkipVerify disables verification of the leaf's certificate.
	// This removes the guarantee that you are talking to the leaf at all.
	InsecureSkipVerify bool `json:"insecureSkipVerify" yaml:"insecureSkipVerify"`
}

// Endpoint is one leaf traefik instance.
type Endpoint struct {
	// Name namespaces the emitted routers (router "grafana@docker" on
	// leaf "alpha" becomes "grafana-alpha"). Required, unique.
	Name string `json:"name" yaml:"name"`
	// APIURL is the base URL of the leaf's traefik API, e.g.
	// "http://192.0.2.10:8080". "/api/rawdata" is appended.
	APIURL string `json:"apiUrl" yaml:"apiUrl"`
	// WebURL is where matched traffic is forwarded — the leaf's web
	// entrypoint, e.g. "http://192.0.2.10:80".
	WebURL string `json:"webUrl" yaml:"webUrl"`
	// Middlewares are attached to every router emitted for this
	// endpoint, referenced with their full provider suffix, e.g.
	// "forward-auth@file". Optional.
	Middlewares []string `json:"middlewares" yaml:"middlewares"`
	// RouteMiddlewares overrides Middlewares per router, keyed by the
	// leaf router's base name (rawdata key with its "@provider" suffix
	// stripped, e.g. "grafana-leaf" for rawdata key
	// "grafana-leaf@docker" — the same base name used to build the
	// emitted router name). A present key REPLACES Middlewares for that
	// router, including an explicitly-empty list (no middlewares). A
	// router with no key here falls back to Middlewares. Optional.
	RouteMiddlewares map[string][]string `json:"routeMiddlewares" yaml:"routeMiddlewares"`
	// TLS configures both hops for this endpoint. Optional; without it
	// hop 1 uses system trust and hop 2 uses traefik's global default
	// serversTransport, which is the v1 behaviour.
	TLS *ClientTLS `json:"tls" yaml:"tls"`
	// ServersTransport names an existing traefik serversTransport for
	// hop 2, e.g. "leaf-mtls@file". Takes precedence over the transport
	// generated from TLS. Optional.
	ServersTransport string `json:"serversTransport" yaml:"serversTransport"`
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

	// clients holds the per-endpoint poll client for endpoints that set
	// TLS; nil entries fall back to client. Built lazily on first use and
	// cached, so a cert file that is not readable yet degrades that one
	// endpoint (logged, last-known-good served) and is retried next poll,
	// rather than being fatal to every route in the estate at startup.
	clients []*http.Client

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

		// Shape-only checks. Deliberately NOT reading the files here: a
		// cert path that is briefly unreadable (a secret still mounting
		// during a restart) must degrade one endpoint, not abort provider
		// init and take down every route. A half-specified keypair, by
		// contrast, can never become valid, so reject it now.
		if e.TLS != nil {
			for j := range e.TLS.Certificates {
				c := e.TLS.Certificates[j]
				if (c.CertFile == "") != (c.KeyFile == "") {
					return nil, fmt.Errorf("endpoint %q: tls.certificates[%d]: certFile and keyFile must both be set", e.Name, j)
				}
				if c.CertFile == "" {
					return nil, fmt.Errorf("endpoint %q: tls.certificates[%d]: empty entry", e.Name, j)
				}
			}
			if e.TLS.InsecureSkipVerify {
				log.Printf("leaf-provider %s: endpoint %q: tls.insecureSkipVerify is set — the leaf's certificate is NOT verified on either hop", name, e.Name)
			}
		}
	}

	return &Provider{
		name:          name,
		pollInterval:  poll,
		client:        &http.Client{Timeout: reqTimeout},
		endpoints:     config.Endpoints,
		clients:       make([]*http.Client, len(config.Endpoints)),
		lastGood:      make([]*dynamic.HTTPConfiguration, len(config.Endpoints)),
		lastUnmatched: make([][]string, len(config.Endpoints)),
		done:          make(chan struct{}),
	}, nil
}

// clientFor returns the poll client for one endpoint, building and
// caching a TLS-configured one on first use. Errors here are returned to
// the caller so they land on the normal per-endpoint failure path.
func (p *Provider) clientFor(idx int) (*http.Client, error) {
	if p.endpoints[idx].TLS == nil {
		return p.client, nil
	}
	if p.clients[idx] != nil {
		return p.clients[idx], nil
	}

	tlsCfg, err := buildTLSConfig(p.endpoints[idx].TLS)
	if err != nil {
		return nil, err
	}
	c := &http.Client{
		Timeout:   p.client.Timeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	p.clients[idx] = c
	return c, nil
}

// buildTLSConfig turns an endpoint's TLS block into a *tls.Config for
// hop 1. Index-based loops — see package comment.
func buildTLSConfig(c *ClientTLS) (*tls.Config, error) {
	out := &tls.Config{
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.InsecureSkipVerify,
		MinVersion:         tls.VersionTLS12,
	}

	if len(c.RootCAs) > 0 {
		// Add to the system pool rather than replacing it, matching how
		// traefik's own serversTransport rootCAs behaves.
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		for i := range c.RootCAs {
			pem, err := os.ReadFile(c.RootCAs[i])
			if err != nil {
				return nil, fmt.Errorf("tls: rootCAs[%d]: %w", i, err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("tls: rootCAs[%d] (%s): no PEM certificates found", i, c.RootCAs[i])
			}
		}
		out.RootCAs = pool
	}

	for i := range c.Certificates {
		crt, err := tls.LoadX509KeyPair(c.Certificates[i].CertFile, c.Certificates[i].KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: certificates[%d]: %w", i, err)
		}
		out.Certificates = append(out.Certificates, crt)
	}

	return out, nil
}

// serversTransportFor mirrors an endpoint's TLS block into the traefik
// serversTransport that governs hop 2, so both hops are gated alike.
func serversTransportFor(c *ClientTLS) *dynamic.ServersTransport {
	out := &dynamic.ServersTransport{
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.InsecureSkipVerify,
		RootCAs:            c.RootCAs,
	}
	for i := range c.Certificates {
		out.Certificates = append(out.Certificates, tlstypes.Certificate{
			CertFile: c.Certificates[i].CertFile,
			KeyFile:  c.Certificates[i].KeyFile,
		})
	}
	return out
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
		Routers:           map[string]*dynamic.Router{},
		Services:          map[string]*dynamic.Service{},
		ServersTransports: map[string]*dynamic.ServersTransport{},
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
		for name, st := range cfg.ServersTransports {
			merged.ServersTransports[name] = st
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

	client, err := p.clientFor(idx)
	if err != nil {
		return nil, err
	}

	res, err := client.Do(req)
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
		Routers:           map[string]*dynamic.Router{},
		Services:          map[string]*dynamic.Service{},
		ServersTransports: map[string]*dynamic.ServersTransport{},
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
		lb := &dynamic.ServersLoadBalancer{
			Servers: []dynamic.Server{{URL: e.WebURL}},
		}
		// An explicit serversTransport wins: it lets an operator hand hop 2
		// to a transport this plugin does not model (SPIFFE, custom cipher
		// suites, forwarding timeouts). Otherwise mirror the TLS block.
		// Neither set leaves it empty — traefik's global default, as in v1.
		switch {
		case e.ServersTransport != "":
			lb.ServersTransport = e.ServersTransport
		case e.TLS != nil:
			transportName := e.Name + "-transport"
			out.ServersTransports[transportName] = serversTransportFor(e.TLS)
			lb.ServersTransport = transportName
		}
		out.Services[serviceName] = &dynamic.Service{LoadBalancer: lb}
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
