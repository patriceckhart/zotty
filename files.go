package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Read-only file and git access for clients such as igor, on the same
// token as /term. The token grants a shell, so these endpoints add no
// access beyond it; they only make browsing cheaper than a terminal.
//
//	GET /files/list?path=      directory entries
//	GET /files/read?path=      file content (text up to maxRead)
//	GET /git/status?path=      repository root, branch and changed files
//	GET /git/diff?path=&file=  unified diff of one file or the whole tree
//
// Requests carry "Authorization: Bearer <token>". Relative paths resolve
// against the shell directory.

const (
	maxRead    = 1 << 20
	maxDiff    = 2 << 20
	maxEntries = 5000
	gitTimeout = 15 * time.Second
)

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if !s.originAllowed(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func (s *Server) resolve(p string) string {
	if p == "" {
		return s.Dir
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.Dir, p)
	}
	return filepath.Clean(p)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, os.ErrPermission):
		http.Error(w, "permission denied", http.StatusForbidden)
	default:
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
	}
}

type fileEntry struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Link     bool      `json:"link,omitempty"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	dir := s.resolve(r.URL.Query().Get("path"))
	des, err := os.ReadDir(dir)
	if err != nil {
		fsError(w, err)
		return
	}
	entries := make([]fileEntry, 0, len(des))
	for _, de := range des {
		if len(entries) >= maxEntries {
			break
		}
		e := fileEntry{Name: de.Name(), Dir: de.IsDir(), Link: de.Type()&os.ModeSymlink != 0}
		info, err := os.Stat(filepath.Join(dir, de.Name()))
		if err == nil {
			e.Dir, e.Size, e.Modified = info.IsDir(), info.Size(), info.ModTime()
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	parent := filepath.Dir(dir)
	if parent == dir {
		parent = ""
	}
	writeJSON(w, map[string]any{"path": dir, "parent": parent, "entries": entries, "truncated": len(des) > maxEntries})
}

func (s *Server) serveRead(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	path := s.resolve(r.URL.Query().Get("path"))
	f, err := os.Open(path)
	if err != nil {
		fsError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fsError(w, err)
		return
	}
	if info.IsDir() {
		http.Error(w, "is a directory", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRead))
	if err != nil {
		fsError(w, err)
		return
	}
	res := map[string]any{"path": path, "size": info.Size(), "truncated": info.Size() > maxRead}
	if isBinary(data) {
		res["binary"] = true
	} else {
		res["content"] = string(data)
	}
	writeJSON(w, res)
}

func isBinary(b []byte) bool {
	head := b[:min(len(b), 8000)]
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	// A cut at maxRead may split a rune; only the head decides.
	for len(head) > 0 {
		r, n := utf8.DecodeRune(head)
		if r == utf8.RuneError && n == 1 && len(head) > 3 {
			return true
		}
		head = head[n:]
	}
	return false
}

func git(ctx context.Context, dir string, max int, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotepath=off", "-c", "color.ui=never"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: max}
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return out.String(), nil
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	k := min(len(p), l.n)
	l.n -= k
	_, err := l.w.Write(p[:k])
	return len(p), err
}

// gitDir returns the directory to run git in: path itself, or its parent
// when path is a file.
func gitDir(path string) string {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return filepath.Dir(path)
	}
	return path
}

type gitChange struct {
	Path string `json:"path"`
	From string `json:"from,omitempty"`
	// Index and Worktree are the porcelain status letters, e.g. "M", "A",
	// "D", "R", "?" for untracked or " " for unchanged.
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
}

func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	dir := gitDir(s.resolve(r.URL.Query().Get("path")))
	root, err := git(r.Context(), dir, 4096, "rev-parse", "--show-toplevel")
	if err != nil {
		writeJSON(w, map[string]any{"repo": false})
		return
	}
	root = strings.TrimSpace(root)
	out, err := git(r.Context(), root, maxDiff, "status", "--porcelain=v1", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
		return
	}
	branch, changes := parseStatus(out)
	writeJSON(w, map[string]any{"repo": true, "root": root, "branch": branch, "changes": changes})
}

func parseStatus(out string) (string, []gitChange) {
	branch := ""
	changes := []gitChange{}
	parts := strings.Split(out, "\x00")
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if strings.HasPrefix(p, "## ") {
			branch = strings.TrimPrefix(p, "## ")
			if j := strings.Index(branch, "..."); j >= 0 {
				branch = branch[:j]
			}
			branch = strings.TrimPrefix(branch, "No commits yet on ")
			continue
		}
		if len(p) < 4 {
			continue
		}
		c := gitChange{Index: p[:1], Worktree: p[1:2], Path: p[3:]}
		if c.Index == "R" || c.Index == "C" {
			if i+1 < len(parts) {
				c.From = parts[i+1]
				i++
			}
		}
		changes = append(changes, c)
	}
	return branch, changes
}

func (s *Server) serveDiff(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	q := r.URL.Query()
	dir := gitDir(s.resolve(q.Get("path")))
	root, err := git(r.Context(), dir, 4096, "rev-parse", "--show-toplevel")
	if err != nil {
		http.Error(w, "not a git repository", http.StatusBadRequest)
		return
	}
	root = strings.TrimSpace(root)
	file := q.Get("file")
	if strings.HasPrefix(file, "-") {
		http.Error(w, "bad file", http.StatusBadRequest)
		return
	}
	args := []string{"diff", "--no-ext-diff", "--find-renames"}
	if q.Get("staged") == "1" {
		args = append(args, "--cached")
	} else if q.Get("all") == "1" {
		// Staged and unstaged changes against the last commit.
		if _, err := git(r.Context(), root, 4096, "rev-parse", "--verify", "HEAD"); err == nil {
			args = append(args, "HEAD")
		}
	}
	if file != "" {
		args = append(args, "--", file)
	}
	diff, err := git(r.Context(), root, maxDiff, args...)
	if err != nil {
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
		return
	}
	// Untracked files have no diff; show them as fully added.
	if diff == "" && file != "" {
		if _, err := git(r.Context(), root, 4096, "ls-files", "--error-unmatch", "--", file); err != nil {
			out, _ := gitNoIndex(r.Context(), root, file)
			diff = out
		}
	}
	writeJSON(w, map[string]any{"root": root, "file": file, "diff": diff, "truncated": len(diff) >= maxDiff})
}

// gitNoIndex diffs an untracked file against nothing. git exits 1 when
// there are differences, which is the normal case here.
func gitNoIndex(ctx context.Context, root, file string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "color.ui=never", "diff", "--no-index", "--no-ext-diff", "--", "/dev/null", file)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: maxDiff}
	_ = cmd.Run()
	return out.String(), nil
}
