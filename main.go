// Command zotty (zero-overhead-tty) is a small terminal bridge for the zot
// web UI.
//
// It serves a WebSocket endpoint that starts a login shell in a
// pseudo-terminal for each authenticated connection. It is independent of
// zot itself and grants full shell access as the user running it, so it
// listens on localhost by default and requires a token.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

func init() {
	// go install records the module version (e.g. v0.0.1) in the binary.
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(info.Main.Version, "v") {
			version = strings.TrimPrefix(info.Main.Version, "v")
		}
	}
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("zotty", version)
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: zotty serve [flags]\n       zotty version")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7789", "address to listen on")
	tokenFile := fs.String("token-file", defaultTokenFile(), "file holding the access token (created if missing)")
	origins := fs.String("origins", "https://web.zot.sh", "comma-separated browser origins allowed in addition to localhost")
	shell := fs.String("shell", "", "shell to start (default: $SHELL, then /bin/sh)")
	dir := fs.String("dir", "", "working directory for new shells (default: home directory)")
	upstream := fs.String("upstream", "", "forward all other paths to this address, e.g. 127.0.0.1:7787 for zot continuous --web")
	_ = fs.Parse(os.Args[2:])

	token, created, err := loadOrCreateToken(*tokenFile)
	if err != nil {
		log.Fatalf("zotty: token: %v", err)
	}
	if created {
		log.Printf("zotty: created token file %s", *tokenFile)
	}

	srv := &Server{
		Token:   token,
		Origins: splitOrigins(*origins),
		Shell:   resolveShell(*shell),
		Dir:     resolveDir(*dir),
	}
	if *upstream != "" {
		proxy, target, err := newUpstream(*upstream)
		if err != nil {
			log.Fatalf("zotty: upstream: %v", err)
		}
		srv.Upstream = proxy
		log.Printf("zotty: forwarding other paths to %s", target)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("zotty: listen: %v", err)
	}
	if !isLoopback(ln.Addr()) {
		log.Printf("zotty: WARNING listening on %s, which is not loopback. Anyone who reaches it and has the token gets a shell.", ln.Addr())
	}
	log.Printf("zotty %s: listening on ws://%s/term", version, ln.Addr())
	log.Printf("zotty: token %s", token)

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("zotty: %v", err)
	}
}

// newUpstream builds a reverse proxy to addr (host:port or http URL).
// httputil.ReverseProxy passes WebSocket upgrades through.
func newUpstream(addr string) (http.Handler, *url.URL, error) {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	target, err := url.Parse(addr)
	if err != nil {
		return nil, nil, err
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, nil, fmt.Errorf("unsupported scheme %q", target.Scheme)
	}
	if target.Host == "" {
		return nil, nil, fmt.Errorf("missing host in %q", addr)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
		},
	}
	return proxy, target, nil
}

func defaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "zotty-token"
	}
	return filepath.Join(dir, "zotty", "token")
}

// loadOrCreateToken reads the token file or creates it with a random
// 32-byte token, readable only by the current user.
func loadOrCreateToken(path string) (string, bool, error) {
	if b, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(b))
		if len(t) < 16 {
			return "", false, fmt.Errorf("%s: token shorter than 16 characters", path)
		}
		return t, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return t, true, nil
}

func splitOrigins(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func resolveShell(s string) string {
	if s != "" {
		return s
	}
	if s = os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func resolveDir(d string) string {
	if d != "" {
		return d
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/"
}

func isLoopback(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
