package scrape

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zombar/tailraid/internal/sysfs"
)

// KeyEnv names the variable `media key` and `media scrape` read a TMDB key from.
const KeyEnv = "TAILRAID_TMDB_API_KEY"

// KeyFile is the key's file in a data dir (docs/18 §11, I-302).
const KeyFile = "tmdb.key"

// Attribution is the line TMDB's terms ask for.
const Attribution = "This product uses the TMDB API but is not endorsed or certified by TMDB."

// ErrNoKey is a scrape with no TMDB key saved or in the environment.
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

// LoadKey is the environment's key, else the saved one.
func LoadKey(dataDir string) (string, error) {
	if k := strings.TrimSpace(os.Getenv(KeyEnv)); k != "" {
		return k, nil
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
