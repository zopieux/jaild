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
	stageDir := filepath.Join(rootDir, "dev", "stage")

	sys := sysOps{
		mount: func(source, target, fstype string, flags uintptr, data string) error {
			return nil
		},
		unmount: func(target string, flags int) error {
			if target == stageDir {
				entries, _ := os.ReadDir(stageDir)
				for _, e := range entries {
					_ = os.RemoveAll(filepath.Join(stageDir, e.Name()))
				}
			}
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
		filepath.Join(rootDir, "dev", "stub.sock"): true,
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

func TestCreateOrUpdateOverwritesExistingJailAndRemountsStage(t *testing.T) {
	base := t.TempDir()
	storeDir := filepath.Join(base, "store")
	rootDir := filepath.Join(base, "root")
	workDir := filepath.Join(base, "work")
	hostDir := filepath.Join(base, "host-dir")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	st, err := store.NewTOMLStore(storeDir)
	if err != nil {
		t.Fatalf("NewTOMLStore: %v", err)
	}

	stageUnmounts := 0
	stageDir := filepath.Join(rootDir, "dev", "stage")
	sys := sysOps{
		mount: func(source, target, fstype string, flags uintptr, data string) error {
			return nil
		},
		unmount: func(target string, flags int) error {
			if target == stageDir {
				stageUnmounts++
				entries, _ := os.ReadDir(stageDir)
				for _, e := range entries {
					_ = os.RemoveAll(filepath.Join(stageDir, e.Name()))
				}
			}
			return nil
		},
		statfs: func(path string, buf *syscall.Statfs_t) error {
			return nil
		},
		remove: os.Remove,
	}

	mgr, err := newWithSys(Options{
		Store:    st,
		RootDir:  rootDir,
		WorkDir:  workDir,
		HostUID:  1000,
		HostGID:  1000,
		HostUser: "someuser",
	}, sys)
	if err != nil {
		t.Fatalf("newWithSys: %v", err)
	}

	if _, err := mgr.CreateOrUpdateJail(model.JailConfig{
		Name:    "dev",
		User:    "firstuser",
		Network: true,
		GPU:     true,
		NixMode: model.NixModeFull,
	}); err != nil {
		t.Fatalf("first CreateOrUpdateJail: %v", err)
	}
	if _, err := mgr.EnsureStage("dev"); err != nil {
		t.Fatalf("EnsureStage: %v", err)
	}
	if _, err := mgr.Expose("dev", hostDir, "/work/data", true); err != nil {
		t.Fatalf("Expose: %v", err)
	}

	updated, err := mgr.CreateOrUpdateJail(model.JailConfig{
		Name:    "dev",
		User:    "seconduser",
		Network: false,
		GPU:     false,
		NixMode: model.NixModeFull,
	})
	if err != nil {
		t.Fatalf("second CreateOrUpdateJail: %v", err)
	}

	if updated.Config.User != "seconduser" || updated.Config.Home != "/home/seconduser" {
		t.Fatalf("expected user/home to be overwritten, got %#v", updated.Config)
	}
	if updated.Config.Network || updated.Config.GPU {
		t.Fatalf("expected network/gpu flags to be reset, got %#v", updated.Config)
	}
	if len(updated.Config.Mounts) != 0 || len(updated.Config.Packages) != 0 {
		t.Fatalf("expected previous mounts and packages to be cleared, got %#v", updated.Config)
	}
	if !updated.Active || stageUnmounts != 1 {
		t.Fatalf("expected active stage to be torn down and remounted once (active=%v, unmounts=%d)", updated.Active, stageUnmounts)
	}
	if mgr.jails["dev"].stage.mounted["/work/data"] {
		t.Fatalf("expected remounted stage not to retain old /work/data mount")
	}
}

func TestExtractStoreRootsAndResolveJailPath(t *testing.T) {
	text := `export PATH='/nix/store/aaaa-hello-2.12/bin:/nix/store/bbbb-ripgrep-14.1/bin:/bin'
declare -x FOO="/nix/store/aaaa-hello-2.12/share/info"`
	roots := ExtractStoreRoots(text)
	if len(roots) != 2 || roots[0] != "/nix/store/aaaa-hello-2.12" || roots[1] != "/nix/store/bbbb-ripgrep-14.1" {
		t.Fatalf("unexpected store roots: %v", roots)
	}

	m := &Manager{}
	cfg := model.JailConfig{
		Home:    "/home/alex",
		HomeSrc: "/host/home/alex",
		Mounts: []model.MountEntry{
			{Path: "/host/projects/demo", Target: "/home/alex/dev/demo", ReadWrite: true},
			{Path: "/tmp/test", Target: "/tmp/test", ReadWrite: true},
		},
	}
	got1, rw1, err1 := m.resolveJailPathToHostLocked(cfg, "/home/alex/dev/demo/.direnv")
	if err1 != nil || !rw1 || got1 != "/host/projects/demo/.direnv" {
		t.Fatalf("expected /host/projects/demo/.direnv (rw=true), got %q (rw=%v, err=%v)", got1, rw1, err1)
	}
	got2, rw2, err2 := m.resolveJailPathToHostLocked(cfg, "/home/alex/.cache/direnv")
	if err2 != nil || !rw2 || got2 != "/host/home/alex/.cache/direnv" {
		t.Fatalf("expected /host/home/alex/.cache/direnv (rw=true), got %q (rw=%v, err=%v)", got2, rw2, err2)
	}
	if _, _, err := m.resolveJailPathToHostLocked(cfg, "/etc/shadow"); err == nil {
		t.Fatalf("expected /etc/shadow not to resolve to a host path")
	}
}
