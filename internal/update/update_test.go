package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGitHub serves one release with assets for linux/amd64 and a sums file.
func fakeGitHub(t *testing.T, tag string, binary []byte, goodSum bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(binary)
	hexSum := hex.EncodeToString(sum[:])
	if !goodSum {
		hexSum = strings.Repeat("0", 64)
	}
	name := AssetName("tailraid", "linux", "amd64")
	mux := http.NewServeMux()
	var srv *httptest.Server
	release := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"sha256sums.txt","browser_download_url":%q}]}`,
			tag, name, srv.URL+"/dl/"+name, srv.URL+"/dl/sha256sums.txt")
	}
	mux.HandleFunc("/repos/zombar/tailraid/releases/latest", release)
	mux.HandleFunc("/repos/zombar/tailraid/releases/tags/"+tag, release)
	mux.HandleFunc("/dl/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/dl/sha256sums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n%s  tailraid_darwin_arm64\n", hexSum, name, strings.Repeat("1", 64))
	})
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
	u := &Updater{Owner: "zombar", Repo: "tailraid", BaseURL: gh.URL, Token: "tok",
		Verify: func(_ context.Context, path, tag string) error { verified = tag; return nil }}
	ctx := context.Background()
	if _, err := (&Updater{Owner: "zombar", Repo: "tailraid", BaseURL: gh.URL}).Resolve(ctx, ""); !errors.Is(err, ErrNoToken) {
		t.Fatalf("anonymous on a private repo: %v", err)
	}
	rel, err := u.Resolve(ctx, "")
	if err != nil || rel.Tag != "v1.2.0" {
		t.Fatalf("resolve: %v %+v", err, rel)
	}
	if err := u.Apply(ctx, rel, exe, "v1.1.9", "linux", "amd64", false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, newBin) || verified != "v1.2.0" {
		t.Fatalf("binary not replaced (verified %q)", verified)
	}
	if st, err := os.Stat(exe); err != nil || (runtime.GOOS != "windows" && st.Mode()&0o100 == 0) {
		t.Fatal("new binary not executable")
	}
	ents, _ := os.ReadDir(dir)
	want := 1
	if runtime.GOOS == "windows" { // the running image cannot be deleted: .old stays until the next update
		want = 2
	}
	if len(ents) != want {
		t.Fatalf("leftovers in %s: %v", dir, ents)
	}
	// same version: nothing to do; explicit tag resolves too
	if err := u.Apply(ctx, rel, exe, "v1.2.0", "linux", "amd64", false); !errors.Is(err, ErrUpToDate) {
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
	u := &Updater{Owner: "zombar", Repo: "tailraid", BaseURL: gh.URL, Token: "tok", Verify: func(context.Context, string, string) error { return nil }}
	rel, err := u.Resolve(ctx, "")
	must(err)
	if err := u.Apply(ctx, rel, exe, "v1.0.0", "linux", "amd64", false); !errors.Is(err, ErrChecksum) {
		t.Fatalf("bad checksum: %v", err)
	}
	untouched("after checksum mismatch")
	// good checksum but the binary reports the wrong version
	gh = fakeGitHub(t, "v2.0.0", newBin, true)
	u.BaseURL = gh.URL
	u.Verify = func(context.Context, string, string) error { return errBadVersion }
	rel, err = u.Resolve(ctx, "")
	must(err)
	if err := u.Apply(ctx, rel, exe, "v1.0.0", "linux", "amd64", false); !errors.Is(err, errBadVersion) {
		t.Fatalf("wrong version: %v", err)
	}
	untouched("after version mismatch")
	// not newer unless forced; dev builds always update
	u.Verify = func(context.Context, string, string) error { return nil }
	if err := u.Apply(ctx, rel, exe, "v2.1.0", "linux", "amd64", false); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("older release: %v", err)
	}
	untouched("after refusing an older release")
	if !Newer("dev", "v0.0.1") || !Newer("abc1234-dirty", "v2.0.0") || Newer("v2.0.0", "v2.0.0") || Newer("v2.0.1", "v2.0.0") || !Newer("v1.9.9", "v2.0.0") {
		t.Fatal("Newer rules")
	}
	// no asset for this platform
	if err := u.Apply(ctx, rel, exe, "v1.0.0", "plan9", "mips", false); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("no asset: %v", err)
	}
	untouched("after no asset")
	// forced older release applies
	if err := u.Apply(ctx, rel, exe, "v2.1.0", "linux", "amd64", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Fatal("forced update not applied")
	}
}
