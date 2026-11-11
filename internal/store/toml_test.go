package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jaild/internal/model"
)

func TestTOMLCodecRoundTrip(t *testing.T) {
	orig := model.JailConfig{
		Name:     "dev",
		Hostname: "dev-box",
		User:     "someuser",
		Home:     "/home/someuser",
		HomeSrc:  "/mnt/homes/dev",
		Network:  true,
		GPU:      true,
		GUI:      false,
		NixMode:  model.NixModeClosure,
		DBusTalk: []string{"org.freedesktop.Notifications"},
		Packages: []string{"/nix/store/abc-bash", "/nix/store/def-git"},
		PassEnv:  []string{"TERM", "EDITOR"},
		Command:  []string{"/bin/sh", "-l"},
		Env: map[string]string{
			"FOO":     "bar",
			"COMPLEX": `hello "world" # not a comment`,
		},
		Mounts: []model.MountEntry{
			{Path: "/mnt/data", Target: "/mnt/data", ReadWrite: true},
			{Path: "/mnt/models", Target: "/models", ReadWrite: false},
		},
	}

	encoded, err := EncodeTOML(orig)
	if err != nil {
		t.Fatalf("EncodeTOML failed: %v", err)
	}
	if strings.Contains(encoded, `target = "/mnt/data"`) {
		t.Fatalf("expected target to be omitted when equal to path, got:\n%s", encoded)
	}
	if !strings.Contains(encoded, `target = "/models"`) {
		t.Fatalf("expected distinct target to be present, got:\n%s", encoded)
	}
	decoded, err := DecodeTOML(encoded)
	if err != nil {
		t.Fatalf("DecodeTOML failed: %v\nencoded:\n%s", err, encoded)
	}
	if !reflect.DeepEqual(decoded, orig) {
		t.Fatalf("round-trip mismatch:\ngot:  %#v\nwant: %#v", decoded, orig)
	}
}

func TestTOMLStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	var s Store
	ts, err := NewTOMLStore(dir)
	if err != nil {
		t.Fatalf("NewTOMLStore: %v", err)
	}
	s = ts

	cfg, err := s.Put(model.JailConfig{
		Name:    "comfy",
		GPU:     true,
		Network: false,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if cfg.User != "comfy" || cfg.Home != "/home/comfy" || cfg.NixMode != model.NixModeClosure {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}

	if _, err := os.Stat(filepath.Join(dir, "comfy.toml")); err != nil {
		t.Fatalf("expected comfy.toml on disk: %v", err)
	}

	cfg, err = s.SetMount("comfy", model.MountEntry{Path: "/mnt/models", Target: "/mnt/models", ReadWrite: false})
	if err != nil {
		t.Fatalf("SetMount: %v", err)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0].ReadWrite {
		t.Fatalf("unexpected mounts: %#v", cfg.Mounts)
	}

	cfg, err = s.SetMount("comfy", model.MountEntry{Path: "/mnt/models", Target: "/mnt/models", ReadWrite: true})
	if err != nil {
		t.Fatalf("SetMount update: %v", err)
	}
	if len(cfg.Mounts) != 1 || !cfg.Mounts[0].ReadWrite {
		t.Fatalf("expected mount to be updated in-place: %#v", cfg.Mounts)
	}

	cfg, err = s.AddPackages("comfy", []string{"/nix/store/xxx-hello", "/nix/store/xxx-hello"})
	if err != nil {
		t.Fatalf("AddPackages: %v", err)
	}
	if len(cfg.Packages) != 1 {
		t.Fatalf("expected deduplicated packages: %#v", cfg.Packages)
	}

	overwritten, err := s.Put(model.JailConfig{
		Name:    "comfy",
		User:    "altuser",
		GPU:     false,
		Network: true,
	})
	if err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	if overwritten.User != "altuser" || overwritten.GPU || !overwritten.Network || len(overwritten.Mounts) != 0 || len(overwritten.Packages) != 0 {
		t.Fatalf("expected Put to overwrite previous config semantics, got: %#v", overwritten)
	}

	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Name != "comfy" {
		t.Fatalf("List: %#v, err=%v", list, err)
	}

	if err := s.Delete("comfy"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("comfy"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

