package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"jaild/internal/client"
	"jaild/internal/manager"
	"jaild/internal/server"
	"jaild/internal/store"
	"jaild/internal/stub"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case "nix-shell":
		os.Exit(stub.RunNixShell(os.Args[1:]))
	case "nix":
		os.Exit(stub.RunNix(os.Args[1:]))
	}

	if os.Getenv("JAILD_IN_NS") != "1" {
		if err := reexecInNamespace(); err != nil {
			log.Fatalf("jaild namespace bootstrap failed: %v", err)
		}
		return
	}

	if err := runDaemon(); err != nil {
		log.Fatalf("jaild failed: %v", err)
	}
}

func reexecInNamespace() error {
	hostUID := os.Getuid()
	hostGID := os.Getgid()
	hostUser := os.Getenv("USER")
	if hostUser == "" {
		if u, err := user.Current(); err == nil {
			hostUser = u.Username
		} else {
			hostUser = "user"
		}
	}
	hostHome, _ := os.UserHomeDir()

	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"JAILD_IN_NS=1",
		fmt.Sprintf("JAILD_HOST_UID=%d", hostUID),
		fmt.Sprintf("JAILD_HOST_GID=%d", hostGID),
		fmt.Sprintf("JAILD_HOST_USER=%s", hostUser),
		fmt.Sprintf("JAILD_HOST_HOME=%s", hostHome),
	)

	if listenFDs, _ := strconv.Atoi(os.Getenv("LISTEN_FDS")); listenFDs >= 1 {
		cmd.ExtraFiles = []*os.File{os.NewFile(3, "systemd-socket")}
		cmd.Env = append(cmd.Env, "JAILD_SOCKET_FD=3")
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: hostUID, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: hostGID, Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			_ = cmd.Process.Signal(sig)
		}
	}()

	return cmd.Wait()
}

func runDaemon() error {
	var (
		sockPath string
		dataDir  string
		rootDir  string
		workDir  string
		httpAddr string
	)
	flag.StringVar(&sockPath, "socket", client.DefaultSocketPath(), "Unix socket path")
	flag.StringVar(&dataDir, "data-dir", store.DefaultDir(), "Directory for persisted jail state")
	flag.StringVar(&rootDir, "root-dir", "", "Stage root directory (default /run/user/$UID/jails)")
	flag.StringVar(&workDir, "work-dir", "", "Private scratch work directory (default /run/user/$UID/jail-work)")
	flag.StringVar(&httpAddr, "http", "", "Optional TCP address for embedded web interface (e.g. 127.0.0.1:7480)")
	flag.Parse()

	if err := syscall.Mount("", "/", "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("set root mount propagation slave: %w", err)
	}

	hostUID, _ := strconv.Atoi(os.Getenv("JAILD_HOST_UID"))
	hostGID, _ := strconv.Atoi(os.Getenv("JAILD_HOST_GID"))
	hostUser := os.Getenv("JAILD_HOST_USER")
	hostHome := os.Getenv("JAILD_HOST_HOME")

	st, err := store.NewTOMLStore(dataDir)
	if err != nil {
		return err
	}

	mgr, err := manager.New(manager.Options{
		Store:    st,
		RootDir:  rootDir,
		WorkDir:  workDir,
		HostUID:  hostUID,
		HostGID:  hostGID,
		HostUser: hostUser,
		HostHome: hostHome,
	})
	if err != nil {
		return err
	}

	srv := server.New(mgr)
	httpSrv := &http.Server{Handler: srv}

	ln, cleanupSock, err := openUnixListener(sockPath)
	if err != nil {
		return err
	}
	defer cleanupSock()

	if httpAddr != "" {
		tcpLn, err := net.Listen("tcp", httpAddr)
		if err != nil {
			return fmt.Errorf("listen http %s: %w", httpAddr, err)
		}
		defer tcpLn.Close()
		go func() {
			_ = httpSrv.Serve(tcpLn)
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = httpSrv.Shutdown(context.Background())
	}()

	err = httpSrv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func openUnixListener(sockPath string) (net.Listener, func(), error) {
	if fdStr := os.Getenv("JAILD_SOCKET_FD"); fdStr != "" {
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid JAILD_SOCKET_FD: %w", err)
		}
		f := os.NewFile(uintptr(fd), "systemd-socket")
		if f == nil {
			return nil, nil, errors.New("failed to open inherited systemd socket fd")
		}
		ln, err := net.FileListener(f)
		_ = f.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("net.FileListener on systemd socket: %w", err)
		}
		return ln, func() {}, nil
	}

	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, nil, err
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen unix %s: %w", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o600)
	return ln, func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}, nil
}

