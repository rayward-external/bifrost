package handlers

import (
	"strings"
	"testing"
)

func FuzzFuzzyMatch(f *testing.F) {
	f.Add("gpt-4-turbo", "gpt4")
	f.Add("OpenAI GPT-4o", "g4o")
	f.Add("", "")
	f.Add("provider/model-name", "pmn")

	f.Fuzz(func(t *testing.T, text string, query string) {
		got := fuzzyMatch(text, query)
		want := containsOrderedRunes(strings.ToLower(text), strings.ToLower(query))
		if got != want {
			t.Fatalf("fuzzyMatch(%q, %q) = %t, want %t", text, query, got, want)
		}
	})
}

func containsOrderedRunes(text string, query string) bool {
	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return true
	}

	queryIndex := 0
	for _, textChar := range text {
		if textChar == queryRunes[queryIndex] {
			queryIndex++
			if queryIndex == len(queryRunes) {
				return true
			}
		}
	}

	return false
}
