package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// Wire protocol on /term:
//
//	client -> server, text:   {"type":"auth","token":"...","cols":80,"rows":24}  (first frame, required)
//	client -> server, text:   {"type":"resize","cols":120,"rows":40}
//	client -> server, binary: raw terminal input
//	server -> client, text:   {"type":"ready"}
//	server -> client, text:   {"type":"exit","code":0}
//	server -> client, binary: raw terminal output
//
// The token travels in the first frame instead of the URL so it does not end
// up in proxy or tunnel access logs.

const (
	authTimeout = 10 * time.Second
	maxSessions = 8
	maxControl  = 4 << 10
)

// Server bridges WebSocket connections to shells running in a PTY.
type Server struct {
	Token   string
	Origins []string
	Shell   string
	Dir     string
	// Upstream, when set, receives every request except /term and /health.
	// This lets one tunnel serve both zot and zotty.
	Upstream http.Handler

	mu       sync.Mutex
	sessions int
}

type control struct {
	Type  string `json:"type"`
	Token string `json:"token,omitempty"`
	Cols  uint16 `json:"cols,omitempty"`
	Rows  uint16 `json:"rows,omitempty"`
	Code  *int   `json:"code,omitempty"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/term", s.serveTerm)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	if s.Upstream != nil {
		mux.Handle("/", s.Upstream)
	}
	return mux
}

// originAllowed accepts requests without an Origin (non-browser clients,
// which still need the token), localhost origins, and configured origins.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	origin = strings.TrimRight(origin, "/")
	for _, o := range s.Origins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func (s *Server) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions >= maxSessions {
		return false
	}
	s.sessions++
	return true
}

func (s *Server) release() {
	s.mu.Lock()
	s.sessions--
	s.mu.Unlock()
}

func (s *Server) serveTerm(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	// The origin was checked above; the library check would only allow
	// same-host origins.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	auth, err := readAuth(r.Context(), conn)
	if err != nil || subtle.ConstantTimeCompare([]byte(auth.Token), []byte(s.Token)) != 1 {
		conn.Close(websocket.StatusPolicyViolation, "unauthorized")
		return
	}
	if !s.acquire() {
		conn.Close(websocket.StatusTryAgainLater, "too many sessions")
		return
	}
	defer s.release()

	if err := s.run(r.Context(), conn, auth); err != nil {
		log.Printf("zotty: session: %v", err)
		conn.Close(websocket.StatusInternalError, truncate(err.Error(), 100))
		return
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func readAuth(ctx context.Context, conn *websocket.Conn) (control, error) {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return control{}, err
	}
	var c control
	if typ != websocket.MessageText || len(data) > maxControl || json.Unmarshal(data, &c) != nil || c.Type != "auth" {
		return control{}, errors.New("expected auth frame")
	}
	return c, nil
}

func (s *Server) run(ctx context.Context, conn *websocket.Conn, auth control) error {
	cmd := exec.Command(s.Shell, "-l")
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	size := &pty.Winsize{Cols: orDefault(auth.Cols, 80), Rows: orDefault(auth.Rows, 24)}
	f, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	defer func() {
		_ = f.Close()
		// Closing the PTY hangs up the session. Escalate if the shell
		// ignores it.
		_ = cmd.Process.Signal(syscall.SIGHUP)
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := writeControl(ctx, conn, control{Type: "ready"}); err != nil {
		return nil
	}

	// PTY output -> WebSocket.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WebSocket -> PTY input and resize.
	go func() {
		defer cancel()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := f.Write(data); err != nil {
					return
				}
				continue
			}
			var c control
			if len(data) > maxControl || json.Unmarshal(data, &c) != nil {
				continue
			}
			if c.Type == "resize" && c.Cols > 0 && c.Rows > 0 {
				_ = pty.Setsize(f, &pty.Winsize{Cols: c.Cols, Rows: c.Rows})
			}
		}
	}()

	select {
	case <-exited:
		// Flush remaining output before reporting the exit.
		select {
		case <-outDone:
		case <-time.After(time.Second):
		}
		code := cmd.ProcessState.ExitCode()
		_ = writeControl(ctx, conn, control{Type: "exit", Code: &code})
	case <-ctx.Done():
	}
	return nil
}

func writeControl(ctx context.Context, conn *websocket.Conn, c control) error {
	b, _ := json.Marshal(c)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func orDefault(v, d uint16) uint16 {
	if v == 0 {
		return d
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
