package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Release lookups go through the HTML redirect instead of the REST API:
// it has no rate limit for anonymous callers. Variables so tests can
// point them at a fake server.
var (
	latestReleaseURL = "https://github.com/patriceckhart/zotty/releases/latest"
	downloadBaseURL  = "https://github.com/patriceckhart/zotty/releases/download"
)

// updateCheckTTL is how long a cached check result is trusted.
const updateCheckTTL = 12 * time.Hour

type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	// CurrentAt is the zotty version that ran the check. A different
	// running version invalidates the cache.
	CurrentAt string `json:"current_at"`
	Latest    string `json:"latest"`
}

// isDevVersion reports builds without a release version, which are never
// offered updates.
func isDevVersion(v string) bool {
	return v == "" || v == "dev" || v == "0.0.0"
}

// parseVersion parses "1.2.3" or "v1.2.3".
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// versionLess reports whether a is older than b. Invalid versions return
// false, so a broken lookup never offers a downgrade.
func versionLess(a, b string) bool {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for i := range va {
		if va[i] != vb[i] {
			return va[i] < vb[i]
		}
	}
	return false
}

// fetchLatestVersion returns the newest release version without the "v".
func fetchLatestVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, latestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		// Read the redirect target instead of following it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("latest release lookup: no redirect (status %d)", resp.StatusCode)
	}
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("latest release lookup: unexpected redirect %q", loc)
	}
	tag := loc[i+len("/tag/"):]
	if _, ok := parseVersion(tag); !ok {
		return "", fmt.Errorf("latest release lookup: unexpected tag %q", tag)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

func updateCachePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "zotty", "update-check.json")
}

// checkForUpdate returns the newer version, or "" when there is none or the
// check failed. A fresh cached result avoids the network; an up-to-date
// cache is re-checked so a new release shows up the same day.
func checkForUpdate(ctx context.Context, current, cachePath string) string {
	if isDevVersion(current) {
		return ""
	}
	if cachePath != "" {
		if b, err := os.ReadFile(cachePath); err == nil {
			var c updateCache
			if json.Unmarshal(b, &c) == nil && c.CurrentAt == current && time.Since(c.CheckedAt) < updateCheckTTL && versionLess(current, c.Latest) {
				return c.Latest
			}
		}
	}
	latest, err := fetchLatestVersion(ctx)
	if err != nil {
		return ""
	}
	if cachePath != "" {
		if b, err := json.Marshal(updateCache{CheckedAt: time.Now().UTC(), CurrentAt: current, Latest: latest}); err == nil {
			_ = os.MkdirAll(filepath.Dir(cachePath), 0o700)
			_ = os.WriteFile(cachePath, b, 0o600)
		}
	}
	if versionLess(current, latest) {
		return latest
	}
	return ""
}

// noticeUpdate logs a hint when a newer release exists. It runs in the
// background and never delays or fails startup. Set ZOTTY_NO_UPDATE_CHECK=1
// to disable it.
func noticeUpdate(logf func(string, ...any)) {
	if os.Getenv("ZOTTY_NO_UPDATE_CHECK") != "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if latest := checkForUpdate(ctx, version, updateCachePath()); latest != "" {
			logf("zotty: update available %s -> %s, run 'zotty update' (then restart zotty)", version, latest)
		}
	}()
}

func printUpdateHelp() {
	fmt.Fprintln(os.Stderr, `zotty update: replace this zotty binary with the latest release

usage:
  zotty update           download and install the newest release
  zotty update --check   show whether an update is available, install nothing

The binary must be writable by the current user (use sudo for a
root-owned install). Restart a running 'zotty serve' afterwards.
Development builds are not updated. 'zotty serve' also logs a notice
when a newer release exists (disable with ZOTTY_NO_UPDATE_CHECK=1).`)
}

func runUpdateCommand(args []string) error {
	check := false
	for _, a := range args {
		switch a {
		case "--check":
			check = true
		case "-h", "--help", "help":
			printUpdateHelp()
			return nil
		default:
			printUpdateHelp()
			return fmt.Errorf("unknown update flag: %s", a)
		}
	}
	if isDevVersion(version) {
		return errors.New("development build: 'zotty update' is disabled. Install a release with the install script or go install")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	latest, err := fetchLatestVersion(ctx)
	if err != nil {
		return fmt.Errorf("query latest release: %w", err)
	}
	if !versionLess(version, latest) {
		fmt.Printf("zotty %s is up to date (latest: %s)\n", version, latest)
		return nil
	}
	if check {
		fmt.Printf("zotty %s -> %s available\n  run 'zotty update' to install\n", version, latest)
		return nil
	}
	cur, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(cur); err == nil {
		cur = resolved
	}
	fmt.Printf("zotty update: %s -> %s\n", version, latest)
	if err := installRelease(ctx, latest, runtime.GOOS, runtime.GOARCH, cur); err != nil {
		return err
	}
	fmt.Printf("zotty update: installed %s at %s. Restart 'zotty serve' to use it.\n", latest, cur)
	return nil
}

// installRelease downloads the release archive for goos/goarch, verifies
// its checksum, and atomically replaces the binary at dst.
func installRelease(ctx context.Context, ver, goos, goarch, dst string) error {
	if goos != "darwin" && goos != "linux" {
		return fmt.Errorf("no zotty release for %s", goos)
	}
	name := fmt.Sprintf("zotty_%s_%s_%s", ver, goos, goarch)
	archive := name + ".tar.gz"
	base := fmt.Sprintf("%s/v%s", downloadBaseURL, ver)

	sums, err := download(ctx, base+"/checksums.txt", 1<<20)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	want, err := lookupChecksum(sums, archive)
	if err != nil {
		return err
	}
	data, err := download(ctx, base+"/"+archive, 64<<20)
	if err != nil {
		return fmt.Errorf("download %s: %w", archive, err)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s", archive)
	}
	bin, err := extractBinary(data, name+"/zotty")
	if err != nil {
		return err
	}
	return replaceBinary(dst, bin)
}

func download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return data, nil
}

// lookupChecksum finds the hash for file in checksums.txt, whose lines are
// "<sha256>  <file>" or "<sha256>  ./<file>".
func lookupChecksum(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "./") == file {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no checksum for %s", file)
}

// extractBinary returns the file at path from a gzipped tar archive.
func extractBinary(archive []byte, path string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive does not contain %s", path)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && strings.TrimPrefix(h.Name, "./") == path {
			return io.ReadAll(io.LimitReader(tr, 64<<20))
		}
	}
}

// replaceBinary writes data next to dst and renames it over dst. The rename
// is atomic, and a running zotty keeps executing its old inode.
func replaceBinary(dst string, data []byte) error {
	mode := os.FileMode(0o755)
	if st, err := os.Stat(dst); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".zotty-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (try sudo): %w", dst, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	return nil
}
