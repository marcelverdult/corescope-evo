package main

import (
	"os"
	"path/filepath"
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
