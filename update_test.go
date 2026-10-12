package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.0.1", "0.0.2", true},
		{"0.0.9", "0.0.10", true},
		{"0.0.99", "0.1.0", true},
		{"v0.1.0", "0.0.99", false},
		{"0.2.0", "0.2.0", false},
		{"1.0.0", "0.9.9", false},
		{"dev", "0.0.2", false},
		{"0.0.1", "garbage", false},
		{"0.0.1", "0.0", false},
	} {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// fakeReleases serves a latest-release redirect and release assets.
func fakeReleases(t *testing.T, latest string, assets map[string][]byte) *int {
	t.Helper()
	hits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			*hits++
			w.Header().Set("Location", "https://github.com/patriceckhart/zotty/releases/tag/v"+latest)
			w.WriteHeader(http.StatusFound)
			return
		}
		if data, ok := assets[r.URL.Path]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	oldLatest, oldBase := latestReleaseURL, downloadBaseURL
	latestReleaseURL, downloadBaseURL = srv.URL+"/latest", srv.URL+"/download"
	t.Cleanup(func() { latestReleaseURL, downloadBaseURL = oldLatest, oldBase })
	return hits
}

func TestCheckForUpdate(t *testing.T) {
	hits := fakeReleases(t, "0.0.5", nil)
	cache := filepath.Join(t.TempDir(), "update-check.json")
	ctx := context.Background()

	if got := checkForUpdate(ctx, "0.0.3", cache); got != "0.0.5" {
		t.Fatalf("got %q, want 0.0.5", got)
	}
	// A fresh cached update is reused without another request.
	if got := checkForUpdate(ctx, "0.0.3", cache); got != "0.0.5" || *hits != 1 {
		t.Fatalf("cached: got %q after %d requests", got, *hits)
	}
	// Up to date: nothing to report.
	if got := checkForUpdate(ctx, "0.0.5", cache); got != "" {
		t.Fatalf("up to date: got %q", got)
	}
	// Dev builds never check.
	before := *hits
	if got := checkForUpdate(ctx, "dev", cache); got != "" || *hits != before {
		t.Fatalf("dev: got %q after %d requests", got, *hits-before)
	}
}

func TestCheckForUpdateIgnoresStaleCache(t *testing.T) {
	fakeReleases(t, "0.0.7", nil)
	cache := filepath.Join(t.TempDir(), "update-check.json")
	b, _ := json.Marshal(updateCache{CheckedAt: time.Now().Add(-24 * time.Hour), CurrentAt: "0.0.3", Latest: "0.0.4"})
	if err := os.WriteFile(cache, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checkForUpdate(context.Background(), "0.0.3", cache); got != "0.0.7" {
		t.Fatalf("got %q, want fresh 0.0.7", got)
	}
}

func TestCheckForUpdateOffline(t *testing.T) {
	old := latestReleaseURL
	latestReleaseURL = "http://127.0.0.1:1/latest"
	t.Cleanup(func() { latestReleaseURL = old })
	if got := checkForUpdate(context.Background(), "0.0.3", ""); got != "" {
		t.Fatalf("offline: got %q", got)
	}
}

func makeArchive(t *testing.T, path string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestInstallRelease(t *testing.T) {
	name := "zotty_0.0.9_linux_amd64"
	archive := makeArchive(t, name+"/zotty", []byte("new binary"))
	sum := sha256.Sum256(archive)
	sums := fmt.Sprintf("%s  ./%s.tar.gz\n", hex.EncodeToString(sum[:]), name)
	fakeReleases(t, "0.0.9", map[string][]byte{
		"/download/v0.0.9/checksums.txt":       []byte(sums),
		"/download/v0.0.9/" + name + ".tar.gz": archive,
	})

	dst := filepath.Join(t.TempDir(), "zotty")
	if err := os.WriteFile(dst, []byte("old binary"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := installRelease(context.Background(), "0.0.9", "linux", "amd64", dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	st, _ := os.Stat(dst)
	if string(got) != "new binary" || st.Mode().Perm() != 0o750 {
		t.Fatalf("installed %q with mode %v", got, st.Mode().Perm())
	}
}

func TestInstallReleaseRejectsBadChecksum(t *testing.T) {
	name := "zotty_0.0.9_linux_amd64"
	archive := makeArchive(t, name+"/zotty", []byte("tampered"))
	fakeReleases(t, "0.0.9", map[string][]byte{
		"/download/v0.0.9/checksums.txt":       []byte(strings.Repeat("0", 64) + "  ./" + name + ".tar.gz\n"),
		"/download/v0.0.9/" + name + ".tar.gz": archive,
	})
	dst := filepath.Join(t.TempDir(), "zotty")
	if err := os.WriteFile(dst, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := installRelease(context.Background(), "0.0.9", "linux", "amd64", dst)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "old binary" {
		t.Fatalf("binary was replaced: %q", got)
	}
}
