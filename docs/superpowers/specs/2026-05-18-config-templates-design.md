# Config-Activatable Branding Templates — Design

**Date:** 2026-05-18
**Status:** Approved design, pre-implementation

## Goal

Add config-activatable "templates" to CoreScope-EVO. A template is a named bundle of
optical/branding/help customizations. An operator activates one with a single key in
`config.json`. The first concrete template, `cornmeister`, reproduces the look, branding,
and help content of the older `CoreScope_Cornmeister` fork.

Scope of a template: optics (colors, fonts, meta), the logo, and help/onboarding text.
Three structural home-page blocks — donate section, bilingual announcement modal,
first-visit experience chooser — are also template-toggleable.

## Background

Three sibling repos exist on the dev machine:

- `CoreScope` — original.
- `CoreScope_Cornmeister` — older re-branded fork (blue concentric-arc logo,
  CORNMEISTER.NL identity, Dutch/NL help content, donate + announcement + chooser UI).
- `corescope-evo` — current development line (sage/teal identity, US help content;
  donate/announcement/chooser were deliberately removed in commits `da579fe`, `3ed0d7d`).

corescope-evo already ships config-driven branding scaffolding: `branding`, `theme`,
`themeDark`, `nodeColors`, and `home` config sections, served via `/api/config/theme`
and merged in `buildThemeResponse()`. Named color PRESETS exist in `customize-v2.js`.
A template extends this existing pipeline rather than replacing it.

Parts NOT config-backed today (the work this design adds):

- Logo — hardcoded inline `<svg>` in `index.html` and `home.js`. The Cornmeister logo
  is a distinct design, not a recolor.
- Help/onboarding copy — scattered hardcoded strings in `observers.js`, `channels.js`,
  `nodes.js`, `packets.js` (MQTT setup how-to, region frequency presets, literal
  `"Cornmeister.nl"` strings).
- `<title>` and OG/Twitter meta tags — static in `index.html`.
- `GH` repo-URL constant in `app.js`.
- Donate section, announcement modal, first-visit chooser — structural HTML blocks
  present only in the Cornmeister `home.js`/`home.css`.

## Approach

Server-resolved bundle (chosen over build-time baking and pure client-side loading).
Build-time baking needs a rebuild, not a config flip. Client-side loading causes color
flash and cannot set `<title>`/OG meta for crawlers and social cards. Server resolution
reuses the existing `__THEME_STYLE__` server-inlining mechanism, so there is no flash
and meta tags work.

## Template Anatomy

Templates are bundled in the repo, one directory per template:

```
templates/
  default/
    template.json
    assets/                       (optional — logo.svg etc.)
  cornmeister/
    template.json
    assets/
      logo.svg
      powered-by-dutchmeshcore.png
```

`template.json` schema:

```json
{
  "name": "cornmeister",
  "branding":  { "siteName": "...", "tagline": "...", "logoUrl": "...", "faviconUrl": "..." },
  "meta":      { "title": "...", "description": "...", "ogImage": "...", "themeColor": "...", "repoUrl": "..." },
  "theme":     { "...color tokens..." },
  "themeDark": { "...color tokens..." },
  "nodeColors":{ "...": "..." },
  "logo":      { "asset": "logo.svg", "wordmark": "...", "subtitle": "..." },
  "home":      { "heroTitle": "...", "heroSubtitle": "...", "steps": [], "checklist": [], "footerLinks": [] },
  "help": {
    "observerSetup": { "...MQTT how-to, collector hostnames..." },
    "regionPresets": [],
    "labels":        { "...": "..." }
  },
  "sections": {
    "donate":            { "enabled": true, "title": "...", "links": [], "image": "..." },
    "announcement":      { "enabled": true, "modal": { "...bilingual content..." } },
    "firstVisitChooser": { "enabled": true }
  }
}
```

Asset references (`logoUrl`, `meta.ogImage`, `logo.asset`, `sections.donate.image`,
`branding.faviconUrl`) are filenames relative to the template's `assets/` directory.
The server rewrites them to `/template-assets/<file>` URLs.

## Config and Merge Precedence

One new key in `config.json`:

```json
{ "template": "cornmeister" }
```

Absent or empty → `default`. The `default` template is corescope-evo's current look
extracted verbatim, so an unset key produces zero behavior change.

Merge precedence, lowest to highest:

```
built-in defaults  <  template bundle  <  config.json sections  <  theme.json overlay
```

An operator selects a template and may still override individual values via existing
`branding`/`theme`/`home` config sections or `theme.json`.

Invalid `template` value → log a warning, fall back to `default`, never crash.

## Backend Changes (Go)

- `cmd/server/config.go` (~line 37) — add `Template string \`json:"template,omitempty"\``
  to the `Config` struct, alongside `Branding`/`Home`.
- New `cmd/server/template.go` — load `templates/<name>/template.json`, validate,
  fall back to `default` on missing/malformed input with a logged warning.
- `cmd/server/routes.go` `buildThemeResponse()` (~line 393) — insert the resolved
  template's maps into the existing `mergeMap` overlay chain, between built-in defaults
  and `cfg.*`. Extend the served payload to include `meta`, `help`, `sections`, `logo`.
- `cmd/server/main.go` `spaHandler` (~line 664) — add `__SITE_TITLE__`, `__OG_*__`,
  `__LOGO_*__` placeholder substitution in `index.html`, using the same string-replace
  mechanism as the existing `__THEME_STYLE__` and `__BUST__` placeholders.
- New static route `/template-assets/` → serves the active template's `assets/`
  directory via an `http.FileServer`.
- `proto/config.proto` — add the `template` field and any new doc messages, keeping
  `tools/validate-protos.py` green (it is a CI gate; there is no protoc/buf regen step —
  the proto is hand-kept in sync with the Go structs).

## Frontend Changes (JS / HTML / CSS)

- `public/index.html` — replace the static `<title>`, OG/Twitter meta, and
  `theme-color` with placeholders. Replace the inline logo `<svg>` block with an
  `<img src="<logoUrl>">` (asset-file logo).
- `public/home.js` — render the donate, announcement, and first-visit-chooser blocks,
  each gated on `SITE_CONFIG.sections.*.enabled`. Port the existing Cornmeister blocks
  as config-driven components.
- `public/observers.js`, `channels.js`, `nodes.js`, `packets.js` — replace hardcoded
  help text and `"Cornmeister.nl"` literals with reads from `SITE_CONFIG.help`. Every
  moved string keeps an inline default fallback so a thin template never blanks the UI.
- `public/app.js` (~line 364) — source the `GH` repo URL from `SITE_CONFIG.meta.repoUrl`.
- `public/customize-v2.js` — surface the active template name (display only); per-user
  color presets continue to work as overrides on top of the template.
- `public/home.css` — port the Cornmeister donate/announcement/chooser styles, scoped
  so they only apply when the corresponding section is rendered.

## Bundled Templates

- `default` — corescope-evo's current sage/teal identity plus the Bay Area / US
  910.525 MHz help content, extracted verbatim from today's hardcoded values.
- `cornmeister` — blue concentric-arc animated logo, CORNMEISTER.NL wordmark, Dutch
  help content (868 MHz presets, dutchmeshcore collector hostnames), donate +
  announcement + first-visit-chooser enabled, `powered-by-dutchmeshcore.png` asset.

## Testing

- `cmd/server/routes_test.go` — `/api/config/theme` with `template` set, unset, and
  invalid.
- Template-loader unit test — valid bundle, missing directory, malformed JSON.
- End-to-end — load with `template: cornmeister` and assert title, logo, help text,
  and donate block render; load with `default` and assert the UI is unchanged from
  pre-feature behavior.
- `tools/validate-protos.py` stays green.

## Risks

- Help-string de-hardcoding spans five JS modules — the largest portion of the work
  and the main regression risk. Mitigation: every moved string keeps an inline default
  fallback; the `default` template carries today's exact strings; the e2e default-case
  test asserts no visible change.
- An invalid `template` value must degrade gracefully (warn + fall back to `default`),
  never crash the server at startup.

## Out of Scope

- Operator-supplied template files (only repo-bundled named presets in this iteration).
- A template-authoring UI.
- Migrating the in-app color PRESETS into the template system.
