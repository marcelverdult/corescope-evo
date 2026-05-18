# Config-Activatable Branding Templates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `template` config key that activates a named bundle of branding, optical, and help customizations; ship a `cornmeister` template reproducing the Cornmeister fork look.

**Architecture:** A template is `templates/<name>/template.json` + an `assets/` directory. The server loads the named template at startup and merges its maps into the existing `buildThemeResponse()` overlay chain (`built-in defaults < template < config.json < theme.json`). Title/OG meta inject server-side via an `__SITE_META__` placeholder in `index.html`. Structural home-page blocks (donate, announcement, first-visit chooser) and an observer-setup help block render client-side, gated on `window.SITE_CONFIG.sections.*.enabled`.

**Tech Stack:** Go 1.x (`cmd/server`), vanilla JS (`public/`), protobuf contract doc validated by `tools/validate-protos.py`, Playwright e2e (`test-*.js` run via `node`).

**Spec:** `docs/superpowers/specs/2026-05-18-config-templates-design.md`

## Refinements vs. spec (discovered during planning)

- The `default` template is **minimal** (`{"name":"default"}`), not a verbatim copy of current values. Built-in Go defaults in `buildThemeResponse()` stay the single source of truth; an empty `default` template contributes nothing, so `template` unset and `template:"default"` are identical. Avoids two sources of truth.
- `observers.js` has **no** existing MQTT/setup help block (it is a status table only). The Cornmeister observer-setup help is therefore a **net-new config-gated block**, not a de-hardcoding (Task 12).
- Logo: no new `__LOGO__` placeholder. The existing `branding.logoUrl` → `<img>` swap (`customize-v2.js:606`) already works end-to-end; the `cornmeister` template supplies `logoUrl`. The built-in inline SVG remains the default. One minor client-side logo swap on load is accepted.
- Meta is injected via a single `__SITE_META__` placeholder (whole title+meta block), not one placeholder per tag.

## File Structure

**New files:**
- `cmd/server/template.go` — `SiteTemplate` struct + `LoadTemplate()` loader + asset-path resolution.
- `cmd/server/template_test.go` — loader unit tests.
- `templates/default/template.json` — minimal default template.
- `templates/cornmeister/template.json` — Cornmeister branding bundle.
- `templates/cornmeister/assets/logo.svg` — Cornmeister logo (extracted from fork).
- `templates/cornmeister/assets/powered-by-dutchmeshcore.png` — donate image (copied from fork).
- `test-config-templates.js` — e2e test.

**Modified files:**
- `cmd/server/config.go` — add `Template`, `Meta`, `Sections` fields to `Config`.
- `cmd/server/routes.go` — `Server.tmpl` field; template merge in `buildThemeResponse()`; `/template-assets/` route.
- `cmd/server/types.go` — add `Meta`, `Sections` to `ThemeResponse`.
- `cmd/server/main.go` — load template at startup; `__SITE_META__` injection in `spaHandler`.
- `public/index.html` — replace static title/meta block with `__SITE_META__`.
- `public/customize-v2.js` — pass `meta`/`sections` through to `window.SITE_CONFIG`.
- `public/home.js` — render donate, announcement, first-visit-chooser sections.
- `public/observers.js` — render config-gated setup-help block.
- `public/channels.js`, `public/nodes.js`, `public/app.js` — source site name / repo URL from `SITE_CONFIG`.
- `proto/config.proto` — `template` field + `ThemeResponse` additions.
- `config.example.json`, `docs/user-guide/configuration.md` — document the key.

---

# Phase 1 — Backend infrastructure

### Task 1: Add `template` / `meta` / `sections` fields to Config

**Files:**
- Modify: `cmd/server/config.go:32-37`
- Test: `cmd/server/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/server/config_test.go`:

```go
func TestConfigParsesTemplateField(t *testing.T) {
	dir := t.TempDir()
	js := `{"port":3000,"template":"cornmeister",
	"meta":{"title":"X"},"sections":{"donate":{"enabled":true}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Template != "cornmeister" {
		t.Errorf("Template = %q, want cornmeister", cfg.Template)
	}
	if cfg.Meta["title"] != "X" {
		t.Errorf("Meta[title] = %v, want X", cfg.Meta["title"])
	}
	if cfg.Sections["donate"] == nil {
		t.Errorf("Sections[donate] missing")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestConfigParsesTemplateField -v`
Expected: FAIL — `cfg.Template undefined` (compile error).

- [ ] **Step 3: Add the fields**

In `cmd/server/config.go`, in the `Config` struct after the `Home` field (line 37):

```go
	Branding   map[string]interface{} `json:"branding"`
	Theme      map[string]interface{} `json:"theme"`
	ThemeDark  map[string]interface{} `json:"themeDark"`
	NodeColors map[string]interface{} `json:"nodeColors"`
	TypeColors map[string]interface{} `json:"typeColors"`
	Home       map[string]interface{} `json:"home"`

	// Template is the name of a bundled branding template under templates/.
	// Empty or "default" = built-in look. See cmd/server/template.go.
	Template string                 `json:"template,omitempty"`
	// Meta overrides <title>/OpenGraph tags. Merged below template, above defaults.
	Meta     map[string]interface{} `json:"meta,omitempty"`
	// Sections toggles structural home-page blocks (donate, announcement, etc.).
	Sections map[string]interface{} `json:"sections,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestConfigParsesTemplateField -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/config.go cmd/server/config_test.go
git commit -m "feat(config): add template/meta/sections config fields"
```

---

### Task 2: Template loader

**Files:**
- Create: `cmd/server/template.go`
- Test: `cmd/server/template_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/server/template_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestLoadTemplate -v`
Expected: FAIL — `LoadTemplate undefined`.

- [ ] **Step 3: Write the loader**

Create `cmd/server/template.go`:

```go
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
			break
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestLoadTemplate -v`
Expected: PASS (all 5 subtests).

- [ ] **Step 5: Commit**

```bash
git add cmd/server/template.go cmd/server/template_test.go
git commit -m "feat(server): add branding template loader"
```

---

### Task 3: Merge template into buildThemeResponse, extend ThemeResponse

**Files:**
- Modify: `cmd/server/routes.go:25-76` (add `tmpl` field), `cmd/server/routes.go:393-499` (`buildThemeResponse`)
- Modify: `cmd/server/types.go:979-986` (`ThemeResponse`)
- Test: `cmd/server/routes_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/server/routes_test.go`:

```go
func TestBuildThemeResponseAppliesTemplate(t *testing.T) {
	s := &Server{
		cfg: &Config{},
		tmpl: &SiteTemplate{
			Name:     "acme",
			Branding: map[string]interface{}{"siteName": "Acme Mesh"},
			Meta:     map[string]interface{}{"title": "Acme"},
			Sections: map[string]interface{}{"donate": map[string]interface{}{"enabled": true}},
		},
	}
	tr := s.buildThemeResponse()
	if tr.Branding["siteName"] != "Acme Mesh" {
		t.Errorf("siteName = %v, want Acme Mesh", tr.Branding["siteName"])
	}
	if tr.Meta["title"] != "Acme" {
		t.Errorf("meta.title = %v, want Acme", tr.Meta["title"])
	}
	if tr.Sections["donate"] == nil {
		t.Errorf("sections.donate missing")
	}
}

func TestBuildThemeResponseConfigOverridesTemplate(t *testing.T) {
	s := &Server{
		cfg:  &Config{Branding: map[string]interface{}{"siteName": "Operator Override"}},
		tmpl: &SiteTemplate{Name: "acme", Branding: map[string]interface{}{"siteName": "Acme Mesh"}},
	}
	tr := s.buildThemeResponse()
	if tr.Branding["siteName"] != "Operator Override" {
		t.Errorf("siteName = %v, want Operator Override (config beats template)", tr.Branding["siteName"])
	}
}

func TestBuildThemeResponseNilTemplateUsesDefaults(t *testing.T) {
	s := &Server{cfg: &Config{}, tmpl: &SiteTemplate{Name: "default"}}
	tr := s.buildThemeResponse()
	if tr.Branding["siteName"] != "CoreScope" {
		t.Errorf("siteName = %v, want built-in default CoreScope", tr.Branding["siteName"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestBuildThemeResponse -v`
Expected: FAIL — `Server has no field tmpl`, `ThemeResponse has no field Meta`.

- [ ] **Step 3: Add `tmpl` to Server struct**

In `cmd/server/routes.go`, in `type Server struct` after the `cfg` field (line 27):

```go
	db        *DB
	cfg       *Config
	tmpl      *SiteTemplate // active branding template (never nil after startup)
	hub       *Hub
```

- [ ] **Step 4: Extend ThemeResponse**

In `cmd/server/types.go`, replace the `ThemeResponse` struct (lines 979-986):

```go
type ThemeResponse struct {
	Branding   map[string]interface{} `json:"branding"`
	Theme      map[string]interface{} `json:"theme"`
	ThemeDark  map[string]interface{} `json:"themeDark"`
	NodeColors map[string]interface{} `json:"nodeColors"`
	TypeColors map[string]interface{} `json:"typeColors"`
	Home       interface{}            `json:"home"`
	Meta       map[string]interface{} `json:"meta"`
	Sections   map[string]interface{} `json:"sections"`
	Template   string                 `json:"template"`
}
```

- [ ] **Step 5: Merge the template in buildThemeResponse**

In `cmd/server/routes.go`, in `buildThemeResponse()`: first guard against a nil `tmpl` (defensive for tests that build a `Server` without one), then insert `s.tmpl.*` as the overlay **after** the built-in default map and **before** `s.cfg.*` in each `mergeMap` call. Add this at the top of the function (after `theme := LoadTheme(".")`, line 394):

```go
	tmpl := s.tmpl
	if tmpl == nil {
		tmpl = &SiteTemplate{Name: "default"}
	}
```

Then change the four existing `mergeMap` calls so the template map sits between defaults and config. Branding (line 396):

```go
	branding := mergeMap(map[string]interface{}{
		"siteName": "CoreScope",
		"tagline":  "Real-time MeshCore LoRa mesh network analyzer",
	}, tmpl.Branding, s.cfg.Branding, theme.Branding)
```

Theme colors (line 401) — append `tmpl.Theme` before `s.cfg.Theme`:

```go
	}, tmpl.Theme, s.cfg.Theme, theme.Theme)
```

Node colors (line 428):

```go
	}, tmpl.NodeColors, s.cfg.NodeColors, theme.NodeColors)
```

themeDark (line 436):

```go
	}, tmpl.ThemeDark, s.cfg.ThemeDark, theme.ThemeDark)
```

typeColors (line 462):

```go
	}, tmpl.TypeColors, s.cfg.TypeColors, theme.TypeColors)
```

home (line 489):

```go
	home := mergeMap(defaultHome, tmpl.Home, s.cfg.Home, theme.Home)
```

Then add `meta` and `sections` before the `return`, and extend the returned struct (lines 491-498):

```go
	meta := mergeMap(map[string]interface{}{
		"title":       "CoreScope-EVO",
		"description": "Real-time MeshCore LoRa mesh network analyzer",
		"repoUrl":     "https://github.com/marcelverdult/corescope-evo",
		"themeColor":  "#0a0a0a",
	}, tmpl.Meta, s.cfg.Meta)

	sections := mergeMap(map[string]interface{}{}, tmpl.Sections, s.cfg.Sections)

	return ThemeResponse{
		Branding:   branding,
		Theme:      themeColors,
		ThemeDark:  themeDark,
		NodeColors: nodeColors,
		TypeColors: typeColors,
		Home:       home,
		Meta:       meta,
		Sections:   sections,
		Template:   tmpl.Name,
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./cmd/server/ -run TestBuildThemeResponse -v`
Expected: PASS (all 3 subtests).

- [ ] **Step 7: Run the full server test package for regressions**

Run: `go test ./cmd/server/ 2>&1 | tail -20`
Expected: PASS — no regressions in existing theme/route tests.

- [ ] **Step 8: Commit**

```bash
git add cmd/server/routes.go cmd/server/types.go cmd/server/routes_test.go
git commit -m "feat(server): merge active template into theme response"
```

---

### Task 4: Load template at startup, serve `/template-assets/`

**Files:**
- Modify: `cmd/server/main.go:95-110` (load template), `cmd/server/main.go:357-361` (asset route)
- Test: `cmd/server/main.go` change is verified by Task 15 e2e + a route test below.

- [ ] **Step 1: Write the failing test**

Add to `cmd/server/routes_test.go`:

```go
func TestTemplateAssetsRouteServesFiles(t *testing.T) {
	base := t.TempDir()
	assetsDir := filepath.Join(base, "templates", "acme", "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetsDir, "logo.svg"), []byte("<svg/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &Config{}, tmpl: &SiteTemplate{Name: "acme", Dir: filepath.Join(base, "templates", "acme")}}
	router := mux.NewRouter()
	s.registerTemplateAssets(router)

	req := httptest.NewRequest("GET", "/template-assets/logo.svg", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "<svg/>" {
		t.Errorf("body = %q, want <svg/>", rr.Body.String())
	}
}
```

(Ensure `net/http/httptest` and `github.com/gorilla/mux` are imported in the test file — they are already used elsewhere in `cmd/server`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestTemplateAssetsRoute -v`
Expected: FAIL — `s.registerTemplateAssets undefined`.

- [ ] **Step 3: Add the asset route helper**

In `cmd/server/template.go`, add:

```go
import "net/http" // add to the existing import block

// registerTemplateAssets serves the active template's assets/ directory at
// /template-assets/. No-op when the template has no resolved Dir (built-in
// fallback). Must be registered before the catch-all static handler.
func (s *Server) registerTemplateAssets(r *mux.Router) {
	if s.tmpl == nil || s.tmpl.Dir == "" {
		return
	}
	assetsDir := filepath.Join(s.tmpl.Dir, "assets")
	if _, err := os.Stat(assetsDir); err != nil {
		return
	}
	fs := http.FileServer(http.Dir(assetsDir))
	r.PathPrefix("/template-assets/").Handler(
		http.StripPrefix("/template-assets/", fs))
}
```

Add `"github.com/gorilla/mux"` to the import block of `template.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestTemplateAssetsRoute -v`
Expected: PASS

- [ ] **Step 5: Load the template at startup and register the route**

In `cmd/server/main.go`, immediately after the CLI-flag override block (after line 110, after `cfg.DBPath = dbPath`), add:

```go
	// Load the active branding template (templates/<name>/template.json).
	tmpl := LoadTemplate(cfg.Template, configDir, ".")
	log.Printf("[template] active template: %s", tmpl.Name)
```

Find where the `Server` struct is constructed in `main.go` (the `srv` / `&Server{...}` literal that sets `cfg:`), and add `tmpl: tmpl,` next to `cfg: cfg,`.

Then, in `cmd/server/main.go` around line 357, register the asset route **before** the catch-all `PathPrefix("/")`:

```go
	srv.registerTemplateAssets(router)
	absPublic, _ := filepath.Abs(publicDir)
	if _, err := os.Stat(absPublic); err == nil {
		fs := http.FileServer(http.Dir(absPublic))
		router.PathPrefix("/").Handler(wsOrStatic(hub, srv.spaHandler(absPublic, fs)))
```

- [ ] **Step 6: Build and smoke-test**

Run: `go build ./cmd/server/ && echo BUILD_OK`
Expected: `BUILD_OK`

- [ ] **Step 7: Commit**

```bash
git add cmd/server/template.go cmd/server/main.go cmd/server/routes_test.go
git commit -m "feat(server): load active template at startup, serve /template-assets/"
```

---

### Task 5: Inject title/OG meta server-side via `__SITE_META__`

**Files:**
- Modify: `public/index.html:8-32` (replace static block), `cmd/server/main.go:613-705` (add `buildSiteMetaTag`, substitute in `spaHandler`)
- Test: `cmd/server/helpers_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/server/helpers_test.go`:

```go
func TestSpaHandlerInjectsSiteMeta(t *testing.T) {
	dir := t.TempDir()
	html := `<!DOCTYPE html><html><head>__SITE_META__</head><body></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(html), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:  &Config{},
		tmpl: &SiteTemplate{Name: "acme", Meta: map[string]interface{}{"title": "Acme Mesh", "description": "d"}},
	}
	fs := http.FileServer(http.Dir(dir))
	handler := s.spaHandler(dir, fs)
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "<title>Acme Mesh</title>") {
		t.Errorf("title not injected, body=%q", body)
	}
	if strings.Contains(body, "__SITE_META__") {
		t.Errorf("placeholder not replaced")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestSpaHandlerInjectsSiteMeta -v`
Expected: FAIL — placeholder not replaced (still `__SITE_META__` in body).

- [ ] **Step 3: Add `buildSiteMetaTag` in main.go**

In `cmd/server/main.go`, after `buildThemeStyleTag` (after line 662), add:

```go
// metaStr safely reads a string value from a meta map, returning fallback when
// absent or when the value contains characters that could break out of an HTML
// attribute or tag context.
func metaStr(m map[string]interface{}, key, fallback string) string {
	raw, ok := m[key]
	if !ok {
		return fallback
	}
	s, ok := raw.(string)
	if !ok || s == "" || strings.ContainsAny(s, "<>\"\n\r") {
		return fallback
	}
	return s
}

// buildSiteMetaTag renders the <title> and OpenGraph/Twitter meta tags from the
// active template/config meta map. Replaces the static __SITE_META__ placeholder
// in index.html so social crawlers see template-specific values.
func buildSiteMetaTag(tr ThemeResponse) string {
	m := tr.Meta
	if m == nil {
		m = map[string]interface{}{}
	}
	title := metaStr(m, "title", "CoreScope-EVO")
	desc := metaStr(m, "description", "Real-time MeshCore LoRa mesh network analyzer")
	ogImage := metaStr(m, "ogImage", "")
	ogURL := metaStr(m, "ogUrl", "")
	themeColor := metaStr(m, "themeColor", "#0a0a0a")
	var b strings.Builder
	b.WriteString("<title>" + title + "</title>")
	b.WriteString(`<meta name="description" content="` + desc + `">`)
	b.WriteString(`<meta property="og:title" content="` + title + `">`)
	b.WriteString(`<meta property="og:description" content="` + desc + `">`)
	if ogImage != "" {
		b.WriteString(`<meta property="og:image" content="` + ogImage + `">`)
		b.WriteString(`<meta name="twitter:image" content="` + ogImage + `">`)
	}
	if ogURL != "" {
		b.WriteString(`<meta property="og:url" content="` + ogURL + `">`)
	}
	b.WriteString(`<meta property="og:type" content="website">`)
	b.WriteString(`<meta name="twitter:card" content="summary_large_image">`)
	b.WriteString(`<meta name="twitter:title" content="` + title + `">`)
	b.WriteString(`<meta name="twitter:description" content="` + desc + `">`)
	b.WriteString(`<meta name="theme-color" content="` + themeColor + `">`)
	return b.String()
}
```

- [ ] **Step 4: Substitute the placeholder in spaHandler**

In `cmd/server/main.go` `spaHandler`, after the `__THEME_STYLE__` replacement (line 678), add:

```go
	processed = strings.ReplaceAll(processed, "__THEME_STYLE__", buildThemeStyleTag(s.buildThemeResponse()))
	processed = strings.ReplaceAll(processed, "__SITE_META__", buildSiteMetaTag(s.buildThemeResponse()))
```

- [ ] **Step 5: Update index.html**

In `public/index.html`, delete the static `<title>` tag and every OpenGraph (`og:*`), Twitter (`twitter:*`), `theme-color`, and `description` `<meta>` tag, replacing the whole group with a single placeholder line:

```html
    __SITE_META__
```

Leave untouched: `<meta charset>`, `<meta name="viewport">`, `<link rel="icon">`, any `<link rel="stylesheet">`, the `__THEME_STYLE__` placeholder (line 45), and `__BUST__`. Edit by tag identity, not line numbers — do not delete non-meta tags that may sit between the title and the OG block.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestSpaHandlerInjectsSiteMeta -v`
Expected: PASS

- [ ] **Step 7: Run full server tests for regressions**

Run: `go test ./cmd/server/ 2>&1 | tail -20`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add cmd/server/main.go public/index.html cmd/server/helpers_test.go
git commit -m "feat(server): inject template title/OG meta into index.html"
```

---

### Task 6: Sync protobuf contract

**Files:**
- Modify: `proto/config.proto:50-64`
- Verify: `tools/validate-protos.py`

- [ ] **Step 1: Add fields to the proto contract**

In `proto/config.proto`, in `message ThemeResponse` (lines 50-64), after the `home_json` field add:

```proto
  // Site metadata (title, description, OpenGraph) — opaque object.
  optional string meta_json = 7 [json_name = "meta"];
  // Structural section toggles (donate, announcement, chooser) — opaque object.
  optional string sections_json = 8 [json_name = "sections"];
  // Active template name.
  string template = 9;
```

- [ ] **Step 2: Run the proto validator**

Run: `python3 tools/validate-protos.py 2>&1 | tail -15`
Expected: PASS. If it reports a missing/extra field for `config-theme.json` ↔ `ThemeResponse`, reconcile the proto field `json_name` values with the Go `ThemeResponse` JSON tags from Task 3 until it passes.

- [ ] **Step 3: Commit**

```bash
git add proto/config.proto
git commit -m "docs(proto): document template fields on ThemeResponse"
```

---

# Phase 2 — Bundled templates

### Task 7: Create the `default` template

**Files:**
- Create: `templates/default/template.json`

- [ ] **Step 1: Create the minimal default template**

Create `templates/default/template.json`:

```json
{
  "name": "default",
  "label": "CoreScope-EVO (default)"
}
```

It is intentionally minimal — built-in Go defaults remain the source of truth, so an empty `default` template produces zero behavior change.

- [ ] **Step 2: Verify the server loads it cleanly**

Run: `go test ./cmd/server/ -run TestLoadTemplate -v`
Expected: PASS (no change, sanity check the package still builds).

- [ ] **Step 3: Commit**

```bash
git add templates/default/template.json
git commit -m "feat(templates): add minimal default template"
```

---

### Task 8: Create the `cornmeister` template

The Cornmeister fork lives at `/Users/verdi/Projects/corescope/CoreScope_Cornmeister`. This task extracts its branding values into a template bundle. **Read the fork files cited below to source exact values — do not invent them.**

**Files:**
- Create: `templates/cornmeister/template.json`
- Create: `templates/cornmeister/assets/logo.svg`
- Create: `templates/cornmeister/assets/powered-by-dutchmeshcore.png`

- [ ] **Step 1: Copy the donate image asset**

```bash
mkdir -p templates/cornmeister/assets
cp /Users/verdi/Projects/corescope/CoreScope_Cornmeister/public/img/powered-by-dutchmeshcore.png \
   templates/cornmeister/assets/powered-by-dutchmeshcore.png
```

- [ ] **Step 2: Extract the Cornmeister logo to a standalone SVG**

Open `/Users/verdi/Projects/corescope/CoreScope_Cornmeister/public/index.html` and locate the `nav-brand` inline `<svg>` (the blue concentric-arc logo using gradient id `cornmeisterNavGrad`, colors `#3b82f6`→`#1d4ed8`). Copy that `<svg>...</svg>` markup verbatim into a new standalone file `templates/cornmeister/assets/logo.svg`, prepending `<?xml version="1.0" encoding="UTF-8"?>`. Keep the inline `<defs>`/`<linearGradient>` and any `<animate>` elements so the animation survives as a standalone SVG. Set the root `<svg>` `width`/`height` to the fork's nav logo dimensions.

- [ ] **Step 3: Author template.json**

Create `templates/cornmeister/template.json`. Fill each value from the cited Cornmeister source:

```json
{
  "name": "cornmeister",
  "label": "Cornmeister.nl (Dutch mesh)",
  "branding": {
    "siteName": "CORNMEISTER.NL",
    "tagline": "Dutch mesh analyzer",
    "logoUrl": "/template-assets/logo.svg"
  },
  "meta": {
    "title": "Cornmeister.nl",
    "description": "<from CoreScope_Cornmeister/public/index.html og:description>",
    "ogImage": "<from fork index.html og:image, or a /template-assets/ path>",
    "ogUrl": "https://cornmeister.nl",
    "themeColor": "#1d4ed8",
    "repoUrl": "https://github.com/Cornmeister/corescope"
  },
  "home": {
    "heroTitle": "Cornmeister.nl",
    "heroSubtitle": "<from fork home.js hero text>",
    "steps": [ "<port the Dutch 868 MHz setup steps from fork home.js checklist()>" ],
    "checklist": [ "<port the Dutch FAQ items from fork home.js>" ],
    "footerLinks": [ "<port from fork home.js footer>" ]
  },
  "sections": {
    "donate": {
      "enabled": true,
      "title": "<from fork home.js home-donate section>",
      "image": "/template-assets/powered-by-dutchmeshcore.png",
      "links": [
        { "label": "❤️ Support us!", "url": "https://bunq.me/CornmeisterNL" },
        { "label": "💬 Discord", "url": "https://discord.gg/HfJVk9J29K" }
      ]
    },
    "announcement": {
      "enabled": true,
      "modal": { "<port the bilingual NL/EN migration modal content from fork home.js announcementModal()>" }
    },
    "firstVisitChooser": { "enabled": true },
    "observerSetup": {
      "enabled": true,
      "html": "<port the observer/MQTT setup how-to from CoreScope_Cornmeister/public/observers.js (the collector1/2.dutchmeshcore.nl instructions)>"
    }
  }
}
```

Source map for the placeholders above:
- `meta.description`, `meta.ogImage` — `CoreScope_Cornmeister/public/index.html` `<head>`.
- `home.*` — `CoreScope_Cornmeister/public/home.js` (`checklist()`, hero text, footer).
- `sections.donate` — `CoreScope_Cornmeister/public/home.js` `home-donate` block.
- `sections.announcement.modal` — `CoreScope_Cornmeister/public/home.js` `announcementModal()` (the "Belangrijke migratie" NL/EN modal).
- `sections.observerSetup.html` — `CoreScope_Cornmeister/public/observers.js` (the MQTT how-to block, roughly lines 131-231).

Keep `theme`/`themeDark` out unless the fork's colors differ from evo's — per the analysis the fork only recolors `--logo-accent`; the blue logo is carried by `logo.svg`, so no color overrides are needed.

- [ ] **Step 4: Verify the server loads it**

```bash
go build ./cmd/server/ && echo BUILD_OK
```

Then verify JSON validity:

```bash
python3 -c "import json; json.load(open('templates/cornmeister/template.json')); print('JSON_OK')"
```

Expected: `BUILD_OK` then `JSON_OK`.

- [ ] **Step 5: Commit**

```bash
git add templates/cornmeister/
git commit -m "feat(templates): add cornmeister branding template"
```

---

# Phase 3 — Frontend wiring

### Task 9: Pass `meta` and `sections` through to `window.SITE_CONFIG`

Without this, the `customize-v2.js` pipeline drops unknown keys and the home page never sees `sections`.

**Files:**
- Modify: `public/customize-v2.js:36-37` (`VALID_SECTIONS` / `OBJECT_SECTIONS`)

- [ ] **Step 1: Inspect the merge pipeline**

Read `public/customize-v2.js` `computeEffective()` and `init()`. Confirm whether keys outside `VALID_SECTIONS` survive into `window.SITE_CONFIG` (line 625, `window.SITE_CONFIG = effective`). If `computeEffective` only copies `VALID_SECTIONS` keys, `meta`/`sections` are dropped.

- [ ] **Step 2: Add `meta` and `sections` as valid object sections**

In `public/customize-v2.js`, update lines 36-37:

```js
  var VALID_SECTIONS = ['branding', 'theme', 'themeDark', 'nodeColors', 'typeColors', 'home', 'meta', 'sections', 'timestamps', 'heatmapOpacity', 'liveHeatmapOpacity', 'distanceUnit', 'favorites', 'myNodes'];
  var OBJECT_SECTIONS = ['branding', 'theme', 'themeDark', 'nodeColors', 'typeColors', 'home', 'meta', 'sections', 'timestamps'];
```

`meta`/`sections` are server-driven only (no in-app customizer UI for them); adding them here just lets them flow through the merge into `window.SITE_CONFIG`.

- [ ] **Step 3: Manual verification**

Run the server with `templates/cornmeister` active (set `"template":"cornmeister"` in a local `config.json`), open the app, and in the browser console run `window.SITE_CONFIG.sections` and `window.SITE_CONFIG.meta`.
Expected: both objects are populated from the template.

- [ ] **Step 4: Commit**

```bash
git add public/customize-v2.js
git commit -m "feat(ui): pass meta/sections config through to SITE_CONFIG"
```

---

### Task 10: Render the donate section on the home page

**Files:**
- Modify: `public/home.js` (`renderHome`, ~lines 45-99), `public/home.css`
- Reference: `CoreScope_Cornmeister/public/home.js` (`home-donate` block) and `home.css`

- [ ] **Step 1: Add a `donateSection` renderer in home.js**

In `public/home.js`, add a function near `checklist()` (around line 519):

```js
  function donateSection() {
    var d = window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.donate;
    if (!d || !d.enabled) return '';
    var links = (d.links || []).map(function (l) {
      return '<a class="home-donate-link" href="' + escapeAttr(l.url) + '" target="_blank" rel="noopener">' + escapeHtml(l.label) + '</a>';
    }).join('');
    var img = d.image ? '<img class="home-donate-img" src="' + escapeAttr(d.image) + '" alt="" loading="lazy">' : '';
    return '<section class="home-donate">' +
      (d.title ? '<h2>' + escapeHtml(d.title) + '</h2>' : '') +
      img + '<div class="home-donate-links">' + links + '</div></section>';
  }
```

- [ ] **Step 2: Mount it in renderHome**

In `public/home.js` `renderHome`, inside the `container.innerHTML = \`...\`` template, add `${donateSection()}` immediately before the `<section class="home-footer">` block (before line 91).

- [ ] **Step 3: Port the donate styles**

Copy the `.home-donate*` CSS rules from `CoreScope_Cornmeister/public/home.css` into `public/home.css`. Scope them under `.home-donate` (they only render when the section is present, so no global leakage).

- [ ] **Step 4: Manual verification**

With `template:"cornmeister"`: load `#/` — the donate section with the bunq + Discord links and the DutchMeshCore image renders above the footer.
With `template` unset: no donate section appears.

- [ ] **Step 5: Commit**

```bash
git add public/home.js public/home.css
git commit -m "feat(ui): render config-gated home donate section"
```

---

### Task 11: Render the announcement modal

**Files:**
- Modify: `public/home.js` (`init`, ~line 32)
- Reference: `CoreScope_Cornmeister/public/home.js` `announcementModal()`

- [ ] **Step 1: Add an `announcementModal` renderer in home.js**

Port `announcementModal()` from `CoreScope_Cornmeister/public/home.js` into `public/home.js`, adapted to read content from config and to dismiss-once via `localStorage`:

```js
  function maybeShowAnnouncement() {
    var a = window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.announcement;
    if (!a || !a.enabled || !a.modal) return;
    var key = 'meshcore-announcement-dismissed-' + (a.modal.id || 'default');
    if (localStorage.getItem(key) === '1') return;
    // ... build the modal DOM from a.modal (port markup/structure from the
    //     Cornmeister announcementModal()); wire the close button to
    //     localStorage.setItem(key, '1') then remove the modal node.
  }
```

Source the modal markup, the bilingual NL/EN copy structure, and the close-button behavior from the fork's `announcementModal()`. The copy text itself comes from `a.modal` (config), not hardcoded.

- [ ] **Step 2: Call it from init**

In `public/home.js` `init()` (line 32-36), after `renderHome(container);` add:

```js
    maybeShowAnnouncement();
```

- [ ] **Step 3: Port the modal CSS**

Copy the announcement-modal CSS rules from `CoreScope_Cornmeister/public/home.css` into `public/home.css`.

- [ ] **Step 4: Manual verification**

With `template:"cornmeister"`: first load of `#/` shows the announcement modal; closing it sets the localStorage key; reload — modal does not reappear.
With `template` unset: no modal.

- [ ] **Step 5: Commit**

```bash
git add public/home.js public/home.css
git commit -m "feat(ui): render config-gated announcement modal"
```

---

### Task 12: Render the first-visit experience chooser

The chooser was removed in commits `da579fe`/`3ed0d7d`. This re-introduces it **only** when `sections.firstVisitChooser.enabled` is true.

**Files:**
- Modify: `public/home.js` (`init`, ~lines 28-36)
- Reference: `CoreScope_Cornmeister/public/home.js` `showChooser()`

- [ ] **Step 1: Add a config-gated chooser in home.js**

In `public/home.js`, port `showChooser()` from the Cornmeister fork as `showChooser(container)`. Then change `init()` (lines 32-36):

```js
  function init(container) {
    var chooser = window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.firstVisitChooser;
    var levelChosen = localStorage.getItem(PREF_KEY) !== null;
    if (chooser && chooser.enabled && !levelChosen) {
      showChooser(container); // sets PREF_KEY via setLevel(), then calls renderHome
      return;
    }
    renderHome(container);
  }
```

`showChooser` must call `setLevel(...)` (line 30) on choice and then `renderHome(container)`, so the existing `isExperienced()` path is unaffected.

- [ ] **Step 2: Port the chooser CSS**

Copy the chooser CSS rules from `CoreScope_Cornmeister/public/home.css` into `public/home.css`.

- [ ] **Step 3: Manual verification**

With `template:"cornmeister"` and a cleared `meshcore-user-level` localStorage key: first load shows the chooser; picking an option records the level and renders home; reload skips the chooser.
With `template` unset: no chooser, users default to experienced (unchanged behavior).

- [ ] **Step 4: Commit**

```bash
git add public/home.js public/home.css
git commit -m "feat(ui): re-introduce first-visit chooser as config-gated section"
```

---

### Task 13: Config-gated observer-setup help block

`observers.js` has no help block today. This adds one, gated on `sections.observerSetup.enabled`.

**Files:**
- Modify: `public/observers.js` (render path), `public/observers.css` (if present, else `public/style.css`)
- Reference: `CoreScope_Cornmeister/public/observers.js` (MQTT how-to, ~lines 131-231)

- [ ] **Step 1: Add the help block renderer**

In `public/observers.js`, add:

```js
  function observerSetupHelp() {
    var s = window.SITE_CONFIG && window.SITE_CONFIG.sections && window.SITE_CONFIG.sections.observerSetup;
    if (!s || !s.enabled || !s.html) return '';
    return '<section class="observer-setup-help">' + s.html + '</section>';
  }
```

Note `s.html` is operator/template-supplied trusted config (server-side `templates/`), rendered as-is — same trust model as the existing `home` config block.

- [ ] **Step 2: Mount it**

In `public/observers.js`, find where the observer table is injected into the page container and prepend `observerSetupHelp()` to that markup so the help block renders above the status table.

- [ ] **Step 3: Style it**

Add minimal `.observer-setup-help` styling consistent with `.home-checklist`. Port relevant rules from `CoreScope_Cornmeister` if it has them.

- [ ] **Step 4: Manual verification**

With `template:"cornmeister"`: `#/observers` shows the MQTT setup how-to above the table.
With `template` unset: no help block.

- [ ] **Step 5: Commit**

```bash
git add public/observers.js public/style.css
git commit -m "feat(ui): add config-gated observer setup help block"
```

---

### Task 14: Source repo URL and site-name literals from SITE_CONFIG

**Files:**
- Modify: `public/app.js:364`, `public/channels.js:795`, `public/nodes.js:550`

- [ ] **Step 1: Repo URL in app.js**

In `public/app.js`, replace line 364:

```js
  var GH = (window.SITE_CONFIG && window.SITE_CONFIG.meta && window.SITE_CONFIG.meta.repoUrl) || 'https://github.com/marcelverdult/corescope-evo';
```

- [ ] **Step 2: Site name in channels.js**

In `public/channels.js`, line 795, replace the hardcoded `CoreScope` in the `🔒 Keys stay in your browser — CoreScope is a passive observer ...` string with a `siteName` lookup. Near the top of the relevant function add:

```js
  var siteName = (window.SITE_CONFIG && window.SITE_CONFIG.branding && window.SITE_CONFIG.branding.siteName) || 'CoreScope';
```

and interpolate `siteName` into the message string.

- [ ] **Step 3: Site name in nodes.js**

In `public/nodes.js`, line 550, do the same for the `title="...observed by CoreScope..."` attribute — read `siteName` from `window.SITE_CONFIG.branding.siteName` with a `'CoreScope'` fallback and interpolate it.

(Leave `packets.js:2689` — it is a `console.warn`, not user-facing. Leave all `meshcore-*` localStorage keys — they are a storage namespace, not branding.)

- [ ] **Step 4: Manual verification**

With `template:"cornmeister"`: the version footer GitHub link points at the Cornmeister repo; the channels key-privacy notice and the nodes repeater tooltip say `CORNMEISTER.NL`.
With `template` unset: all three show the evo defaults.

- [ ] **Step 5: Commit**

```bash
git add public/app.js public/channels.js public/nodes.js
git commit -m "feat(ui): source repo URL and site name from SITE_CONFIG"
```

---

# Phase 4 — Documentation and end-to-end verification

### Task 15: Document the `template` key

**Files:**
- Modify: `config.example.json`, `docs/user-guide/configuration.md`

- [ ] **Step 1: Add to config.example.json**

In `config.example.json`, after the `branding` block (after line 31), add:

```json
  "template": "",
  "_comment_template": "Activates a bundled branding template from templates/<name>/. Empty or 'default' = built-in CoreScope-EVO look. 'cornmeister' = Dutch mesh branding. A template sets branding/theme/home/meta/sections; explicit branding/theme/home keys in this file still override the template.",
```

- [ ] **Step 2: Document in configuration.md**

In `docs/user-guide/configuration.md`, add a "Branding templates" section: explain the `template` key, list the bundled templates (`default`, `cornmeister`), describe the precedence (`defaults < template < config.json < theme.json`), and note that `templates/<name>/assets/` is served at `/template-assets/`.

- [ ] **Step 3: Commit**

```bash
git add config.example.json docs/user-guide/configuration.md
git commit -m "docs: document the template config key"
```

---

### Task 16: End-to-end test

**Files:**
- Create: `test-config-templates.js`

- [ ] **Step 1: Write the e2e test**

Create `test-config-templates.js` following the pattern of an existing root `test-*.js` Playwright file (e.g. `test-logo-rebrand-e2e.js`). It must:

1. Start the server twice (or restart with a swapped `config.json`): once with `template` unset, once with `"template":"cornmeister"`.
2. **Default case:** assert `document.title` is `CoreScope-EVO`, the nav logo is the built-in inline SVG, `#/` has no `.home-donate` section, `#/observers` has no `.observer-setup-help`.
3. **Cornmeister case:** assert `document.title` is `Cornmeister.nl`, the served `/` HTML contains `<meta property="og:title" content="Cornmeister.nl">`, `#/` renders `.home-donate` with the bunq link, the announcement modal appears on first visit, `#/observers` renders `.observer-setup-help`, and `/template-assets/logo.svg` returns HTTP 200.
4. Assert `GET /api/config/theme` returns `template: "cornmeister"` and a populated `sections` object.

- [ ] **Step 2: Run the e2e test**

Run: `node test-config-templates.js`
Expected: all assertions pass.

- [ ] **Step 3: Run the full Go test suite for regressions**

Run: `go test ./cmd/server/ 2>&1 | tail -20`
Expected: PASS

- [ ] **Step 4: Run the proto validator**

Run: `python3 tools/validate-protos.py 2>&1 | tail -10`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add test-config-templates.js
git commit -m "test: e2e coverage for config-activatable templates"
```

---

## Done-when

- `template:"cornmeister"` in `config.json` re-skins title, OG meta, logo, home content, donate/announcement/chooser sections, observer setup help, and repo links.
- `template` unset or `"default"` produces byte-identical behavior to pre-feature `main`.
- An invalid `template` value logs a warning and falls back to `default` without crashing.
- `go test ./cmd/server/`, `node test-config-templates.js`, and `python3 tools/validate-protos.py` all pass.
