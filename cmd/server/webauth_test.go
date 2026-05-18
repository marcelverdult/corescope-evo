package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// okHandler is a trivial next-handler that records that it ran.
func okHandler(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func TestWebAuthTokenRoundTrip(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")
	if !wa.enabled {
		t.Fatal("expected gate enabled with both creds set")
	}
	tok := wa.signToken(time.Now().Add(time.Hour).Unix())
	if !wa.verifyToken(tok) {
		t.Fatal("freshly signed token should verify")
	}
}

func TestWebAuthTamperedTokenFails(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")
	tok := wa.signToken(time.Now().Add(time.Hour).Unix())

	// Flip the last character of the signature.
	bad := tok[:len(tok)-1]
	if tok[len(tok)-1] == 'A' {
		bad += "B"
	} else {
		bad += "A"
	}
	if wa.verifyToken(bad) {
		t.Fatal("tampered token must not verify")
	}

	// Garbage / malformed inputs.
	for _, junk := range []string{"", "no-dot", "a.b.c", "!!!.???"} {
		if wa.verifyToken(junk) {
			t.Fatalf("malformed token %q must not verify", junk)
		}
	}

	// A token signed with a different password (different derived secret)
	// must not verify — proves the cookie auto-invalidates on password change.
	other := newWebAuth("admin", "different-password")
	if wa.verifyToken(other.signToken(time.Now().Add(time.Hour).Unix())) {
		t.Fatal("token signed with a different password must not verify")
	}
}

func TestWebAuthExpiredTokenFails(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")
	expired := wa.signToken(time.Now().Add(-time.Minute).Unix())
	if wa.verifyToken(expired) {
		t.Fatal("expired token must not verify")
	}
}

func TestWebAuthDisabledWhenCredsMissing(t *testing.T) {
	for _, tc := range []struct{ user, pass string }{
		{"", ""},
		{"admin", ""},
		{"", "pw"},
	} {
		wa := newWebAuth(tc.user, tc.pass)
		if wa.enabled {
			t.Fatalf("gate must be disabled for user=%q pass=%q", tc.user, tc.pass)
		}
	}
}

func TestWebAuthMiddlewareDisabledIsPassThrough(t *testing.T) {
	wa := newWebAuth("", "") // disabled
	ran := false
	h := wa.middleware(okHandler(&ran))

	// A non-exempt path with no cookie still passes through when disabled.
	req := httptest.NewRequest("GET", "/api/nodes", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !ran {
		t.Fatal("disabled gate must call next (true no-op)")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestWebAuthMiddlewareNoCookieHTMLRedirects(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")
	ran := false
	h := wa.middleware(okHandler(&ran))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if ran {
		t.Fatal("next must not run for unauthenticated request")
	}
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestWebAuthMiddlewareNoCookieAPIReturns401(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")
	ran := false
	h := wa.middleware(okHandler(&ran))

	req := httptest.NewRequest("GET", "/api/nodes", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if ran {
		t.Fatal("next must not run for unauthenticated API request")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestWebAuthMiddlewareExemptPathsPass(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")
	for _, path := range []string{"/api/stats", "/login", "/healthz", "/api/healthz", "/metrics", "/favicon.ico", "/favicon.svg", "/logout"} {
		ran := false
		h := wa.middleware(okHandler(&ran))
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if !ran {
			t.Fatalf("exempt path %q must pass through", path)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("exempt path %q: expected 200, got %d", path, w.Code)
		}
	}
}

func TestWebAuthMiddlewareValidCookiePasses(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")
	ran := false
	h := wa.middleware(okHandler(&ran))

	req := httptest.NewRequest("GET", "/api/nodes", nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{
		Name:  webAuthCookieName,
		Value: wa.signToken(time.Now().Add(time.Hour).Unix()),
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !ran {
		t.Fatal("request with a valid cookie must pass through")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestWebAuthLoginCorrectCredsSetsCookie(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")

	form := url.Values{"user": {"admin"}, "password": {"s3cret-password"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	wa.handleLogin(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("expected redirect to /, got %q", loc)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == webAuthCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login must set the cs_session cookie")
	}
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attributes wrong: HttpOnly=%v Secure=%v SameSite=%v", session.HttpOnly, session.Secure, session.SameSite)
	}
	if !wa.verifyToken(session.Value) {
		t.Fatal("cookie value must be a valid signed token")
	}
}

func TestWebAuthLoginWrongCredsRedirectsToError(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")

	for _, form := range []url.Values{
		{"user": {"admin"}, "password": {"wrong"}},
		{"user": {"wrong"}, "password": {"s3cret-password"}},
		{"user": {""}, "password": {""}},
	} {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		wa.handleLogin(w, req)

		if w.Code != http.StatusSeeOther {
			t.Fatalf("expected 303, got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/login?error=1" {
			t.Fatalf("expected redirect to /login?error=1, got %q", loc)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == webAuthCookieName && c.Value != "" && c.MaxAge >= 0 {
				t.Fatal("failed login must not set a session cookie")
			}
		}
	}
}

func TestWebAuthLoginGetRendersForm(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	wa.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /login expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="user"`) || !strings.Contains(body, `name="password"`) {
		t.Fatal("login page must contain user + password fields")
	}
	if strings.Contains(body, "Invalid credentials") {
		t.Fatal("login page without ?error=1 must not show the error banner")
	}

	reqErr := httptest.NewRequest("GET", "/login?error=1", nil)
	wErr := httptest.NewRecorder()
	wa.handleLogin(wErr, reqErr)
	if !strings.Contains(wErr.Body.String(), "Invalid credentials") {
		t.Fatal("login page with ?error=1 must show the error banner")
	}
}

func TestWebAuthLogoutClearsCookie(t *testing.T) {
	wa := newWebAuth("admin", "s3cret-password")

	req := httptest.NewRequest("GET", "/logout", nil)
	w := httptest.NewRecorder()
	wa.handleLogout(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == webAuthCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout must expire the cs_session cookie (MaxAge < 0)")
	}
}
