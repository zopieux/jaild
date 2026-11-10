package store

import (
	"errors"
	"os"
	"path/filepath"

	"jaild/internal/model"
)

var (
	ErrNotFound    = errors.New("jail not found")
	ErrInvalidName = errors.New("invalid jail name")
)

// Store defines the persistence interface for jail configurations.
type Store interface {
	List() ([]model.JailConfig, error)
	Get(name string) (model.JailConfig, error)
	Put(cfg model.JailConfig) (model.JailConfig, error)
	Delete(name string) error
	SetMount(name string, entry model.MountEntry) (model.JailConfig, error)
	RemoveMount(name, path string) (model.JailConfig, error)
	AddPackages(name string, pkgs []string) (model.JailConfig, error)
}

func DefaultDir() string {
	if d := os.Getenv("JAILD_DATA_DIR"); d != "" {
		return d
	}
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "jaild", "jails")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/tmp/jaild-jails"
	}
	return filepath.Join(home, ".local", "share", "jaild", "jails")
}
