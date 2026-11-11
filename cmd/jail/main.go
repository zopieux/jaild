package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"jaild/internal/client"
	"jaild/internal/model"
)

type sliceFlag []string

func (s *sliceFlag) String() string { return strings.Join(*s, ",") }
func (s *sliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var sockPath string
	args := os.Args[1:]
	for len(args) >= 2 && (args[0] == "--socket" || args[0] == "-s") {
		sockPath = args[1]
		args = args[2:]
	}
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	cli := client.New(sockPath)
	cmd := args[0]
	rest := args[1:]

	var err error
	switch cmd {
	case "new", "create":
		err = cmdNew(cli, rest)
	case "run", "exec":
		err = cmdRun(cli, rest)
	case "expose":
		err = cmdExpose(cli, rest)
	case "hide":
		err = cmdHide(cli, rest)
	case "pkg":
		err = cmdPkg(cli, rest)
	case "ls", "list":
		err = cmdList(cli)
	case "status", "inspect":
		err = cmdStatus(cli, rest)
	case "stop":
		err = cmdStop(cli, rest)
	case "del", "rm", "delete":
		err = cmdDelete(cli, rest)
	case "web", "ui":
		err = cmdWeb(cli, rest)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "jail: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: jail [--socket PATH] <command> [args...]

Commands:
  new <name> [flags] [-- cmd...]   Create or update a jail and mount its stage
  run <name> [cmd...]              Run a command inside a jail
  expose <name> <path> [--rw|--ro] Dynamically expose a host path into a jail (default: ro)
  hide <name> <path>               Dynamically unmount/hide an exposed path
  pkg <name> <pkg...>              Add Nix packages/closures into a jail
  ls                               List jails and their stage status
  status <name>                    Show detailed JSON status for a jail
  stop <name>                      Unmount a jail's stage without deleting config
  del <name>                       Unmount stage and delete jail configuration
  web [--listen 127.0.0.1:7480]    Serve the embedded web UI over HTTP
`)
}

func cmdNew(cli *client.Client, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: jail new <name> [--user USER] [--network] [--gpu] [--gui] [--nix-mode closure|full] [--home DIR] [--pkg PKG...]")
	}
	name := args[0]
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	var (
		network    bool
		gpu        bool
		gui        bool
		nixMode    string
		homeSrc    string
		hostname   string
		user       string
		mountStage bool
		pkgs       sliceFlag
		dbusTalk   sliceFlag
		passEnv    sliceFlag
	)
	fs.BoolVar(&network, "network", false, "Enable host network access")
	fs.BoolVar(&gpu, "gpu", false, "Enable GPU/CUDA access")
	fs.BoolVar(&gui, "gui", false, "Enable X11 GUI access")
	fs.StringVar(&nixMode, "nix-mode", string(model.NixModeClosure), "Nix store mode (closure or full)")
	fs.StringVar(&homeSrc, "home", "", "Host directory bound as jail home (empty = ephemeral tmpfs)")
	fs.StringVar(&hostname, "hostname", "", "Hostname inside jail")
	fs.StringVar(&user, "user", "", "Username inside jail (default: jail name)")
	fs.BoolVar(&mountStage, "stage", true, "Mount the jail stage immediately")
	fs.Var(&pkgs, "pkg", "Nix store path or installable to include in closure (repeatable)")
	fs.Var(&dbusTalk, "dbus-talk", "D-Bus service name to allow via xdg-dbus-proxy (repeatable)")
	fs.Var(&passEnv, "pass-env", "Host environment variable to forward (repeatable)")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if homeSrc != "" {
		abs, err := filepath.Abs(homeSrc)
		if err != nil {
			return err
		}
		homeSrc = abs
	}

	cfg := model.JailConfig{
		Name:     name,
		Hostname: hostname,
		User:     user,
		HomeSrc:  homeSrc,
		Network:  network,
		GPU:      gpu,
		GUI:      gui,
		NixMode:  model.NixMode(nixMode),
		Packages: pkgs,
		DBusTalk: dbusTalk,
		PassEnv:  passEnv,
		Command:  fs.Args(),
	}
	if _, err := cli.CreateOrUpdate(cfg); err != nil {
		return err
	}
	if len(pkgs) > 0 {
		if _, err := cli.AddPackages(name, pkgs); err != nil {
			return err
		}
	}
	if mountStage {
		if _, err := cli.EnsureStage(name); err != nil {
			return err
		}
	}
	return nil
}

func cmdRun(cli *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: jail run <name> [command...]")
	}
	name := args[0]
	cmdArgs := args[1:]
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}

	cwd, _ := os.Getwd()
	hostEnv := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			hostEnv[k] = v
		}
	}

	spec, err := cli.GetRunSpec(name, model.RunRequest{
		Cwd:     cwd,
		HostEnv: hostEnv,
	})
	if err != nil {
		return err
	}

	if len(cmdArgs) == 0 {
		if len(spec.DefaultCmd) > 0 {
			cmdArgs = spec.DefaultCmd
		} else {
			cmdArgs = []string{"/bin/bash"}
		}
	}

	nsenterBin, err := exec.LookPath("nsenter")
	if err != nil {
		return fmt.Errorf("nsenter not found in PATH: %w", err)
	}

	argFD, err := openBwrapArgsFD(spec.BwrapArgs)
	if err != nil {
		return err
	}

	execArgs := []string{
		"nsenter",
		"--preserve-credentials",
		"-t", fmt.Sprintf("%d", spec.ManagerPID),
		"-U", "-m",
		"--",
		spec.BwrapPath,
		"--args", strconv.Itoa(argFD),
		"--",
	}
	execArgs = append(execArgs, cmdArgs...)

	return syscall.Exec(nsenterBin, execArgs, os.Environ())
}

func openBwrapArgsFD(args []string) (int, error) {
	f, err := os.CreateTemp("", "jail-bwrap-args-*")
	if err != nil {
		return -1, err
	}
	_ = os.Remove(f.Name())
	defer f.Close()

	var buf strings.Builder
	for _, a := range args {
		buf.WriteString(a)
		buf.WriteByte(0)
	}
	if _, err := f.WriteString(buf.String()); err != nil {
		return -1, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return -1, err
	}
	return syscall.Dup(int(f.Fd()))
}

func cmdExpose(cli *client.Client, args []string) error {
	var rw bool
	var target string
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--ro" || a == "-ro" || (len(positional) == 2 && a == "ro") {
			rw = false
		} else if a == "--rw" || a == "-rw" || (len(positional) == 2 && a == "rw") {
			rw = true
		} else if (a == "--target" || a == "-t") && i+1 < len(args) {
			i++
			target = args[i]
		} else {
			positional = append(positional, a)
		}
	}
	if len(positional) != 2 {
		return fmt.Errorf("usage: jail expose <name> <path> [--target JAIL_PATH] [--rw|--ro]")
	}
	absPath, err := filepath.Abs(positional[1])
	if err != nil {
		return err
	}
	if target != "" {
		target, err = filepath.Abs(target)
		if err != nil {
			return err
		}
	}
	_, err = cli.Expose(positional[0], absPath, target, rw)
	return err
}

func cmdHide(cli *client.Client, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: jail hide <name> <path>")
	}
	absPath, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	_, err = cli.Hide(args[0], absPath)
	return err
}

func cmdPkg(cli *client.Client, args []string) error {
	if len(args) > 0 && args[0] == "add" {
		args = args[1:]
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: jail pkg [add] <name> <pkg...>")
	}
	_, err := cli.AddPackages(args[0], args[1:])
	return err
}

func cmdList(cli *client.Client) error {
	list, err := cli.List()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tACTIVE\tNIX\tNET\tGPU\tGUI\tMOUNTS")
	for _, j := range list {
		c := j.Config
		fmt.Fprintf(tw, "%s\t%t\t%s\t%t\t%t\t%t\t%d\n",
			c.Name, j.Active, c.NixMode, c.Network, c.GPU, c.GUI, len(c.Mounts))
	}
	return tw.Flush()
}

func cmdStatus(cli *client.Client, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: jail status <name>")
	}
	st, err := cli.Get(args[0])
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}

func cmdStop(cli *client.Client, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: jail stop <name>")
	}
	return cli.StopStage(args[0])
}

func cmdDelete(cli *client.Client, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: jail del <name>")
	}
	return cli.Delete(args[0])
}

func cmdWeb(cli *client.Client, args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	var listenAddr string
	fs.StringVar(&listenAddr, "listen", "127.0.0.1:7480", "HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	targetURL, _ := url.Parse("http://jaild")
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.FlushInterval = -1
	proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cli.SocketPath())
		},
	}

	fmt.Fprintf(os.Stderr, "jail web UI listening on http://%s (proxying to %s)\n", listenAddr, cli.SocketPath())
	return http.ListenAndServe(listenAddr, proxy)
}
