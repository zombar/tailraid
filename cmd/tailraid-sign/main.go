// Command tailraid-sign makes and uses the release signing key (docs/12 §4, I-128). It
// is a build tool, not part of the release matrix.
//
//	tailraid-sign -gen -key release.key -pub release.pub      # once; the pub goes into internal/update/pubkey.go, the key into the CI secret
//	tailraid-sign -key release.key -sign dist/sha256sums.txt   # writes dist/sha256sums.txt.minisig
//	tailraid-sign -key-env MINISIGN_SECRET_KEY -sign dist/sha256sums.txt
//	tailraid-sign -pub release.pub -verify dist/sha256sums.txt
//
// The key files are minisign's own formats (the secret key unencrypted, as `minisign -G
// -W` writes), so `minisign -Vm dist/sha256sums.txt -p release.pub` verifies the same
// signature.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zombar/tailraid/internal/update"
)

func main() {
	os.Exit(run())
}

func run() int {
	gen := flag.Bool("gen", false, "generate a key pair into -key and -pub")
	keyPath := flag.String("key", "", "secret key file (minisign unencrypted format)")
	keyEnv := flag.String("key-env", "", "environment variable holding the secret key file's contents")
	pubPath := flag.String("pub", "", "public key file, or the key's base64 line")
	sign := flag.String("sign", "", "file to sign; writes <file>.minisig")
	verify := flag.String("verify", "", "file to verify against <file>.minisig under -pub")
	trusted := flag.String("trusted", "", "trusted comment (default: timestamp and file name)")
	flag.Parse()
	switch {
	case *gen:
		if *keyPath == "" || *pubPath == "" {
			return fail("-gen needs -key and -pub")
		}
		pub, sec, err := update.GenerateKey(nil)
		if err != nil {
			return fail(err.Error())
		}
		if err := os.WriteFile(*keyPath, []byte(update.EncodeSecretKey(sec, "tailraid release signing key")), 0o600); err != nil {
			return fail(err.Error())
		}
		pubText := update.EncodePublicKey(pub, "tailraid release signing key")
		if err := os.WriteFile(*pubPath, []byte(pubText), 0o644); err != nil { //nolint:gosec // a public key
			return fail(err.Error())
		}
		fmt.Printf("secret key: %s (keep it out of the repository; its contents are the CI secret)\npublic key: %s\n%s", *keyPath, *pubPath, pubText)
		return 0
	case *sign != "":
		sec, err := loadSecret(*keyPath, *keyEnv)
		if err != nil {
			return fail(err.Error())
		}
		msg, err := os.ReadFile(*sign)
		if err != nil {
			return fail(err.Error())
		}
		comment := *trusted
		if comment == "" {
			comment = fmt.Sprintf("timestamp:%d\tfile:%s", time.Now().Unix(), filepath.Base(*sign))
		}
		out := *sign + ".minisig"
		if err := os.WriteFile(out, []byte(update.Sign(sec, msg, "", comment)), 0o644); err != nil { //nolint:gosec // a signature
			return fail(err.Error())
		}
		fmt.Println("wrote", out)
		return 0
	case *verify != "":
		pubText, err := loadPub(*pubPath)
		if err != nil {
			return fail(err.Error())
		}
		msg, err := os.ReadFile(*verify)
		if err != nil {
			return fail(err.Error())
		}
		sig, err := os.ReadFile(*verify + ".minisig")
		if err != nil {
			return fail(err.Error())
		}
		if err := update.VerifyFile(pubText, msg, string(sig)); err != nil {
			return fail(err.Error())
		}
		fmt.Println("signature verified:", *verify)
		return 0
	}
	flag.Usage()
	return 2
}

func loadSecret(path, env string) (update.SecretKey, error) {
	var text string
	switch {
	case env != "":
		text = os.Getenv(env)
		if text == "" {
			return update.SecretKey{}, fmt.Errorf("%s is empty", env)
		}
	case path != "":
		b, err := os.ReadFile(path) //nolint:gosec // -key names the file
		if err != nil {
			return update.SecretKey{}, err
		}
		text = string(b)
	default:
		return update.SecretKey{}, fmt.Errorf("-sign needs -key or -key-env")
	}
	return update.ParseSecretKey(text)
}

func loadPub(path string) (string, error) {
	if path == "" {
		if update.ReleaseKey == "" {
			return "", fmt.Errorf("-verify needs -pub (no key is embedded in this build)")
		}
		return update.ReleaseKey, nil
	}
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // -pub names the file
		return string(b), nil
	}
	return path, nil // the base64 line itself
}

func fail(msg string) int {
	fmt.Fprintln(os.Stderr, "tailraid-sign:", msg)
	return 1
}
