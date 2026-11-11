package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"jaild/internal/model"
)

type TOMLStore struct {
	mu  sync.RWMutex
	dir string
}

func NewTOMLStore(dir string) (*TOMLStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create toml store dir: %w", err)
	}
	return &TOMLStore{dir: dir}, nil
}

func validName(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || name == "." || name == ".." {
		return false
	}
	return true
}

func (s *TOMLStore) filePath(name string) string {
	return filepath.Join(s.dir, name+".toml")
}

func (s *TOMLStore) List() ([]model.JailConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read store dir: %w", err)
	}

	var out []model.JailConfig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".toml")
		cfg, err := s.readOneLocked(name)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *TOMLStore) Get(name string) (model.JailConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !validName(name) {
		return model.JailConfig{}, ErrInvalidName
	}
	return s.readOneLocked(name)
}

func (s *TOMLStore) readOneLocked(name string) (model.JailConfig, error) {
	raw, err := os.ReadFile(s.filePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return model.JailConfig{}, ErrNotFound
	}
	if err != nil {
		return model.JailConfig{}, fmt.Errorf("read jail %q: %w", name, err)
	}
	cfg, err := DecodeTOML(string(raw))
	if err != nil {
		return model.JailConfig{}, fmt.Errorf("decode %s.toml: %w", name, err)
	}
	if cfg.Name == "" {
		cfg.Name = name
	}
	return model.NormalizeConfig(cfg), nil
}

func (s *TOMLStore) writeOneLocked(cfg model.JailConfig) (model.JailConfig, error) {
	if !validName(cfg.Name) {
		return model.JailConfig{}, ErrInvalidName
	}
	cfg = model.NormalizeConfig(cfg)
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return model.JailConfig{}, err
	}
	raw, err := EncodeTOML(cfg)
	if err != nil {
		return model.JailConfig{}, err
	}
	dst := s.filePath(cfg.Name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw), 0o600); err != nil {
		return model.JailConfig{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return model.JailConfig{}, err
	}
	return cfg, nil
}

func (s *TOMLStore) Put(cfg model.JailConfig) (model.JailConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeOneLocked(cfg)
}

func (s *TOMLStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validName(name) {
		return ErrInvalidName
	}
	err := os.Remove(s.filePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (s *TOMLStore) SetMount(name string, entry model.MountEntry) (model.JailConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.readOneLocked(name)
	if err != nil {
		return model.JailConfig{}, err
	}
	replaced := false
	for i := range cfg.Mounts {
		if cfg.Mounts[i].Target == entry.Target {
			cfg.Mounts[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Mounts = append(cfg.Mounts, entry)
	}
	return s.writeOneLocked(cfg)
}

func (s *TOMLStore) RemoveMount(name, target string) (model.JailConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.readOneLocked(name)
	if err != nil {
		return model.JailConfig{}, err
	}
	cfg.Mounts = slices.DeleteFunc(cfg.Mounts, func(m model.MountEntry) bool {
		return m.Target == target
	})
	return s.writeOneLocked(cfg)
}

func (s *TOMLStore) AddPackages(name string, pkgs []string) (model.JailConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.readOneLocked(name)
	if err != nil {
		return model.JailConfig{}, err
	}
	for _, p := range pkgs {
		if !slices.Contains(cfg.Packages, p) {
			cfg.Packages = append(cfg.Packages, p)
		}
	}
	return s.writeOneLocked(cfg)
}

func EncodeTOML(cfg model.JailConfig) (string, error) {
	if len(cfg.Mounts) > 0 {
		mounts := slices.Clone(cfg.Mounts)
		for i := range mounts {
			if mounts[i].Target == mounts[i].Path {
				mounts[i].Target = ""
			}
		}
		cfg.Mounts = mounts
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return "", fmt.Errorf("encode toml: %w", err)
	}
	return buf.String(), nil
}

func DecodeTOML(input string) (model.JailConfig, error) {
	var cfg model.JailConfig
	md, err := toml.Decode(input, &cfg)
	if err != nil {
		return cfg, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return cfg, fmt.Errorf("unknown configuration key %q", undecoded[0].String())
	}
	for i := range cfg.Mounts {
		if cfg.Mounts[i].Target == "" {
			cfg.Mounts[i].Target = cfg.Mounts[i].Path
		}
	}
	return cfg, nil
}
