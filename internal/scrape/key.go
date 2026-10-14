package scrape

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zombar/tailraid/internal/secrets"
	"github.com/zombar/tailraid/internal/sysfs"
)

// KeyEnv names the variable `media key` and `media scrape` read a TMDB key from.
const KeyEnv = "TAILRAID_TMDB_API_KEY"

// KeyFile is the key's file in a data dir from before the secrets store (docs/18 §11,
// I-302): read while not yet imported (docs/19 §6), and written only while a member's
// codec is older than the store's.
const KeyFile = secrets.TMDBLegacyFile

// SecretName is the key's name in the secrets store (docs/19 §6).
const SecretName = secrets.TMDBName

// Attribution is the line TMDB's terms ask for.
const Attribution = "This product uses the TMDB API but is not endorsed or certified by TMDB."

// ErrNoKey is a scrape with no TMDB key stored, saved or in the environment.
var ErrNoKey = errors.New("no TMDB key: run `tailraid media key` (or set " + KeyEnv + ")")

// SaveKey keeps key in dataDir, mode 0600, the dir 0700.
func SaveKey(dataDir, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("scrape: empty key")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := sysfs.WriteFileSync(filepath.Join(dataDir, KeyFile), []byte(key+"\n")); err != nil { // mode 0600
		return fmt.Errorf("scrape: save key: %w", err)
	}
	return nil
}

// LoadKey is the environment's key, else the store's (stored, the secret "tmdb" through
// this device's node; "" when the store has none), else the file of a device not yet
// imported (docs/19 §6, I-308).
func LoadKey(dataDir string, stored func() (string, error)) (string, error) {
	if k := strings.TrimSpace(os.Getenv(KeyEnv)); k != "" {
		return k, nil
	}
	if stored != nil {
		if k, err := stored(); err == nil && strings.TrimSpace(k) != "" {
			return strings.TrimSpace(k), nil
		}
	}
	b, err := os.ReadFile(filepath.Join(dataDir, KeyFile)) //nolint:gosec // the data dir's own key file
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoKey
	}
	if err != nil {
		return "", err
	}
	if k := strings.TrimSpace(string(b)); k != "" {
		return k, nil
	}
	return "", ErrNoKey
}
