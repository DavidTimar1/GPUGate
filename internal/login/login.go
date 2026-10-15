// Package login puts the whole console behind one password and the fixed address behind an
// optional access key. Wrap goes around the entire route table, so a route added later is guarded
// without anyone remembering to guard it: everything is refused unless it is one of the few
// routes named open below, or carries a valid session.
//
// Sessions live in memory only (a restart logs the browser out) and each one carries a digest of
// the password hash it was made under, so setting a new password ends every session.
package login

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	cookieName       = "gpugate_session"
	sessionLifetime  = 30 * 24 * time.Hour
	maxLoginBodySize = 4096
	loginReadTimeout = 10 * time.Second
	// proxyPrefix is the fixed address's path (console.projectProxyPrefix); it and the project list
	// are the routes the access key opens to tools.
	proxyPrefix  = "/p/"
	projectsPath = "/api/projects"
)

// consoleCSP allows exactly what ui/index.html loads: its own scripts (none inline), its own
// styles plus the style attributes and the style element htmx adds, and the IBM Plex fonts served
// from ui/vendor/fonts. Nothing is fetched from another host.
const consoleCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// loginCSP allows what the login page is: one inline style block, the favicon, and its own form.
const loginCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

type session struct {
	expires time.Time
	stamp   string
}

// Auth is the login: the password file, the live sessions, the failure counter and the access key.
type Auth struct {
	hashPath     string
	keyDigest    [sha256.Size]byte
	hasKey       bool
	hosts        map[string]bool
	cookieSecure bool
	logf         func(string)
	lim          *limiter

	mu       sync.Mutex
	sessions map[string]session // keyed by the digest of the cookie's token

	// attemptMu is held for a whole password attempt, so concurrent attempts cannot get more than
	// maxFailures tries in before the lock lands, and only one memory-hungry hash runs at a time.
	attemptMu sync.Mutex
}

// New makes the login. accessKey "" means the fixed address and the project list stay open (to
// whoever can reach a listening address). hosts are the names a request may be addressed to (the
// listen addresses, localhost, and any extra names); anything else is refused, which stops a web
// page from reaching this program through a name it controls (DNS rebinding). cookieSecure marks
// the session cookie Secure for a TLS front. logf receives one-line events and never a secret.
func New(hashPath, accessKey string, hosts []string, cookieSecure bool, logf func(string)) *Auth {
	a := &Auth{hashPath: hashPath, logf: logf, sessions: map[string]session{}, lim: newLimiter(),
		hosts: map[string]bool{}, cookieSecure: cookieSecure}
	for _, h := range hosts {
		a.hosts[strings.ToLower(strings.Trim(strings.TrimSpace(h), "[]"))] = true
	}
	if accessKey != "" {
		a.hasKey = true
		a.keyDigest = sha256.Sum256([]byte(accessKey))
	}
	return a
}

// Wrap guards next. Only /login, /logout and the favicon are answered without a session; the
// fixed address and GET /api/projects also accept the access key when one is set, and stay open
// when none is set.
func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// Not "no-referrer": under that policy a browser sends "Origin: null" on a same-origin form
		// post (the login and log out forms), which the forged-request check would have to refuse.
		// "same-origin" sends nothing to any other site and keeps a real Origin on our own posts.
		h.Set("Referrer-Policy", "same-origin")
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		if p == "/" || p == "/console" {
			h.Set("Content-Security-Policy", consoleCSP)
		}

		// The first check of all: a request addressed to a name that is not ours is never answered.
		if !a.hostAllowed(r.Host) {
			writeDetail(w, http.StatusMisdirectedRequest, "This address is not served here.")
			return
		}

		// The guard decides on the path as sent; the mux routes on the cleaned path. A path that
		// differs between the two is refused here, so the two can never disagree about a route.
		if !canonical(p) {
			writeDetail(w, http.StatusBadRequest, "That path is not in its plain form.")
			return
		}

		switch p {
		case "/login":
			a.handleLogin(w, r)
			return
		case "/logout":
			a.handleLogout(w, r)
			return
		case "/favicon.svg":
			next.ServeHTTP(w, r)
			return
		}

		if _, err := readStored(a.hashPath); err != nil {
			a.deny(w, r, false)
			return
		}

		keyRoute := strings.HasPrefix(p, proxyPrefix) ||
			(p == projectsPath && (r.Method == http.MethodGet || r.Method == http.MethodHead))

		source := sourceOf(r)
		if keyRoute && a.hasKey {
			if wait := a.lim.locked(source, time.Now()); wait > 0 {
				tooMany(w, wait)
				return
			}
			if presented, ok := a.keyMatches(r); ok {
				next.ServeHTTP(w, r)
				return
			} else if presented {
				a.recordFailure(source, "a wrong access key")
			}
		}
		if _, ok := a.validSession(r); ok {
			if unsafeMethod(r.Method) && !sameOrigin(r) {
				writeDetail(w, http.StatusForbidden, "That request did not come from this page.")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if keyRoute && !a.hasKey {
			// Open to tools, which send no Origin. A browser always does, so a page on another site
			// cannot drive the fixed address through a visitor's browser.
			if unsafeMethod(r.Method) && r.Header.Get("Origin") != "" && !sameOrigin(r) {
				writeDetail(w, http.StatusForbidden, "That request did not come from this page.")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		a.deny(w, r, keyRoute)
	})
}

// hostAllowed compares the Host header, without its port, with the names this program answers to.
func (a *Auth) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return a.hosts[strings.ToLower(strings.Trim(host, "[]"))]
}

func (a *Auth) recordFailure(source, what string) {
	locked, backstop := a.lim.fail(source, time.Now())
	switch {
	case backstop:
		a.logf(fmt.Sprintf("login: %s from %s; %d failures from anywhere in %d minutes, so everything is locked for %d seconds",
			what, source, backstopCount, int(backstopWindow.Minutes()), int(lockoutWindow.Seconds())))
	case locked:
		a.logf(fmt.Sprintf("login: %s from %s; %d in a row, so that address is locked for %d seconds",
			what, source, maxFailures, int(lockoutWindow.Seconds())))
	default:
		a.logf("login: " + what + " from " + source)
	}
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", fmt.Sprint(secs))
	writeDetail(w, http.StatusTooManyRequests, fmt.Sprintf("Too many wrong attempts. Try again in %d seconds.", secs))
}

// canonical reports whether p is already the form the mux would route it as.
func canonical(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	trimmed := p
	if len(trimmed) > 1 {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	return path.Clean(trimmed) == trimmed
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// deny answers a request that has no right to the route: the two pages send the browser to the
// login page, everything else gets a 401 the page and the tools can read.
func (a *Auth) deny(w http.ResponseWriter, r *http.Request, keyRoute bool) {
	if (r.URL.Path == "/" || r.URL.Path == "/console") && r.Method == http.MethodGet {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if keyRoute {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeDetail(w, http.StatusUnauthorized, "Not logged in.")
}

func writeDetail(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, "{\"detail\":%q}\n", detail)
}

// keyMatches compares an access key presented as a bearer token or as X-Api-Key with the
// configured one, in constant time (both sides are digested to one length first).
func (a *Auth) keyMatches(r *http.Request) (presented, ok bool) {
	var candidates []string
	if v := r.Header.Get("Authorization"); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		candidates = append(candidates, strings.TrimSpace(v[7:]))
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		candidates = append(candidates, strings.TrimSpace(v))
	}
	matched := 0
	for _, c := range candidates {
		sum := sha256.Sum256([]byte(c))
		matched |= subtle.ConstantTimeCompare(sum[:], a.keyDigest[:])
	}
	return len(candidates) > 0, matched == 1
}

// sameOrigin is the forged-request check: the Origin header (the Referer when there is none) must
// name this request's own host.
func sameOrigin(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func digestOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// validSession answers the session the request's cookie names, when it is live and was made under
// the password now on file.
func (a *Auth) validSession(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	stored, err := readStored(a.hashPath)
	if err != nil {
		return "", false
	}
	key := digestOf(c.Value)
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[key]
	if !ok {
		return "", false
	}
	if time.Now().After(s.expires) {
		delete(a.sessions, key)
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(s.stamp), []byte(stampOf(stored))) != 1 {
		delete(a.sessions, key)
		return "", false
	}
	return key, true
}

func (a *Auth) startSession(w http.ResponseWriter, r *http.Request, stored string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	a.mu.Lock()
	for k, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, k)
		}
	}
	a.sessions[digestOf(token)] = session{expires: now.Add(sessionLifetime), stamp: stampOf(stored)}
	a.mu.Unlock()
	http.SetCookie(w, a.cookie(r, token, int(sessionLifetime.Seconds())))
	return nil
}

func (a *Auth) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || a.cookieSecure,
	}
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeDetail(w, http.StatusMethodNotAllowed, "POST is the only method /logout answers.")
		return
	}
	if !sameOrigin(r) {
		writeDetail(w, http.StatusForbidden, "That request did not come from this page.")
		return
	}
	if key, ok := a.validSession(r); ok {
		a.mu.Lock()
		delete(a.sessions, key)
		a.mu.Unlock()
	}
	http.SetCookie(w, a.cookie(r, "", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	stored, err := readStored(a.hashPath)
	if err != nil && err != errNoPassword {
		a.logf("login: could not read the password file: " + err.Error())
		writeDetail(w, http.StatusInternalServerError, "The password file could not be read.")
		return
	}
	noPassword := err == errNoPassword

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if _, ok := a.validSession(r); ok {
			http.Redirect(w, r, "/console", http.StatusSeeOther)
			return
		}
		writeLoginPage(w, http.StatusOK, noPassword, "")
	case http.MethodPost:
		if !sameOrigin(r) {
			writeDetail(w, http.StatusForbidden, "That request did not come from this page.")
			return
		}
		if noPassword {
			writeLoginPage(w, http.StatusServiceUnavailable, true, "")
			return
		}
		// A slow body cannot hold the connection open: the form is small and arrives in seconds.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(loginReadTimeout))
		r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodySize)
		if err := r.ParseForm(); err != nil {
			writeLoginPage(w, http.StatusBadRequest, false, "That form could not be read.")
			return
		}
		a.attempt(w, r, stored, r.PostFormValue("password"))
	default:
		writeDetail(w, http.StatusMethodNotAllowed, "GET and POST are the only methods /login answers.")
	}
}

// attempt checks one password under the failure limiter (see limiter.go).
func (a *Auth) attempt(w http.ResponseWriter, r *http.Request, stored, password string) {
	a.attemptMu.Lock()
	defer a.attemptMu.Unlock()

	source := sourceOf(r)
	if wait := a.lim.locked(source, time.Now()); wait > 0 {
		lockedPage(w, wait)
		return
	}
	ok, err := verifyPassword(stored, password)
	if err != nil {
		a.logf("login: " + err.Error())
		writeDetail(w, http.StatusInternalServerError, "The password file could not be checked.")
		return
	}
	if !ok {
		a.recordFailure(source, "a wrong password")
		if wait := a.lim.locked(source, time.Now()); wait > 0 {
			lockedPage(w, wait)
			return
		}
		writeLoginPage(w, http.StatusUnauthorized, false, "That password was wrong.")
		return
	}
	a.lim.succeed(source)
	if err := a.startSession(w, r, stored); err != nil {
		a.logf("login: could not start a session: " + err.Error())
		writeDetail(w, http.StatusInternalServerError, "Could not start a session.")
		return
	}
	a.logf("login: a browser logged in from " + source)
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

func lockedPage(w http.ResponseWriter, remaining time.Duration) {
	secs := int(remaining.Seconds()) + 1
	w.Header().Set("Retry-After", fmt.Sprint(secs))
	writeLoginPage(w, http.StatusTooManyRequests, false,
		fmt.Sprintf("Too many wrong passwords in a row. Login is locked for %d more seconds.", secs))
}

// writeLoginPage draws the login page. It is one self-contained document: no outside fonts or
// scripts, so it loads on a machine with no route to the internet.
func writeLoginPage(w http.ResponseWriter, status int, noPassword bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", loginCSP)
	w.WriteHeader(status)
	body := ""
	if noPassword {
		body = `<p>No password is set yet. On the machine GPUGate runs on, in the project folder, run <code>./dist/gpugate set-password</code>, then reload this page.</p>`
	} else {
		if message != "" {
			body += `<p class="msg" role="alert">` + html.EscapeString(message) + `</p>`
		}
		body += `<form method="post" action="/login">
<label for="login-password">Password</label>
<input type="password" id="login-password" name="password" autocomplete="current-password" required autofocus>
<button type="submit">Log in</button>
</form>`
	}
	fmt.Fprintf(w, loginPage, body)
}

const loginPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>GPUGate login</title>
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<style>
:root { color-scheme: light dark; --bg: #f6f7f9; --fg: #1b1f24; --card: #fff; --line: #c9ced6; --accent: #1f5fbf; --bad: #b3261e; }
@media (prefers-color-scheme: dark) { :root { --bg: #14171b; --fg: #e6e8eb; --card: #1d2227; --line: #3a424b; --accent: #6ea3f0; --bad: #ff8a80; } }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, sans-serif; }
main { width: min(24rem, calc(100vw - 2rem)); background: var(--card); border: 1px solid var(--line); border-radius: 8px; padding: 1.5rem; }
h1 { margin: 0 0 1rem; font-size: 1.25rem; }
label { display: block; margin-bottom: .25rem; font-weight: 600; }
input { box-sizing: border-box; width: 100%%; padding: .6rem; font: inherit; color: inherit; background: var(--bg); border: 1px solid var(--line); border-radius: 6px; }
button { margin-top: 1rem; width: 100%%; padding: .65rem; font: inherit; font-weight: 600; color: #fff; background: var(--accent); border: 0; border-radius: 6px; cursor: pointer; }
.msg { color: var(--bad); font-weight: 600; }
code { word-break: break-all; }
</style>
</head>
<body>
<main>
<h1>GPUGate</h1>
%s
</main>
</body>
</html>
`
