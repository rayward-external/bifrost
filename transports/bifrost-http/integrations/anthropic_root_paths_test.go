package integrations

import (
	"testing"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// TestNewAnthropicRouterMountsRootPaths verifies that the Anthropic surface is
// served both under the /anthropic prefix and at the bare root paths, and that
// registering both onto one shared router does not collide (fasthttp/router
// panics on duplicate method+path registration, so a completed registration IS
// the collision test).
func TestNewAnthropicRouterMountsRootPaths(t *testing.T) {
	ar := NewAnthropicRouter(nil, nil, &testLogger{})

	r := router.New()
	// Simulate the core routes that share the router in production
	// (handlers/inference.go RegisterRoutes) which must not collide with the
	// root-mounted Anthropic paths.
	noop := func(_ *fasthttp.RequestCtx) {}
	r.POST("/v1/chat/completions", noop)
	r.POST("/v1/batches", noop)
	r.GET("/v1/batches/{batch_id}", noop)
	r.POST("/v1/files", noop)
	r.GET("/v1/models", noop)

	ar.RegisterRoutes(r)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{"POST", "/v1/messages"},
		{"POST", "/v1/messages/count_tokens"},
		{"POST", "/v1/messages/batches"},
		{"GET", "/v1/messages/batches/msgbatch_test"},
		{"GET", "/v1/messages/batches/msgbatch_test/results"},
		{"POST", "/v1/messages/batches/msgbatch_test/cancel"},
		{"POST", "/anthropic/v1/messages"},
		{"POST", "/anthropic/v1/messages/count_tokens"},
		{"POST", "/anthropic/v1/messages/batches"},
	} {
		lookupCtx := &fasthttp.RequestCtx{}
		handler, _ := r.Lookup(tc.method, tc.path, lookupCtx)
		if handler == nil {
			t.Errorf("expected %s %s to be routable, got no handler", tc.method, tc.path)
		}
	}

	// The literal routes must win over the /v1/messages/{path:*} wildcard —
	// if the wildcard shadowed them, count_tokens would run generic inference
	// and batch creates would be treated as message posts. Handler pointers
	// can't discriminate (closures from one source line share a code pointer),
	// but a wildcard match stores the {path:*} route param and a literal match
	// does not.
	matchedByWildcard := func(method, path string) bool {
		lookupCtx := &fasthttp.RequestCtx{}
		handler, _ := r.Lookup(method, path, lookupCtx)
		if handler == nil {
			t.Fatalf("expected %s %s to be routable", method, path)
		}
		return lookupCtx.UserValue("path") != nil
	}
	if !matchedByWildcard("POST", "/v1/messages/arbitrary-subpath") {
		t.Error("expected POST /v1/messages/arbitrary-subpath to be matched by the wildcard route")
	}
	if matchedByWildcard("POST", "/v1/messages/count_tokens") {
		t.Error("POST /v1/messages/count_tokens matched the wildcard messages route — literal route shadowed")
	}
	if matchedByWildcard("POST", "/v1/messages/batches") {
		t.Error("POST /v1/messages/batches matched the wildcard messages route — literal route shadowed")
	}

	// The list-models and files families must NOT be mounted at root — they
	// belong to the core OpenAI-shape handler there.
	ctx := &fasthttp.RequestCtx{}
	handler, _ := r.Lookup("GET", "/v1/models", ctx)
	if handler == nil {
		t.Error("core GET /v1/models must remain routable")
	}
}
