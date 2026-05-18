package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// SiteTemplate is a named bundle of branding/optical/help overrides loaded from
// templates/<name>/template.json. Maps are nil when absent; the merge in
// buildThemeResponse() treats nil overlays as no-ops.
type SiteTemplate struct {
	Name       string                 `json:"name"`
	Label      string                 `json:"label"`
	Branding   map[string]interface{} `json:"branding"`
	Meta       map[string]interface{} `json:"meta"`
	Theme      map[string]interface{} `json:"theme"`
	ThemeDark  map[string]interface{} `json:"themeDark"`
	NodeColors map[string]interface{} `json:"nodeColors"`
	TypeColors map[string]interface{} `json:"typeColors"`
	Home       map[string]interface{} `json:"home"`
	Sections   map[string]interface{} `json:"sections"`

	// Dir is the resolved templates/<name> directory. Empty for the built-in
	// fallback. Used to serve templates/<name>/assets/ at /template-assets/.
	Dir string `json:"-"`
}

// LoadTemplate loads templates/<name>/template.json from the first baseDir that
// has it. Unknown name, missing file, or malformed JSON falls back to "default"
// (and a bare {Name:"default"} if even default is absent). Never returns nil.
func LoadTemplate(name string, baseDirs ...string) *SiteTemplate {
	if name == "" {
		name = "default"
	}
	if len(baseDirs) == 0 {
		baseDirs = []string{"."}
	}
	for _, d := range baseDirs {
		dir := filepath.Join(d, "templates", name)
		data, err := os.ReadFile(filepath.Join(dir, "template.json"))
		if err != nil {
			continue
		}
		var t SiteTemplate
		if err := json.Unmarshal(data, &t); err != nil {
			log.Printf("[template] WARNING: template %q is malformed (%v) — falling back to default", name, err)
			continue
		}
		if t.Name == "" {
			t.Name = name
		}
		t.Dir = dir
		return &t
	}
	if name != "default" {
		log.Printf("[template] WARNING: template %q not found — falling back to default", name)
		return LoadTemplate("default", baseDirs...)
	}
	return &SiteTemplate{Name: "default"}
}
