package main

import (
	"net/http"
	"strconv"
	"strings"
)

// History endpoints, read-only like the rest:
//
//	GET /git/branches?path=                 local and remote branches
//	GET /git/log?path=&ref=&skip=&limit=    commits of a ref (default HEAD)
//	GET /git/show?path=&commit=&file=       one commit: metadata, files, diff

type gitBranch struct {
	Name     string `json:"name"`
	Remote   bool   `json:"remote"`
	Current  bool   `json:"current"`
	Commit   string `json:"commit"`
	Subject  string `json:"subject"`
	Date     string `json:"date"`
	Upstream string `json:"upstream,omitempty"`
	// Track is git's ahead/behind summary, e.g. "ahead 2, behind 1".
	Track string `json:"track,omitempty"`
}

type gitCommit struct {
	Hash    string   `json:"hash"`
	Short   string   `json:"short"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Email   string   `json:"email"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
	Body    string   `json:"body,omitempty"`
	Refs    string   `json:"refs,omitempty"`
}

type commitFile struct {
	Path    string `json:"path"`
	From    string `json:"from,omitempty"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// repoRoot answers with 400 when path is not inside a repository.
func (s *Server) repoRoot(w http.ResponseWriter, r *http.Request) (string, bool) {
	dir := gitDir(s.resolve(r.URL.Query().Get("path")))
	root, err := git(r.Context(), dir, 4096, "rev-parse", "--show-toplevel")
	if err != nil {
		http.Error(w, "not a git repository", http.StatusBadRequest)
		return "", false
	}
	return strings.TrimSpace(root), true
}

// validRef rejects refs that git would read as options or ranges.
func validRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "-") && !strings.Contains(ref, "..") && !strings.ContainsAny(ref, " \t\n\x00")
}

func (s *Server) serveBranches(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	root, ok := s.repoRoot(w, r)
	if !ok {
		return
	}
	const sep = "\x1f"
	format := strings.Join([]string{"%(HEAD)", "%(refname)", "%(refname:short)", "%(objectname:short)", "%(contents:subject)", "%(committerdate:iso-strict)", "%(upstream:short)", "%(upstream:track,nobracket)"}, sep)
	out, err := git(r.Context(), root, maxDiff, "for-each-ref", "--sort=-committerdate", "--format="+format, "refs/heads", "refs/remotes")
	if err != nil {
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
		return
	}
	branches := []gitBranch{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, sep)
		if len(f) < 8 || strings.HasSuffix(f[1], "/HEAD") {
			continue
		}
		branches = append(branches, gitBranch{
			Current: f[0] == "*", Remote: strings.HasPrefix(f[1], "refs/remotes/"), Name: f[2],
			Commit: f[3], Subject: f[4], Date: f[5], Upstream: f[6], Track: f[7],
		})
	}
	writeJSON(w, map[string]any{"root": root, "branches": branches})
}

const logFormat = "%H%x1f%h%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%D%x1f%b%x1e"

func parseLog(out string) []gitCommit {
	commits := []gitCommit{}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		f := strings.Split(rec, "\x1f")
		if len(f) < 9 {
			continue
		}
		parents := strings.Fields(f[2])
		if parents == nil {
			parents = []string{}
		}
		commits = append(commits, gitCommit{Hash: f[0], Short: f[1], Parents: parents, Author: f[3], Email: f[4], Date: f[5], Subject: f[6], Refs: f[7], Body: strings.TrimSpace(f[8])})
	}
	return commits
}

func (s *Server) serveLog(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	root, ok := s.repoRoot(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	if ref == "" {
		ref = "HEAD"
	}
	if !validRef(ref) {
		http.Error(w, "bad ref", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	skip, _ := strconv.Atoi(q.Get("skip"))
	if skip < 0 {
		skip = 0
	}
	// A repository without commits has no HEAD; that is an empty log.
	if _, err := git(r.Context(), root, 4096, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		writeJSON(w, map[string]any{"root": root, "ref": ref, "commits": []gitCommit{}, "more": false})
		return
	}
	out, err := git(r.Context(), root, maxDiff, "log", "--format="+logFormat, "--skip="+strconv.Itoa(skip), "-n", strconv.Itoa(limit+1), ref, "--")
	if err != nil {
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
		return
	}
	commits := parseLog(out)
	more := len(commits) > limit
	if more {
		commits = commits[:limit]
	}
	writeJSON(w, map[string]any{"root": root, "ref": ref, "commits": commits, "more": more})
}

func (s *Server) serveShow(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	root, ok := s.repoRoot(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	commit, file := q.Get("commit"), q.Get("file")
	if !validRef(commit) || strings.HasPrefix(file, "-") {
		http.Error(w, "bad commit", http.StatusBadRequest)
		return
	}
	meta, err := git(r.Context(), root, 1<<20, "show", "-s", "--format="+logFormat, commit, "--")
	if err != nil {
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
		return
	}
	commits := parseLog(meta)
	if len(commits) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// --root shows the first commit against the empty tree; -m with
	// --first-parent shows a merge against its first parent.
	base := []string{"show", "--format=", "--no-ext-diff", "--find-renames", "--root", "-m", "--first-parent"}
	stat, err := git(r.Context(), root, maxDiff, append(base, "--numstat", "-z", commit, "--")...)
	if err != nil {
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
		return
	}
	names, _ := git(r.Context(), root, maxDiff, append(base, "--name-status", "-z", commit, "--")...)
	args := append(base, "--patch", commit, "--")
	if file != "" {
		args = append(args, file)
	}
	diff, err := git(r.Context(), root, maxDiff, args...)
	if err != nil {
		http.Error(w, truncate(err.Error(), 300), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"root": root, "commit": commits[0], "files": parseCommitFiles(stat, names), "diff": diff, "truncated": len(diff) >= maxDiff})
}

// parseCommitFiles merges -z --numstat (counts) and -z --name-status
// (status letters) output, both in the same file order.
func parseCommitFiles(numstat, nameStatus string) []commitFile {
	files := []commitFile{}
	parts := strings.Split(numstat, "\x00")
	for i := 0; i < len(parts); i++ {
		p := strings.TrimLeft(parts[i], "\n")
		f := strings.SplitN(p, "\t", 3)
		if len(f) < 3 {
			continue
		}
		cf := commitFile{Path: f[2]}
		if f[0] == "-" {
			cf.Binary = true
		} else {
			cf.Added, _ = strconv.Atoi(f[0])
			cf.Deleted, _ = strconv.Atoi(f[1])
		}
		// A rename has an empty path followed by the old and new paths.
		if f[2] == "" && i+2 < len(parts) {
			cf.From, cf.Path = parts[i+1], parts[i+2]
			i += 2
		}
		files = append(files, cf)
	}
	ns := strings.Split(nameStatus, "\x00")
	k := 0
	for i := 0; i < len(ns) && k < len(files); i++ {
		st := strings.TrimLeft(ns[i], "\n")
		if st == "" {
			continue
		}
		files[k].Status = st[:1]
		if st[0] == 'R' || st[0] == 'C' {
			i += 2
		} else {
			i++
		}
		k++
	}
	return files
}
