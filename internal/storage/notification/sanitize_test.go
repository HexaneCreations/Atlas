package notification

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeErrorRedactsURLQuerySecrets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, mustNot, must string }{
		{`Post "https://x.logic.azure.com/workflows/1/triggers/manual/paths/invoke?api-version=2016-06-01&sig=abcDEF123": EOF`,
			"abcDEF123", "https://x.logic.azure.com/workflows/1/triggers/manual/paths/invoke?[REDACTED]"},
		{`dial smtp.example.invalid:587: password=hunter2 rejected`, "hunter2", "smtp.example.invalid:587"},
		{`plain failure with no secrets`, "", "plain failure with no secrets"},
	} {
		got := sanitizeError(tc.in)
		if tc.mustNot != "" && strings.Contains(got, tc.mustNot) {
			t.Errorf("sanitizeError(%q) = %q still contains %q", tc.in, got, tc.mustNot)
		}
		if !strings.Contains(got, tc.must) {
			t.Errorf("sanitizeError(%q) = %q, want it to keep %q", tc.in, got, tc.must)
		}
	}
}

func TestSanitizeErrorTruncatesByCharacters(t *testing.T) {
	t.Parallel()
	got := sanitizeError(strings.Repeat("é", 2000))
	if n := utf8.RuneCountInString(got); n != MaxLastErrorLen {
		t.Errorf("length = %d characters, want %d", n, MaxLastErrorLen)
	}
	if !utf8.ValidString(got) {
		t.Error("truncation split a multi-byte character")
	}
	short := "short"
	if sanitizeError(short) != short {
		t.Error("a short message was altered")
	}
}
