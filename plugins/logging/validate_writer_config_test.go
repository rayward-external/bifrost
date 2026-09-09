package logging

import (
	"testing"

	"github.com/maximhq/bifrost/framework/logstore"
)

// validWriterConfigWithLogstoreDefaults returns a baseline config that passes
// validateWriterConfig, so each test case can mutate a single field to exercise
// one bound. Named distinctly from writerconfig_test.go's own validWriterConfig
// (upstream-added, hardcoded literals) since this one is keyed on the shared
// logstore.DefaultWriter*/MaxWriter* constants both files' assertions rely on.
func validWriterConfigWithLogstoreDefaults() logstore.WriterConfig {
	return logstore.WriterConfig{
		MaxBatchSize:             logstore.DefaultWriterMaxBatchSize,
		BatchInterval:            logstore.DefaultWriterBatchInterval,
		MaxBatchBytes:            logstore.DefaultWriterMaxBatchBytes,
		WriteQueueCapacity:       logstore.DefaultWriterQueueCapacity,
		DeferredUsageConcurrency: logstore.DefaultWriterDeferredUsageConcurrency,
	}
}

// TestValidateWriterConfig_UpperBounds covers MaxBatchSize only: validateWriterConfig
// bounds it directly against the shared logstore.MaxWriterMaxBatchSize constant.
// WriteQueueCapacity and DeferredUsageConcurrency are bounded against this package's
// own (tighter, upstream-added) maxWriterQueueCapacity/maxWriterDeferredUsageConcurrency
// constants instead — see writerconfig_test.go's TestValidateWriterConfigBounds, which
// already covers both against those actual values.
func TestValidateWriterConfig_UpperBounds(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *logstore.WriterConfig)
		wantErr bool
	}{
		{"defaults are valid", func(c *logstore.WriterConfig) {}, false},
		{"max_batch_size at limit", func(c *logstore.WriterConfig) {
			c.MaxBatchSize = logstore.MaxWriterMaxBatchSize
		}, false},
		{"max_batch_size over limit", func(c *logstore.WriterConfig) {
			c.MaxBatchSize = logstore.MaxWriterMaxBatchSize + 1
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validWriterConfigWithLogstoreDefaults()
			tt.mutate(&c)
			err := validateWriterConfig(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateWriterConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
