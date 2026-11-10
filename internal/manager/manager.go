package manager

import (
	"bufio"
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

type Options struct {
	Store    store.Store
	RootDir  string
	WorkDir  string
	HostUID  int
	HostGID  int
	HostUser string
	HostHome string
}

type stageState struct {
	stageDir    string
	mounted     map[string]bool
	storeMounts map[string]bool
	binPaths    []string
	dbusProxy   *exec.Cmd
	dbusSock    string
}

type Manager struct {
	mu          sync.Mutex
	store       store.Store
	rootDir     string
	workDir     string
	hostUID     int
	hostGID     int
	hostUser    string
	hostHome    string
	stages      map[string]*stageState
	pkgBusy     map[string]int
	pkgProgress map[string]string

	subMu sync.Mutex
	subs  map[chan model.Event]struct{}
}

func New(opts Options) (*Manager, error) {
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
		store:       opts.Store,
		rootDir:     opts.RootDir,
		workDir:     opts.WorkDir,
		hostUID:     opts.HostUID,
		hostGID:     opts.HostGID,
		hostUser:    opts.HostUser,
		hostHome:    opts.HostHome,
		stages:      make(map[string]*stageState),
		pkgBusy:     make(map[string]int),
		pkgProgress: make(map[string]string),
		subs:        make(map[chan model.Event]struct{}),
	}
	if err := m.initWorkspace(); err != nil {
		return nil, err
	}
	return m, nil
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
	st, active := m.stages[cfg.Name]
	storeCount := 0
	if active {
		storeCount = len(st.storeMounts)
	}
	return model.JailStatus{
		Config:      cfg,
		Active:      active,
		StagePath:   filepath.Join(m.rootDir, cfg.Name, "stage"),
		StorePaths:  storeCount,
		AddingPkg:   m.pkgBusy[cfg.Name] > 0,
		PkgProgress: m.pkgProgress[cfg.Name],
	}
}

func (m *Manager) initWorkspace() error {
	if err := os.MkdirAll(m.rootDir, 0o700); err != nil {
		return fmt.Errorf("mkdir rootDir %s: %w", m.rootDir, err)
	}
	if err := os.MkdirAll(m.workDir, 0o700); err != nil {
		return fmt.Errorf("mkdir workDir %s: %w", m.workDir, err)
	}
	if err := syscall.Mount("tmpfs", m.workDir, "tmpfs", 0, "mode=0700"); err != nil {
		return fmt.Errorf("mount tmpfs on workDir %s: %w", m.workDir, err)
	}
	if err := syscall.Mount("", m.workDir, "", syscall.MS_PRIVATE, ""); err != nil {
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

func (m *Manager) CreateOrUpdateJail(cfg model.JailConfig) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	saved, err := m.store.Put(cfg)
	if err != nil {
		return model.JailStatus{}, err
	}
	return m.statusLocked(saved), nil
}

func (m *Manager) EnsureStage(name string) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := m.store.Get(name)
	if err != nil {
		return model.JailStatus{}, err
	}
	if _, err := m.ensureStageLocked(cfg); err != nil {
		return model.JailStatus{}, err
	}
	return m.statusLocked(cfg), nil
}

func (m *Manager) DeleteJail(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if st, ok := m.stages[name]; ok {
		if err := m.teardownStageLocked(name, st); err != nil {
			return err
		}
	}
	return m.store.Delete(name)
}

func (m *Manager) StopStage(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.store.Get(name); err != nil {
		return err
	}
	if st, ok := m.stages[name]; ok {
		return m.teardownStageLocked(name, st)
	}
	return nil
}

func (m *Manager) teardownStageLocked(name string, st *stageState) error {
	if st.dbusProxy != nil && st.dbusProxy.Process != nil {
		_ = st.dbusProxy.Process.Kill()
		_ = st.dbusProxy.Wait()
	}
	if st.dbusSock != "" {
		_ = os.Remove(st.dbusSock)
	}
	if err := syscall.Unmount(st.stageDir, syscall.MNT_DETACH); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unmount stage %s: %w", st.stageDir, err)
	}
	_ = os.Remove(st.stageDir)
	_ = os.Remove(filepath.Join(m.rootDir, name))
	delete(m.stages, name)
	return nil
}

func (m *Manager) Expose(name, rawPath string, ro bool) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleanPath := filepath.Clean(rawPath)
	if !filepath.IsAbs(cleanPath) {
		return model.JailStatus{}, fmt.Errorf("path must be absolute: %q", rawPath)
	}
	if _, err := os.Stat(cleanPath); err != nil {
		return model.JailStatus{}, fmt.Errorf("source path %q: %w", cleanPath, err)
	}

	cfg, err := m.store.SetMount(name, model.MountEntry{Path: cleanPath, ReadOnly: ro})
	if err != nil {
		return model.JailStatus{}, err
	}

	st, active := m.stages[name]
	storeCount := 0
	if active {
		if err := m.withWritableStage(st, func() error {
			return m.exposePathLocked(st, cleanPath, cleanPath, ro)
		}); err != nil {
			return model.JailStatus{}, err
		}
		storeCount = len(st.storeMounts)
	}
	return model.JailStatus{
		Config:     cfg,
		Active:     active,
		StagePath:  filepath.Join(m.rootDir, name, "stage"),
		StorePaths: storeCount,
	}, nil
}

func (m *Manager) Hide(name, rawPath string) (model.JailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleanPath := filepath.Clean(rawPath)
	if !filepath.IsAbs(cleanPath) {
		return model.JailStatus{}, fmt.Errorf("path must be absolute: %q", rawPath)
	}

	cfg, err := m.store.RemoveMount(name, cleanPath)
	if err != nil {
		return model.JailStatus{}, err
	}

	st, active := m.stages[name]
	storeCount := 0
	if active {
		if err := m.withWritableStage(st, func() error {
			return m.hidePathLocked(st, cleanPath)
		}); err != nil {
			return model.JailStatus{}, err
		}
		storeCount = len(st.storeMounts)
	}
	return model.JailStatus{
		Config:     cfg,
		Active:     active,
		StagePath:  filepath.Join(m.rootDir, name, "stage"),
		StorePaths: storeCount,
	}, nil
}

func (m *Manager) AddPackagesAsync(name string, specs []string) error {
	m.mu.Lock()
	if _, err := m.store.Get(name); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	go func() {
		if _, err := m.AddPackages(name, specs); err != nil {
			m.broadcast(model.Event{
				Type:  "pkg_error",
				Jail:  name,
				Error: err.Error(),
			})
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
	if _, err := m.store.Get(name); err != nil {
		m.mu.Unlock()
		return model.JailStatus{}, err
	}
	m.pkgBusy[name]++
	m.pkgProgress[name] = "resolving package closure..."
	m.mu.Unlock()

	m.broadcast(model.Event{
		Type:    "pkg_progress",
		Jail:    name,
		Message: "resolving package closure...",
	})

	defer func() {
		m.mu.Lock()
		m.pkgBusy[name]--
		if m.pkgBusy[name] <= 0 {
			delete(m.pkgBusy, name)
			delete(m.pkgProgress, name)
		}
		m.mu.Unlock()
	}()

	onProgress := func(msg string) {
		m.mu.Lock()
		m.pkgProgress[name] = msg
		m.mu.Unlock()
		m.broadcast(model.Event{
			Type:    "pkg_progress",
			Jail:    name,
			Message: msg,
		})
	}

	var resolvedRoots []string
	for _, spec := range specs {
		roots, err := ResolveInstallableOrPathWithProgress(spec, onProgress)
		if err != nil {
			return model.JailStatus{}, err
		}
		resolvedRoots = append(resolvedRoots, roots...)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := m.store.AddPackages(name, resolvedRoots)
	if err != nil {
		return model.JailStatus{}, err
	}

	if st, active := m.stages[name]; active && cfg.NixMode == model.NixModeClosure {
		if err := m.withWritableStage(st, func() error {
			return m.exposeClosureRootsLocked(st, resolvedRoots)
		}); err != nil {
			return model.JailStatus{}, err
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
	st, err := m.ensureStageLocked(cfg)
	if err != nil {
		return model.RunSpec{}, err
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
		"--new-session",
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
				jailCwd = req.Cwd
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
		"SHELL":           "/bin/sh",
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

func (m *Manager) ensureStageLocked(cfg model.JailConfig) (*stageState, error) {
	if st, ok := m.stages[cfg.Name]; ok {
		return st, nil
	}

	stageDir := filepath.Join(m.rootDir, cfg.Name, "stage")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir stage %s: %w", stageDir, err)
	}

	if err := syscall.Mount("tmpfs", stageDir, "tmpfs", 0, "mode=0755"); err != nil {
		return nil, fmt.Errorf("mount stage tmpfs %s: %w", stageDir, err)
	}
	if err := syscall.Mount("", stageDir, "", syscall.MS_SHARED, ""); err != nil {
		_ = syscall.Unmount(stageDir, syscall.MNT_DETACH)
		return nil, fmt.Errorf("make-shared stage %s: %w", stageDir, err)
	}

	st := &stageState{
		stageDir:    stageDir,
		mounted:     make(map[string]bool),
		storeMounts: make(map[string]bool),
	}

	if err := m.populateStageLocked(cfg, st); err != nil {
		_ = m.teardownStageLocked(cfg.Name, st)
		return nil, err
	}

	if err := remountBind(stageDir, true); err != nil {
		_ = m.teardownStageLocked(cfg.Name, st)
		return nil, fmt.Errorf("remount stage ro %s: %w", stageDir, err)
	}

	m.stages[cfg.Name] = st
	return st, nil
}

func (m *Manager) populateStageLocked(cfg model.JailConfig, st *stageState) error {
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
		if err := os.MkdirAll(filepath.Join(st.stageDir, d), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	if err := syscall.Mount("tmpfs", filepath.Join(st.stageDir, "tmp"), "tmpfs", 0, "mode=1777"); err != nil {
		return fmt.Errorf("mount /tmp in stage: %w", err)
	}
	if err := syscall.Mount("tmpfs", filepath.Join(st.stageDir, jailRuntime), "tmpfs", 0, "mode=0700"); err != nil {
		return fmt.Errorf("mount %s in stage: %w", jailRuntime, err)
	}

	if cfg.HomeSrc != "" {
		if err := os.MkdirAll(cfg.HomeSrc, 0o700); err != nil {
			return fmt.Errorf("mkdir home_src %s: %w", cfg.HomeSrc, err)
		}
		if err := m.exposePathLocked(st, cfg.HomeSrc, cfg.Home, false); err != nil {
			return fmt.Errorf("bind home_src: %w", err)
		}
	} else {
		if err := syscall.Mount("tmpfs", filepath.Join(st.stageDir, cfg.Home), "tmpfs", 0, "mode=0700"); err != nil {
			return fmt.Errorf("mount home tmpfs: %w", err)
		}
	}

	passwd := fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\n%s:x:%d:%d::%s:/bin/sh\nnobody:x:65534:65534:Nobody:/:/bin/false\n",
		cfg.User, m.hostUID, m.hostGID, cfg.Home)
	group := fmt.Sprintf("root:x:0:\n%s:x:%d:\nnogroup:x:65534:\n", cfg.User, m.hostGID)
	if err := os.WriteFile(filepath.Join(st.stageDir, "etc/passwd"), []byte(passwd), 0o444); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(st.stageDir, "etc/group"), []byte(group), 0o444); err != nil {
		return err
	}

	shPath := resolveHostBin("bash", "sh")
	if shPath != "" {
		_ = os.Symlink(shPath, filepath.Join(st.stageDir, "bin/sh"))
	}
	envPath := resolveHostBin("env")
	if envPath != "" {
		_ = os.Symlink(envPath, filepath.Join(st.stageDir, "usr/bin/env"))
	}

	for _, p := range []string{"/etc/os-release", "/etc/localtime", "/etc/nix"} {
		m.exposeIfExistsLocked(st, p, p, true)
	}

	if m.hostHome != "" {
		m.exposeIfExistsLocked(st, filepath.Join(m.hostHome, ".gitconfig"), filepath.Join(cfg.Home, ".gitconfig"), true)
		m.exposeIfExistsLocked(st, filepath.Join(m.hostHome, ".config/git"), filepath.Join(cfg.Home, ".config/git"), true)
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
			m.exposeIfExistsLocked(st, p, p, true)
		}
	}

	if cfg.GUI {
		m.exposeIfExistsLocked(st, "/tmp/.X11-unix", "/tmp/.X11-unix", false)
		m.exposeIfExistsLocked(st, "/etc/fonts", "/etc/fonts", true)
		m.exposeIfExistsLocked(st, "/etc/machine-id", "/etc/machine-id", true)
		xauth := os.Getenv("XAUTHORITY")
		if xauth == "" && m.hostHome != "" {
			xauth = filepath.Join(m.hostHome, ".Xauthority")
		}
		if xauth != "" {
			m.exposeIfExistsLocked(st, xauth, filepath.Join(jailRuntime, "Xauthority"), true)
		}
	}

	if cfg.GPU {
		m.exposeIfExistsLocked(st, "/sys", "/sys", true)
		m.exposeIfExistsLocked(st, "/run/opengl-driver", "/run/opengl-driver", true)
		m.exposeIfExistsLocked(st, "/run/opengl-driver-32", "/run/opengl-driver-32", true)
	}

	if len(cfg.DBusTalk) > 0 {
		if err := m.setupDBusProxyLocked(cfg, st, jailRuntime); err != nil {
			return err
		}
	}

	if cfg.NixMode == model.NixModeFull {
		m.exposeIfExistsLocked(st, "/nix/store", "/nix/store", true)
		m.exposeIfExistsLocked(st, "/nix/var", "/nix/var", true)
		m.exposeIfExistsLocked(st, "/run/current-system", "/run/current-system", true)
		for _, spec := range cfg.Packages {
			if roots, err := ResolveInstallableOrPath(spec); err == nil {
				for _, b := range CollectBinPaths(roots) {
					if !slices.Contains(st.binPaths, b) {
						st.binPaths = append(st.binPaths, b)
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
		if err := m.exposeClosureRootsLocked(st, roots); err != nil {
			return err
		}
	}

	for _, mnt := range cfg.Mounts {
		if _, err := os.Stat(mnt.Path); err != nil {
			continue
		}
		if err := m.exposePathLocked(st, mnt.Path, mnt.Path, mnt.ReadOnly); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) exposeClosureRootsLocked(st *stageState, roots []string) error {
	for _, b := range CollectBinPaths(roots) {
		if !slices.Contains(st.binPaths, b) {
			st.binPaths = append(st.binPaths, b)
		}
	}
	closure, err := QueryClosure(roots)
	if err != nil {
		return err
	}
	for _, p := range closure {
		if st.storeMounts[p] {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := m.exposePathLocked(st, p, p, true); err != nil {
			return fmt.Errorf("expose store path %s: %w", p, err)
		}
		st.storeMounts[p] = true
	}
	return nil
}

func (m *Manager) setupDBusProxyLocked(cfg model.JailConfig, st *stageState, jailRuntime string) error {
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
	_ = os.Remove(sockPath)

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

	st.dbusProxy = cmd
	st.dbusSock = sockPath
	return m.exposePathLocked(st, sockPath, filepath.Join(jailRuntime, "bus"), false)
}

func (m *Manager) withWritableStage(st *stageState, fn func() error) error {
	if err := remountBind(st.stageDir, false); err != nil {
		return fmt.Errorf("remount stage rw: %w", err)
	}
	defer func() {
		_ = remountBind(st.stageDir, true)
	}()
	return fn()
}

func (m *Manager) exposeIfExistsLocked(st *stageState, srcPath, dstPath string, ro bool) {
	resolved, err := filepath.EvalSymlinks(srcPath)
	if err != nil {
		return
	}
	if _, err := os.Stat(resolved); err != nil {
		return
	}
	_ = m.exposePathLocked(st, resolved, dstPath, ro)
}

func (m *Manager) exposePathLocked(st *stageState, srcPath, dstPath string, ro bool) error {
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
	defer func() {
		if workTarget != workParent {
			_ = os.Remove(workTarget)
		}
		_ = os.Remove(workParent)
	}()

	if !info.IsDir() {
		workTarget = filepath.Join(workParent, "file")
		if err := os.WriteFile(workTarget, nil, 0o600); err != nil {
			return fmt.Errorf("create work file stub: %w", err)
		}
	}

	if err := syscall.Mount(resolvedSrc, workTarget, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rbind %s to %s: %w", resolvedSrc, workTarget, err)
	}

	if ro {
		if err := remountTreeReadOnly(workTarget); err != nil {
			_ = syscall.Unmount(workTarget, syscall.MNT_DETACH)
			return fmt.Errorf("remount ro %s: %w", workTarget, err)
		}
	}

	stageTarget := filepath.Join(st.stageDir, dstPath)
	if st.mounted[dstPath] {
		_ = syscall.Unmount(stageTarget, syscall.MNT_DETACH)
	}

	if info.IsDir() {
		if err := os.MkdirAll(stageTarget, 0o755); err != nil {
			_ = syscall.Unmount(workTarget, syscall.MNT_DETACH)
			return fmt.Errorf("mkdir target %s: %w", stageTarget, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(stageTarget), 0o755); err != nil {
			_ = syscall.Unmount(workTarget, syscall.MNT_DETACH)
			return fmt.Errorf("mkdir parent %s: %w", filepath.Dir(stageTarget), err)
		}
		if _, err := os.Lstat(stageTarget); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(stageTarget, nil, 0o644); err != nil {
				_ = syscall.Unmount(workTarget, syscall.MNT_DETACH)
				return fmt.Errorf("touch target %s: %w", stageTarget, err)
			}
		}
	}

	if err := syscall.Mount(workTarget, stageTarget, "", syscall.MS_MOVE, ""); err != nil {
		_ = syscall.Unmount(workTarget, syscall.MNT_DETACH)
		return fmt.Errorf("mount --move %s -> %s: %w", workTarget, stageTarget, err)
	}

	st.mounted[dstPath] = true
	return nil
}

func (m *Manager) hidePathLocked(st *stageState, dstPath string) error {
	stageTarget := filepath.Join(st.stageDir, dstPath)
	if err := syscall.Unmount(stageTarget, syscall.MNT_DETACH); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unmount %s: %w", stageTarget, err)
	}
	_ = os.Remove(stageTarget)
	delete(st.mounted, dstPath)
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

func remountBind(mp string, readOnly bool) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mp, &st); err != nil {
		return err
	}
	flags := (uintptr(st.Flags) & preservedMountFlags) | syscall.MS_BIND | syscall.MS_REMOUNT
	if readOnly {
		flags |= syscall.MS_RDONLY
	}
	return syscall.Mount("", mp, "", flags, "")
}

func remountTreeReadOnly(root string) error {
	mounts, err := submountsUnder(root)
	if err != nil || len(mounts) == 0 {
		return remountBind(root, true)
	}
	for _, mp := range mounts {
		if err := remountBind(mp, true); err != nil {
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
