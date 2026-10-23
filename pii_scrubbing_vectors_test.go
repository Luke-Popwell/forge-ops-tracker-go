package forgeops

import (
	"encoding/json"
	"os"
	"testing"
)

// piiScrubbingVectors is testdata/pii_scrubbing_vectors.json, a byte-identical copy of this repo's
// sdks/pii_scrubbing_vectors.json, generated from the server's own scrubber (see
// sdks/PII_SCRUBBING.md).
type piiScrubbingVectors struct {
	Strings []struct {
		Input    string `json:"input"`
		Scrubbed string `json:"scrubbed"`
	} `json:"strings"`
	Keys []struct {
		Key      string `json:"key"`
		Redacted bool   `json:"redacted"`
	} `json:"keys"`
}

func loadPIIScrubbingVectors(t *testing.T) piiScrubbingVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/pii_scrubbing_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors piiScrubbingVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Strings) == 0 || len(vectors.Keys) == 0 {
		t.Fatal("vectors file is missing a section")
	}
	return vectors
}

func TestPIIScrubbingStringVectors(t *testing.T) {
	for _, vector := range loadPIIScrubbingVectors(t).Strings {
		if got := ScrubString(vector.Input); got != vector.Scrubbed {
			t.Errorf("ScrubString(%q) = %q, want %q", vector.Input, got, vector.Scrubbed)
		}
	}
}

func TestPIIScrubbingKeyVectors(t *testing.T) {
	for _, vector := range loadPIIScrubbingVectors(t).Keys {
		scrubbed := ScrubValue(map[string]any{vector.Key: "x"}, "").(map[string]any)
		if got := scrubbed[vector.Key] == redacted; got != vector.Redacted {
			t.Errorf("key %q redacted = %v, want %v", vector.Key, got, vector.Redacted)
		}
	}
}

func TestPIIScrubbingBacksOffToTheLastSeparatorBeforeAWordCharacter(t *testing.T) {
	cases := map[string]string{
		"4242424242424242 123x":  "[CREDIT CARD FILTERED] 123x",
		"x4242 4242424242424242": "x4242 [CREDIT CARD FILTERED]",
		"4242424242424242x":      "4242424242424242x",
	}
	for input, want := range cases {
		if got := ScrubString(input); got != want {
			t.Errorf("ScrubString(%q) = %q, want %q", input, got, want)
		}
	}
}
