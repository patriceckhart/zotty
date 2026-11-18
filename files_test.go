package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func filesServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer((&Server{Token: "secret", Dir: dir}).Handler())
	t.Cleanup(srv.Close)
	return srv, dir
}

func get(t *testing.T, srv *httptest.Server, path string, q url.Values, out any) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestFilesRequireToken(t *testing.T) {
	srv, _ := filesServer(t)
	for _, p := range []string{"/files/list", "/files/read", "/git/status", "/git/diff"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
	}
}

func TestListAndRead(t *testing.T) {
	srv, dir := filesServer(t)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.bin"), []byte{0, 1, 2}, 0o644)

	var list struct {
		Path    string      `json:"path"`
		Entries []fileEntry `json:"entries"`
	}
	if code := get(t, srv, "/files/list", nil, &list); code != 200 {
		t.Fatalf("status %d", code)
	}
	names := []string{}
	for _, e := range list.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "sub,a.bin,b.txt" {
		t.Fatalf("entries %v", names)
	}

	var file struct {
		Content string `json:"content"`
		Binary  bool   `json:"binary"`
	}
	get(t, srv, "/files/read", url.Values{"path": {"b.txt"}}, &file)
	if file.Content != "hello" || file.Binary {
		t.Fatalf("read %+v", file)
	}
	file.Content = ""
	get(t, srv, "/files/read", url.Values{"path": {"a.bin"}}, &file)
	if !file.Binary || file.Content != "" {
		t.Fatalf("binary %+v", file)
	}
	if code := get(t, srv, "/files/read", url.Values{"path": {"missing"}}, nil); code != 404 {
		t.Fatalf("missing: %d", code)
	}
}

func TestGitStatusAndDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv, dir := filesServer(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("fresh\n"), 0o644)

	var st struct {
		Repo    bool        `json:"repo"`
		Branch  string      `json:"branch"`
		Changes []gitChange `json:"changes"`
	}
	get(t, srv, "/git/status", nil, &st)
	if !st.Repo || st.Branch != "main" || len(st.Changes) != 2 {
		t.Fatalf("status %+v", st)
	}

	var d struct {
		Diff string `json:"diff"`
	}
	get(t, srv, "/git/diff", url.Values{"file": {"a.txt"}, "all": {"1"}}, &d)
	if !strings.Contains(d.Diff, "-one") || !strings.Contains(d.Diff, "+two") {
		t.Fatalf("diff %q", d.Diff)
	}
	get(t, srv, "/git/diff", url.Values{"file": {"new.txt"}, "all": {"1"}}, &d)
	if !strings.Contains(d.Diff, "+fresh") {
		t.Fatalf("untracked diff %q", d.Diff)
	}
	if code := get(t, srv, "/git/diff", url.Values{"file": {"--output=/tmp/x"}}, nil); code != 400 {
		t.Fatalf("option injection: %d", code)
	}
}
