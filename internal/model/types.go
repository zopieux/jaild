package model

type NixMode string

const (
	NixModeClosure NixMode = "closure"
	NixModeFull    NixMode = "full"
)

type MountEntry struct {
	Path      string `json:"path" toml:"path"`
	Target    string `json:"target,omitempty" toml:"target,omitempty"`
	ReadWrite bool   `json:"rw" toml:"rw"`
}

type JailConfig struct {
	Name     string            `json:"name" toml:"name"`
	Hostname string            `json:"hostname,omitempty" toml:"hostname,omitempty"`
	User     string            `json:"user,omitempty" toml:"user,omitempty"`
	Home     string            `json:"home,omitempty" toml:"home,omitempty"`
	HomeSrc  string            `json:"home_src,omitempty" toml:"home_src,omitempty"`
	Network  bool              `json:"network" toml:"network"`
	GPU      bool              `json:"gpu" toml:"gpu"`
	GUI      bool              `json:"gui" toml:"gui"`
	DBusTalk []string          `json:"dbus_talk,omitempty" toml:"dbus_talk,omitempty"`
	NixMode  NixMode           `json:"nix_mode" toml:"nix_mode"`
	Packages []string          `json:"packages,omitempty" toml:"packages,omitempty"`
	Mounts   []MountEntry      `json:"mounts,omitempty" toml:"mounts,omitempty"`
	Env      map[string]string `json:"env,omitempty" toml:"env,omitempty"`
	PassEnv  []string          `json:"pass_env,omitempty" toml:"pass_env,omitempty"`
	Command  []string          `json:"command,omitempty" toml:"command,omitempty"`
}

func NormalizeConfig(cfg JailConfig) JailConfig {
	if cfg.User == "" {
		cfg.User = cfg.Name
	}
	if cfg.Home == "" {
		cfg.Home = "/home/" + cfg.User
	}
	if cfg.NixMode == "" {
		cfg.NixMode = NixModeClosure
	}
	for i := range cfg.Mounts {
		if cfg.Mounts[i].Target == "" {
			cfg.Mounts[i].Target = cfg.Mounts[i].Path
		}
	}
	return cfg
}

type JailStatus struct {
	Config      JailConfig `json:"config"`
	Active      bool       `json:"active"`
	StagePath   string     `json:"stage_path"`
	StorePaths  int        `json:"store_paths"`
	AddingPkg   bool       `json:"adding_pkg,omitempty"`
	PkgProgress string     `json:"pkg_progress,omitempty"`
}

type Event struct {
	Type    string `json:"type"`
	Jail    string `json:"jail,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

type RunRequest struct {
	Cwd     string            `json:"cwd,omitempty"`
	HostEnv map[string]string `json:"host_env,omitempty"`
}

type RunSpec struct {
	ManagerPID int      `json:"manager_pid"`
	BwrapPath  string   `json:"bwrap_path"`
	BwrapArgs  []string `json:"bwrap_args"`
	PasswdData string   `json:"passwd_data"`
	GroupData  string   `json:"group_data"`
	DefaultCmd []string `json:"default_cmd,omitempty"`
}
