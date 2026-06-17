package logging

import (
	"testing"

	"github.com/maximhq/bifrost/framework/logstore"
)

// validWriterConfig returns a baseline config that passes validateWriterConfig,
// so each test case can mutate a single field to exercise one bound.
func validWriterConfig() logstore.WriterConfig {
	return logstore.WriterConfig{
		MaxBatchSize:             logstore.DefaultWriterMaxBatchSize,
		BatchInterval:            logstore.DefaultWriterBatchInterval,
		MaxBatchBytes:            logstore.DefaultWriterMaxBatchBytes,
		WriteQueueCapacity:       logstore.DefaultWriterQueueCapacity,
		DeferredUsageConcurrency: logstore.DefaultWriterDeferredUsageConcurrency,
	}
}

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
		{"write_queue_capacity at limit", func(c *logstore.WriterConfig) {
			c.WriteQueueCapacity = logstore.MaxWriterQueueCapacity
		}, false},
		{"write_queue_capacity over limit", func(c *logstore.WriterConfig) {
			c.WriteQueueCapacity = logstore.MaxWriterQueueCapacity + 1
		}, true},
		{"deferred_usage_concurrency at limit", func(c *logstore.WriterConfig) {
			c.DeferredUsageConcurrency = logstore.MaxWriterDeferredUsageConcurrency
		}, false},
		{"deferred_usage_concurrency over limit", func(c *logstore.WriterConfig) {
			c.DeferredUsageConcurrency = logstore.MaxWriterDeferredUsageConcurrency + 1
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validWriterConfig()
			tt.mutate(&c)
			err := validateWriterConfig(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateWriterConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
