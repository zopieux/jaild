package manager

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func StoreRootFromPath(p string) string {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		resolved = p
	}
	resolved = filepath.Clean(resolved)
	const prefix = "/nix/store/"
	if !strings.HasPrefix(resolved, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(resolved, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if parts[0] == "" {
		return ""
	}
	return prefix + parts[0]
}

func ResolveInstallableOrPath(spec string) ([]string, error) {
	return ResolveInstallableOrPathWithProgress(spec, nil)
}

func ResolveInstallableOrPathWithProgress(spec string, onProgress func(string)) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if strings.HasPrefix(spec, "/nix/store/") {
		if root := StoreRootFromPath(spec); root != "" {
			return []string{root}, nil
		}
	}
	if filepath.IsAbs(spec) {
		if stPaths, err := os.ReadFile(filepath.Join(spec, "store-paths")); err == nil {
			return parseLines(string(stPaths)), nil
		}
		if root := StoreRootFromPath(spec); root != "" {
			return []string{root}, nil
		}
	}
	if binPath, err := exec.LookPath(spec); err == nil {
		if root := StoreRootFromPath(binPath); root != "" {
			return []string{root}, nil
		}
	}
	nixBin, err := exec.LookPath("nix")
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %q: nix binary not found in PATH", spec)
	}
	installable := spec
	if !strings.Contains(installable, "#") && !strings.HasPrefix(installable, "/") && !strings.HasPrefix(installable, ".") {
		installable = "nixpkgs#" + installable
	}
	if onProgress != nil {
		onProgress("resolving " + installable + "...")
	}
	cmd := exec.Command(nixBin, "--extra-experimental-features", "nix-command flakes", "build", "--no-link", "--print-out-paths", installable)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(io.TeeReader(stderrPipe, &stderr))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && onProgress != nil {
			onProgress(line)
		}
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("nix build %q failed: %v (%s)", installable, err, strings.TrimSpace(stderr.String()))
	}
	return parseLines(stdout.String()), nil
}

func QueryClosure(roots []string) ([]string, error) {
	var cleanRoots []string
	for _, r := range roots {
		if root := StoreRootFromPath(r); root != "" {
			if _, err := os.Stat(root); err == nil && !slices.Contains(cleanRoots, root) {
				cleanRoots = append(cleanRoots, root)
			}
		}
	}
	if len(cleanRoots) == 0 {
		return nil, nil
	}
	nixStoreBin, err := exec.LookPath("nix-store")
	if err != nil {
		return cleanRoots, nil
	}
	args := append([]string{"-qR"}, cleanRoots...)
	cmd := exec.Command(nixStoreBin, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return cleanRoots, nil
	}
	paths := parseLines(stdout.String())
	if len(paths) == 0 {
		return cleanRoots, nil
	}
	return paths, nil
}

func CollectBinPaths(storeRoots []string) []string {
	var bins []string
	for _, r := range storeRoots {
		root := StoreRootFromPath(r)
		if root == "" {
			continue
		}
		b := filepath.Join(root, "bin")
		if info, err := os.Stat(b); err == nil && info.IsDir() {
			if !slices.Contains(bins, b) {
				bins = append(bins, b)
			}
		}
	}
	return bins
}

func parseLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !slices.Contains(out, line) {
			out = append(out, line)
		}
	}
	return out
}
