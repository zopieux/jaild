package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

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
	raw := EncodeTOML(cfg)
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
		if cfg.Mounts[i].Path == entry.Path {
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

func (s *TOMLStore) RemoveMount(name, path string) (model.JailConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.readOneLocked(name)
	if err != nil {
		return model.JailConfig{}, err
	}
	cfg.Mounts = slices.DeleteFunc(cfg.Mounts, func(m model.MountEntry) bool {
		return m.Path == path
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

func EncodeTOML(cfg model.JailConfig) string {
	var b strings.Builder
	writeKV := func(k string, v string) {
		fmt.Fprintf(&b, "%s = %s\n", k, strconv.Quote(v))
	}
	writeBool := func(k string, v bool) {
		fmt.Fprintf(&b, "%s = %t\n", k, v)
	}
	writeSlice := func(k string, vs []string) {
		if len(vs) == 0 {
			return
		}
		quoted := make([]string, len(vs))
		for i, v := range vs {
			quoted[i] = strconv.Quote(v)
		}
		fmt.Fprintf(&b, "%s = [%s]\n", k, strings.Join(quoted, ", "))
	}

	writeKV("name", cfg.Name)
	if cfg.Hostname != "" {
		writeKV("hostname", cfg.Hostname)
	}
	if cfg.User != "" {
		writeKV("user", cfg.User)
	}
	if cfg.Home != "" {
		writeKV("home", cfg.Home)
	}
	if cfg.HomeSrc != "" {
		writeKV("home_src", cfg.HomeSrc)
	}
	writeBool("network", cfg.Network)
	writeBool("gpu", cfg.GPU)
	writeBool("gui", cfg.GUI)
	writeKV("nix_mode", string(cfg.NixMode))
	writeSlice("dbus_talk", cfg.DBusTalk)
	writeSlice("packages", cfg.Packages)
	writeSlice("pass_env", cfg.PassEnv)
	writeSlice("command", cfg.Command)

	if len(cfg.Env) > 0 {
		b.WriteString("\n[env]\n")
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %s\n", formatTOMLKey(k), strconv.Quote(cfg.Env[k]))
		}
	}

	for _, m := range cfg.Mounts {
		b.WriteString("\n[[mounts]]\n")
		writeKV("path", m.Path)
		writeBool("ro", m.ReadOnly)
	}

	return b.String()
}

func formatTOMLKey(k string) string {
	for _, r := range k {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return strconv.Quote(k)
		}
	}
	if k == "" {
		return `""`
	}
	return k
}

func DecodeTOML(input string) (model.JailConfig, error) {
	var cfg model.JailConfig
	section := ""
	var curMount *model.MountEntry

	lines := strings.Split(input, "\n")
	for lineNo, rawLine := range lines {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
			sec := strings.TrimSpace(line[2 : len(line)-2])
			if sec != "mounts" {
				return cfg, fmt.Errorf("line %d: unknown array table [[%s]]", lineNo+1, sec)
			}
			section = "[[mounts]]"
			cfg.Mounts = append(cfg.Mounts, model.MountEntry{})
			curMount = &cfg.Mounts[len(cfg.Mounts)-1]
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sec := strings.TrimSpace(line[1 : len(line)-1])
			if sec != "env" {
				return cfg, fmt.Errorf("line %d: unknown table [%s]", lineNo+1, sec)
			}
			section = "[env]"
			if cfg.Env == nil {
				cfg.Env = make(map[string]string)
			}
			curMount = nil
			continue
		}

		eqIdx := findTOMLEquals(line)
		if eqIdx < 0 {
			return cfg, fmt.Errorf("line %d: expected key = value", lineNo+1)
		}
		key, err := parseTOMLKey(strings.TrimSpace(line[:eqIdx]))
		if err != nil {
			return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		valStr := strings.TrimSpace(line[eqIdx+1:])

		switch section {
		case "[env]":
			v, err := parseTOMLString(valStr)
			if err != nil {
				return cfg, fmt.Errorf("line %d: env.%s: %w", lineNo+1, key, err)
			}
			cfg.Env[key] = v

		case "[[mounts]]":
			switch key {
			case "path":
				v, err := parseTOMLString(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: mounts.path: %w", lineNo+1, err)
				}
				curMount.Path = v
			case "ro":
				v, err := strconv.ParseBool(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: mounts.ro: %w", lineNo+1, err)
				}
				curMount.ReadOnly = v
			default:
				return cfg, fmt.Errorf("line %d: unknown mounts key %q", lineNo+1, key)
			}

		default:
			switch key {
			case "name":
				v, err := parseTOMLString(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.Name = v
			case "hostname":
				v, err := parseTOMLString(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.Hostname = v
			case "user":
				v, err := parseTOMLString(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.User = v
			case "home":
				v, err := parseTOMLString(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.Home = v
			case "home_src":
				v, err := parseTOMLString(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.HomeSrc = v
			case "nix_mode":
				v, err := parseTOMLString(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.NixMode = model.NixMode(v)
			case "network":
				v, err := strconv.ParseBool(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.Network = v
			case "gpu":
				v, err := strconv.ParseBool(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.GPU = v
			case "gui":
				v, err := strconv.ParseBool(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.GUI = v
			case "dbus_talk":
				vs, err := parseTOMLStringArray(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.DBusTalk = vs
			case "packages":
				vs, err := parseTOMLStringArray(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.Packages = vs
			case "pass_env":
				vs, err := parseTOMLStringArray(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.PassEnv = vs
			case "command":
				vs, err := parseTOMLStringArray(valStr)
				if err != nil {
					return cfg, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.Command = vs
			default:
				return cfg, fmt.Errorf("line %d: unknown key %q", lineNo+1, key)
			}
		}
	}
	return cfg, nil
}

func stripTOMLComment(s string) string {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inDouble {
			escaped = true
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '#' && !inDouble && !inSingle {
			return s[:i]
		}
	}
	return s
}

func findTOMLEquals(s string) int {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inDouble {
			escaped = true
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '=' && !inDouble && !inSingle {
			return i
		}
	}
	return -1
}

func parseTOMLKey(s string) (string, error) {
	if strings.HasPrefix(s, `"`) || strings.HasPrefix(s, `'`) {
		return parseTOMLString(s)
	}
	if s == "" {
		return "", errors.New("empty key")
	}
	return s, nil
}

func parseTOMLString(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1], nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strconv.Unquote(s)
	}
	return "", fmt.Errorf("invalid string literal %q", s)
}

func parseTOMLStringArray(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("expected array, got %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil
	}
	var items []string
	var cur strings.Builder
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && inDouble {
			cur.WriteByte(c)
			escaped = true
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			cur.WriteByte(c)
		} else if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteByte(c)
		} else if c == ',' && !inDouble && !inSingle {
			token := strings.TrimSpace(cur.String())
			if token != "" {
				v, err := parseTOMLString(token)
				if err != nil {
					return nil, err
				}
				items = append(items, v)
			}
			cur.Reset()
		} else {
			cur.WriteByte(c)
		}
	}
	token := strings.TrimSpace(cur.String())
	if token != "" {
		v, err := parseTOMLString(token)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, nil
}

