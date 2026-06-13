package governance

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestBlackListIsBlocked(t *testing.T) {
	tests := []struct {
		name      string
		blacklist schemas.BlackList
		model     string
		want      bool
	}{
		{
			name:      "empty blacklist allows",
			blacklist: schemas.BlackList{},
			model:     "mistral:latest",
			want:      false,
		},
		{
			name:      "wildcard blocks all",
			blacklist: schemas.BlackList{"*"},
			model:     "llama3.2:latest",
			want:      true,
		},
		{
			name:      "bare blacklist blocks bare request",
			blacklist: schemas.BlackList{"mistral:latest"},
			model:     "mistral:latest",
			want:      true,
		},
		{
			name:      "prefixed blacklist does not block bare request",
			blacklist: schemas.BlackList{"ollama/mistral:latest"},
			model:     "mistral:latest",
			want:      false,
		},
		{
			name:      "bare blacklist does not block prefixed request",
			blacklist: schemas.BlackList{"mistral:latest"},
			model:     "ollama/mistral:latest",
			want:      false,
		},
		{
			name:      "prefixed blacklist blocks prefixed request",
			blacklist: schemas.BlackList{"ollama/mistral:latest"},
			model:     "ollama/mistral:latest",
			want:      true,
		},
		{
			name:      "different model is not blocked",
			blacklist: schemas.BlackList{"mistral:latest"},
			model:     "llama3.2:latest",
			want:      false,
		},
		{
			name:      "case-insensitive match preserved",
			blacklist: schemas.BlackList{"Ollama/Mistral:Latest"},
			model:     "ollama/mistral:latest",
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.blacklist.IsBlocked(tt.model); got != tt.want {
				t.Fatalf("BlackList.IsBlocked(%v, %q) = %v, want %v", tt.blacklist, tt.model, got, tt.want)
			}
		})
	}
}
