package handlers

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/valyala/fasthttp"
)

// TestHealthHandler_Livez_AlwaysOK locks the liveness contract: /livez must
// return 200 purely on the process being able to serve a request — it must
// never depend on the DB or any external store, so a dependency outage cannot
// crash-loop the container via the liveness probe.
func TestHealthHandler_Livez_AlwaysOK(t *testing.T) {
	h := NewHealthHandler(&lib.Config{})
	ctx := &fasthttp.RequestCtx{}

	h.getLivez(ctx)

	assert.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	assert.Contains(t, string(ctx.Response.Body()), `"status":"ok"`)
}

// TestHealthHandler_PingStores_NoStores verifies the readiness ping logic
// reports no errors when no stores are configured (so /readyz returns 200).
func TestHealthHandler_PingStores_NoStores(t *testing.T) {
	h := NewHealthHandler(&lib.Config{})

	errs := h.pingStores(context.Background(), readinessPingTimeout)

	assert.Empty(t, errs)
}
