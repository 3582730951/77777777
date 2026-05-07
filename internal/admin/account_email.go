package admin

import (
	"encoding/json"
	"strings"
)

func accountEmailFromImport(explicit, session string) string {
	if email := strings.TrimSpace(explicit); email != "" {
		return email
	}
	return extractSessionEmail(session)
}

func extractSessionEmail(session string) string {
	session = strings.TrimSpace(session)
	if !strings.HasPrefix(session, "{") {
		return ""
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(session), &raw); err != nil {
		return ""
	}
	for _, path := range [][]string{
		{"email"},
		{"email_address"},
		{"user", "email"},
		{"account", "email"},
		{"account", "email_address"},
	} {
		if email := stringAtPath(raw, path...); email != "" {
			return email
		}
	}
	return ""
}

func stringAtPath(raw map[string]any, path ...string) string {
	var cur any = raw
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	s, _ := cur.(string)
	return strings.TrimSpace(s)
}
