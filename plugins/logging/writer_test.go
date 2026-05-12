package logging

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/logstore"
)

type captureLogger struct {
	mu       sync.Mutex
	warnings []string
}

func (l *captureLogger) Debug(string, ...any) {}
func (l *captureLogger) Info(string, ...any)  {}
func (l *captureLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprintf(msg, args...))
}
func (l *captureLogger) Error(string, ...any)                   {}
func (l *captureLogger) Fatal(string, ...any)                   {}
func (l *captureLogger) SetLevel(schemas.LogLevel)              {}
func (l *captureLogger) SetOutputType(schemas.LoggerOutputType) {}
func (l *captureLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func (l *captureLogger) warningText() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.warnings, "\n")
}

func TestQueueFullWarningsDoNotLogEntryIDs(t *testing.T) {
	sensitiveID := "client-secret\nforged-log-line"
	logger := &captureLogger{}
	plugin := &LoggerPlugin{
		logger:     logger,
		writeQueue: make(chan *writeQueueEntry, 1),
	}

	plugin.writeQueue <- &writeQueueEntry{log: &logstore.Log{ID: "queued"}}
	plugin.enqueueLogEntry(&logstore.Log{ID: sensitiveID}, nil)

	plugin.writeQueue = make(chan *writeQueueEntry, 1)
	plugin.writeQueue <- &writeQueueEntry{mcpLog: &logstore.MCPToolLog{ID: "queued-mcp"}}
	plugin.enqueueMCPToolLogEntry(&logstore.MCPToolLog{ID: sensitiveID}, nil)

	warnings := logger.warningText()
	if strings.Contains(warnings, sensitiveID) {
		t.Fatalf("queue-full warning leaked log entry ID: %q", warnings)
	}
	if strings.Contains(warnings, "forged-log-line") {
		t.Fatalf("queue-full warning allowed log injection text: %q", warnings)
	}
}
