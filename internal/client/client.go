package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"jaild/internal/model"
)

type Client struct {
	sockPath string
	http     *http.Client
}

func DefaultSocketPath() string {
	if s := os.Getenv("JAILD_SOCKET"); s != "" {
		return s
	}
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, "jaild.sock")
	}
	return fmt.Sprintf("/run/user/%d/jaild.sock", os.Getuid())
}

func New(sockPath string) *Client {
	if sockPath == "" {
		sockPath = DefaultSocketPath()
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	return &Client{
		sockPath: sockPath,
		http:     &http.Client{Transport: tr},
	}
}

func (c *Client) SocketPath() string {
	return c.sockPath
}

func (c *Client) List() ([]model.JailStatus, error) {
	var out []model.JailStatus
	err := c.doJSON(http.MethodGet, "/api/jails", nil, &out)
	return out, err
}

func (c *Client) Get(name string) (model.JailStatus, error) {
	var out model.JailStatus
	err := c.doJSON(http.MethodGet, "/api/jails/"+url.PathEscape(name), nil, &out)
	return out, err
}

func (c *Client) CreateOrUpdate(cfg model.JailConfig) (model.JailStatus, error) {
	var out model.JailStatus
	err := c.doJSON(http.MethodPost, "/api/jails", cfg, &out)
	return out, err
}

func (c *Client) Delete(name string) error {
	return c.doJSON(http.MethodDelete, "/api/jails/"+url.PathEscape(name), nil, nil)
}

func (c *Client) EnsureStage(name string) (model.JailStatus, error) {
	var out model.JailStatus
	err := c.doJSON(http.MethodPost, "/api/jails/"+url.PathEscape(name)+"/stage", nil, &out)
	return out, err
}

func (c *Client) StopStage(name string) error {
	return c.doJSON(http.MethodDelete, "/api/jails/"+url.PathEscape(name)+"/stage", nil, nil)
}

func (c *Client) Expose(name, path string, ro bool) (model.JailStatus, error) {
	var out model.JailStatus
	body := model.MountEntry{Path: path, ReadOnly: ro}
	err := c.doJSON(http.MethodPost, "/api/jails/"+url.PathEscape(name)+"/expose", body, &out)
	return out, err
}

func (c *Client) Hide(name, path string) (model.JailStatus, error) {
	var out model.JailStatus
	body := map[string]string{"path": path}
	err := c.doJSON(http.MethodPost, "/api/jails/"+url.PathEscape(name)+"/hide", body, &out)
	return out, err
}

func (c *Client) AddPackages(name string, pkgs []string) (model.JailStatus, error) {
	var out model.JailStatus
	body := map[string][]string{"packages": pkgs}
	err := c.doJSON(http.MethodPost, "/api/jails/"+url.PathEscape(name)+"/packages", body, &out)
	return out, err
}

func (c *Client) GetRunSpec(name string, req model.RunRequest) (model.RunSpec, error) {
	var out model.RunSpec
	err := c.doJSON(http.MethodPost, "/api/jails/"+url.PathEscape(name)+"/run-spec", req, &out)
	return out, err
}

func (c *Client) doJSON(method, path string, reqBody any, respOut any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://jaild"+path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect to jaild (%s): %w", c.sockPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jaild %s %s: %s", method, path, strings.TrimSpace(string(msg)))
	}
	if respOut != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(respOut)
	}
	return nil
}

