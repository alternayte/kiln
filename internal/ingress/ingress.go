// Package ingress serves published sandbox ports over HTTPS. It routes one
// hostname to one sandbox port, wakes a sleeping sandbox and holds the
// request while it wakes, limits wakes, and strips host credentials from
// guest traffic. Viewer sessions come from Auth-All on the same hostnames.
package ingress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
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
	Auth *authall.Auth
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
	// Every preview response is unindexable and runs with an opaque origin.
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
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.proxy(w, r, row, waking)
	})
	if row.Visibility != store.VisibilityTeam {
		proxy.ServeHTTP(w, r)
		return
	}
	if s.cfg.Auth == nil {
		http.Error(w, "a team preview needs a viewer session, and no viewer store is configured", http.StatusServiceUnavailable)
		return
	}
	// The session is checked before anything reaches the guest.
	s.cfg.Auth.RequireAuth(proxy).ServeHTTP(w, r)
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
		s.cfg.Auth.Handler().ServeHTTP(w, r)
	default:
		s.notFound(w, r)
	}
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
			// No host credential, cookie or control token enters the guest.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Proxy-Authorization")
		},
		Transport: transport,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("X-Robots-Tag", "noindex")
			resp.Header.Set("Content-Security-Policy", "sandbox")
			stripCookieDomains(resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("ingress: %s: %v", row.Subdomain, err)
			http.Error(w, "the sandbox closed the connection", http.StatusBadGateway)
		},
		FlushInterval: 100 * time.Millisecond,
	}
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
// cannot set a cookie for the whole zone.
func stripCookieDomains(h http.Header) {
	cookies := h.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, cookie := range cookies {
		h.Add("Set-Cookie", dropDomainAttribute(cookie))
	}
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
