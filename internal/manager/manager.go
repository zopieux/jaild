package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"

	"jaild/internal/model"
	"jaild/internal/store"
)

var ErrJailBusy = errors.New("jail is busy with an in-flight operation")

type Options struct {
	Store    store.Store
	RootDir  string
	WorkDir  string
	HostUID  int
	HostGID  int
	HostUser string
	HostHome string
}

type stageMountState int

const (
	stageUnmounted stageMountState = iota
	stageMountedRO
	stageMountedRW
)

type stageState struct {
	stageDir    string
	mountState  stageMountState
	rwDepth     int
	mounted     map[string]bool
	storeMounts map[string]bool
	binPaths    []string
	dbusProxy   *exec.Cmd
	dbusSock    string
}

type writableStage struct {
	*stageState
}

type jailState int

const (
	stateStopped jailState = iota
	stateMounted
	stateAddingPkgStopped
	stateAddingPkgMounted
	stateDeleting
)

func (s jailState) isMounted() bool {
	return s == stateMounted || s == stateAddingPkgMounted
}

func (s jailState) isAddingPkg() bool {
	return s == stateAddingPkgStopped || s == stateAddingPkgMounted
}

type jailRuntime struct {
	state       jailState
	stage       *stageState
	pkgCancel   context.CancelFunc
	pkgProgress string
}

type sysOps struct {
	mount   func(source string, target string, fstype string, flags uintptr, data string) error
	unmount func(target string, flags int) error
	statfs  func(path string, buf *syscall.Statfs_t) error
	remove  func(path string) error
}

func defaultSysOps() sysOps {
	return sysOps{
		mount:   syscall.Mount,
		unmount: syscall.Unmount,
		statfs:  syscall.Statfs,
		remove:  os.Remove,
	}
}

type Manager struct {
	mu       sync.Mutex
	store    store.Store
	rootDir  string
	workDir  string
	hostUID  int
	hostGID  int
	hostUser string
	hostHome string
	jails    map[string]*jailRuntime
	sys      sysOps

	subMu sync.Mutex
	subs  map[chan model.Event]struct{}
}

func New(opts Options) (*Manager, error) {
	return newWithSys(opts, defaultSysOps())
}

func newWithSys(opts Options, sys sysOps) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("manager requires a store")
	}
	if opts.RootDir == "" {
		opts.RootDir = fmt.Sprintf("/run/user/%d/jails", opts.HostUID)
	}
	if opts.WorkDir == "" {
		opts.WorkDir = fmt.Sprintf("/run/user/%d/jail-work", opts.HostUID)
	}
	m := &Manager{
		store:    opts.Store,
		rootDir:  filepath.Clean(opts.RootDir),
		workDir:  filepath.Clean(opts.WorkDir),
		hostUID:  opts.HostUID,
		hostGID:  opts.HostGID,
		hostUser: opts.HostUser,
		hostHome: opts.HostHome,
		jails:    make(map[string]*jailRuntime),
		sys:      sys,
		subs:     make(map[chan model.Event]struct{}),
	}
	if err := m.initWorkspace(); err != nil {
		return nil, err
	}
	return m, nil
}

func validJailName(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || name == "." || name == ".." {
		return false
	}
	return true
}

func checkStrictSubpath(allowedRoot, target string) error {
	cleanRoot := filepath.Clean(allowedRoot)
	cleanTarget := filepath.Clean(target)
	if !filepath.IsAbs(cleanRoot) || cleanRoot == "/" {
		return fmt.Errorf("invalid allowed root %q", allowedRoot)
	}
	if !filepath.IsAbs(cleanTarget) {
		return fmt.Errorf("target path must be absolute: %q", target)
	}
	if cleanTarget == cleanRoot || !strings.HasPrefix(cleanTarget, cleanRoot+string(filepath.Separator)) {
		return fmt.Errorf("refusing to remove path %q outside %q", cleanTarget, cleanRoot)
	}
	if resolvedRoot, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(cleanTarget)); err == nil {
			resolvedRoot = filepath.Clean(resolvedRoot)
			resolvedParent = filepath.Clean(resolvedParent)
			if resolvedParent != resolvedRoot && !strings.HasPrefix(resolvedParent, resolvedRoot+string(filepath.Separator)) {
				return fmt.Errorf("refusing to remove path %q: parent resolves to %q outside %q", cleanTarget, resolvedParent, resolvedRoot)
			}
		}
	}
	return nil
}

func (m *Manager) safeRemoveUnder(allowedRoot, target string) error {
	if err := checkStrictSubpath(allowedRoot, target); err != nil {
		return err
	}
	return m.sys.remove(filepath.Clean(target))
}

func hasMountedAncestor(mounted map[string]bool, dstPath string) bool {
	cleanDst := filepath.Clean(dstPath)
	for mp, active := range mounted {
		if !active {
			continue
		}
		cleanMP := filepath.Clean(mp)
		if cleanMP != cleanDst && strings.HasPrefix(cleanDst, cleanMP+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (m *Manager) runtimeLocked(name string) *jailRuntime {
	rt, ok := m.jails[name]
	if !ok {
		rt = &jailRuntime{state: stateStopped}
		m.jails[name] = rt
	}
	return rt
}

func (m *Manager) SubscribeEvents() (chan model.Event, func()) {
	ch := make(chan model.Event, 32)
	m.subMu.Lock()
	m.subs[ch] = struct{}{}
	m.subMu.Unlock()
	return ch, func() {
		m.subMu.Lock()
		delete(m.subs, ch)
		close(ch)
		m.subMu.Unlock()
	}
}

func (m *Manager) broadcast(ev model.Event) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (m *Manager) statusLocked(cfg model.JailConfig) model.JailStatus {
	rt := m.runtimeLocked(cfg.Name)
	active := rt.state.isMounted()
	storeCount := 0
	if active && rt.stage != nil {
		storeCount = len(rt.stage.storeMounts)
	}
	return model.JailStatus{
		Config:      cfg,
		Active:      active,
		StagePath:   filepath.Join(m.rootDir, cfg.Name, "stage"),
		StorePaths:  storeCount,
		AddingPkg:   rt.state.isAddingPkg(),
		PkgProgress: rt.pkgProgress,
	}
}

func (m *Manager) initWorkspace() error {
	if err := os.MkdirAll(m.rootDir, 0o700); err != nil {
		return fmt.Errorf("mkdir rootDir %s: %w", m.rootDir, err)
	}
	if err := os.MkdirAll(m.workDir, 0o700); err != nil {
		return fmt.Errorf("mkdir workDir %s: %w", m.workDir, err)
	}
	if err := m.sys.mount("tmpfs", m.workDir, "tmpfs", 0, "mode=0700"); err != nil {
		return fmt.Errorf("mount tmpfs on workDir %s: %w", m.workDir, err)
	}
	if err := m.sys.mount("", m.workDir, "", syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make-private workDir %s: %w", m.workDir, err)
	}
	return nil
}

func (m *Manager) Store() store.Store {
	return m.store
}

func (m *Manager) ListStatuses() ([]model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfgs, err := m.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]model.JailStatus, 0, len(cfgs))
	for _, cfg := range cfgs {
		out = append(out, m.statusLocked(cfg))
	}
	return out, nil
}

func (m *Manager) GetStatus(name string) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := m.store.Get(name)
	if err != nil {
		return model.JailStatus{}, err
	}
	return m.statusLocked(cfg), nil
}

func stageConfigEqual(a, b model.JailConfig) bool {
	return a.User == b.User &&
		a.Home == b.Home &&
		a.HomeSrc == b.HomeSrc &&
		a.Network == b.Network &&
		a.GPU == b.GPU &&
		a.GUI == b.GUI &&
		a.NixMode == b.NixMode &&
		slices.Equal(a.DBusTalk, b.DBusTalk) &&
		slices.Equal(a.Packages, b.Packages) &&
		slices.Equal(a.Mounts, b.Mounts)
}

func (m *Manager) CreateOrUpdateJail(cfg model.JailConfig) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	prevCfg, prevErr := m.store.Get(cfg.Name)
	rt := m.runtimeLocked(cfg.Name)

	switch rt.state {
	case stateStopped:
		saved, err := m.store.Put(cfg)
		if err != nil {
			return model.JailStatus{}, err
		}
		return m.statusLocked(saved), nil

	case stateMounted:
		saved, err := m.store.Put(cfg)
		if err != nil {
			return model.JailStatus{}, err
		}
		if prevErr == nil && !stageConfigEqual(prevCfg, saved) {
			if err := m.teardownStageLocked(saved.Name, rt); err != nil {
				return model.JailStatus{}, err
			}
			if _, err := m.ensureStageLocked(saved, rt); err != nil {
				return model.JailStatus{}, err
			}
		}
		return m.statusLocked(saved), nil

	case stateAddingPkgStopped, stateAddingPkgMounted, stateDeleting:
		return model.JailStatus{}, ErrJailBusy

	default:
		return model.JailStatus{}, fmt.Errorf("unexpected jail state %d", rt.state)
	}
}

func (m *Manager) EnsureStage(name string) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := m.store.Get(name)
	if err != nil {
		return model.JailStatus{}, err
	}
	rt := m.runtimeLocked(name)

	switch rt.state {
	case stateStopped:
		if _, err := m.ensureStageLocked(cfg, rt); err != nil {
			return model.JailStatus{}, err
		}
		return m.statusLocked(cfg), nil

	case stateMounted, stateAddingPkgMounted:
		return m.statusLocked(cfg), nil

	case stateAddingPkgStopped, stateDeleting:
		return model.JailStatus{}, ErrJailBusy

	default:
		return model.JailStatus{}, fmt.Errorf("unexpected jail state %d", rt.state)
	}
}

func (m *Manager) StopStage(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.store.Get(name); err != nil {
		return err
	}
	rt := m.runtimeLocked(name)

	switch rt.state {
	case stateStopped:
		return nil

	case stateMounted:
		return m.teardownStageLocked(name, rt)

	case stateAddingPkgStopped, stateAddingPkgMounted, stateDeleting:
		return ErrJailBusy

	default:
		return fmt.Errorf("unexpected jail state %d", rt.state)
	}
}

func (m *Manager) DeleteJail(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !validJailName(name) {
		return store.ErrInvalidName
	}

	rt, ok := m.jails[name]
	if !ok {
		return m.store.Delete(name)
	}
	switch rt.state {
	case stateDeleting:
		return ErrJailBusy

	case stateAddingPkgStopped, stateAddingPkgMounted:
		if rt.pkgCancel != nil {
			rt.pkgCancel()
			rt.pkgCancel = nil
		}
		rt.pkgProgress = ""
		if rt.stage != nil {
			if err := m.teardownStageLocked(name, rt); err != nil {
				return err
			}
		}
		rt.state = stateDeleting

	case stateMounted:
		if err := m.teardownStageLocked(name, rt); err != nil {
			return err
		}
		rt.state = stateDeleting

	case stateStopped:
		rt.state = stateDeleting

	default:
		return fmt.Errorf("unexpected jail state %d", rt.state)
	}

	delete(m.jails, name)
	return m.store.Delete(name)
}

func (m *Manager) Expose(name, rawPath, rawTarget string, rw bool) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleanPath := filepath.Clean(rawPath)
	if !filepath.IsAbs(cleanPath) || cleanPath == "/" {
		return model.JailStatus{}, fmt.Errorf("path must be an absolute non-root path: %q", rawPath)
	}
	if _, err := os.Stat(cleanPath); err != nil {
		return model.JailStatus{}, fmt.Errorf("source path %q: %w", cleanPath, err)
	}

	cleanTarget := cleanPath
	if strings.TrimSpace(rawTarget) != "" {
		cleanTarget = filepath.Clean(strings.TrimSpace(rawTarget))
		if !filepath.IsAbs(cleanTarget) || cleanTarget == "/" {
			return model.JailStatus{}, fmt.Errorf("target path must be an absolute non-root path: %q", rawTarget)
		}
	}

	rt := m.runtimeLocked(name)
	switch rt.state {
	case stateStopped, stateAddingPkgStopped:
		entry := model.MountEntry{Path: cleanPath, Target: cleanTarget, ReadWrite: rw}
		cfg, err := m.store.SetMount(name, entry)
		if err != nil {
			return model.JailStatus{}, err
		}
		return m.statusLocked(cfg), nil

	case stateMounted, stateAddingPkgMounted:
		entry := model.MountEntry{Path: cleanPath, Target: cleanTarget, ReadWrite: rw}
		cfg, err := m.store.SetMount(name, entry)
		if err != nil {
			return model.JailStatus{}, err
		}
		if err := m.withWritableStage(rt.stage, func(ws writableStage) error {
			return m.exposePathLocked(ws, entry.Path, entry.Target, !rw)
		}); err != nil {
			return model.JailStatus{}, err
		}
		return m.statusLocked(cfg), nil

	case stateDeleting:
		return model.JailStatus{}, ErrJailBusy

	default:
		return model.JailStatus{}, fmt.Errorf("unexpected jail state %d", rt.state)
	}
}

func (m *Manager) Hide(name, rawTarget string) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleanTarget := filepath.Clean(rawTarget)
	if !filepath.IsAbs(cleanTarget) || cleanTarget == "/" {
		return model.JailStatus{}, fmt.Errorf("target path must be an absolute non-root path: %q", rawTarget)
	}

	rt := m.runtimeLocked(name)
	switch rt.state {
	case stateStopped, stateAddingPkgStopped:
		cfg, err := m.store.RemoveMount(name, cleanTarget)
		if err != nil {
			return model.JailStatus{}, err
		}
		return m.statusLocked(cfg), nil

	case stateMounted, stateAddingPkgMounted:
		cfg, err := m.store.RemoveMount(name, cleanTarget)
		if err != nil {
			return model.JailStatus{}, err
		}
		if err := m.withWritableStage(rt.stage, func(ws writableStage) error {
			return m.hidePathLocked(ws, cleanTarget)
		}); err != nil {
			return model.JailStatus{}, err
		}
		return m.statusLocked(cfg), nil

	case stateDeleting:
		return model.JailStatus{}, ErrJailBusy

	default:
		return model.JailStatus{}, fmt.Errorf("unexpected jail state %d", rt.state)
	}
}

func (m *Manager) beginAddPackagesLocked(name string) (context.Context, error) {
	if _, err := m.store.Get(name); err != nil {
		return nil, err
	}
	rt := m.runtimeLocked(name)
	switch rt.state {
	case stateStopped:
		rt.state = stateAddingPkgStopped
	case stateMounted:
		rt.state = stateAddingPkgMounted
	case stateAddingPkgStopped, stateAddingPkgMounted, stateDeleting:
		return nil, ErrJailBusy
	default:
		return nil, fmt.Errorf("unexpected jail state %d", rt.state)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.pkgCancel = cancel
	rt.pkgProgress = "resolving package closure..."
	return ctx, nil
}

func (m *Manager) finishAddPackagesLocked(name string) {
	rt, ok := m.jails[name]
	if !ok {
		return
	}
	if rt.pkgCancel != nil {
		rt.pkgCancel()
		rt.pkgCancel = nil
	}
	rt.pkgProgress = ""
	switch rt.state {
	case stateAddingPkgStopped:
		rt.state = stateStopped
	case stateAddingPkgMounted:
		rt.state = stateMounted
	}
}

func (m *Manager) AddPackagesAsync(name string, specs []string) error {
	m.mu.Lock()
	ctx, err := m.beginAddPackagesLocked(name)
	m.mu.Unlock()
	if err != nil {
		return err
	}

	go func() {
		if _, err := m.runAddPackages(ctx, name, specs); err != nil {
			if !errors.Is(err, context.Canceled) {
				m.broadcast(model.Event{
					Type:  "pkg_error",
					Jail:  name,
					Error: err.Error(),
				})
			}
			return
		}
		m.broadcast(model.Event{
			Type: "pkg_done",
			Jail: name,
		})
	}()
	return nil
}

func (m *Manager) AddPackages(name string, specs []string) (model.JailStatus, error) {
	m.mu.Lock()
	ctx, err := m.beginAddPackagesLocked(name)
	m.mu.Unlock()
	if err != nil {
		return model.JailStatus{}, err
	}
	return m.runAddPackages(ctx, name, specs)
}

func (m *Manager) runAddPackages(ctx context.Context, name string, specs []string) (model.JailStatus, error) {
	m.broadcast(model.Event{
		Type:    "pkg_progress",
		Jail:    name,
		Message: "resolving package closure...",
	})

	defer func() {
		m.mu.Lock()
		m.finishAddPackagesLocked(name)
		m.mu.Unlock()
	}()

	onProgress := func(msg string) {
		m.mu.Lock()
		rt, ok := m.jails[name]
		if !ok || !rt.state.isAddingPkg() {
			m.mu.Unlock()
			return
		}
		rt.pkgProgress = msg
		m.mu.Unlock()
		m.broadcast(model.Event{
			Type:    "pkg_progress",
			Jail:    name,
			Message: msg,
		})
	}

	var resolvedRoots []string
	for _, spec := range specs {
		roots, err := ResolveInstallableOrPathWithProgress(ctx, spec, onProgress)
		if err != nil {
			return model.JailStatus{}, err
		}
		for _, r := range roots {
			if !slices.Contains(resolvedRoots, r) {
				resolvedRoots = append(resolvedRoots, r)
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return model.JailStatus{}, err
	}
	rt, ok := m.jails[name]
	if !ok || !rt.state.isAddingPkg() {
		return model.JailStatus{}, context.Canceled
	}

	cfg, err := m.store.AddPackages(name, resolvedRoots)
	if err != nil {
		return model.JailStatus{}, err
	}

	if rt.state == stateAddingPkgMounted && rt.stage != nil {
		if cfg.NixMode == model.NixModeClosure {
			if err := m.withWritableStage(rt.stage, func(ws writableStage) error {
				return m.exposeClosureRootsLocked(ctx, ws, resolvedRoots)
			}); err != nil {
				return model.JailStatus{}, err
			}
		} else {
			for _, b := range CollectBinPaths(resolvedRoots) {
				if !slices.Contains(rt.stage.binPaths, b) {
					rt.stage.binPaths = append(rt.stage.binPaths, b)
				}
			}
		}
	}

	return m.statusLocked(cfg), nil
}

func (m *Manager) BuildRunSpec(name string, req model.RunRequest) (model.RunSpec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := m.store.Get(name)
	if err != nil {
		return model.RunSpec{}, err
	}
	rt := m.runtimeLocked(name)

	var st *stageState
	switch rt.state {
	case stateStopped:
		st, err = m.ensureStageLocked(cfg, rt)
		if err != nil {
			return model.RunSpec{}, err
		}
	case stateMounted, stateAddingPkgMounted:
		st = rt.stage
	case stateAddingPkgStopped, stateDeleting:
		return model.RunSpec{}, ErrJailBusy
	default:
		return model.RunSpec{}, fmt.Errorf("unexpected jail state %d", rt.state)
	}

	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return model.RunSpec{}, fmt.Errorf("bwrap not found in PATH: %w", err)
	}

	jailRuntime := fmt.Sprintf("/run/user/%d", m.hostUID)
	jailHome := cfg.Home
	if jailHome == "" {
		jailHome = "/home/" + cfg.User
	}

	args := []string{
		"--clearenv",
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
		"--uid", fmt.Sprintf("%d", m.hostUID),
		"--gid", fmt.Sprintf("%d", m.hostGID),
	}
	if !cfg.Network {
		args = append(args, "--unshare-net")
	}
	if cfg.Hostname != "" {
		args = append(args, "--hostname", cfg.Hostname)
	} else {
		args = append(args, "--hostname", cfg.Name)
	}

	args = append(args,
		"--bind", st.stageDir, "/",
		"--proc", "/proc",
		"--dev", "/dev",
	)

	if cfg.GPU {
		gpuDevs := []string{"/dev/dri", "/dev/shm"}
		matches, _ := filepath.Glob("/dev/nvidia*")
		gpuDevs = append(gpuDevs, matches...)
		for _, dev := range gpuDevs {
			if _, err := os.Stat(dev); err == nil {
				args = append(args, "--dev-bind-try", dev, dev)
			}
		}
	}

	jailCwd := jailHome
	if req.Cwd != "" {
		for _, mnt := range cfg.Mounts {
			if req.Cwd == mnt.Path || strings.HasPrefix(req.Cwd, mnt.Path+"/") {
				jailCwd = mnt.Target + strings.TrimPrefix(req.Cwd, mnt.Path)
				break
			}
		}
	}

	pathParts := slices.Clone(st.binPaths)
	pathParts = append(pathParts, "/run/current-system/sw/bin", "/bin", "/usr/bin")
	envMap := map[string]string{
		"USER":            cfg.User,
		"LOGNAME":         cfg.User,
		"HOME":            jailHome,
		"PWD":             jailCwd,
		"SHELL":           "/bin/bash",
		"TMPDIR":          "/tmp",
		"XDG_RUNTIME_DIR": jailRuntime,
		"TERM":            "xterm-256color",
		"LANG":            "C.UTF-8",
		"SSL_CERT_FILE":   "/etc/ssl/certs/ca-bundle.crt",
		"PATH":            strings.Join(pathParts, ":"),
	}
	if term, ok := req.HostEnv["TERM"]; ok && term != "" {
		envMap["TERM"] = term
	}
	if lang, ok := req.HostEnv["LANG"]; ok && lang != "" {
		envMap["LANG"] = lang
	}
	if cfg.GUI {
		if disp, ok := req.HostEnv["DISPLAY"]; ok && disp != "" {
			envMap["DISPLAY"] = disp
		} else if disp := os.Getenv("DISPLAY"); disp != "" {
			envMap["DISPLAY"] = disp
		}
		xauthTarget := filepath.Join(st.stageDir, jailRuntime, "Xauthority")
		if _, err := os.Stat(xauthTarget); err == nil {
			envMap["XAUTHORITY"] = filepath.Join(jailRuntime, "Xauthority")
		}
	}
	if st.dbusSock != "" {
		envMap["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + filepath.Join(jailRuntime, "bus")
	}
	for _, k := range cfg.PassEnv {
		if v, ok := req.HostEnv[k]; ok && v != "" {
			envMap[k] = v
		} else if v := os.Getenv(k); v != "" {
			envMap[k] = v
		}
	}
	for k, v := range cfg.Env {
		envMap[k] = v
	}

	args = append(args, "--chdir", jailCwd)
	envKeys := make([]string, 0, len(envMap))
	for k := range envMap {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		args = append(args, "--setenv", k, envMap[k])
	}

	return model.RunSpec{
		ManagerPID: os.Getpid(),
		BwrapPath:  bwrapPath,
		BwrapArgs:  args,
		DefaultCmd: cfg.Command,
	}, nil
}

func (m *Manager) ensureStageLocked(cfg model.JailConfig, rt *jailRuntime) (*stageState, error) {
	if !validJailName(cfg.Name) {
		return nil, store.ErrInvalidName
	}
	if rt.stage != nil {
		return rt.stage, nil
	}

	stageDir := filepath.Join(m.rootDir, cfg.Name, "stage")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir stage %s: %w", stageDir, err)
	}

	if err := m.sys.mount("tmpfs", stageDir, "tmpfs", 0, "mode=0755"); err != nil {
		return nil, fmt.Errorf("mount stage tmpfs %s: %w", stageDir, err)
	}
	if err := m.sys.mount("", stageDir, "", syscall.MS_SHARED, ""); err != nil {
		_ = m.sys.unmount(stageDir, syscall.MNT_DETACH)
		return nil, fmt.Errorf("make-shared stage %s: %w", stageDir, err)
	}

	st := &stageState{
		stageDir:    stageDir,
		mountState:  stageMountedRW,
		rwDepth:     1,
		mounted:     make(map[string]bool),
		storeMounts: make(map[string]bool),
	}
	rt.stage = st

	if err := m.populateStageLocked(cfg, writableStage{st}); err != nil {
		_ = m.teardownStageLocked(cfg.Name, rt)
		return nil, err
	}

	if err := m.remountBind(stageDir, true); err != nil {
		_ = m.teardownStageLocked(cfg.Name, rt)
		return nil, fmt.Errorf("remount stage ro %s: %w", stageDir, err)
	}
	st.mountState = stageMountedRO
	st.rwDepth = 0

	if rt.state == stateStopped {
		rt.state = stateMounted
	}
	return st, nil
}

func (m *Manager) teardownStageLocked(name string, rt *jailRuntime) error {
	if !validJailName(name) {
		return store.ErrInvalidName
	}
	st := rt.stage
	if st == nil {
		return nil
	}
	if st.dbusProxy != nil && st.dbusProxy.Process != nil {
		_ = st.dbusProxy.Process.Kill()
		_ = st.dbusProxy.Wait()
	}
	if st.dbusSock != "" {
		_ = m.safeRemoveUnder(m.rootDir, st.dbusSock)
	}
	if st.mountState != stageUnmounted {
		if err := m.sys.unmount(st.stageDir, syscall.MNT_DETACH); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("unmount stage %s: %w", st.stageDir, err)
		}
		st.mountState = stageUnmounted
		st.rwDepth = 0
	}
	_ = m.safeRemoveUnder(m.rootDir, st.stageDir)
	_ = m.safeRemoveUnder(m.rootDir, filepath.Join(m.rootDir, name))
	rt.stage = nil
	switch rt.state {
	case stateMounted:
		rt.state = stateStopped
	case stateAddingPkgMounted:
		rt.state = stateAddingPkgStopped
	}
	return nil
}

func (m *Manager) withWritableStage(st *stageState, fn func(ws writableStage) error) error {
	if st == nil {
		return errors.New("stage is not mounted")
	}
	switch st.mountState {
	case stageUnmounted:
		return errors.New("cannot write to unmounted stage")
	case stageMountedRO:
		if err := m.remountBind(st.stageDir, false); err != nil {
			return fmt.Errorf("remount stage rw: %w", err)
		}
		st.mountState = stageMountedRW
		st.rwDepth = 1
	case stageMountedRW:
		st.rwDepth++
	default:
		return fmt.Errorf("unexpected stage mount state %d", st.mountState)
	}

	defer func() {
		st.rwDepth--
		if st.rwDepth == 0 && st.mountState == stageMountedRW {
			_ = m.remountBind(st.stageDir, true)
			st.mountState = stageMountedRO
		}
	}()

	return fn(writableStage{st})
}

func (m *Manager) populateStageLocked(cfg model.JailConfig, ws writableStage) error {
	jailRuntime := fmt.Sprintf("/run/user/%d", m.hostUID)
	baseDirs := []string{
		"/proc",
		"/dev",
		"/tmp",
		"/mnt",
		"/home",
		"/etc",
		"/bin",
		"/usr/bin",
		"/run",
		jailRuntime,
		"/nix/store",
		"/sys",
		cfg.Home,
	}
	for _, d := range baseDirs {
		if err := os.MkdirAll(filepath.Join(ws.stageDir, d), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	if err := m.sys.mount("tmpfs", filepath.Join(ws.stageDir, "tmp"), "tmpfs", 0, "mode=1777"); err != nil {
		return fmt.Errorf("mount /tmp in stage: %w", err)
	}
	if err := m.sys.mount("tmpfs", filepath.Join(ws.stageDir, jailRuntime), "tmpfs", 0, "mode=0700"); err != nil {
		return fmt.Errorf("mount %s in stage: %w", jailRuntime, err)
	}

	if cfg.HomeSrc != "" {
		if err := os.MkdirAll(cfg.HomeSrc, 0o700); err != nil {
			return fmt.Errorf("mkdir home_src %s: %w", cfg.HomeSrc, err)
		}
		if err := m.exposePathLocked(ws, cfg.HomeSrc, cfg.Home, false); err != nil {
			return fmt.Errorf("bind home_src: %w", err)
		}
	} else {
		if err := m.sys.mount("tmpfs", filepath.Join(ws.stageDir, cfg.Home), "tmpfs", 0, "mode=0700"); err != nil {
			return fmt.Errorf("mount home tmpfs: %w", err)
		}
		bashrcPath := filepath.Join(ws.stageDir, cfg.Home, ".bashrc")
		_ = os.WriteFile(bashrcPath, []byte("export PS1='[\\u@\\h:\\w]\\$ '\n"), 0o644)
	}

	passwd := fmt.Sprintf("root:x:0:0:root:/root:/bin/bash\n%s:x:%d:%d::%s:/bin/bash\nnobody:x:65534:65534:Nobody:/:/bin/false\n",
		cfg.User, m.hostUID, m.hostGID, cfg.Home)
	group := fmt.Sprintf("root:x:0:\n%s:x:%d:\nnogroup:x:65534:\n", cfg.User, m.hostGID)
	if err := os.WriteFile(filepath.Join(ws.stageDir, "etc/passwd"), []byte(passwd), 0o444); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ws.stageDir, "etc/group"), []byte(group), 0o444); err != nil {
		return err
	}

	shPath := resolveHostBin("bash", "sh")
	if shPath != "" {
		_ = os.Symlink(shPath, filepath.Join(ws.stageDir, "bin/sh"))
		_ = os.Symlink(shPath, filepath.Join(ws.stageDir, "bin/bash"))
	}
	envPath := resolveHostBin("env")
	if envPath != "" {
		_ = os.Symlink(envPath, filepath.Join(ws.stageDir, "usr/bin/env"))
	}

	for _, p := range []string{"/etc/os-release", "/etc/localtime", "/etc/nix"} {
		m.exposeIfExistsLocked(ws, p, p, true)
	}

	if m.hostHome != "" {
		m.exposeIfExistsLocked(ws, filepath.Join(m.hostHome, ".gitconfig"), filepath.Join(cfg.Home, ".gitconfig"), true)
		m.exposeIfExistsLocked(ws, filepath.Join(m.hostHome, ".config/git"), filepath.Join(cfg.Home, ".config/git"), true)
	}

	if cfg.Network {
		for _, p := range []string{
			"/etc/resolv.conf",
			"/etc/hosts",
			"/etc/nsswitch.conf",
			"/etc/ssl",
			"/etc/pki",
			"/run/systemd/resolve",
		} {
			m.exposeIfExistsLocked(ws, p, p, true)
		}
	}

	if cfg.GUI {
		m.exposeIfExistsLocked(ws, "/tmp/.X11-unix", "/tmp/.X11-unix", false)
		m.exposeIfExistsLocked(ws, "/etc/fonts", "/etc/fonts", true)
		m.exposeIfExistsLocked(ws, "/etc/machine-id", "/etc/machine-id", true)
		xauth := os.Getenv("XAUTHORITY")
		if xauth == "" && m.hostHome != "" {
			xauth = filepath.Join(m.hostHome, ".Xauthority")
		}
		if xauth != "" {
			m.exposeIfExistsLocked(ws, xauth, filepath.Join(jailRuntime, "Xauthority"), true)
		}
	}

	if cfg.GPU {
		m.exposeIfExistsLocked(ws, "/sys", "/sys", true)
		m.exposeIfExistsLocked(ws, "/run/opengl-driver", "/run/opengl-driver", true)
		m.exposeIfExistsLocked(ws, "/run/opengl-driver-32", "/run/opengl-driver-32", true)
	}

	if len(cfg.DBusTalk) > 0 {
		if err := m.setupDBusProxyLocked(cfg, ws, jailRuntime); err != nil {
			return err
		}
	}

	if cfg.NixMode == model.NixModeFull {
		m.exposeIfExistsLocked(ws, "/nix/store", "/nix/store", true)
		m.exposeIfExistsLocked(ws, "/nix/var", "/nix/var", true)
		m.exposeIfExistsLocked(ws, "/run/current-system", "/run/current-system", true)
		for _, spec := range cfg.Packages {
			if roots, err := ResolveInstallableOrPath(spec); err == nil {
				for _, b := range CollectBinPaths(roots) {
					if !slices.Contains(ws.binPaths, b) {
						ws.binPaths = append(ws.binPaths, b)
					}
				}
			}
		}
	} else {
		var roots []string
		for _, bin := range []string{shPath, envPath, resolveHostBin("ls"), resolveHostBin("cat")} {
			if bin != "" {
				if r := StoreRootFromPath(bin); r != "" {
					roots = append(roots, r)
				}
			}
		}
		for _, spec := range cfg.Packages {
			if r, err := ResolveInstallableOrPath(spec); err == nil {
				roots = append(roots, r...)
			}
		}
		if len(cfg.Command) > 0 {
			if r := StoreRootFromPath(cfg.Command[0]); r != "" {
				roots = append(roots, r)
			}
		}
		if cfg.GPU {
			for _, p := range []string{"/run/opengl-driver", "/run/opengl-driver-32"} {
				if r := StoreRootFromPath(p); r != "" {
					roots = append(roots, r)
				}
			}
			matches, _ := filepath.Glob("/run/opengl-driver/lib/*.so*")
			for _, mPath := range matches {
				if r := StoreRootFromPath(mPath); r != "" {
					roots = append(roots, r)
				}
			}
		}
		if err := m.exposeClosureRootsLocked(context.Background(), ws, roots); err != nil {
			return err
		}
	}

	for _, mnt := range cfg.Mounts {
		if _, err := os.Stat(mnt.Path); err != nil {
			continue
		}
		if err := m.exposePathLocked(ws, mnt.Path, mnt.Target, !mnt.ReadWrite); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) exposeClosureRootsLocked(ctx context.Context, ws writableStage, roots []string) error {
	for _, b := range CollectBinPaths(roots) {
		if !slices.Contains(ws.binPaths, b) {
			ws.binPaths = append(ws.binPaths, b)
		}
	}
	closure, err := QueryClosure(ctx, roots)
	if err != nil {
		return err
	}
	for _, p := range closure {
		if ws.storeMounts[p] {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := m.exposePathLocked(ws, p, p, true); err != nil {
			return fmt.Errorf("expose store path %s: %w", p, err)
		}
		ws.storeMounts[p] = true
	}
	return nil
}

func (m *Manager) setupDBusProxyLocked(cfg model.JailConfig, ws writableStage, jailRuntime string) error {
	busAddr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if busAddr == "" {
		busPath := filepath.Join(jailRuntime, "bus")
		if _, err := os.Stat(busPath); err == nil {
			busAddr = "unix:path=" + busPath
		}
	}
	if busAddr == "" {
		return nil
	}
	proxyBin, err := exec.LookPath("xdg-dbus-proxy")
	if err != nil {
		return nil
	}
	sockPath := filepath.Join(m.rootDir, cfg.Name, "dbus-proxy.sock")
	_ = m.safeRemoveUnder(m.rootDir, sockPath)

	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		return err
	}
	defer rPipe.Close()

	proxyArgs := []string{busAddr, sockPath, "--filter", "--fd=3"}
	for _, name := range cfg.DBusTalk {
		proxyArgs = append(proxyArgs, "--talk="+name)
	}
	cmd := exec.Command(proxyBin, proxyArgs...)
	cmd.ExtraFiles = []*os.File{wPipe}
	if err := cmd.Start(); err != nil {
		wPipe.Close()
		return fmt.Errorf("start xdg-dbus-proxy: %w", err)
	}
	wPipe.Close()

	buf := make([]byte, 1)
	_, _ = rPipe.Read(buf)

	ws.dbusProxy = cmd
	ws.dbusSock = sockPath
	return m.exposePathLocked(ws, sockPath, filepath.Join(jailRuntime, "bus"), false)
}

func (m *Manager) exposeIfExistsLocked(ws writableStage, srcPath, dstPath string, ro bool) {
	resolved, err := filepath.EvalSymlinks(srcPath)
	if err != nil {
		return
	}
	if _, err := os.Stat(resolved); err != nil {
		return
	}
	_ = m.exposePathLocked(ws, resolved, dstPath, ro)
}

func (m *Manager) exposePathLocked(ws writableStage, srcPath, dstPath string, ro bool) error {
	if ws.mountState != stageMountedRW {
		return errors.New("stage must be writable to expose paths")
	}
	cleanDst := filepath.Clean(dstPath)
	if !filepath.IsAbs(cleanDst) || cleanDst == "/" {
		return fmt.Errorf("invalid target path %q", dstPath)
	}
	stageTarget := filepath.Join(ws.stageDir, cleanDst)
	if err := checkStrictSubpath(ws.stageDir, stageTarget); err != nil {
		return err
	}

	resolvedSrc, err := filepath.EvalSymlinks(srcPath)
	if err != nil {
		resolvedSrc = srcPath
	}
	info, err := os.Stat(resolvedSrc)
	if err != nil {
		return fmt.Errorf("stat %s: %w", resolvedSrc, err)
	}

	workParent, err := os.MkdirTemp(m.workDir, "exp-*")
	if err != nil {
		return fmt.Errorf("mktemp in workDir: %w", err)
	}
	workTarget := workParent
	mountedWork := false
	defer func() {
		if mountedWork {
			_ = m.sys.unmount(workTarget, syscall.MNT_DETACH)
		}
		if workTarget != workParent {
			_ = m.safeRemoveUnder(m.workDir, workTarget)
		}
		_ = m.safeRemoveUnder(m.workDir, workParent)
	}()

	if !info.IsDir() {
		workTarget = filepath.Join(workParent, "file")
		if err := os.WriteFile(workTarget, nil, 0o600); err != nil {
			return fmt.Errorf("create work file stub: %w", err)
		}
	}

	if err := m.sys.mount(resolvedSrc, workTarget, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rbind %s to %s: %w", resolvedSrc, workTarget, err)
	}
	mountedWork = true

	if ro {
		if err := m.remountTreeReadOnly(workTarget); err != nil {
			return fmt.Errorf("remount ro %s: %w", workTarget, err)
		}
	}

	if ws.mounted[cleanDst] {
		_ = m.sys.unmount(stageTarget, syscall.MNT_DETACH)
	}

	if info.IsDir() {
		if err := os.MkdirAll(stageTarget, 0o755); err != nil {
			return fmt.Errorf("mkdir target %s: %w", stageTarget, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(stageTarget), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", filepath.Dir(stageTarget), err)
		}
		if _, err := os.Lstat(stageTarget); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(stageTarget, nil, 0o644); err != nil {
				return fmt.Errorf("touch target %s: %w", stageTarget, err)
			}
		}
	}

	if err := m.sys.mount(workTarget, stageTarget, "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("mount --move %s -> %s: %w", workTarget, stageTarget, err)
	}
	mountedWork = false

	ws.mounted[cleanDst] = true
	return nil
}

func (m *Manager) hidePathLocked(ws writableStage, dstPath string) error {
	if ws.mountState != stageMountedRW {
		return errors.New("stage must be writable to hide paths")
	}
	cleanDst := filepath.Clean(dstPath)
	if !filepath.IsAbs(cleanDst) || cleanDst == "/" {
		return fmt.Errorf("invalid target path %q", dstPath)
	}
	if !ws.mounted[cleanDst] {
		return nil
	}
	stageTarget := filepath.Join(ws.stageDir, cleanDst)
	if err := checkStrictSubpath(ws.stageDir, stageTarget); err != nil {
		return err
	}
	if err := m.sys.unmount(stageTarget, syscall.MNT_DETACH); err != nil && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unmount %s: %w", stageTarget, err)
	}
	delete(ws.mounted, cleanDst)
	if !hasMountedAncestor(ws.mounted, cleanDst) {
		_ = m.safeRemoveUnder(ws.stageDir, stageTarget)
	}
	return nil
}

const preservedMountFlags = uintptr(
	syscall.MS_NOSUID |
		syscall.MS_NODEV |
		syscall.MS_NOEXEC |
		syscall.MS_NOATIME |
		syscall.MS_NODIRATIME |
		syscall.MS_RELATIME |
		syscall.MS_SYNCHRONOUS,
)

func (m *Manager) remountBind(mp string, readOnly bool) error {
	var st syscall.Statfs_t
	if err := m.sys.statfs(mp, &st); err != nil {
		return err
	}
	flags := (uintptr(st.Flags) & preservedMountFlags) | syscall.MS_BIND | syscall.MS_REMOUNT
	if readOnly {
		flags |= syscall.MS_RDONLY
	}
	return m.sys.mount("", mp, "", flags, "")
}

func (m *Manager) remountTreeReadOnly(root string) error {
	mounts, err := submountsUnder(root)
	if err != nil || len(mounts) == 0 {
		return m.remountBind(root, true)
	}
	for _, mp := range mounts {
		if err := m.remountBind(mp, true); err != nil {
			return err
		}
	}
	return nil
}

func submountsUnder(root string) ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cleanRoot := filepath.Clean(root)
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountPath(fields[4])
		if mp == cleanRoot || strings.HasPrefix(mp, cleanRoot+"/") {
			out = append(out, mp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i]) < len(out[j])
	})
	return out, sc.Err()
}

func unescapeMountPath(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

func resolveHostBin(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			if resolved, err := filepath.EvalSymlinks(p); err == nil {
				return resolved
			}
			return p
		}
	}
	return ""
}
