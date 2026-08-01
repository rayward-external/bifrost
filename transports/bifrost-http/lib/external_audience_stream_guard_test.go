package lib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every response body stream in the transport must be installed through
// WrapSSEForExternalAudience.
//
// # WHY THIS GUARD IS REPO-WIDE, AND WHY THE FIRST VERSION WAS NOT ENOUGH
//
// The first version of this test scanned integrations/router.go and pinned the
// number of SetBodyStream calls IN THAT FILE. It passed, and it was wrong: an
// independent review found handlers/inference.go installing the PRIMARY
// inference stream — `stream:true` on /v1/chat/completions — with a bare
// SetBodyStream. The most important streaming path in the product was
// unprotected while a green guard said otherwise.
//
// Pinning a count inside one file proves nothing about the file next to it. So
// the scan is now the whole transport tree, and any new stream site fails until
// someone decides deliberately which side of the policy it is on.
//
// Comments are stripped before matching. external_audience_body.go documents
// this exact API in prose, and a guard that trips on the text describing it is
// noise that gets suppressed rather than read.

// unwrappedStreamSites are the sites deliberately NOT wrapped. Each needs a
// reason that survives a reader asking "why is this one different?".
var unwrappedStreamSites = map[string]string{
	// StreamLargeResponseBody pipes the UPSTREAM's bytes through unchanged, so
	// it cannot carry our extra_fields — there is nothing to scrub. It also sets
	// Content-Length from the upstream value when one is known, and any rewrite
	// would leave that length describing a body that no longer exists.
	"lib/lib.go": "verbatim upstream passthrough with a declared Content-Length; carries no extra_fields",
}

func TestEveryResponseStreamIsWrappedForExternalAudience(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving transport root: %v", err)
	}

	var scanned, found int
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		for i, line := range strings.Split(string(source), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			if !strings.Contains(code, "Response.SetBodyStream(") {
				continue
			}
			found++

			if reason, exempt := unwrappedStreamSites[filepath.ToSlash(rel)]; exempt {
				if strings.Contains(code, "WrapSSEForExternalAudience") {
					t.Errorf("%s:%d is listed as an exempt stream site (%q) but now wraps. "+
						"Remove it from unwrappedStreamSites.", rel, i+1, reason)
				}
				continue
			}

			if !strings.Contains(code, "WrapSSEForExternalAudience") {
				t.Errorf("%s:%d installs an unwrapped response stream:\n    %s\n"+
					"External callers would receive extra_fields on every SSE chunk. Either wrap it with "+
					"lib.WrapSSEForExternalAudience(ctx, reader), or add the file to unwrappedStreamSites "+
					"with a reason.", rel, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// Vacuity checks. Both of these have failed silently in this repo before: a
	// guard whose pattern stops matching reports success forever.
	if scanned < 50 {
		t.Fatalf("only %d .go files scanned under %s — the walk is not reaching the transport tree", scanned, root)
	}
	if found == 0 {
		t.Fatal("no Response.SetBodyStream( call sites found — the API was renamed or the guard's pattern is stale, " +
			"and this test is now asserting nothing")
	}
}
