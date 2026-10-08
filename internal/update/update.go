// Package update replaces the running binary with a verified release artifact
// (docs/12 §5): resolve the GitHub release, download the asset for this OS/arch and the
// release's sha256sums.txt, verify, run the download's `version`, swap atomically.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Release is the subset of GitHub's release document the updater uses.
type Release struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

// Asset is one release file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Updater talks to one repository.
type Updater struct {
	Owner, Repo string
	Binary      string       // asset base name ("tailraid")
	BaseURL     string       // GitHub API base; tests point it at a fake
	Token       string       // bearer token for a private repository
	HTTP        *http.Client // default: 2 min timeout
	// Verify runs a downloaded binary and checks it reports tag (default: exec `<path> version`).
	Verify func(ctx context.Context, path, tag string) error
	Out    io.Writer
}

// Errors reported to the CLI.
var (
	ErrUpToDate   = errors.New("update: already up to date")
	ErrNotNewer   = errors.New("update: release is not newer than the running version")
	ErrChecksum   = errors.New("update: checksum mismatch")
	ErrNoAsset    = errors.New("update: no asset for this platform")
	ErrNoRelease  = errors.New("update: release not found")
	ErrNoToken    = errors.New("update: repository needs a token (TAILRAID_GITHUB_TOKEN, GITHUB_TOKEN or `gh auth login`)")
	errBadVersion = errors.New("update: downloaded binary reports a different version")
)

func (u *Updater) defaults() {
	if u.Binary == "" {
		u.Binary = "tailraid"
	}
	if u.BaseURL == "" {
		u.BaseURL = "https://api.github.com"
	}
	if u.HTTP == nil {
		u.HTTP = &http.Client{Timeout: 2 * time.Minute}
	}
	if u.Verify == nil {
		u.Verify = execVersion
	}
	if u.Out == nil {
		u.Out = io.Discard
	}
}

// AssetName is the artifact name `make release` produces for a platform.
func AssetName(binary, goos, goarch string) string {
	n := fmt.Sprintf("%s_%s_%s", binary, goos, goarch)
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// Resolve fetches the latest release, or the one tagged tag when tag is not empty.
func (u *Updater) Resolve(ctx context.Context, tag string) (*Release, error) {
	u.defaults()
	path := "/releases/latest"
	if tag != "" {
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		path = "/releases/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s%s", u.BaseURL, u.Owner, u.Repo, path), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	u.auth(req)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		if u.Token == "" {
			return nil, ErrNoToken // a private repository answers 404 to anonymous callers
		}
		return nil, ErrNoRelease
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w (%s)", ErrNoToken, resp.Status)
	default:
		return nil, fmt.Errorf("update: GitHub answered %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("update: decode release: %w", err)
	}
	return &rel, nil
}

func (u *Updater) auth(req *http.Request) {
	req.Header.Set("User-Agent", u.Binary+"-update")
	if u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
}

// Newer reports whether tag should replace current: always for a non-semver build,
// otherwise only when the release is strictly newer.
func Newer(current, tag string) bool {
	cur, ok := parse(current)
	if !ok {
		return true
	}
	next, ok := parse(tag)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if next[i] != cur[i] {
			return next[i] > cur[i]
		}
	}
	return false
}

var semver = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$`)

func parse(v string) ([3]int, bool) {
	m := semver.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

// Apply downloads, verifies and swaps the binary at exe with the release's asset for
// goos/goarch. current is the running version; force applies a release that is not newer.
func (u *Updater) Apply(ctx context.Context, rel *Release, exe, current, goos, goarch string, force bool) error {
	u.defaults()
	if rel.Tag == current {
		return ErrUpToDate
	}
	if !force && !Newer(current, rel.Tag) {
		return fmt.Errorf("%w: running %s, release %s", ErrNotNewer, current, rel.Tag)
	}
	name := AssetName(u.Binary, goos, goarch)
	var asset, sums *Asset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case name:
			asset = &rel.Assets[i]
		case "sha256sums.txt":
			sums = &rel.Assets[i]
		}
	}
	if asset == nil || sums == nil {
		return fmt.Errorf("%w: want %s and sha256sums.txt in %s", ErrNoAsset, name, rel.Tag)
	}
	sumBody, err := u.fetch(ctx, sums.URL, 1<<20)
	if err != nil {
		return err
	}
	want, ok := parseSums(string(sumBody))[name]
	if !ok {
		return fmt.Errorf("%w: %s has no entry for %s", ErrChecksum, sums.Name, name)
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, "."+u.Binary+"-update-*")
	if err != nil {
		return fmt.Errorf("update: temp file beside %s: %w", exe, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	fmt.Fprintf(u.Out, "downloading %s (%s)\n", asset.Name, rel.Tag)
	h := sha256.New()
	if err := u.stream(ctx, asset.URL, io.MultiWriter(tmp, h)); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		cleanup()
		return fmt.Errorf("%w: %s: got %s want %s", ErrChecksum, name, got, want)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		cleanup()
		return err
	}
	if err := u.Verify(ctx, tmpPath, rel.Tag); err != nil {
		cleanup()
		return err
	}
	if err := swap(exe, tmpPath); err != nil {
		cleanup()
		return err
	}
	fmt.Fprintf(u.Out, "installed %s at %s\n", rel.Tag, exe)
	return nil
}

func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	u.auth(req)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (u *Updater) stream(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	u.auth(req)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// parseSums reads "<hex>  <name>" lines.
func parseSums(s string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	return out
}

// execVersion runs `<path> version` and requires the tag in its output.
func execVersion(ctx context.Context, path, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("update: downloaded binary does not run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), tag) {
		return fmt.Errorf("%w: %q", errBadVersion, strings.TrimSpace(string(out)))
	}
	return nil
}

// swap renames exe to exe.old and tmp into exe's place; .old is removed when the OS
// allows it (Windows keeps the running image locked until exit).
func swap(exe, tmp string) error {
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("update: move current binary aside: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(old, exe) // put the original back
		return fmt.Errorf("update: install new binary: %w", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Remove(old)
	}
	return nil
}

// Token finds a GitHub token: env, then the gh CLI.
func Token(ctx context.Context) string {
	for _, k := range []string{"TAILRAID_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if gh, err := exec.LookPath("gh"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, gh, "auth", "token").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}
