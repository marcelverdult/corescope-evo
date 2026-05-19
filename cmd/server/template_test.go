package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemplate(t *testing.T, base, name, json string) {
	t.Helper()
	dir := filepath.Join(base, "templates", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "template.json"), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTemplateValid(t *testing.T) {
	base := t.TempDir()
	writeTemplate(t, base, "acme", `{"name":"acme","branding":{"siteName":"Acme"}}`)
	tpl := LoadTemplate("acme", base)
	if tpl.Name != "acme" {
		t.Errorf("Name = %q, want acme", tpl.Name)
	}
	if tpl.Branding["siteName"] != "Acme" {
		t.Errorf("Branding[siteName] = %v, want Acme", tpl.Branding["siteName"])
	}
}

func TestLoadTemplateMissingFallsBackToDefault(t *testing.T) {
	base := t.TempDir()
	writeTemplate(t, base, "default", `{"name":"default"}`)
	tpl := LoadTemplate("does-not-exist", base)
	if tpl.Name != "default" {
		t.Errorf("Name = %q, want default fallback", tpl.Name)
	}
}

func TestLoadTemplateMalformedFallsBackToDefault(t *testing.T) {
	base := t.TempDir()
	writeTemplate(t, base, "default", `{"name":"default"}`)
	writeTemplate(t, base, "broken", `{not valid json`)
	tpl := LoadTemplate("broken", base)
	if tpl.Name != "default" {
		t.Errorf("Name = %q, want default fallback on malformed JSON", tpl.Name)
	}
}

func TestLoadTemplateEmptyNameIsDefault(t *testing.T) {
	base := t.TempDir()
	writeTemplate(t, base, "default", `{"name":"default"}`)
	tpl := LoadTemplate("", base)
	if tpl.Name != "default" {
		t.Errorf("Name = %q, want default for empty name", tpl.Name)
	}
}

func TestLoadTemplateNoFilesReturnsEmptyDefault(t *testing.T) {
	tpl := LoadTemplate("default", t.TempDir())
	if tpl.Name != "default" {
		t.Errorf("Name = %q, want default", tpl.Name)
	}
}

// Fix 2: Dir is set on a successful load.
func TestLoadTemplateDirIsPopulated(t *testing.T) {
	base := t.TempDir()
	writeTemplate(t, base, "acme", `{"name":"acme"}`)
	tpl := LoadTemplate("acme", base)
	if tpl.Dir == "" {
		t.Fatal("Dir must be non-empty on successful load")
	}
	wantSuffix := filepath.Join("templates", "acme")
	if !filepath.IsAbs(tpl.Dir) {
		t.Errorf("Dir %q should be absolute", tpl.Dir)
	}
	if base := filepath.Base(filepath.Dir(tpl.Dir)); base != "templates" {
		// simpler: check the last two path components
		if got := filepath.Join(filepath.Base(filepath.Dir(tpl.Dir)), filepath.Base(tpl.Dir)); got != wantSuffix {
			t.Errorf("Dir ends with %q, want %q", got, wantSuffix)
		}
	}
}

// Fix 3a: First baseDir wins when the template exists in multiple baseDirs.
func TestLoadTemplateFirstBaseDirWins(t *testing.T) {
	base1 := t.TempDir()
	base2 := t.TempDir()
	writeTemplate(t, base1, "brand", `{"name":"brand","label":"First"}`)
	writeTemplate(t, base2, "brand", `{"name":"brand","label":"Second"}`)
	tpl := LoadTemplate("brand", base1, base2)
	if tpl.Label != "First" {
		t.Errorf("Label = %q, want First (first baseDir should win)", tpl.Label)
	}
}

// Fix 3b: A malformed template.json in the first baseDir falls through to a
// valid copy in a later baseDir (verifies the break→continue fix).
func TestLoadTemplateMalformedFirstBaseDirFallsThroughToValid(t *testing.T) {
	base1 := t.TempDir()
	base2 := t.TempDir()
	writeTemplate(t, base1, "brand", `{not valid json`)
	writeTemplate(t, base2, "brand", `{"name":"brand","label":"Valid"}`)
	tpl := LoadTemplate("brand", base1, base2)
	if tpl.Name != "brand" {
		t.Errorf("Name = %q, want brand (should have loaded from base2)", tpl.Name)
	}
	if tpl.Label != "Valid" {
		t.Errorf("Label = %q, want Valid", tpl.Label)
	}
}

func TestLoadTemplateReadsNav(t *testing.T) {
	base := t.TempDir()
	writeTemplate(t, base, "withnav",
		`{"name":"withnav","nav":[{"route":"home","hash":"#/home","label":"Home","priority":"high"}]}`)
	tpl := LoadTemplate("withnav", base)
	if len(tpl.Nav) != 1 {
		t.Fatalf("Nav length = %d, want 1", len(tpl.Nav))
	}
	if tpl.Nav[0]["label"] != "Home" || tpl.Nav[0]["hash"] != "#/home" {
		t.Errorf("Nav[0] = %v, want home entry", tpl.Nav[0])
	}
}

func TestBuildNavLinksDefaultWhenNoTemplateNav(t *testing.T) {
	out := buildNavLinks(ThemeResponse{})
	if out != defaultNavLinks {
		t.Errorf("empty Nav must yield defaultNavLinks")
	}
	if !strings.Contains(out, "🔴 Live") || !strings.Contains(out, ">Tools<") {
		t.Errorf("default nav missing expected links: %s", out)
	}
	// An all-invalid Nav (no hash/label) also falls back to the default.
	if buildNavLinks(ThemeResponse{Nav: []map[string]interface{}{{"route": "x"}}}) != defaultNavLinks {
		t.Errorf("Nav with no valid entries must fall back to defaultNavLinks")
	}
}

func TestBuildNavLinksFromTemplate(t *testing.T) {
	tr := ThemeResponse{Nav: []map[string]interface{}{
		{"route": "home", "hash": "#/home", "label": "Home", "priority": "high"},
		{"route": "live", "hash": "#/live", "label": "Live"},
		{"label": "skipped — no hash"},
	}}
	out := buildNavLinks(tr)
	if !strings.Contains(out, `<a href="#/home" class="nav-link" data-route="home" data-priority="high">Home</a>`) {
		t.Errorf("missing home link, got: %s", out)
	}
	if !strings.Contains(out, `<a href="#/live" class="nav-link" data-route="live">Live</a>`) {
		t.Errorf("missing live link (priority omitted), got: %s", out)
	}
	if strings.Contains(out, "🔴") {
		t.Errorf("template nav must not carry the default red dot")
	}
	if strings.Contains(out, "skipped") {
		t.Errorf("entries without a hash must be skipped")
	}
}
