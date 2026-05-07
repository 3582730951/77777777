package admin

import "testing"

func TestAccountEmailFromImport(t *testing.T) {
	session := `{"user":{"email":"session@example.com"}}`
	if got := accountEmailFromImport("explicit@example.com", session); got != "explicit@example.com" {
		t.Fatalf("explicit email was not preferred: %q", got)
	}
	if got := accountEmailFromImport("", session); got != "session@example.com" {
		t.Fatalf("session email was not extracted: %q", got)
	}
}

func TestExtractSessionEmailFallbackPaths(t *testing.T) {
	for name, session := range map[string]string{
		"top-level": `{"email":"top@example.com"}`,
		"account":   `{"account":{"email_address":"account@example.com"}}`,
	} {
		if got := extractSessionEmail(session); got == "" {
			t.Fatalf("%s email was not extracted", name)
		}
	}
}
