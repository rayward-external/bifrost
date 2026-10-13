package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// readinessPingTimeout bounds the readiness DB pings. It is deliberately shorter
// than a typical platform probe timeout so a slow store yields a fast 503 rather
// than a probe-side timeout (which is what turned a DB stall into a restart loop).
const readinessPingTimeout = 3 * time.Second

// HealthHandler manages HTTP requests for health checks.
type HealthHandler struct {
	config *lib.Config
}

// NewHealthHandler creates a new health handler instance.
func NewHealthHandler(config *lib.Config) *HealthHandler {
	return &HealthHandler{
		config: config,
	}
}

// RegisterRoutes registers the health-related routes.
//
// Three endpoints, with distinct contracts:
//   - /livez  — liveness: process-only. Never touches the DB. A restart cannot
//     fix a down dependency, so a liveness probe must not depend on one.
//   - /readyz — readiness: pings the backing stores with a short timeout. A
//     failure should withhold traffic from this replica, NOT restart it.
//   - /health — legacy combined endpoint, kept for backward compatibility;
//     honors client.disable_db_pings_in_health.
func (h *HealthHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/health", lib.ChainMiddlewares(h.getHealth, middlewares...))
	r.GET("/livez", lib.ChainMiddlewares(h.getLivez, middlewares...))
	r.GET("/readyz", lib.ChainMiddlewares(h.getReadyz, middlewares...))
}

// getLivez handles GET /livez - the liveness endpoint. It returns ok as long as
// the HTTP server can serve the request. It never checks the DB or any external
// dependency: point a liveness probe here so a transient DB outage does not
// crash-loop the container.
func (h *HealthHandler) getLivez(ctx *fasthttp.RequestCtx) {
	SendJSON(ctx, map[string]any{"status": "ok"})
}

// getReadyz handles GET /readyz - the readiness endpoint. It pings the backing
// stores with a short timeout so the platform can withhold traffic from a
// replica whose dependencies are unavailable. Unlike /health it always checks
// the DB (it is not gated by disable_db_pings_in_health) — checking dependencies
// is the entire point of a readiness probe.
func (h *HealthHandler) getReadyz(ctx *fasthttp.RequestCtx) {
	if errs := h.pingStores(ctx, readinessPingTimeout); len(errs) > 0 {
		SendError(ctx, fasthttp.StatusServiceUnavailable, errs[0])
		return
	}
	SendJSON(ctx, map[string]any{"status": "ok", "components": map[string]any{"db_pings": "ok"}})
}

// getHealth handles GET /health - the legacy combined health endpoint.
func (h *HealthHandler) getHealth(ctx *fasthttp.RequestCtx) {
	// If DB pings are disabled, just return OK
	if h.config.ClientConfig.DisableDBPingsInHealth {
		SendJSON(ctx, map[string]any{"status": "ok", "components": map[string]any{"db_pings": "disabled"}})
		return
	}
	if errs := h.pingStores(ctx, 10*time.Second); len(errs) > 0 {
		SendError(ctx, fasthttp.StatusServiceUnavailable, errs[0])
		return
	}
	SendJSON(ctx, map[string]any{"status": "ok", "components": map[string]any{"db_pings": "ok"}})
}

// pingStores pings the config, logs, and vector stores concurrently, bounded by
// timeout. It returns a human-readable error string for each store that failed
// (empty slice = all healthy).
func (h *HealthHandler) pingStores(ctx context.Context, timeout time.Duration) []string {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var errors []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	if h.config.ConfigStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.config.ConfigStore.Ping(reqCtx); err != nil {
				mu.Lock()
				errors = append(errors, "config store not available")
				mu.Unlock()
			}
		}()
	}

	if h.config.LogsStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.config.LogsStore.Ping(reqCtx); err != nil {
				mu.Lock()
				errors = append(errors, "log store not available")
				mu.Unlock()
			}
		}()
	}

	if h.config.VectorStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.config.VectorStore.Ping(reqCtx); err != nil {
				mu.Lock()
				errors = append(errors, "vector store not available")
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return errors
}
