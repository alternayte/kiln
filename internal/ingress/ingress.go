// Package ingress serves published sandbox ports over HTTPS. It routes one
// hostname to one sandbox port, wakes a sleeping sandbox and holds the
// request while it wakes, limits wakes, and strips host credentials from
// guest traffic. Viewer sessions come from Auth-All on the same hostnames.
package ingress

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	authall "github.com/alternayte/auth-all"

	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
)

// AuthPrefix is where the viewer login lives on every published hostname.
const AuthPrefix = "/_kiln/auth"

// Wake limits. Over either limit the proxy refuses the request and queues
// nothing.
const (
	wakesPerMinute = 10
	wakesInFlight  = 4
	wakeRetryAfter = 30 * time.Second
)

// Config holds the ingress dependencies.
type Config struct {
	Zone      string
	Store     store.Store
	Sandboxes *sandbox.Manager
	// Auth is the viewer login. Nil refuses every team preview.
	Auth *Viewers
	// Now returns the current time. Tests set it.
	Now func() time.Time
}

// Server routes published hostnames to sandbox ports.
type Server struct {
	cfg Config

	// mu guards wakes and inFlight. wakes holds the wake times of each
	// subdomain in the rolling minute.
	mu       sync.Mutex
	wakes    map[string][]time.Time
	inFlight int
}

// New returns the preview server.
func New(cfg Config) *Server {
	return &Server{cfg: cfg, wakes: map[string][]time.Time{}}
}

// Handler returns the preview surface.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Every preview response is unindexable. A page of this server runs with
	// an opaque origin; the ingress lifts that for the guest's own responses.
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Security-Policy", "sandbox")
	sub, ok := subdomainOf(r.Host, s.cfg.Zone)
	if !ok {
		s.notFound(w, r)
		return
	}
	row, err := s.cfg.Store.PublishedBySubdomain(r.Context(), sub)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		log.Printf("ingress: lookup %s: %v", sub, err)
		http.Error(w, "the preview lookup failed", http.StatusBadGateway)
		return
	}
	sb, err := s.cfg.Store.GetSandbox(r.Context(), row.SandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		log.Printf("ingress: sandbox %s: %v", row.SandboxID, err)
		http.Error(w, "the preview lookup failed", http.StatusBadGateway)
		return
	}
	if sb.DestroyedAt != nil || sb.State == store.SandboxDestroyed || sb.State == store.SandboxFailed {
		s.notFound(w, r)
		return
	}
	viewerFork := false
	if _, err := s.cfg.Store.GetViewerOrigin(r.Context(), row.SandboxID); err == nil {
		viewerFork = true
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("ingress: viewer origin %s: %v", row.SandboxID, err)
		http.Error(w, "the preview lookup failed", http.StatusBadGateway)
		return
	}
	waking := sb.State == store.SandboxSleeping || sb.State == store.SandboxWaking
	// The viewer login is served on every published hostname, and nowhere
	// else. Only the exact prefix counts, so a decorated path cannot claim to
	// be the login.
	if r.URL.Path == AuthPrefix || strings.HasPrefix(r.URL.Path, AuthPrefix+"/") {
		s.serveAuth(w, r)
		return
	}
	// A control path on the ingress port is a 404 and is never routed. The
	// cleaned path is checked, so //v1 or /./v1 cannot slip past it.
	if cleaned := path.Clean(r.URL.Path); cleaned == "/v1" || strings.HasPrefix(cleaned, "/v1/") {
		s.notFound(w, r)
		return
	}
	toGuest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.proxy(w, r, row, waking)
	})
	if row.Visibility != store.VisibilityTeam {
		toGuest.ServeHTTP(w, r)
		return
	}
	if s.cfg.Auth == nil {
		http.Error(w, "a team preview needs a viewer session, and no viewer store is configured", http.StatusServiceUnavailable)
		return
	}
	// The session is checked before anything reaches the guest, and the
	// viewer of another tenant is refused the same way as a caller with no
	// session at all, so a hostname leaks nothing.
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := authall.PrincipalFrom(r.Context())
		if p == nil || p.User == nil || !s.cfg.Auth.Belongs(r.Context(), row.TenantID, p.User.ID) {
			s.challenge(w, r)
			return
		}
		if viewerFork {
			s.serveViewerFork(w, r, row, p.User.ID)
			return
		}
		toGuest.ServeHTTP(w, r)
	})
	s.cfg.Auth.Auth.RequireAuth(guarded).ServeHTTP(w, r)
}

// serveViewerFork routes one viewer to its own fork, and serves the page that
// resets it. A visit never wakes the origin.
func (s *Server) serveViewerFork(w http.ResponseWriter, r *http.Request, row store.Published, viewer string) {
	ctx := store.WithTenant(r.Context(), row.TenantID)
	switch path.Clean(r.URL.Path) {
	case viewerForkPage:
		s.viewerForkPage(w, r)
		return
	case viewerForkReset:
		s.resetViewerFork(w, r, row, viewer)
		return
	}
	id, live, err := s.cfg.Sandboxes.LiveViewerFork(ctx, row.SandboxID, viewer)
	if err == nil && !live {
		if !s.allowWake(row.Subdomain) {
			w.Header().Set("Retry-After", strconv.Itoa(int(wakeRetryAfter.Seconds())))
			http.Error(w, "the wake limit is reached; retry later", http.StatusServiceUnavailable)
			return
		}
		defer s.releaseWake()
		id, err = s.cfg.Sandboxes.ViewerFork(ctx, row.SandboxID, viewer)
	}
	if err != nil {
		var limit *sandbox.LimitError
		switch {
		case errors.As(err, &limit):
			s.limitPage(w, limit.Message)
		case errors.Is(err, store.ErrNotFound):
			s.notFound(w, r)
		default:
			log.Printf("ingress: %s: viewer fork: %v", row.Subdomain, err)
			http.Error(w, "your copy of this preview did not start", http.StatusServiceUnavailable)
		}
		return
	}
	row.SandboxID = id
	s.proxy(w, r, row, false)
}

// Paths on a viewer fork hostname that never reach a guest.
const (
	viewerForkPage  = "/_kiln/fork"
	viewerForkReset = "/_kiln/fork/reset"
)

// viewerForkPage offers the reset. The page is this server's own and holds no
// script, so it replaces the sandbox policy with one that lets its form post.
func (s *Server) viewerForkPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Your copy of this preview</title></head>
<body><h1>Your copy of this preview</h1><p>A reset removes your changes. Your next visit starts from the original state.</p>
<form method="post" action="%s"><button>Reset my copy</button></form></body></html>
`, viewerForkReset)
}

// resetViewerFork destroys the fork of the viewer who asks. Only a page of this
// hostname may ask, so another site cannot reset a viewer's copy. The guest's
// own pages share the hostname, and a reset only ends the copy they run in.
func (s *Server) resetViewerFork(w http.ResponseWriter, r *http.Request, row store.Published, viewer string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host == "" || !strings.EqualFold(origin.Host, r.Host) {
		http.Error(w, "the reset must come from this preview's page", http.StatusForbidden)
		return
	}
	if err := s.cfg.Sandboxes.ResetViewerFork(store.WithTenant(r.Context(), row.TenantID), row.SandboxID, viewer); err != nil {
		log.Printf("ingress: %s: reset a viewer fork: %v", row.Subdomain, err)
		http.Error(w, "the reset failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// limitPage names the cap that refused a viewer. A silent failure looks like an
// application bug.
func (s *Server) limitPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", strconv.Itoa(int(wakeRetryAfter.Seconds())))
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Preview limit reached</title></head>
<body><h1>Preview limit reached</h1><p>%s. A copy that nobody uses ends after its idle time, so retry later.</p></body></html>
`, html.EscapeString(message))
}

// serveAuth mounts the viewer login. Only sign-in, sign-out and the session
// read are served: there is no self-signup and no email flow.
func (s *Server) serveAuth(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth == nil {
		s.notFound(w, r)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, AuthPrefix) {
	case "/session", "/sign-in/email", "/sign-out":
		s.cfg.Auth.Auth.Handler().ServeHTTP(w, r)
	default:
		s.notFound(w, r)
	}
}

// challenge answers a viewer who has no session for this tenant. It is the
// same answer as no session at all, so a caller learns nothing about which
// tenant owns a hostname.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"Authentication is required."}}`))
}

// proxy forwards one request to the guest port. A sleeping sandbox wakes
// first and the request waits for the restore.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, row store.Published, waking bool) {
	if waking && !s.allowWake(row.Subdomain) {
		w.Header().Set("Retry-After", strconv.Itoa(int(wakeRetryAfter.Seconds())))
		http.Error(w, "the wake limit is reached; retry later", http.StatusServiceUnavailable)
		return
	}
	if waking {
		defer s.releaseWake()
	}
	conn, err := s.cfg.Sandboxes.DialGuest(r.Context(), row.SandboxID, row.GuestPort)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.notFound(w, r)
		case waking:
			http.Error(w, "the sandbox did not wake", http.StatusServiceUnavailable)
		default:
			log.Printf("ingress: %s: dial the guest: %v", row.Subdomain, err)
			http.Error(w, "the sandbox is not reachable", http.StatusBadGateway)
		}
		return
	}
	defer conn.Close()

	var once sync.Once
	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var out net.Conn
			once.Do(func() { out = conn })
			if out == nil {
				return nil, errors.New("ingress: one connection per request")
			}
			return out, nil
		},
		DisableKeepAlives: true,
	}
	host := net.JoinHostPort(guestAddress, strconv.Itoa(row.GuestPort))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = host
			// The guest sees the preview hostname, not the guest address, so
			// a dev server can build correct URLs.
			pr.Out.Host = pr.In.Host
			// No host credential, viewer session or control token enters
			// the guest. The guest's own cookies pass, so an application
			// that signs in with a cookie works.
			dropKilnCookies(pr.Out.Header)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Proxy-Authorization")
		},
		Transport: transport,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("X-Robots-Tag", "noindex")
			stripCookieDomains(resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("X-Robots-Tag", "noindex")
			w.Header().Set("Content-Security-Policy", "sandbox")
			log.Printf("ingress: %s: %v", row.Subdomain, err)
			http.Error(w, "the sandbox closed the connection", http.StatusBadGateway)
		},
		FlushInterval: 100 * time.Millisecond,
	}
	// A guest response keeps its own Content-Security-Policy. A sandbox
	// policy blocks every script, so no client-rendered application would run
	// behind a preview (#31). ModifyResponse sets noindex on the guest
	// response, and the reverse handler adds that to these headers, so drop both here.
	w.Header().Del("Content-Security-Policy")
	w.Header().Del("X-Robots-Tag")
	proxy.ServeHTTP(w, r)
}

// guestAddress is the address every guest port answers on. The attachment's
// mark and TAP device select which sandbox receives the connection.
const guestAddress = "172.31.0.2"

// allowWake counts one wake against the per-subdomain and host-wide limits.
// It returns false when either limit is reached.
func (s *Server) allowWake(subdomain string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	cutoff := now.Add(-time.Minute)
	kept := s.wakes[subdomain][:0]
	for _, at := range s.wakes[subdomain] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= wakesPerMinute || s.inFlight >= wakesInFlight {
		s.wakes[subdomain] = kept
		return false
	}
	s.wakes[subdomain] = append(kept, now)
	s.inFlight++
	return true
}

func (s *Server) releaseWake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight > 0 {
		s.inFlight--
	}
}

func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintln(w, "no such preview")
}

// subdomainOf returns the single label of host below zone. A host outside
// the zone, the zone itself or a deeper name has no preview.
func subdomainOf(host, zone string) (string, bool) {
	if zone == "" {
		return "", false
	}
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	zone = strings.ToLower(strings.TrimSuffix(zone, "."))
	suffix := "." + zone
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || len(label) > 63 || strings.Contains(label, ".") {
		return "", false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i != 0 && i != len(label)-1:
		default:
			return "", false
		}
	}
	return label, true
}

// stripCookieDomains makes every guest cookie host-only, so one preview
// cannot set a cookie for the whole zone, and drops a guest cookie that takes
// the name of the viewer session, so a preview cannot shadow it.
func stripCookieDomains(h http.Header) {
	cookies := h.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, cookie := range cookies {
		name, _, _ := strings.Cut(cookie, "=")
		if isKilnCookie(name) {
			continue
		}
		h.Add("Set-Cookie", dropDomainAttribute(cookie))
	}
}

// dropKilnCookies removes the viewer session from a request to the guest and
// keeps every other cookie. Only the viewer session covers the whole zone,
// because a guest cookie is host-only.
func dropKilnCookies(h http.Header) {
	lines := h.Values("Cookie")
	h.Del("Cookie")
	var kept []string
	for _, line := range lines {
		for _, pair := range strings.Split(line, ";") {
			pair = strings.TrimSpace(pair)
			name, _, _ := strings.Cut(pair, "=")
			if pair == "" || isKilnCookie(name) {
				continue
			}
			kept = append(kept, pair)
		}
	}
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

func isKilnCookie(name string) bool {
	return strings.TrimSpace(name) == ViewerCookie
}

// dropDomainAttribute removes the Domain attribute from one Set-Cookie value.
func dropDomainAttribute(cookie string) string {
	parts := strings.Split(cookie, ";")
	kept := parts[:0]
	for _, part := range parts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "domain=") {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, ";")
}
