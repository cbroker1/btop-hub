package hub

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateToken returns the shared agent token, creating it on first start.
func LoadOrCreateToken(dir string) (tok string, created bool, err error) {
	p := filepath.Join(dir, "token")
	b, err := os.ReadFile(p)
	if err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, false, nil
		}
		return "", false, errors.New(p + " is too short; delete it to generate a new token")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	tok = hex.EncodeToString(raw)
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return tok, true, nil
}

// ReadToken reads an existing token, for the "token" and "forget" subcommands.
func ReadToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
