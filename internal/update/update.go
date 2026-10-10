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

// Asset is one release file. A private repository serves its bytes only through the
// API URL (Accept: application/octet-stream, bearer token); the browser URL is for
// public ones.
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	APIURL string `json:"url"`
	Size   int64  `json:"size"`
}

// Updater talks to one repository.
type Updater struct {
	Owner, Repo string
	Binary      string       // asset base name ("tailraid")
	BaseURL     string       // GitHub API base; tests point it at a fake
	Token       string       // bearer token; needed only for a private repository
	HTTP        *http.Client // default: 2 min timeout
	// Verify runs a downloaded binary and checks it reports tag (default: exec `<path> version`).
	Verify func(ctx context.Context, path, tag string) error
	Out    io.Writer
	// PublicKey is the minisign public key the release's sha256sums.txt.minisig must
	// verify under (default ReleaseKey; tests sign with their own).
	PublicKey string
}

// Errors reported to the CLI.
var (
	ErrUpToDate   = errors.New("update: already up to date")
	ErrNotNewer   = errors.New("update: release is not newer than the running version")
	ErrChecksum   = errors.New("update: checksum mismatch")
	ErrNoAsset    = errors.New("update: no asset for this platform")
	ErrNoRelease  = errors.New("update: release not found")
	ErrNoToken    = errors.New("update: GitHub refused the token (TAILRAID_GITHUB_TOKEN, GITHUB_TOKEN or `gh auth login`)")
	ErrSize       = errors.New("update: downloaded size differs from the release asset's")
	ErrDowngrade  = errors.New("update: release is older than the running version; --allow-downgrade installs it anyway (an older node cannot read a newer snapshot, docs/12 §6)")
	ErrNoRollback = errors.New("update: no previous binary to roll back to")
	errBadVersion = errors.New("update: downloaded binary reports a different version")
)

// downloadURL picks the API URL when a token is in play (private repository).
func (u *Updater) downloadURL(a *Asset) string {
	if u.Token != "" && a.APIURL != "" {
		return a.APIURL
	}
	return a.URL
}

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
	if u.PublicKey == "" {
		u.PublicKey = ReleaseKey
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
		if u.Token == "" { // a private repository answers 404 to anonymous callers
			return nil, fmt.Errorf("%w (anonymous: a private repository needs TAILRAID_GITHUB_TOKEN, GITHUB_TOKEN or `gh auth login`)", ErrNoRelease)
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
	for i := range 3 {
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
// goos/goarch (docs/12 §5, I-128). current is the running version; force applies a
// release that is not newer (an explicit --version), and a release that is older still
// needs allowDowngrade. The release's sha256sums.txt must carry a minisign signature
// that verifies under PublicKey; the download must have the asset's announced size and
// is fsynced before the swap; the previous binary stays beside the new one as
// <exe>.old until ForgetOld or Rollback.
func (u *Updater) Apply(ctx context.Context, rel *Release, exe, current, goos, goarch string, force, allowDowngrade bool) error {
	u.defaults()
	if rel.Tag == current {
		return ErrUpToDate
	}
	if !Newer(current, rel.Tag) {
		if !force {
			return fmt.Errorf("%w: running %s, release %s", ErrNotNewer, current, rel.Tag)
		}
		if !allowDowngrade {
			return fmt.Errorf("%w: running %s, release %s", ErrDowngrade, current, rel.Tag)
		}
	}
	if u.PublicKey == "" {
		return ErrNoReleaseKey
	}
	name := AssetName(u.Binary, goos, goarch)
	var asset, sums, sig *Asset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case name:
			asset = &rel.Assets[i]
		case "sha256sums.txt":
			sums = &rel.Assets[i]
		case "sha256sums.txt.minisig":
			sig = &rel.Assets[i]
		}
	}
	if asset == nil || sums == nil {
		return fmt.Errorf("%w: want %s and sha256sums.txt in %s", ErrNoAsset, name, rel.Tag)
	}
	if sig == nil {
		return fmt.Errorf("%w: %s", ErrNoSignature, rel.Tag)
	}
	sumBody, err := u.fetch(ctx, u.downloadURL(sums), 1<<20)
	if err != nil {
		return err
	}
	sigBody, err := u.fetch(ctx, u.downloadURL(sig), 64<<10)
	if err != nil {
		return err
	}
	if err := VerifyFile(u.PublicKey, sumBody, string(sigBody)); err != nil {
		return fmt.Errorf("%s of %s: %w", sig.Name, rel.Tag, err)
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
	n, err := u.stream(ctx, u.downloadURL(asset), io.MultiWriter(tmp, h))
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if asset.Size > 0 && n != asset.Size {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("%w: %s: got %d bytes, release says %d", ErrSize, name, n, asset.Size)
	}
	if err := tmp.Sync(); err != nil { // the bytes are on disk before anything points at them
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
	if err := os.Chmod(tmpPath, 0o755); err != nil { //nolint:gosec // it is the new binary; it must be executable
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

func (u *Updater) stream(ctx context.Context, url string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	u.auth(req)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("update: %s: %s", url, resp.Status)
	}
	return io.Copy(w, resp.Body)
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

// swap renames exe to exe.old and tmp into exe's place. The .old stays: `serve` removes
// it a minute after a clean start (ForgetOld) and `update --rollback` restores it.
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
	return nil
}

// Rollback puts <exe>.old back in exe's place (I-128): the binary an update replaced,
// kept until the next clean start. The replaced one is removed.
func Rollback(exe string) error {
	old := exe + ".old"
	if _, err := os.Stat(old); err != nil {
		return ErrNoRollback
	}
	aside := exe + ".rolled-back"
	_ = os.Remove(aside)
	if err := os.Rename(exe, aside); err != nil {
		return fmt.Errorf("update: move current binary aside: %w", err)
	}
	if err := os.Rename(old, exe); err != nil {
		_ = os.Rename(aside, exe)
		return fmt.Errorf("update: restore previous binary: %w", err)
	}
	if runtime.GOOS != "windows" { // Windows keeps a running image locked until exit
		_ = os.Remove(aside)
	}
	return nil
}

// ForgetOld removes <exe>.old once the new binary has proven itself (a minute into a
// clean serve). It is a no-op when there is none.
func ForgetOld(exe string) error {
	err := os.Remove(exe + ".old")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
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
		if out, err := exec.CommandContext(ctx, gh, "auth", "token").Output(); err == nil { //nolint:gosec // gh is looked up on PATH; its token is the documented fallback (docs/12 §5)
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}
