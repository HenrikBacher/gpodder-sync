package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gpodder-sync/internal/store"
)

const proxyAddr = "10.0.0.2:4433" // inside the trusted 10.0.0.0/8

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type env struct {
	t     *testing.T
	h     http.Handler
	st    *store.Store
	clock *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	srv := New(Config{
		Store:          st,
		Now:            c.Now,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	return &env{t: t, h: srv.Handler(), st: st, clock: c}
}

type req struct {
	method, path string
	user         string // value of the Remote-User header ("" = not set)
	remote       string // peer address (default: the trusted proxy)
	xff          string
	cookie       string // sessionid cookie value
	body         any    // JSON-encoded
	rawBody      string // sent as-is instead of body
	contentType  string // default application/json when a body is set
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	e.t.Helper()
	body := bytes.NewBufferString(r.rawBody)
	if r.body != nil {
		if err := json.NewEncoder(body).Encode(r.body); err != nil {
			e.t.Fatal(err)
		}
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	switch {
	case r.contentType != "":
		hr.Header.Set("Content-Type", r.contentType)
	case body.Len() > 0:
		hr.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	hr.RemoteAddr = proxyAddr
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	if r.user != "" {
		hr.Header.Set("Remote-User", r.user)
	}
	if r.xff != "" {
		hr.Header.Set("X-Forwarded-For", r.xff)
	}
	if r.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: sessionCookie, Value: r.cookie})
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, hr)
	return rec
}

func (e *env) alice(method, path string, body any) *httptest.ResponseRecorder {
	return e.do(req{method: method, path: path, user: "alice", body: body})
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	path := "/api/2/devices/alice.json"

	wantStatus(t, e.do(req{method: "GET", path: path, user: "alice"}), http.StatusOK)
	wantStatus(t, e.do(req{method: "POST", path: "/api/2/auth/alice/login.json", user: "alice"}), http.StatusOK)
	// A client that reaches the server directly cannot claim to be anyone.
	wantStatus(t, e.do(req{method: "GET", path: path, user: "alice", remote: "192.0.2.1:1"}), http.StatusForbidden)
	// ...even if it forges X-Forwarded-For to look like the proxy.
	wantStatus(t, e.do(req{method: "GET", path: path, user: "alice", remote: "192.0.2.1:1", xff: "10.0.0.2"}), http.StatusForbidden)
	// The proxy forwarded no user: misconfigured proxy, refuse.
	wantStatus(t, e.do(req{method: "GET", path: path}), http.StatusUnauthorized)
	// Bob, authenticated by the proxy, cannot read Alice's data.
	wantStatus(t, e.do(req{method: "GET", path: path, user: "bob"}), http.StatusUnauthorized)
	wantStatus(t, e.do(req{method: "GET", path: "/api/2/devices/bad%20name.json", user: "bad name"}), http.StatusNotFound)
	// Email-style names, as Authelia/LDAP often use.
	wantStatus(t, e.do(req{method: "GET", path: "/api/2/devices/carol@example.com.json", user: "carol@example.com"}), http.StatusOK)
	// Health checks come from localhost inside the container.
	wantStatus(t, e.do(req{method: "GET", path: "/healthz", remote: "127.0.0.1:9"}), http.StatusOK)

	users, err := e.st.ListUsers(context.Background())
	if err != nil || len(users) != 2 || users[0] != "alice" || users[1] != "carol@example.com" {
		t.Fatalf("users = %v, %v (only alice and carol should have been provisioned)", users, err)
	}
}

// AntennaPod sends credentials only on login; the proxy passes later
// cookie-only requests through without a user header.
func TestSessions(t *testing.T) {
	e := newEnv(t)
	path := "/api/2/devices/alice.json"
	login := func(user string) string {
		t.Helper()
		rec := e.do(req{method: "POST", path: "/api/2/auth/" + user + "/login.json", user: user})
		wantStatus(t, rec, http.StatusOK)
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie && c.Value != "" && c.HttpOnly && c.Secure && c.Path == "/api/2/" {
				return c.Value
			}
		}
		t.Fatalf("login set no usable session cookie: %v", rec.Result().Cookies())
		return ""
	}
	tok := login("alice")

	wantStatus(t, e.do(req{method: "GET", path: path, cookie: tok}), http.StatusOK)
	wantStatus(t, e.do(req{method: "POST", path: "/api/2/devices/alice/phone.json", cookie: tok}), http.StatusOK)
	// The cookie is only good for its own user, and only via the proxy.
	wantStatus(t, e.do(req{method: "GET", path: "/api/2/devices/bob.json", cookie: tok}), http.StatusUnauthorized)
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: tok, remote: "192.0.2.1:1"}), http.StatusForbidden)
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: "forged"}), http.StatusUnauthorized)
	// A proxy-authenticated user wins over a cookie for someone else.
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: login("bob"), user: "alice"}), http.StatusOK)

	// Sessions expire after SessionIdle without use; use keeps them alive.
	e.clock.t = e.clock.t.Add(store.SessionIdle - time.Hour)
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: tok}), http.StatusOK)
	e.clock.t = e.clock.t.Add(store.SessionIdle - time.Hour)
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: tok}), http.StatusOK)
	e.clock.t = e.clock.t.Add(store.SessionIdle + time.Second)
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: tok}), http.StatusUnauthorized)

	tok = login("alice")
	wantStatus(t, e.do(req{method: "POST", path: "/api/2/auth/alice/logout.json", cookie: tok}), http.StatusOK)
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: tok}), http.StatusUnauthorized)

	// Deleting a user ends their sessions.
	tok = login("alice")
	if err := e.st.DeleteUser(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.do(req{method: "GET", path: path, cookie: tok}), http.StatusUnauthorized)
}

// A cross-site HTML form can only send form or text/plain bodies, so
// requiring application/json keeps other websites from posting changes with
// credentials the browser has cached for this site.
func TestRequiresJSONContentType(t *testing.T) {
	e := newEnv(t)
	// What <form enctype="text/plain"> produces: valid JSON, wrong type.
	form := `{"add":["https://evil.example/feed.xml"],"x":"="}`
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		rec := e.do(req{method: "POST", path: "/api/2/subscriptions/alice/phone.json", user: "alice", rawBody: form, contentType: ct})
		wantStatus(t, rec, http.StatusUnsupportedMediaType)
	}
	wantStatus(t, e.do(req{method: "POST", path: "/api/2/episodes/alice.json", user: "alice", rawBody: "[]", contentType: "text/plain"}),
		http.StatusUnsupportedMediaType)
	if subs := decode[subsResp](t, e.alice("GET", "/api/2/subscriptions/alice/phone.json", nil)); len(subs.Add) != 0 {
		t.Fatalf("form post was stored: %+v", subs)
	}
	// Device registration may have an empty body without a Content-Type.
	wantStatus(t, e.do(req{method: "POST", path: "/api/2/devices/alice/phone.json", user: "alice"}), http.StatusOK)
	wantStatus(t, e.do(req{method: "POST", path: "/api/2/subscriptions/alice/phone.json", user: "alice", rawBody: "{not json", contentType: "application/json"}),
		http.StatusBadRequest)
}

func TestDevices(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.alice("POST", "/api/2/devices/alice/phone.json", map[string]string{"caption": "Pixel", "type": "mobile"}), http.StatusOK)
	wantStatus(t, e.alice("POST", "/api/2/devices/alice/phone.json", map[string]string{"caption": "Pixel 9"}), http.StatusOK)
	wantStatus(t, e.alice("POST", "/api/2/devices/alice/phone.json", map[string]string{"type": "toaster"}), http.StatusBadRequest)
	wantStatus(t, e.alice("POST", "/api/2/devices/alice/bad%20id.json", nil), http.StatusNotFound)

	devices := decode[[]store.Device](t, e.alice("GET", "/api/2/devices/alice.json", nil))
	if len(devices) != 1 || devices[0].ID != "phone" || devices[0].Caption != "Pixel 9" || devices[0].Type != "mobile" {
		t.Fatalf("devices = %+v", devices)
	}
}

type subsResp struct {
	Add       []string `json:"add"`
	Remove    []string `json:"remove"`
	Timestamp int64    `json:"timestamp"`
}

func TestSubscriptions(t *testing.T) {
	e := newEnv(t)
	path := "/api/2/subscriptions/alice/phone.json"
	a, b := "https://example.com/a.xml", "https://example.com/b.xml"

	up := decode[struct {
		Timestamp  int64       `json:"timestamp"`
		UpdateURLs [][2]string `json:"update_urls"`
	}](t, e.alice("POST", path, map[string][]string{"add": {a, "  " + b + " ", "ftp://nope"}}))
	if len(up.UpdateURLs) != 2 || up.UpdateURLs[0][1] != b || up.UpdateURLs[1][1] != "" {
		t.Errorf("update_urls = %v", up.UpdateURLs)
	}

	all := decode[subsResp](t, e.alice("GET", path+"?since=0", nil))
	if len(all.Add) != 2 || len(all.Remove) != 0 {
		t.Fatalf("since=0: %+v", all)
	}

	// Another device sees the same subscriptions (shared per user).
	other := decode[subsResp](t, e.alice("GET", "/api/2/subscriptions/alice/laptop.json", nil))
	if len(other.Add) != 2 {
		t.Fatalf("laptop: %+v", other)
	}

	e.clock.t = e.clock.t.Add(time.Minute)
	wantStatus(t, e.alice("POST", "/api/2/subscriptions/alice/laptop.json", map[string][]string{"remove": {a}}), http.StatusOK)
	delta := decode[subsResp](t, e.alice("GET", path+"?since="+itoa(all.Timestamp+1), nil))
	if len(delta.Add) != 0 || len(delta.Remove) != 1 || delta.Remove[0] != a {
		t.Fatalf("delta: %+v", delta)
	}

	// Re-adding an existing subscription is not reported as a change.
	e.clock.t = e.clock.t.Add(time.Minute)
	ts := e.clock.t.Unix()
	wantStatus(t, e.alice("POST", path, map[string][]string{"add": {b}}), http.StatusOK)
	if d := decode[subsResp](t, e.alice("GET", path+"?since="+itoa(ts), nil)); len(d.Add)+len(d.Remove) != 0 {
		t.Fatalf("re-add reported as change: %+v", d)
	}

	wantStatus(t, e.alice("POST", path, map[string][]string{"add": {a}, "remove": {a}}), http.StatusBadRequest)
	wantStatus(t, e.alice("GET", path+"?since=abc", nil), http.StatusBadRequest)

	// Bob's view is empty.
	bob := decode[subsResp](t, e.do(req{method: "GET", path: "/api/2/subscriptions/bob/phone.json", user: "bob"}))
	if len(bob.Add) != 0 {
		t.Fatalf("bob sees alice's subscriptions: %+v", bob)
	}
}

type episodesResp struct {
	Actions   []store.EpisodeAction `json:"actions"`
	Timestamp int64                 `json:"timestamp"`
}

func TestEpisodeActions(t *testing.T) {
	e := newEnv(t)
	path := "/api/2/episodes/alice.json"
	pod, other := "https://example.com/feed.xml", "https://example.org/feed.xml"

	wantStatus(t, e.alice("POST", path, []map[string]any{
		{"podcast": pod, "episode": "https://example.com/1.mp3", "action": "download", "timestamp": "2026-01-01T10:00:00"},
		{"podcast": pod, "episode": "https://example.com/1.mp3", "action": "PLAY", "timestamp": "2026-01-01T11:00:00",
			"started": 0, "position": 120, "total": 3600, "device": "phone"},
		{"podcast": other, "episode": "https://example.org/9.mp3", "action": "play", "timestamp": "2026-01-01T12:00:00+02:00",
			"started": 0, "position": 5, "total": 60},
	}), http.StatusOK)

	all := decode[episodesResp](t, e.alice("GET", path+"?since=0", nil))
	if len(all.Actions) != 3 {
		t.Fatalf("actions = %+v", all.Actions)
	}
	// Ordered by timestamp; the +02:00 offset is normalised to UTC (10:00).
	if all.Actions[2].Timestamp != "2026-01-01T11:00:00" || all.Actions[1].Timestamp != "2026-01-01T10:00:00" {
		t.Errorf("timestamps = %s, %s, %s", all.Actions[0].Timestamp, all.Actions[1].Timestamp, all.Actions[2].Timestamp)
	}

	agg := decode[episodesResp](t, e.alice("GET", path+"?aggregated=true&podcast="+pod, nil))
	if len(agg.Actions) != 1 || agg.Actions[0].Action != "play" || *agg.Actions[0].Position != 120 || agg.Actions[0].Device != "phone" {
		t.Fatalf("aggregated = %+v", agg.Actions)
	}

	// Only actions received at or after since are returned.
	e.clock.t = e.clock.t.Add(time.Hour)
	since := e.clock.t.Unix()
	wantStatus(t, e.alice("POST", path, []map[string]any{
		{"podcast": pod, "episode": "https://example.com/1.mp3", "action": "delete", "timestamp": "2026-01-02T09:00:00"},
	}), http.StatusOK)
	if d := decode[episodesResp](t, e.alice("GET", path+"?since="+itoa(since), nil)); len(d.Actions) != 1 || d.Actions[0].Action != "delete" {
		t.Fatalf("delta = %+v", d.Actions)
	}

	// The upload registered the device.
	devices := decode[[]store.Device](t, e.alice("GET", "/api/2/devices/alice.json", nil))
	if len(devices) != 1 || devices[0].ID != "phone" {
		t.Fatalf("devices = %+v", devices)
	}

	for _, bad := range []map[string]any{
		{"podcast": pod, "episode": "x", "action": "explode"},
		{"podcast": pod, "action": "play"},
		{"podcast": pod, "episode": "x", "action": "play", "timestamp": "yesterday"},
		{"podcast": pod, "episode": "x", "action": "play", "device": "bad device"},
	} {
		wantStatus(t, e.alice("POST", path, []map[string]any{bad}), http.StatusBadRequest)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
