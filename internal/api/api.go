// Package api implements the subset of the gpodder.net v2 API that AntennaPod
// uses for synchronisation: auth, devices, subscriptions and episode actions.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpodder-sync/internal/store"
)

const (
	maxBodyBytes = 16 << 20 // a first sync can upload thousands of episode actions
	maxURLLength = 4096
	tsLayout     = "2006-01-02T15:04:05"
)

// DefaultUserHeader is the header Authelia uses for the authenticated user.
const DefaultUserHeader = "Remote-User"

// Config configures a Server.
type Config struct {
	Store *store.Store
	// TrustedProxies are the only peers allowed to talk to the server; their
	// ProxyUserHeader is believed.
	TrustedProxies []netip.Prefix
	// ProxyUserHeader carries the username authenticated by the proxy
	// (default DefaultUserHeader).
	ProxyUserHeader string
	Logger          *slog.Logger
	Now             func() time.Time
}

// Server serves the gpodder API.
type Server struct {
	cfg Config
}

// New creates a Server.
func New(cfg Config) *Server {
	if cfg.ProxyUserHeader == "" {
		cfg.ProxyUserHeader = DefaultUserHeader
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg}
}

// Handler returns the HTTP handler.
//
// ServeMux wildcards must span whole path segments, so "{device}.json" is
// matched as one segment and the ".json" suffix is stripped by the handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /api/2/auth/{user}/login.json", s.login)
	mux.HandleFunc("POST /api/2/auth/{user}/logout.json", s.logout)
	mux.HandleFunc("GET /api/2/devices/{userfmt}", s.listDevices)
	mux.HandleFunc("POST /api/2/devices/{user}/{devicefmt}", s.updateDevice)
	mux.HandleFunc("GET /api/2/subscriptions/{user}/{devicefmt}", s.getSubscriptions)
	mux.HandleFunc("POST /api/2/subscriptions/{user}/{devicefmt}", s.postSubscriptions)
	mux.HandleFunc("GET /api/2/episodes/{userfmt}", s.getEpisodes)
	mux.HandleFunc("POST /api/2/episodes/{userfmt}", s.postEpisodes)
	return s.logRequests(mux)
}

// ---- handlers --------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Store.Ping(r.Context()); err != nil {
		s.serverError(w, r, err)
		return
	}
	io.WriteString(w, "ok\n")
}

// sessionCookie is the cookie gpodder.net clients expect from login.
const sessionCookie = "sessionid"

// login is reached with credentials the proxy has checked. Clients such as
// AntennaPod send them only here and then rely on the session cookie, which
// the proxy passes through without authenticating (see authorize).
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authorize(w, r, r.PathValue("user"))
	if !ok {
		return
	}
	token, err := s.cfg.Store.CreateSession(r.Context(), userID, s.cfg.Now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/api/2/",
		MaxAge: int(store.SessionIdle.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, r.PathValue("user")); !ok {
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.cfg.Store.DeleteSession(r.Context(), c.Value); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/api/2/", MaxAge: -1, HttpOnly: true, Secure: true})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	devices, err := s.cfg.Store.ListDevices(r.Context(), userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, devices)
}

var deviceTypes = map[string]bool{"desktop": true, "laptop": true, "mobile": true, "server": true, "other": true}

func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("user")
	deviceID, ok := trimJSON(r.PathValue("devicefmt"))
	if !ok || !store.ValidDeviceID(deviceID) {
		http.NotFound(w, r)
		return
	}
	userID, ok := s.authorize(w, r, username)
	if !ok {
		return
	}
	var body struct {
		Caption *string `json:"caption"`
		Type    *string `json:"type"`
	}
	if !readJSON(w, r, &body, true) {
		return
	}
	if body.Type != nil && !deviceTypes[*body.Type] {
		http.Error(w, "invalid device type", http.StatusBadRequest)
		return
	}
	if body.Caption != nil && len(*body.Caption) > 256 {
		http.Error(w, "caption too long", http.StatusBadRequest)
		return
	}
	if err := s.cfg.Store.UpdateDevice(r.Context(), userID, deviceID, body.Caption, body.Type, s.cfg.Now()); err != nil {
		s.serverError(w, r, err)
	}
}

func (s *Server) getSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.userAndDevice(w, r)
	if !ok {
		return
	}
	since, err := parseSince(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Take the timestamp before querying: changes are selected with ">= since",
	// so anything written during this request shows up on the next sync.
	now := s.cfg.Now().Unix()
	add, remove, err := s.cfg.Store.SubscriptionChanges(r.Context(), userID, since)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"add": add, "remove": remove, "timestamp": now})
}

func (s *Server) postSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID, deviceID, ok := s.userAndDevice(w, r)
	if !ok {
		return
	}
	var body struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if !readJSON(w, r, &body, false) {
		return
	}
	updateURLs := [][2]string{}
	clean := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, raw := range in {
			u := sanitizeURL(raw)
			if u != raw {
				updateURLs = append(updateURLs, [2]string{raw, u})
			}
			if u != "" {
				out = append(out, u)
			}
		}
		return out
	}
	add, remove := clean(body.Add), clean(body.Remove)
	inAdd := make(map[string]bool, len(add))
	for _, u := range add {
		inAdd[u] = true
	}
	for _, u := range remove {
		if inAdd[u] {
			http.Error(w, "URL in both add and remove: "+u, http.StatusBadRequest)
			return
		}
	}
	now := s.cfg.Now()
	if err := s.cfg.Store.UpdateSubscriptions(r.Context(), userID, deviceID, add, remove, now); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"timestamp": now.Unix(), "update_urls": updateURLs})
}

func (s *Server) getEpisodes(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	since, err := parseSince(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	agg := q.Get("aggregated")
	filter := store.EpisodeFilter{
		Since:      since,
		Podcast:    q.Get("podcast"),
		Device:     q.Get("device"),
		Aggregated: agg == "true" || agg == "1",
	}
	now := s.cfg.Now().Unix()
	actions, err := s.cfg.Store.EpisodeActions(r.Context(), userID, filter)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"actions": actions, "timestamp": now})
}

var episodeActions = map[string]bool{"download": true, "delete": true, "play": true, "new": true, "flattr": true}

func (s *Server) postEpisodes(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	var actions []store.EpisodeAction
	if !readJSON(w, r, &actions, false) {
		return
	}
	now := s.cfg.Now()
	for i := range actions {
		a := &actions[i]
		a.Action = strings.ToLower(a.Action)
		if err := validateAction(a, now); err != nil {
			http.Error(w, fmt.Sprintf("action %d: %v", i, err), http.StatusBadRequest)
			return
		}
	}
	if err := s.cfg.Store.AddEpisodeActions(r.Context(), userID, actions, now); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"timestamp": now.Unix(), "update_urls": [][2]string{}})
}

func validateAction(a *store.EpisodeAction, now time.Time) error {
	switch {
	case a.Podcast == "" || a.Episode == "":
		return errors.New("podcast and episode are required")
	case len(a.Podcast) > maxURLLength || len(a.Episode) > maxURLLength || len(a.GUID) > maxURLLength:
		return errors.New("field too long")
	case !episodeActions[a.Action]:
		return fmt.Errorf("unknown action %q", a.Action)
	case a.Device != "" && !store.ValidDeviceID(a.Device):
		return fmt.Errorf("invalid device %q", a.Device)
	}
	ts, err := normalizeTimestamp(a.Timestamp, now)
	if err != nil {
		return err
	}
	a.Timestamp = ts
	return nil
}

// normalizeTimestamp converts client timestamps to UTC "2006-01-02T15:04:05".
// AntennaPod sends exactly that format; RFC 3339 with offsets is accepted too.
func normalizeTimestamp(s string, now time.Time) (string, error) {
	if s == "" {
		return now.UTC().Format(tsLayout), nil
	}
	for _, layout := range []string{tsLayout, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(tsLayout), nil
		}
	}
	return "", fmt.Errorf("invalid timestamp %q", s)
}

// ---- authentication --------------------------------------------------------

// userFromPath authorizes routes of the form /api/2/<kind>/{user}.json.
func (s *Server) userFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	username, ok := trimJSON(r.PathValue("userfmt"))
	if !ok {
		http.NotFound(w, r)
		return 0, false
	}
	return s.authorize(w, r, username)
}

// userAndDevice authorizes routes of the form /api/2/<kind>/{user}/{device}.json.
func (s *Server) userAndDevice(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	deviceID, ok := trimJSON(r.PathValue("devicefmt"))
	if !ok || !store.ValidDeviceID(deviceID) {
		http.NotFound(w, r)
		return 0, "", false
	}
	userID, ok := s.authorize(w, r, r.PathValue("user"))
	return userID, deviceID, ok
}

// authorize checks that the request is authenticated as the user named in the
// URL. Only connections from a trusted proxy are accepted at all, so a client
// that reaches the server directly cannot impersonate anyone. The user comes
// from the proxy's user header when the proxy checked credentials, and
// otherwise from a session cookie issued by login.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, username string) (int64, bool) {
	if !store.ValidUsername(username) {
		http.NotFound(w, r)
		return 0, false
	}
	addr, host := remoteAddr(r)
	if !addr.IsValid() || !s.trusted(addr) {
		s.cfg.Logger.Warn("request bypassed the reverse proxy", "peer", host)
		http.Error(w, "forbidden", http.StatusForbidden)
		return 0, false
	}
	if user := r.Header.Get(s.cfg.ProxyUserHeader); user != "" {
		// A different user normally means the proxy is misconfigured.
		if user != username {
			s.authFailed(w, r, "username mismatch", user)
			return 0, false
		}
		id, err := s.cfg.Store.EnsureUser(r.Context(), user, s.cfg.Now())
		if err != nil {
			s.serverError(w, r, err)
			return 0, false
		}
		return id, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return 0, false
	}
	id, user, err := s.cfg.Store.SessionUser(r.Context(), c.Value, s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.authFailed(w, r, "unknown or expired session", "")
		return 0, false
	case err != nil:
		s.serverError(w, r, err)
		return 0, false
	case user != username:
		s.authFailed(w, r, "session username mismatch", user)
		return 0, false
	}
	return id, true
}

func (s *Server) authFailed(w http.ResponseWriter, r *http.Request, reason, user string) {
	_, host := remoteAddr(r)
	s.cfg.Logger.Warn("authentication failed", "reason", reason, "user", user,
		"peer", host, "forwarded_for", r.Header.Get("X-Forwarded-For"))
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// ---- proxies ---------------------------------------------------------------

func (s *Server) trusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) (netip.Addr, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	return addr, host
}

// ---- helpers ---------------------------------------------------------------

func trimJSON(segment string) (string, bool) {
	v, ok := strings.CutSuffix(segment, ".json")
	return v, ok && v != ""
}

func parseSince(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid since %q", v)
	}
	return n, nil
}

// sanitizeURL trims whitespace and rejects anything that is not an absolute
// http(s) URL, returning "" for rejected URLs as the gpodder API specifies.
func sanitizeURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" || len(u) > maxURLLength {
		return ""
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return ""
	}
	return u
}

// readJSON decodes the request body into v, writing an error response and
// returning false on failure. Requiring Content-Type: application/json stops
// cross-site HTML forms (which can only send form or text/plain bodies) from
// riding on credentials the browser has cached for this site.
func readJSON(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		if !(allowEmpty && r.ContentLength == 0) {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil || (allowEmpty && errors.Is(err, io.EOF)) {
		return true
	}
	http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) // headers are sent; an error here can't be reported
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" && rec.status == http.StatusOK {
			return
		}
		_, peer := remoteAddr(r)
		s.cfg.Logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond), "peer", peer,
			"forwarded_for", r.Header.Get("X-Forwarded-For"))
	})
}
