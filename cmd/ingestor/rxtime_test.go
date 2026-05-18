package main

import (
	"testing"
	"time"
)

func TestParseEnvelopeTime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"rfc3339 utc", "2026-05-16T10:00:00Z", true},
		{"rfc3339 offset", "2026-05-16T12:00:00+02:00", true},
		{"naive iso", "2026-05-16T10:00:00", true},
		{"naive iso micros", "2026-05-16T10:00:00.123456", true},
		{"garbage", "not-a-time", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseEnvelopeTime(c.in)
			if (err == nil) != c.ok {
				t.Fatalf("parseEnvelopeTime(%q): want ok=%v, got err=%v", c.in, c.ok, err)
			}
		})
	}
}

func TestResolveRxTime(t *testing.T) {
	now := time.Now().UTC()

	mustParse := func(s string) time.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("result %q is not RFC3339: %v", s, err)
		}
		return parsed
	}
	nearNow := func(s string) bool {
		d := mustParse(s).Sub(now)
		if d < 0 {
			d = -d
		}
		return d <= time.Minute
	}

	// Plausible past timestamp (buffered packet) is preserved verbatim.
	rx := now.Add(-5 * time.Hour).Format(time.RFC3339)
	if got := resolveRxTime(map[string]interface{}{"timestamp": rx}, "test"); got != rx {
		t.Errorf("plausible past timestamp: got %q want %q", got, rx)
	}

	// Missing timestamp falls back to ingest time.
	if got := resolveRxTime(map[string]interface{}{}, "test"); !nearNow(got) {
		t.Errorf("missing timestamp: got %q, expected ~now", got)
	}

	// Unparseable timestamp falls back to ingest time.
	if got := resolveRxTime(map[string]interface{}{"timestamp": "garbage"}, "test"); !nearNow(got) {
		t.Errorf("garbage timestamp: got %q, expected ~now", got)
	}

	// Far-future timestamp (>14h ahead) is hard-rejected -> ingest time.
	future := now.Add(48 * time.Hour).Format(time.RFC3339)
	if got := resolveRxTime(map[string]interface{}{"timestamp": future}, "test"); !nearNow(got) {
		t.Errorf("far-future timestamp: got %q, expected ~now (rejected)", got)
	}

	// Naive UTC+N live packet: parsed-as-UTC clock looks a few hours ahead but
	// is within the 14h guard. Soft-clamped to ingest time (not stored future).
	naiveAhead := now.Add(2 * time.Hour).Format(time.RFC3339)
	if got := resolveRxTime(map[string]interface{}{"timestamp": naiveAhead}, "test"); !nearNow(got) {
		t.Errorf("naive UTC+N timestamp: got %q, expected ~now (soft-clamped)", got)
	}
}
