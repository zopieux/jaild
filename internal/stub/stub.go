package stub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"jaild/internal/model"
)

func SocketPath() string {
	if s := os.Getenv("JAILD_STUB_SOCKET"); s != "" {
		return s
	}
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, "jaild-stub.sock")
	}
	return fmt.Sprintf("/run/user/%d/jaild-stub.sock", os.Getuid())
}

type nixShellInvocation struct {
	Packages []string
	Command  string
}

func ParseNixShellArgs(args []string) (nixShellInvocation, error) {
	var inv nixShellInvocation
	inPackages := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-p" || a == "--packages":
			inPackages = true
		case strings.HasPrefix(a, "-p") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			inv.Packages = append(inv.Packages, a[2:])
			inPackages = true
		case a == "--run" || a == "--command":
			inPackages = false
			if i+1 >= len(args) {
				return inv, fmt.Errorf("%s requires an argument", a)
			}
			i++
			inv.Command = args[i]
		case a == "--pure" || a == "--impure":
			inPackages = false
		case a == "-I" || a == "--arg" || a == "--argstr" || a == "--attr" || a == "-A":
			inPackages = false
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "-"):
			inPackages = false
		default:
			if inPackages {
				inv.Packages = append(inv.Packages, a)
			}
		}
	}
	if len(inv.Packages) == 0 {
		return inv, errors.New("nix-shell stub requires -p / --packages <pkg...>")
	}
	return inv, nil
}

type nixSubcommand int

const (
	nixSubShell nixSubcommand = iota
	nixSubRun
	nixSubPrintDevEnv
)

type nixInvocation struct {
	Sub         nixSubcommand
	Packages    []string
	Command     []string
	Profile     string
	Installable string
}

func ParseNixArgs(args []string) (nixInvocation, error) {
	var inv nixInvocation
	idx := 0
	for idx < len(args) {
		a := args[idx]
		if a == "--extra-experimental-features" && idx+1 < len(args) {
			idx += 2
			continue
		}
		if strings.HasPrefix(a, "-") {
			idx++
			continue
		}
		break
	}
	if idx >= len(args) {
		return inv, errors.New("usage: nix <shell|run|print-dev-env> [args...]")
	}

	sub := args[idx]
	rest := args[idx+1:]

	switch sub {
	case "print-dev-env":
		inv.Sub = nixSubPrintDevEnv
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if a == "--profile" {
				if i+1 >= len(rest) {
					return inv, errors.New("--profile requires a path")
				}
				i++
				inv.Profile = rest[i]
				continue
			}
			if a == "--extra-experimental-features" && i+1 < len(rest) {
				i++
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			if inv.Installable == "" {
				inv.Installable = a
			}
		}
		return inv, nil

	case "shell":
		inv.Sub = nixSubShell
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if a == "-c" || a == "--command" {
				if i+1 >= len(rest) {
					return inv, fmt.Errorf("%s requires a command", a)
				}
				inv.Command = rest[i+1:]
				break
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			inv.Packages = append(inv.Packages, a)
		}
		if len(inv.Packages) == 0 {
			return inv, errors.New("nix shell requires at least one installable")
		}
		return inv, nil

	case "run":
		inv.Sub = nixSubRun
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if a == "--" {
				inv.Command = rest[i+1:]
				break
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			if len(inv.Packages) == 0 {
				inv.Packages = append(inv.Packages, a)
			} else {
				inv.Command = append(inv.Command, a)
			}
		}
		if len(inv.Packages) == 0 {
			return inv, errors.New("nix run requires an installable")
		}
		return inv, nil

	default:
		return inv, fmt.Errorf("nix stub only supports 'shell', 'run', and 'print-dev-env' (got %q)", sub)
	}
}

func postStubJSON[Req any, Resp any](sockPath, endpoint string, req Req) (Resp, error) {
	var zero Resp
	if sockPath == "" {
		sockPath = SocketPath()
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	cli := &http.Client{Transport: tr}
	raw, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	resp, err := cli.Post("http://jaild-stub"+endpoint, "application/json", bytes.NewReader(raw))
	if err != nil {
		return zero, fmt.Errorf("connect to jaild stub socket (%s): %w", sockPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return zero, fmt.Errorf("jaild stub error: %s", strings.TrimSpace(string(msg)))
	}
	var out Resp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}

func Provision(sockPath string, pkgs []string) (model.StubProvisionResponse, error) {
	return postStubJSON[model.StubProvisionRequest, model.StubProvisionResponse](
		sockPath,
		"/provision",
		model.StubProvisionRequest{Packages: pkgs},
	)
}

func ProvisionDevEnv(sockPath string, req model.StubDevEnvRequest) (model.StubDevEnvResponse, error) {
	return postStubJSON[model.StubDevEnvRequest, model.StubDevEnvResponse](
		sockPath,
		"/dev-env",
		req,
	)
}

func RunNixShell(args []string) int {
	inv, err := ParseNixShellArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nix-shell: %v\n", err)
		return 1
	}
	res, err := Provision("", inv.Packages)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nix-shell: %v\n", err)
		return 1
	}

	env := buildAugmentedEnv(res.BinPaths)
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}

	execArgs := []string{filepath.Base(shell)}
	if inv.Command != "" {
		execArgs = append(execArgs, "-c", inv.Command)
	}
	if err := syscall.Exec(shell, execArgs, env); err != nil {
		fmt.Fprintf(os.Stderr, "nix-shell: exec %s: %v\n", shell, err)
		return 1
	}
	return 0
}

func RunNix(args []string) int {
	inv, err := ParseNixArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nix: %v\n", err)
		return 1
	}

	if inv.Sub == nixSubPrintDevEnv {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "nix print-dev-env: %v\n", err)
			return 1
		}
		prof := inv.Profile
		if prof != "" && !filepath.IsAbs(prof) {
			prof = filepath.Join(cwd, prof)
		}
		res, err := ProvisionDevEnv("", model.StubDevEnvRequest{
			Dir:         cwd,
			Profile:     prof,
			Installable: inv.Installable,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "nix print-dev-env: %v\n", err)
			return 1
		}
		fmt.Print(res.Script)
		return 0
	}

	res, err := Provision("", inv.Packages)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nix: %v\n", err)
		return 1
	}

	env := buildAugmentedEnv(res.BinPaths)
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			_ = os.Setenv("PATH", v)
		}
	}

	if inv.Sub == nixSubRun {
		bin, err := findRunBinary(inv.Packages[0], res.BinPaths)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nix run: %v\n", err)
			return 1
		}
		execArgs := append([]string{filepath.Base(bin)}, inv.Command...)
		if err := syscall.Exec(bin, execArgs, env); err != nil {
			fmt.Fprintf(os.Stderr, "nix run: exec %s: %v\n", bin, err)
			return 1
		}
		return 0
	}

	if len(inv.Command) > 0 {
		bin, err := exec.LookPath(inv.Command[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "nix shell: %v\n", err)
			return 1
		}
		execArgs := append([]string{filepath.Base(bin)}, inv.Command[1:]...)
		if err := syscall.Exec(bin, execArgs, env); err != nil {
			fmt.Fprintf(os.Stderr, "nix shell: exec %s: %v\n", bin, err)
			return 1
		}
		return 0
	}

	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	if err := syscall.Exec(shell, []string{filepath.Base(shell)}, env); err != nil {
		fmt.Fprintf(os.Stderr, "nix shell: exec %s: %v\n", shell, err)
		return 1
	}
	return 0
}

func buildAugmentedEnv(binPaths []string) []string {
	curPath := os.Getenv("PATH")
	parts := append([]string{}, binPaths...)
	if curPath != "" {
		parts = append(parts, curPath)
	}
	newPath := strings.Join(parts, ":")

	var out []string
	sawPath := false
	sawNixShell := false
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH":
			out = append(out, "PATH="+newPath)
			sawPath = true
		case "IN_NIX_SHELL":
			out = append(out, "IN_NIX_SHELL=impure")
			sawNixShell = true
		default:
			out = append(out, kv)
		}
	}
	if !sawPath {
		out = append(out, "PATH="+newPath)
	}
	if !sawNixShell {
		out = append(out, "IN_NIX_SHELL=impure")
	}
	return out
}

func findRunBinary(spec string, binPaths []string) (string, error) {
	name := spec
	if idx := strings.LastIndex(name, "#"); idx >= 0 {
		name = name[idx+1:]
	}
	name = filepath.Base(name)

	for _, dir := range binPaths {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	for _, dir := range binPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				return filepath.Join(dir, e.Name()), nil
			}
		}
	}
	return "", fmt.Errorf("no executable found for %q in %v", spec, binPaths)
}
