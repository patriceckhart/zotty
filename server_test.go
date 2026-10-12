package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testToken = "0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s := &Server{Token: testToken, Origins: []string{"https://web.zot.sh"}, Shell: "/bin/sh", Dir: t.TempDir()}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http") + "/term"
}

func dial(t *testing.T, ctx context.Context, u, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
}

func sendJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRejectsWrongToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, u := newTestServer(t)
	c, _, err := dial(t, ctx, u, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	sendJSON(t, ctx, c, map[string]any{"type": "auth", "token": "wrong-token-wrong-token"})
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("want policy violation close, got %v", err)
	}
}

func TestRejectsForeignOrigin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, u := newTestServer(t)
	_, resp, err := dial(t, ctx, u, "https://evil.example")
	if err == nil {
		t.Fatal("dial with foreign origin succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %v", resp)
	}
}

func TestOriginAllowed(t *testing.T) {
	s := &Server{Origins: []string{"https://web.zot.sh"}}
	for origin, want := range map[string]bool{
		"":                        true,
		"https://web.zot.sh":      true,
		"https://web.zot.sh/":     true,
		"http://localhost:3000":   true,
		"http://127.0.0.1:7790":   true,
		"https://web.zot.sh.evil": false,
		"https://evil.example":    false,
	} {
		if got := s.originAllowed(origin); got != want {
			t.Errorf("originAllowed(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestShellRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, u := newTestServer(t)
	c, _, err := dial(t, ctx, u, "https://web.zot.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	sendJSON(t, ctx, c, map[string]any{"type": "auth", "token": testToken, "cols": 100, "rows": 30})

	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText || !bytes.Contains(data, []byte(`"ready"`)) {
		t.Fatalf("want ready, got %v %q %v", typ, data, err)
	}
	sendJSON(t, ctx, c, map[string]any{"type": "resize", "cols": 91, "rows": 17})
	if err := c.Write(ctx, websocket.MessageBinary, []byte("stty size; echo zt-$((40+2)); exit 3\n")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (output so far %q)", err, out.String())
		}
		if typ == websocket.MessageBinary {
			out.Write(data)
			continue
		}
		var msg control
		if json.Unmarshal(data, &msg) == nil && msg.Type == "exit" {
			if msg.Code == nil || *msg.Code != 3 {
				t.Fatalf("exit code = %v, want 3", msg.Code)
			}
			break
		}
	}
	if !strings.Contains(out.String(), "zt-42") {
		t.Fatalf("output missing command result: %q", out.String())
	}
	if !strings.Contains(out.String(), "17 91") {
		t.Fatalf("resize not applied: %q", out.String())
	}
}

func TestUpstreamForwardsWebSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fake zot web endpoint: echoes one frame and reports the request.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(r.URL.Path+" "+r.Header.Get("Origin")+" "+string(data)))
	}))
	defer upstream.Close()

	proxy, _, err := newUpstream(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Token: testToken, Shell: "/bin/sh", Dir: t.TempDir(), Upstream: proxy}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")

	c, _, err := dial(t, ctx, base+"/", "https://web.zot.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, got, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "/ https://web.zot.sh hello" {
		t.Fatalf("upstream saw %q", got)
	}

	// /term is still served by zotty, not forwarded.
	term, _, err := dial(t, ctx, base+"/term", "")
	if err != nil {
		t.Fatal(err)
	}
	defer term.CloseNow()
	sendJSON(t, ctx, term, map[string]any{"type": "auth", "token": testToken})
	_, data, err := term.Read(ctx)
	if err != nil || !bytes.Contains(data, []byte(`"ready"`)) {
		t.Fatalf("want ready from /term, got %q %v", data, err)
	}
}

func TestNewUpstreamRejectsBadAddress(t *testing.T) {
	for _, a := range []string{"ftp://x", "http://"} {
		if _, _, err := newUpstream(a); err == nil {
			t.Errorf("newUpstream(%q) succeeded", a)
		}
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "token")
	tok, created, err := loadOrCreateToken(p)
	if err != nil || !created || len(tok) != 64 {
		t.Fatalf("create: %q %v %v", tok, created, err)
	}
	again, created, err := loadOrCreateToken(p)
	if err != nil || created || again != tok {
		t.Fatalf("reload: %q %v %v", again, created, err)
	}
}
