// Compiled unit tests cover the transform/merge logic. CAUTION: green
// here does NOT prove the plugin works inside traefik — it runs under
// Yaegi (interpreted), which has diverged from compiled Go before (a
// prior candidate's multi-endpoint bug existed only under Yaegi while
// its compiled tests passed). The container test in test/e2e is the
// authoritative gate; keep both.
package traefik_leaf_provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/traefik/genconf/dynamic"
)

const sampleRawdata = `{
  "routers": {
    "grafana@docker":  {"rule": "Host(` + "`grafana.29c.sh`" + `)", "service": "grafana", "status": "enabled", "using": ["web"]},
    "status@docker":   {"rule": "Host(` + "`status.29c.sh`" + `)", "priority": 20, "service": "status"},
    "api@internal":    {"rule": "PathPrefix(` + "`/api`" + `)", "service": "api@internal"},
    "phantom@docker":  {"service": "phantom"}
  },
  "services": {
    "grafana@docker": {"loadBalancer": {"servers": [{"url": "http://172.18.0.5:3000"}]}}
  }
}`

func testProvider(t *testing.T, endpoints []Endpoint) *Provider {
	t.Helper()
	cfg := CreateConfig()
	cfg.Endpoints = endpoints
	p, err := New(context.Background(), cfg, "test")
	if err != nil {
		t.Fatalf("New: %s", err)
	}
	return p
}

func TestFetchEndpointTransform(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rawdata" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{{
		Name:        "argus",
		APIURL:      srv.URL,
		WebURL:      "http://10.0.0.8:80",
		Middlewares: []string{"authentik-forwardauth@file"},
	}})

	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatalf("fetchEndpoint: %s", err)
	}

	// @internal skipped, rule-less phantom skipped, two survive.
	if len(cfg.Routers) != 2 {
		t.Fatalf("routers = %d, want 2 (got %v)", len(cfg.Routers), keys(cfg.Routers))
	}

	r, ok := cfg.Routers["grafana-argus"]
	if !ok {
		t.Fatalf("missing router grafana-argus, got %v", keys(cfg.Routers))
	}
	if r.Service != "argus-leaf" || r.Rule != "Host(`grafana.29c.sh`)" {
		t.Fatalf("router mismatch: %+v", r)
	}
	if len(r.Middlewares) != 1 || r.Middlewares[0] != "authentik-forwardauth@file" {
		t.Fatalf("middlewares not attached: %+v", r.Middlewares)
	}
	if cfg.Routers["status-argus"].Priority != 20 {
		t.Fatalf("priority not preserved: %+v", cfg.Routers["status-argus"])
	}

	// One shared service at the leaf's web entrypoint — never the
	// leaf's own container-network backends.
	svc, ok := cfg.Services["argus-leaf"]
	if !ok || len(svc.LoadBalancer.Servers) != 1 || svc.LoadBalancer.Servers[0].URL != "http://10.0.0.8:80" {
		t.Fatalf("service mismatch: %+v", svc)
	}
}

func TestRefreshIsolatesDeadEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{
		{Name: "alive", APIURL: srv.URL, WebURL: "http://10.0.0.8:80"},
		// TEST-NET-1 address: unroutable, fails fast via timeout.
		{Name: "dead", APIURL: "http://192.0.2.1:9", WebURL: "http://192.0.2.1:80"},
	})

	out := make(chan json.Marshaler, 1)
	p.refresh(context.Background(), out)

	payload := (<-out).(dynamic.JSONPayload)
	if _, ok := payload.Configuration.HTTP.Routers["grafana-alive"]; !ok {
		t.Fatalf("alive endpoint's routers missing when sibling is dead: %v", keys(payload.Configuration.HTTP.Routers))
	}
}

func TestRefreshServesLastGoodWhenEndpointDies(t *testing.T) {
	alive := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !alive {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{{Name: "argus", APIURL: srv.URL, WebURL: "http://10.0.0.8:80"}})

	out := make(chan json.Marshaler, 2)
	p.refresh(context.Background(), out)
	<-out

	alive = false
	p.refresh(context.Background(), out)
	payload := (<-out).(dynamic.JSONPayload)
	if _, ok := payload.Configuration.HTTP.Routers["grafana-argus"]; !ok {
		t.Fatal("last-known-good not served after endpoint failure")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for name, endpoints := range map[string][]Endpoint{
		"empty":     {},
		"noName":    {{APIURL: "http://x", WebURL: "http://x"}},
		"duplicate": {{Name: "a", APIURL: "http://x", WebURL: "http://x"}, {Name: "a", APIURL: "http://y", WebURL: "http://y"}},
	} {
		cfg := CreateConfig()
		cfg.Endpoints = endpoints
		if _, err := New(context.Background(), cfg, "test"); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestRouteMiddlewaresOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{{
		Name:        "argus",
		APIURL:      srv.URL,
		WebURL:      "http://10.0.0.8:80",
		Middlewares: []string{"a@file"},
		RouteMiddlewares: map[string][]string{
			"status": {"b@file"},
		},
	}})

	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatalf("fetchEndpoint: %s", err)
	}

	if mw := cfg.Routers["status-argus"].Middlewares; len(mw) != 1 || mw[0] != "b@file" {
		t.Fatalf("status-argus middlewares = %v, want [b@file]", mw)
	}
	if mw := cfg.Routers["grafana-argus"].Middlewares; len(mw) != 1 || mw[0] != "a@file" {
		t.Fatalf("grafana-argus (no override) middlewares = %v, want endpoint default [a@file]", mw)
	}
}

func TestRouteMiddlewaresEmptyListCarveOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{{
		Name:        "argus",
		APIURL:      srv.URL,
		WebURL:      "http://10.0.0.8:80",
		Middlewares: []string{"a@file"},
		RouteMiddlewares: map[string][]string{
			"grafana": {},
		},
	}})

	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatalf("fetchEndpoint: %s", err)
	}

	if mw := cfg.Routers["grafana-argus"].Middlewares; len(mw) != 0 {
		t.Fatalf("grafana-argus middlewares = %v, want none (empty-list carve-out)", mw)
	}
	if mw := cfg.Routers["status-argus"].Middlewares; len(mw) != 1 || mw[0] != "a@file" {
		t.Fatalf("status-argus middlewares = %v, want endpoint default [a@file]", mw)
	}
}

func TestRouteMiddlewaresFallbackToEndpointDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{{
		Name:             "argus",
		APIURL:           srv.URL,
		WebURL:           "http://10.0.0.8:80",
		Middlewares:      []string{"a@file"},
		RouteMiddlewares: map[string][]string{}, // no entries at all: pure fallback
	}})

	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatalf("fetchEndpoint: %s", err)
	}

	for _, name := range []string{"grafana-argus", "status-argus"} {
		if mw := cfg.Routers[name].Middlewares; len(mw) != 1 || mw[0] != "a@file" {
			t.Fatalf("%s middlewares = %v, want endpoint default [a@file]", name, mw)
		}
	}
}

func TestUnmatchedRouteMiddlewaresKeyLoggedOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{{
		Name:   "argus",
		APIURL: srv.URL,
		WebURL: "http://10.0.0.8:80",
		RouteMiddlewares: map[string][]string{
			"ghost": {"a@file"},
		},
	}})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	if _, err := p.fetchEndpoint(context.Background(), 0); err != nil {
		t.Fatalf("fetchEndpoint (1st poll): %s", err)
	}
	if _, err := p.fetchEndpoint(context.Background(), 0); err != nil {
		t.Fatalf("fetchEndpoint (2nd poll): %s", err)
	}

	const marker = "routeMiddlewares keys matching no leaf router"
	if got := strings.Count(buf.String(), marker); got != 1 {
		t.Fatalf("unmatched-key log line appeared %d times, want 1: log = %q", got, buf.String())
	}
	if !strings.Contains(buf.String(), "ghost") {
		t.Fatalf("log output missing unmatched key %q: %q", "ghost", buf.String())
	}
}

func keys(m map[string]*dynamic.Router) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
