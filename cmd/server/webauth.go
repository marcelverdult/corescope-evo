package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// webAuth implements the in-app cookie session gate. It replaces an external
// Traefik HTTP-basic-auth gate (which re-prompts repeatedly on mobile) with
// ONE login -> a signed cookie -> carried to every subresource.
//
// The gate is fail-safe: if either credential env var is empty/unset the gate
// is DISABLED and the middleware becomes a true pass-through no-op. A misconfig
// must never lock anyone out.
type webAuth struct {
	enabled  bool
	user     string
	password string
	secret   []byte // HMAC key derived from the password
}

const (
	webAuthCookieName = "cs_session"
	webAuthMaxAge     = 30 * 24 * time.Hour
)

// webAuthExemptPaths are request paths that bypass the gate even when enabled.
// /api/stats is the Docker/Coolify container healthcheck — gating it would mark
// the container unhealthy. /metrics is the Prometheus scrape. /login + /logout
// must be reachable to authenticate. /healthz (and /api/healthz) is the
// readiness probe. The favicons let the login page render an icon.
var webAuthExemptPaths = map[string]bool{
	"/login":       true,
	"/logout":      true,
	"/healthz":     true,
	"/api/healthz": true,
	"/api/stats":   true,
	"/metrics":     true,
	"/favicon.ico": true,
	"/favicon.svg": true,
}

// newWebAuth builds the gate from the supplied credentials. If either is empty
// the gate is disabled. The signing secret is derived from the password so the
// cookie auto-invalidates whenever the password changes.
func newWebAuth(user, password string) *webAuth {
	wa := &webAuth{user: user, password: password}
	if user == "" || password == "" {
		wa.enabled = false
		return wa
	}
	wa.enabled = true
	sum := sha256.Sum256([]byte("corescope-web-auth:" + password))
	wa.secret = sum[:]
	return wa
}

// logStartup logs whether the web-auth gate is enabled or disabled.
func (wa *webAuth) logStartup() {
	if wa.enabled {
		log.Printf("[webauth] web-auth gate ENABLED (cookie session)")
	} else {
		log.Printf("[webauth] web-auth gate DISABLED (CORESCOPE_WEB_USER/CORESCOPE_WEB_PASSWORD unset) — pass-through")
	}
}

// cookiePayload is the JSON payload signed into the session cookie.
type cookiePayload struct {
	Exp int64 `json:"exp"` // unix seconds at which the cookie expires
}

// signToken produces a signed token: base64url(payload).base64url(HMAC-SHA256(payload)).
func (wa *webAuth) signToken(exp int64) string {
	payload, _ := json.Marshal(cookiePayload{Exp: exp})
	mac := hmac.New(sha256.New, wa.secret)
	mac.Write(payload)
	sig := mac.Sum(nil)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(sig)
}

// verifyToken checks the signature (constant-time) and expiry. It returns true
// only for a well-formed, correctly signed, unexpired token.
func (wa *webAuth) verifyToken(token string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return false
	}
	gotSig, err := enc.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, wa.secret)
	mac.Write(payload)
	wantSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return false
	}
	var p cookiePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	if time.Now().Unix() >= p.Exp {
		return false
	}
	return true
}

// setSessionCookie writes a fresh signed session cookie onto the response.
func (wa *webAuth) setSessionCookie(w http.ResponseWriter) {
	exp := time.Now().Add(webAuthMaxAge)
	http.SetCookie(w, &http.Cookie{
		Name:     webAuthCookieName,
		Value:    wa.signToken(exp.Unix()),
		Path:     "/",
		MaxAge:   int(webAuthMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie expires the session cookie on the response.
func (wa *webAuth) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     webAuthCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// hasValidSession reports whether the request carries a valid session cookie.
func (wa *webAuth) hasValidSession(r *http.Request) bool {
	c, err := r.Cookie(webAuthCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return wa.verifyToken(c.Value)
}

// middleware wraps the whole router. When the gate is disabled it is a true
// pass-through no-op. When enabled, exempt paths pass through; otherwise the
// session cookie is verified. An unauthenticated browser document navigation
// (Accept contains text/html) gets a 303 redirect to /login; everything else
// (API/asset/XHR) gets a 401.
func (wa *webAuth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wa.enabled {
			next.ServeHTTP(w, r)
			return
		}
		if webAuthExemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if wa.hasValidSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized — log in at /login\n"))
	})
}

// handleLogin serves the login form (GET) and processes a login attempt (POST).
func (wa *webAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
			return
		}
		user := r.PostFormValue("user")
		password := r.PostFormValue("password")
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(wa.user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(password), []byte(wa.password)) == 1
		if userOK && passOK {
			wa.setSessionCookie(w)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	// GET — render the form.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	errBanner := ""
	if r.URL.Query().Get("error") == "1" {
		errBanner = `<p class="err">Invalid credentials</p>`
	}
	w.Write([]byte(loginPageHTML(errBanner)))
}

// handleLogout clears the session cookie and redirects to the login page.
func (wa *webAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	wa.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// registerWebAuthRoutes registers /login and /logout on the router. They are
// also exempt in the middleware, so they remain reachable regardless of the
// gate state.
func (wa *webAuth) registerWebAuthRoutes(r *mux.Router) {
	r.HandleFunc("/login", wa.handleLogin).Methods("GET", "POST")
	r.HandleFunc("/logout", wa.handleLogout).Methods("GET")
}

// loginPageHTML returns a minimal, self-contained dark-themed login page.
func loginPageHTML(errBanner string) string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CoreScope — Sign in</title>
<link rel="icon" href="/favicon.svg">
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: flex;
    align-items: center; justify-content: center;
    background: #0d1117; color: #e6edf3;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }
  .card {
    width: 320px; max-width: 90vw; padding: 32px;
    background: #161b22; border: 1px solid #30363d; border-radius: 12px;
  }
  h1 { margin: 0 0 4px; font-size: 20px; text-align: center; }
  .sub { margin: 0 0 24px; font-size: 13px; color: #8b949e; text-align: center; }
  label { display: block; font-size: 13px; margin: 14px 0 6px; color: #8b949e; }
  input {
    width: 100%; padding: 10px 12px; font-size: 14px;
    background: #0d1117; color: #e6edf3;
    border: 1px solid #30363d; border-radius: 6px;
  }
  input:focus { outline: none; border-color: #1f6feb; }
  button {
    width: 100%; margin-top: 22px; padding: 10px;
    font-size: 14px; font-weight: 600; cursor: pointer;
    background: #238636; color: #fff;
    border: none; border-radius: 6px;
  }
  button:hover { background: #2ea043; }
  .err {
    margin: 0 0 16px; padding: 8px 12px; font-size: 13px;
    background: #2d1214; color: #ff7b72;
    border: 1px solid #5c2123; border-radius: 6px; text-align: center;
  }
</style>
</head>
<body>
  <form class="card" method="POST" action="/login">
    <h1>CoreScope</h1>
    <p class="sub">Sign in to continue</p>
    ` + errBanner + `
    <label for="user">Username</label>
    <input id="user" name="user" type="text" autocomplete="username" autofocus>
    <label for="password">Password</label>
    <input id="password" name="password" type="password" autocomplete="current-password">
    <button type="submit">Sign in</button>
  </form>
</body>
</html>
`
}
