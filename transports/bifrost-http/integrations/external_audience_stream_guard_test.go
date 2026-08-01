package integrations

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The SSE scrub for external callers has to be applied where the stream is
// CREATED — fasthttp's SetBodyStream closes the stream it replaces, so the
// response middleware cannot re-wrap one. That makes these call sites
// load-bearing security code in a file that is otherwise pure plumbing, and an
// upstream rebase that reintroduces a bare SetBodyStream would silently
// republish our routing metadata on every streamed token.
//
// A source-level assertion rather than a behavioural one because the failure is
// an OMISSION: a new streaming path added by upstream would not be covered by
// any test that exercises the paths we already know about.
//
// Mutation-tested: reverting either call site to `SetBodyStream(reader, -1)`
// fails this test.

var setBodyStreamCall = regexp.MustCompile(`ctx\.Response\.SetBodyStream\(([^,]+),`)

func TestEverySSEStreamIsWrappedForExternalAudience(t *testing.T) {
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("reading router.go: %v", err)
	}

	matches := setBodyStreamCall.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no SetBodyStream calls found in router.go — the guard has gone vacuous, " +
			"either because the call was renamed or the file moved")
	}

	// Pin the count. A NEW streaming path is exactly the case this guard exists
	// for, and it must fail loudly rather than pass because the newcomer happened
	// not to match the regex above.
	const knownStreamInstalls = 2
	if len(matches) != knownStreamInstalls {
		t.Fatalf("router.go has %d SetBodyStream calls, expected %d. A streaming path was "+
			"added or removed: confirm the new one wraps with lib.WrapSSEForExternalAudience, "+
			"then update this count.", len(matches), knownStreamInstalls)
	}

	for _, match := range matches {
		argument := strings.TrimSpace(match[1])
		if !strings.Contains(argument, "WrapSSEForExternalAudience") {
			t.Errorf("SetBodyStream(%s, …) installs an unwrapped stream — external callers "+
				"would receive extra_fields on every SSE chunk. Wrap it with "+
				"lib.WrapSSEForExternalAudience(ctx, reader).", argument)
		}
	}
}
