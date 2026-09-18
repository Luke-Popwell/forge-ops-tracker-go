package forgeops

import "testing"

func TestScrubStringEmail(t *testing.T) {
	got := ScrubString("contact user@example.com for help")
	want := "contact [EMAIL FILTERED] for help"
	if got != want {
		t.Errorf("ScrubString() = %q, want %q", got, want)
	}
}

func TestScrubStringCreditCard(t *testing.T) {
	got := ScrubString("charged card 4242-4242-4242-4242 successfully")
	if got == "charged card 4242-4242-4242-4242 successfully" {
		t.Error("credit card number was not redacted")
	}
}

func TestScrubStringLeavesOrdinaryNumericIdAlone(t *testing.T) {
	text := "order id 1234567890123456"
	if got := ScrubString(text); got != text {
		t.Errorf("ScrubString() = %q, want unchanged %q", got, text)
	}
}

func TestScrubStringSSN(t *testing.T) {
	got := ScrubString("ssn on file: 123-45-6789")
	want := "ssn on file: [SSN FILTERED]"
	if got != want {
		t.Errorf("ScrubString() = %q, want %q", got, want)
	}
}

func TestScrubStringKnownTokenFormats(t *testing.T) {
	// Each fake credential below is built from concatenated pieces, not one contiguous literal:
	// none of these were ever real, but GitHub's push protection flags the shape regardless of
	// context, and a single literal here would block pushing this file anywhere.
	cases := []string{
		"Authorization: Bearer abc123DEF.456-xyz",
		"aws key " + "AKIA" + "ABCDEFGHIJKLMNOP" + " in use",
		"stripe key " + "sk_live_" + "abcdefghijklmnop",
		"github token " + "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dQw4w9WgXcQ",
	}
	for _, text := range cases {
		if got := ScrubString(text); got == text {
			t.Errorf("ScrubString(%q) left the token unredacted", text)
		}
	}
}

func TestScrubValueRedactsWholeValueUnderSensitiveKey(t *testing.T) {
	got := ScrubValue(12345, "apiKey")
	if got != redacted {
		t.Errorf("ScrubValue() = %v, want %q", got, redacted)
	}
}

func TestScrubValueRecursesIntoMapsAndSlices(t *testing.T) {
	input := map[string]any{
		"password": "hunter2",
		"note":     "email me at user@example.com",
		"items": []any{
			map[string]any{"token": "abc"},
			"visit user@example.com",
		},
	}

	got := ScrubValue(input, "").(map[string]any)

	if got["password"] != redacted {
		t.Errorf("password = %v, want %q", got["password"], redacted)
	}
	if got["note"] != "email me at [EMAIL FILTERED]" {
		t.Errorf("note = %v", got["note"])
	}
	items := got["items"].([]any)
	if items[0].(map[string]any)["token"] != redacted {
		t.Errorf("items[0].token = %v, want %q", items[0], redacted)
	}
	if items[1] != "visit [EMAIL FILTERED]" {
		t.Errorf("items[1] = %v", items[1])
	}
}

func TestIsSensitiveKeyIgnoresCaseAndPunctuation(t *testing.T) {
	for _, key := range []string{"API_KEY", "Api-Key", "apiKey", "X-Api-Key"} {
		if !isSensitiveKey(key) {
			t.Errorf("isSensitiveKey(%q) = false, want true", key)
		}
	}
	if isSensitiveKey("username") {
		t.Error("isSensitiveKey(\"username\") = true, want false")
	}
}
