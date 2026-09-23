// Package proxy contains the sandbox traffic gateway: the single place where
// requests for <subdomain>.sandbox.localhost arrive (routed there by Traefik's
// wildcard rule) and are forwarded to the correct container. Its key job is
// wake-on-request — restarting a hibernated container mid-request so the user
// experience of an idle sandbox is a few seconds of extra latency, never an
// error.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/api-sandbox-links/backend/internal/cache"
	"github.com/api-sandbox-links/backend/internal/config"
	"github.com/api-sandbox-links/backend/internal/db"
	"github.com/api-sandbox-links/backend/internal/orchestrator"
)

// wakeTimeout is how long the gateway waits for a hibernated container to
// become healthy before giving up on the current request.
const wakeTimeout = 90 * time.Second

// Gateway reverse-proxies sandbox subdomain traffic to the matching container
// and performs on-demand wake-ups.
type Gateway struct {
	store  *db.Store
	cache  *cache.Redis
	orch   *orchestrator.Orchestrator
	cfg    *config.Config
	logger *slog.Logger

	// waking guards per-sandbox wake-ups so two simultaneous requests to a
	// sleeping sandbox don't start two containers.
	wakingMu sync.Mutex
	waking   map[string]chan struct{}
}

// NewGateway builds the traffic gateway with its dependencies.
func NewGateway(store *db.Store, cache *cache.Redis, orch *orchestrator.Orchestrator, cfg *config.Config, logger *slog.Logger) *Gateway {
	return &Gateway{
		store:  store,
		cache:  cache,
		orch:   orch,
		cfg:    cfg,
		logger: logger,
		waking: map[string]chan struct{}{},
	}
}

// Handler returns the reverse-proxy HTTP handler for sandbox subdomains.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.serve)
}

// serve resolves the subdomain from the Host header, wakes the sandbox if it
// is hibernated, and proxies the request to the container over its private
// network address.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	subdomain, err := g.subdomainFromHost(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// We show a "gateway still warming the sandbox" page while waking instead
	// of queueing indefinitely; this is the one place a subdomain may not yet
	// exist synchronously.
	sandbox, err := g.store.GetSandboxBySubdomain(subdomain)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// A destroyed sandbox answers with a permanent, honest status: the URL once
	// existed and is gone for good (its tombstone row retains the history).
	if sandbox.DestroyedAt != nil {
		http.Error(w, "This sandbox has been destroyed (its lifetime expired or it was deleted).", http.StatusGone)
		return
	}

	if sandbox.ContainerID == "" {
		http.Error(w, "This sandbox has no running container (it may still be building).", http.StatusServiceUnavailable)
		return
	}

	// Wake-on-request: a hibernated container gets restarted before we forward.
	if sandbox.Status == db.StatusHibernated {
		if err := g.wake(r.Context(), sandbox); err != nil {
			g.logger.Warn("gateway: wake failed", slog.String("sandbox", sandbox.ID), slog.String("err", err.Error()))
			http.Error(w, "Sandbox failed to wake; try again in a moment.", http.StatusServiceUnavailable)
			return
		}
	}

	// Mark activity so the idle reaper's timer restarts. Fire-and-forget: the
	// request must not wait on Redis in the hot path.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = g.cache.Touch(ctx, sandbox.ID, sandbox.IdleTimeoutSeconds)
	}()

	ip, err := g.orch.ContainerIP(r.Context(), sandbox.ContainerID, "sandbox_"+sandbox.ID[:8])
	if err != nil {
		http.Error(w, "Sandbox container is unreachable.", http.StatusServiceUnavailable)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("%s:%d", ip, sandbox.InternalPort)
			// Tell the app which host the user actually typed so redirects and
			// absolute links keep working from outside.
			req.Header.Set("X-Forwarded-Host", r.Host)
			req.Host = fmt.Sprintf("%s:%d", ip, sandbox.InternalPort)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.logger.Warn("gateway: proxy error", slog.String("sandbox", sandbox.ID), slog.String("err", err.Error()))
			http.Error(w, "Sandbox container is not responding.", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// wake restarts a hibernated container and waits for it to be healthy. It is
// concurrency-safe: the first caller for a sandbox performs the wake; any
// other caller blocks on a shared "done" channel, then proceeds to proxy.
func (g *Gateway) wake(ctx context.Context, sandbox *db.Sandbox) error {
	g.wakingMu.Lock()
	if done, ok := g.waking[sandbox.ID]; ok {
		g.wakingMu.Unlock()
		<-done
		return nil
	}
	done := make(chan struct{})
	g.waking[sandbox.ID] = done
	g.wakingMu.Unlock()

	defer func() {
		g.wakingMu.Lock()
		delete(g.waking, sandbox.ID)
		close(done)
		g.wakingMu.Unlock()
	}()

	if err := g.orch.StartContainer(ctx, sandbox.ContainerID); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	wakeCtx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()
	ip, err := g.orch.ContainerIP(wakeCtx, sandbox.ContainerID, "sandbox_"+sandbox.ID[:8])
	if err != nil {
		return err
	}
	if err := g.orch.WaitHealthy(wakeCtx, sandbox.ContainerID, ip, sandbox.InternalPort, wakeTimeout); err != nil {
		return err
	}

	// The sandbox is up again; it is not hibernated anymore. Restart its stats
	// collector so the dashboard keeps flowing.
	g.orch.EnsureStatsCollector(sandbox.ID, sandbox.ContainerID)
	if err := g.store.UpdateSandbox(sandbox.ID, map[string]any{"status": string(db.StatusRunning)}); err != nil {
		return fmt.Errorf("marking running: %w", err)
	}
	return nil
}

// subdomainFromHost strips the base domain from a Host header, returning the
// sandbox's subdomain. It accepts host[:port] forms.
func (g *Gateway) subdomainFromHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i] // strip port
	}
	if !strings.HasSuffix(host, "."+g.cfg.SandboxBaseDomain) {
		return "", fmt.Errorf("unrecognized sandbox host %q", host)
	}
	sub := strings.TrimSuffix(host, "."+g.cfg.SandboxBaseDomain)
	if sub == "" || strings.ContainsAny(sub, " "){
		return "", fmt.Errorf("malformed sandbox host %q", host)
	}
	return sub, nil
}