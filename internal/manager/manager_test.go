package manager

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"jaild/internal/model"
	"jaild/internal/store"
)

func TestCheckStrictSubpathAndSafeRemove(t *testing.T) {
	base := t.TempDir()
	allowedRoot := filepath.Join(base, "jails")
	siblingRoot := filepath.Join(base, "jails-sibling")
	hostDir := filepath.Join(base, "host-important")
	hostVictim := filepath.Join(hostDir, "victim-dir")

	for _, d := range []string{allowedRoot, siblingRoot, hostVictim} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	symlinkInRoot := filepath.Join(allowedRoot, "escaped-link")
	if err := os.Symlink(hostDir, symlinkInRoot); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	validChild := filepath.Join(allowedRoot, "dev-stage")
	if err := os.MkdirAll(validChild, 0o755); err != nil {
		t.Fatalf("mkdir validChild: %v", err)
	}

	var removed []string
	m := &Manager{
		rootDir: allowedRoot,
		sys: sysOps{
			remove: func(p string) error {
				removed = append(removed, p)
				return os.Remove(p)
			},
		},
	}

	rejectedTargets := []string{
		"",
		"relative/path",
		"/",
		allowedRoot,
		filepath.Join(allowedRoot, ".."),
		filepath.Join(allowedRoot, "../host-important/victim-dir"),
		siblingRoot,
		filepath.Join(siblingRoot, "sub"),
		hostDir,
		hostVictim,
		filepath.Join(symlinkInRoot, "victim-dir"),
	}

	for _, target := range rejectedTargets {
		if err := m.safeRemoveUnder(allowedRoot, target); err == nil {
			t.Fatalf("expected safeRemoveUnder(%q, %q) to fail, but it succeeded", allowedRoot, target)
		}
	}

	if len(removed) != 0 {
		t.Fatalf("expected zero removals after rejected calls, got %v", removed)
	}
	if _, err := os.Stat(hostVictim); err != nil {
		t.Fatalf("hostVictim should still exist: %v", err)
	}

	if err := m.safeRemoveUnder(allowedRoot, validChild); err != nil {
		t.Fatalf("expected validChild removal to succeed, got: %v", err)
	}
	if len(removed) != 1 || removed[0] != validChild {
		t.Fatalf("unexpected removed list: %v", removed)
	}
}

func TestManagerDeletedHostFoldersAreStrictlyScoped(t *testing.T) {
	base := t.TempDir()
	storeDir := filepath.Join(base, "store")
	rootDir := filepath.Join(base, "root")
	workDir := filepath.Join(base, "work")

	hostHome := filepath.Join(base, "host-home")
	homeSrc := filepath.Join(base, "host-jail-home")
	homeSrcEmptySub := filepath.Join(homeSrc, "empty-user-dir")
	hostProject := filepath.Join(base, "host-project")
	hostProjectEmptySub := filepath.Join(hostProject, "empty-subdir")
	hostProjectFile := filepath.Join(hostProject, "README.txt")
	hostNestedSrc := filepath.Join(base, "host-nested")
	hostSingleFile := filepath.Join(base, "host-single.conf")

	for _, d := range []string{hostHome, homeSrcEmptySub, hostProjectEmptySub, hostNestedSrc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(hostProjectFile, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(hostSingleFile, []byte("conf"), 0o644); err != nil {
		t.Fatalf("write single file: %v", err)
	}

	st, err := store.NewTOMLStore(storeDir)
	if err != nil {
		t.Fatalf("NewTOMLStore: %v", err)
	}

	var mu sync.Mutex
	var removedPaths []string

	sys := sysOps{
		mount: func(source, target, fstype string, flags uintptr, data string) error {
			return nil
		},
		unmount: func(target string, flags int) error {
			return nil
		},
		statfs: func(path string, buf *syscall.Statfs_t) error {
			return nil
		},
		remove: func(path string) error {
			mu.Lock()
			removedPaths = append(removedPaths, path)
			mu.Unlock()
			return os.Remove(path)
		},
	}

	mgr, err := newWithSys(Options{
		Store:    st,
		RootDir:  rootDir,
		WorkDir:  workDir,
		HostUID:  1000,
		HostGID:  1000,
		HostUser: "someuser",
		HostHome: hostHome,
	}, sys)
	if err != nil {
		t.Fatalf("newWithSys: %v", err)
	}

	_, err = mgr.CreateOrUpdateJail(model.JailConfig{
		Name:    "dev",
		HomeSrc: homeSrc,
		NixMode: model.NixModeFull,
	})
	if err != nil {
		t.Fatalf("CreateOrUpdateJail: %v", err)
	}

	if _, err := mgr.EnsureStage("dev"); err != nil {
		t.Fatalf("EnsureStage: %v", err)
	}

	if _, err := mgr.Expose("dev", hostProject, "/work/project", true); err != nil {
		t.Fatalf("Expose dir: %v", err)
	}
	if _, err := mgr.Expose("dev", hostSingleFile, "/etc/single.conf", false); err != nil {
		t.Fatalf("Expose file: %v", err)
	}

	stageDir := filepath.Join(rootDir, "dev", "stage")
	simulatedBoundSubdir := filepath.Join(stageDir, "work/project/empty-subdir")
	if err := os.MkdirAll(simulatedBoundSubdir, 0o755); err != nil {
		t.Fatalf("mkdir simulatedBoundSubdir: %v", err)
	}
	if _, err := mgr.Hide("dev", "/work/project/empty-subdir"); err != nil {
		t.Fatalf("Hide unmounted subpath: %v", err)
	}
	if _, err := os.Stat(simulatedBoundSubdir); err != nil {
		t.Fatalf("Hide on unmounted subpath deleted %s!", simulatedBoundSubdir)
	}
	_ = os.Remove(simulatedBoundSubdir)

	if _, err := mgr.Expose("dev", hostNestedSrc, "/work/project/nested", false); err != nil {
		t.Fatalf("Expose nested: %v", err)
	}
	nestedStageDir := filepath.Join(stageDir, "work/project/nested")
	if _, err := mgr.Hide("dev", "/work/project/nested"); err != nil {
		t.Fatalf("Hide nested: %v", err)
	}
	for _, p := range removedPaths {
		if p == nestedStageDir {
			t.Fatalf("Hide removed nested mountpoint stub %s inside parent mount /work/project", nestedStageDir)
		}
	}
	_ = os.Remove(nestedStageDir)

	if _, err := mgr.Hide("dev", "/"); err == nil {
		t.Fatalf("expected Hide('/') to fail")
	}
	if _, err := mgr.Expose("dev", hostProject, "/", true); err == nil {
		t.Fatalf("expected Expose('/', target='/') to fail")
	}

	if _, err := mgr.Hide("dev", "/work/project"); err != nil {
		t.Fatalf("Hide /work/project: %v", err)
	}
	if _, err := mgr.Hide("dev", "/etc/single.conf"); err != nil {
		t.Fatalf("Hide /etc/single.conf: %v", err)
	}

	if err := mgr.DeleteJail("dev"); err != nil {
		t.Fatalf("DeleteJail: %v", err)
	}
	if err := mgr.DeleteJail("../escape"); err != store.ErrInvalidName {
		t.Fatalf("expected ErrInvalidName for '../escape', got %v", err)
	}

	if len(removedPaths) == 0 {
		t.Fatalf("expected removals to be recorded")
	}
	expectedStageTargets := map[string]bool{
		filepath.Join(stageDir, "work/project"):    true,
		filepath.Join(stageDir, "etc/single.conf"): true,
		stageDir:                          true,
		filepath.Join(rootDir, "dev"):     true,
		filepath.Join(workDir, "exp-dir"): false,
	}
	sawStageDir := false
	sawJailDir := false
	sawWorkProjectStub := false
	sawSingleConfStub := false

	for _, p := range removedPaths {
		inWork := strings.HasPrefix(p, workDir+string(filepath.Separator)) && p != workDir
		inRoot := strings.HasPrefix(p, rootDir+string(filepath.Separator)) && p != rootDir
		if !inWork && !inRoot {
			t.Fatalf("daemon removed path outside rootDir and workDir: %q", p)
		}
		if inRoot && !expectedStageTargets[p] {
			t.Fatalf("unexpected removal inside rootDir: %q", p)
		}
		switch p {
		case stageDir:
			sawStageDir = true
		case filepath.Join(rootDir, "dev"):
			sawJailDir = true
		case filepath.Join(stageDir, "work/project"):
			sawWorkProjectStub = true
		case filepath.Join(stageDir, "etc/single.conf"):
			sawSingleConfStub = true
		}
	}

	if !sawStageDir || !sawJailDir || !sawWorkProjectStub || !sawSingleConfStub {
		t.Fatalf("missing expected stage cleanup removals; got %v", removedPaths)
	}

	mustExist := []string{
		rootDir,
		workDir,
		storeDir,
		hostHome,
		homeSrc,
		homeSrcEmptySub,
		hostProject,
		hostProjectEmptySub,
		hostProjectFile,
		hostNestedSrc,
		hostSingleFile,
	}
	for _, p := range mustExist {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("host path %q was unexpectedly deleted or missing: %v", p, err)
		}
	}
}
