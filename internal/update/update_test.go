package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease tunes what fakeGitHub serves beside a good signed release.
type fakeRelease struct {
	unsigned bool  // no sha256sums.txt.minisig asset
	badSig   bool  // the signature is for other bytes
	size     int64 // the binary asset's announced size (0: the real one)
}

// testKey signs the fake releases; testPub is what the updater under test trusts.
var testKey, testPub = func() (SecretKey, string) {
	pub, sec, err := GenerateKey(strings.NewReader(strings.Repeat("tailraid test key seed material ", 4)))
	if err != nil {
		panic(err)
	}
	return sec, EncodePublicKey(pub, "test")
}()

// fakeGitHub serves one release with assets for linux/amd64, a sums file and its
// minisign signature.
func fakeGitHub(t *testing.T, tag string, binary []byte, goodSum bool, opts ...fakeRelease) *httptest.Server {
	t.Helper()
	var fr fakeRelease
	if len(opts) > 0 {
		fr = opts[0]
	}
	sum := sha256.Sum256(binary)
	hexSum := hex.EncodeToString(sum[:])
	if !goodSum {
		hexSum = strings.Repeat("0", 64)
	}
	name := AssetName("tailraid", "linux", "amd64")
	sums := fmt.Sprintf("%s  %s\n%s  tailraid_darwin_arm64\n", hexSum, name, strings.Repeat("1", 64))
	signed := []byte(sums)
	if fr.badSig {
		signed = []byte("something else")
	}
	minisig := Sign(testKey, signed, "", "timestamp:1\tfile:sha256sums.txt")
	size := int64(len(binary))
	if fr.size != 0 {
		size = fr.size
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	release := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// a private repository: browser URLs 404, the API asset URLs serve bytes to a token
		assets := fmt.Sprintf(`{"name":%q,"browser_download_url":%q,"url":%q,"size":%d},{"name":"sha256sums.txt","browser_download_url":%q,"url":%q}`,
			name, srv.URL+"/dl/"+name, srv.URL+"/repos/zombar/tailraid/releases/assets/1", size, srv.URL+"/dl/sha256sums.txt", srv.URL+"/repos/zombar/tailraid/releases/assets/2")
		if !fr.unsigned {
			assets += fmt.Sprintf(`,{"name":"sha256sums.txt.minisig","browser_download_url":%q,"url":%q}`, srv.URL+"/dl/sha256sums.txt.minisig", srv.URL+"/repos/zombar/tailraid/releases/assets/3")
		}
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[%s]}`, tag, assets)
	}
	mux.HandleFunc("/repos/zombar/tailraid/releases/latest", release)
	mux.HandleFunc("/repos/zombar/tailraid/releases/tags/"+tag, release)
	private := func(next func(w http.ResponseWriter)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Accept") != "application/octet-stream" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			next(w)
		}
	}
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/repos/zombar/tailraid/releases/assets/1", private(func(w http.ResponseWriter) { _, _ = w.Write(binary) }))
	mux.HandleFunc("/repos/zombar/tailraid/releases/assets/2", private(func(w http.ResponseWriter) { _, _ = io.WriteString(w, sums) }))
	mux.HandleFunc("/repos/zombar/tailraid/releases/assets/3", private(func(w http.ResponseWriter) { _, _ = io.WriteString(w, minisig) }))
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Invariant: I-61
func TestUpdate_AppliesVerifiedRelease(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho tailraid v1.2.0\n")
	gh := fakeGitHub(t, "v1.2.0", newBin, true)
	dir := t.TempDir()
	exe := filepath.Join(dir, "tailraid")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	var verified string
	u := &Updater{
		Owner: "zombar", Repo: "tailraid", BaseURL: gh.URL, Token: "tok", PublicKey: testPub,
		Verify: func(_ context.Context, path, tag string) error { verified = tag; return nil },
	}
	ctx := context.Background()
	if _, err := (&Updater{Owner: "zombar", Repo: "tailraid", BaseURL: gh.URL}).Resolve(ctx, ""); !errors.Is(err, ErrNoToken) {
		t.Fatalf("anonymous on a private repo: %v", err)
	}
	rel, err := u.Resolve(ctx, "")
	if err != nil || rel.Tag != "v1.2.0" {
		t.Fatalf("resolve: %v %+v", err, rel)
	}
	if err := u.Apply(ctx, rel, exe, "v1.1.9", "linux", "amd64", false, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, newBin) || verified != "v1.2.0" {
		t.Fatalf("binary not replaced (verified %q)", verified)
	}
	if st, err := os.Stat(exe); err != nil || (runtime.GOOS != "windows" && st.Mode()&0o100 == 0) {
		t.Fatal("new binary not executable")
	}
	// the previous binary stays as .old until a clean start forgets it or a rollback uses it
	if old, err := os.ReadFile(exe + ".old"); err != nil || string(old) != "old" {
		t.Fatalf(".old after the update: %q %v", old, err)
	}
	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	if got, _ = os.ReadFile(exe); string(got) != "old" {
		t.Fatalf("after rollback: %q", got)
	}
	if err := Rollback(exe); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("second rollback: %v", err)
	}
	if err := u.Apply(ctx, rel, exe, "v1.1.9", "linux", "amd64", false, false); err != nil {
		t.Fatal(err)
	}
	if err := ForgetOld(exe); err != nil {
		t.Fatal(err)
	}
	if err := ForgetOld(exe); err != nil {
		t.Fatalf("forgetting twice: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	want := 1
	if runtime.GOOS == "windows" { // the running image cannot be deleted: the rolled-back copy stays
		want = 2
	}
	if len(ents) != want {
		t.Fatalf("leftovers in %s: %v", dir, ents)
	}
	// same version: nothing to do; explicit tag resolves too
	if err := u.Apply(ctx, rel, exe, "v1.2.0", "linux", "amd64", false, false); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("up to date: %v", err)
	}
	if rel2, err := u.Resolve(ctx, "1.2.0"); err != nil || rel2.Tag != "v1.2.0" {
		t.Fatalf("resolve by tag: %v", err)
	}
}

// Invariant: I-61
func TestUpdate_RefusesBadChecksumAndOlder(t *testing.T) {
	newBin := []byte("new")
	dir := t.TempDir()
	exe := filepath.Join(dir, "tailraid")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(exe, []byte("old"), 0o755))
	ctx := context.Background()
	untouched := func(what string) {
		t.Helper()
		got, _ := os.ReadFile(exe)
		ents, _ := os.ReadDir(dir)
		if string(got) != "old" || len(ents) != 1 {
			t.Fatalf("%s: binary touched or leftovers: %q %v", what, got, ents)
		}
	}
	// bad checksum
	gh := fakeGitHub(t, "v2.0.0", newBin, false)
	u := &Updater{Owner: "zombar", Repo: "tailraid", BaseURL: gh.URL, Token: "tok", PublicKey: testPub, Verify: func(context.Context, string, string) error { return nil }}
	rel, err := u.Resolve(ctx, "")
	must(err)
	if err := u.Apply(ctx, rel, exe, "v1.0.0", "linux", "amd64", false, false); !errors.Is(err, ErrChecksum) {
		t.Fatalf("bad checksum: %v", err)
	}
	untouched("after checksum mismatch")
	// unsigned, mis-signed, no key, wrong size: refused before anything is written (I-128)
	for _, c := range []struct {
		name string
		rel  fakeRelease
		key  string
		want error
	}{
		{"unsigned release", fakeRelease{unsigned: true}, testPub, ErrNoSignature},
		{"signature for other bytes", fakeRelease{badSig: true}, testPub, ErrBadSignature},
		{"size lie", fakeRelease{size: int64(len(newBin)) + 1}, testPub, ErrSize},
	} {
		gh := fakeGitHub(t, "v2.0.0", newBin, true, c.rel)
		uu := &Updater{Owner: "zombar", Repo: "tailraid", BaseURL: gh.URL, Token: "tok", PublicKey: c.key, Verify: func(context.Context, string, string) error { return nil }}
		rel, err := uu.Resolve(ctx, "")
		must(err)
		if err := uu.Apply(ctx, rel, exe, "v1.0.0", "linux", "amd64", false, false); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", c.name, err, c.want)
		}
		untouched("after " + c.name)
	}
	if _, err := ParsePublicKey(ReleaseKey); err != nil { // the embedded key must parse, or every update is refused
		t.Fatalf("embedded release key: %v", err)
	}
	// good checksum but the binary reports the wrong version
	gh = fakeGitHub(t, "v2.0.0", newBin, true)
	u.BaseURL = gh.URL
	u.Verify = func(context.Context, string, string) error { return errBadVersion }
	rel, err = u.Resolve(ctx, "")
	must(err)
	if err := u.Apply(ctx, rel, exe, "v1.0.0", "linux", "amd64", false, false); !errors.Is(err, errBadVersion) {
		t.Fatalf("wrong version: %v", err)
	}
	untouched("after version mismatch")
	// not newer unless forced; a downgrade needs the explicit allowance too; dev builds always update
	u.Verify = func(context.Context, string, string) error { return nil }
	if err := u.Apply(ctx, rel, exe, "v2.1.0", "linux", "amd64", false, false); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("older release: %v", err)
	}
	untouched("after refusing an older release")
	if err := u.Apply(ctx, rel, exe, "v2.1.0", "linux", "amd64", true, false); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("forced downgrade without the allowance: %v", err)
	}
	untouched("after refusing a downgrade")
	if err := u.Apply(ctx, rel, exe, "v2.1.0", "linux", "amd64", true, true); err != nil {
		t.Fatalf("allowed downgrade: %v", err)
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, newBin) {
		t.Fatal("the allowed downgrade did not install")
	}
	must(ForgetOld(exe))
	must(os.WriteFile(exe, []byte("old"), 0o755))
	if !Newer("dev", "v0.0.1") || !Newer("abc1234-dirty", "v2.0.0") || Newer("v2.0.0", "v2.0.0") || Newer("v2.0.1", "v2.0.0") || !Newer("v1.9.9", "v2.0.0") {
		t.Fatal("Newer rules")
	}
	// no asset for this platform
	if err := u.Apply(ctx, rel, exe, "v1.0.0", "plan9", "mips", false, false); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("no asset: %v", err)
	}
	untouched("after no asset")
	// forced older release applies when the downgrade is allowed too
	if err := u.Apply(ctx, rel, exe, "v2.1.0", "linux", "amd64", true, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Fatal("forced update not applied")
	}
}
