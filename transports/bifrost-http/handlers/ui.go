package handlers

import (
	"embed"
	"mime"
	"path"
	"path/filepath"
	"strings"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// UIHandler handles UI routes.
type UIHandler struct {
	uiContent embed.FS
}

// NewUIHandler creates a new UIHandler instance.
func NewUIHandler(uiContent embed.FS) *UIHandler {
	return &UIHandler{
		uiContent: uiContent,
	}
}

// RegisterRoutes registers the UI routes with the provided router.
func (h *UIHandler) RegisterRoutes(router *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	router.GET("/", lib.ChainMiddlewares(h.serveDashboard, middlewares...))
	router.GET("/{filepath:*}", lib.ChainMiddlewares(h.serveDashboard, middlewares...))
}

// apiPathSegments are the namespaces that belong to the HTTP APIs rather than to
// the dashboard SPA. "v1" is matched both at the root (/v1/...) and as the second
// segment (/{provider}/v1/... for the provider-pinned routes).
var apiPathSegments = []string{"v1", "api"}

// isAPIPath reports whether requestPath addresses an API namespace rather than a
// dashboard route.
//
// RegisterRoutes installs `GET /{filepath:*}` as a global catch-all after every
// real route, so an unmatched API GET would otherwise be served the SPA shell
// with HTTP 200 — which an SDK then fails to JSON-decode. Router.NotFound already
// produces a correct JSON 404; this predicate lets API paths reach it.
//
// Matching is on whole path SEGMENTS, never on prefixes: a `strings.HasPrefix`
// check would misclassify UI routes like /v1beta or /apiary and break the
// dashboard, which is a worse failure than the bug being fixed.
func isAPIPath(requestPath string) bool {
	trimmed := strings.Trim(path.Clean("/"+strings.TrimPrefix(requestPath, "/")), "/")
	if trimmed == "" {
		return false
	}

	segments := strings.Split(trimmed, "/")

	for _, apiSegment := range apiPathSegments {
		if segments[0] == apiSegment {
			return true
		}
	}

	// Provider-pinned inference routes: /openai/v1/..., /anthropic/v1/..., etc.
	// Only "v1" is namespaced this way; "api" is root-only.
	if len(segments) >= 2 && segments[1] == "v1" {
		return true
	}

	return false
}

// ServeDashboard serves the dashboard UI.
func (h *UIHandler) serveDashboard(ctx *fasthttp.RequestCtx) {
	// Get the request path
	requestPath := string(ctx.Path())

	// An unmatched path under an API namespace is a genuine 404, not an SPA route.
	// Without this, the catch-all below answers it with index.html and HTTP 200,
	// so an SDK sees a success status and an HTML body. Message matches the
	// server's Router.NotFound handler so the two are indistinguishable to callers.
	if isAPIPath(requestPath) {
		SendError(ctx, fasthttp.StatusNotFound, "Route not found: "+requestPath)
		return
	}

	// Clean the path to prevent directory traversal
	cleanPath := path.Clean(requestPath)

	// Handle .txt files - map from /{page}.txt to /{page}/index.txt
	if strings.HasSuffix(cleanPath, ".txt") {
		// Remove .txt extension and add /index.txt
		basePath := strings.TrimSuffix(cleanPath, ".txt")
		if basePath == "/" || basePath == "" {
			basePath = "/index"
		}
		cleanPath = basePath + "/index.txt"
	}

	// Remove leading slash and add ui prefix
	if cleanPath == "/" {
		cleanPath = "ui/index.html"
	} else {
		cleanPath = "ui" + cleanPath
	}

	// Block hidden directories and files (any path segment starting with .)
	segments := strings.Split(cleanPath, "/")
	for _, segment := range segments {
		if strings.HasPrefix(segment, ".") {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("404 - Not found")
			return
		}
	}

	// Block sensitive files
	baseName := filepath.Base(cleanPath)
	sensitiveFiles := []string{"package.json", "package-lock.json"}
	for _, sensitive := range sensitiveFiles {
		if baseName == sensitive {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("404 - Not found")
			return
		}
	}

	// Check if this is a static asset request (has file extension)
	hasExtension := strings.Contains(filepath.Base(cleanPath), ".")

	// Try to read the file from embedded filesystem
	data, err := h.uiContent.ReadFile(cleanPath)
	if err != nil {

		// If it's a static asset (has extension) and not found, return 404
		if hasExtension {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("404 - Static asset not found: " + requestPath)
			return
		}

		// For routes without extensions (SPA routing), try {path}/index.html first
		if !hasExtension {
			indexPath := cleanPath + "/index.html"
			data, err = h.uiContent.ReadFile(indexPath)
			if err == nil {
				cleanPath = indexPath
			} else {
				// If that fails, serve root index.html as fallback
				data, err = h.uiContent.ReadFile("ui/index.html")
				if err != nil {
					ctx.SetStatusCode(fasthttp.StatusNotFound)
					ctx.SetBodyString("404 - File not found")
					return
				}
				cleanPath = "ui/index.html"
			}
		} else {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("404 - File not found")
			return
		}
	}

	// Set content type based on file extension
	ext := filepath.Ext(cleanPath)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ctx.SetContentType(contentType)

	// Set cache headers for static assets
	if strings.HasPrefix(cleanPath, "ui/assets/") {
		ctx.Response.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if ext == ".html" {
		ctx.Response.Header.Set("Cache-Control", "no-cache")
	} else {
		ctx.Response.Header.Set("Cache-Control", "public, max-age=3600")
	}

	// Send the file content
	ctx.SetBody(data)
}
