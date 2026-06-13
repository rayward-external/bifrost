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

const readinessPingTimeout = 10 * time.Second

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
func (h *HealthHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/health", lib.ChainMiddlewares(h.getHealth, middlewares...))
}

// getLivez handles liveness checks. It always returns 200 as long as the
// process can serve requests — it never depends on stores or external services.
func (h *HealthHandler) getLivez(ctx *fasthttp.RequestCtx) {
	SendJSON(ctx, map[string]any{"status": "ok"})
}

// pingStores concurrently pings all configured stores and returns a list of
// error strings (one per failing store). Returns empty slice when all pass.
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

// getHealth handles GET /health - combined health check for backwards compatibility.
func (h *HealthHandler) getHealth(ctx *fasthttp.RequestCtx) {
	if h.config.ClientConfig.DisableDBPingsInHealth {
		SendJSON(ctx, map[string]any{"status": "ok", "components": map[string]any{"db_pings": "disabled"}})
		return
	}
	errors := h.pingStores(ctx, readinessPingTimeout)
	if len(errors) > 0 {
		SendError(ctx, fasthttp.StatusServiceUnavailable, errors[0])
		return
	}
	SendJSON(ctx, map[string]any{"status": "ok", "components": map[string]any{"db_pings": "ok"}})
}
