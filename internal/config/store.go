package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// FileName is the configuration file in the data directory.
const FileName = "config.json"

// DataDir returns the data directory: LAMPLIGHTER_DATA_DIR when set, else
// lamplighter under XDG_STATE_HOME (in Magpie's Docker image /config/state,
// which survives an upgrade), else ~/.local/state/lamplighter, or
// %LOCALAPPDATA%\lamplighter on Windows.
func DataDir() (string, error) {
	if dir := os.Getenv("LAMPLIGHTER_DATA_DIR"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, PluginID), nil
	}
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, PluginID), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no data directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", PluginID), nil
}

// Store keeps the configuration in a JSON file. The page saves settings with
// Patch, which replaces whole top-level keys, the way the settings page
// expects.
type Store struct {
	path string

	mu  sync.Mutex
	raw map[string]json.RawMessage
}

// Open reads the configuration file at path. A missing file is an empty
// configuration; a file that does not parse is an error, and the caller
// keeps the defaults without writing over the file.
//
// A configuration that Check refuses is still returned: an invalid
// timezone falls back to the time zone of the process (see
// Config.Location) with a warning, and the caller handles SameKeys.
func Open(path string) (*Store, Config, error) {
	s := &Store{path: path, raw: map[string]json.RawMessage{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, Default(), nil
	}
	if err != nil {
		return nil, Default(), err
	}
	if err := json.Unmarshal(data, &s.raw); err != nil {
		return nil, Default(), fmt.Errorf("parse %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, Default(), err
	}
	if !cfg.TimezoneValid() {
		log.Printf("warning: %s: %v; using the time zone of the process", path, ErrTimezone)
	}
	return s, cfg, nil
}

// Raw returns the saved keys, secrets included, for the settings page.
func (s *Store) Raw() map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]json.RawMessage, len(s.raw))
	for key, value := range s.raw {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

// Patch replaces the given top-level keys, a null value deleting the key.
// The result must parse and pass Config.Check; it is then written to the
// file whole (a temporary file and a rename) and returned.
//
// Patch does not apply the result; a caller that applies it holds its own
// lock across Patch and the apply, so that the order of the saved
// configurations is also the order in which they are applied.
func (s *Store) Patch(patch map[string]json.RawMessage) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]json.RawMessage, len(s.raw)+len(patch))
	for key, value := range s.raw {
		next[key] = value
	}
	for key, value := range patch {
		if string(value) == "null" {
			delete(next, key)
			continue
		}
		next[key] = value
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Check(); err != nil {
		return Config{}, err
	}
	if err := writeFile(s.path, data); err != nil {
		return Config{}, err
	}
	s.raw = next
	return cfg, nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
