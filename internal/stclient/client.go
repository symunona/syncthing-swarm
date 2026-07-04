// Package stclient is a thin client for one syncthing node's REST API.
// Docs: https://docs.syncthing.net/dev/rest.html
package stclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Client talks to a single syncthing instance.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

func New(base, apiKey string) *Client {
	return &Client{
		base:   base,
		apiKey: apiKey,
		http:   &http.Client{Timeout: 8 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s -> %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- response shapes (only the fields we use) ---

type Version struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

type SystemStatus struct {
	MyID string `json:"myID"`
}

type Config struct {
	Folders []ConfigFolder `json:"folders"`
	Devices []ConfigDevice `json:"devices"`
}

type ConfigFolder struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Path    string `json:"path"`
	Paused  bool   `json:"paused"`
	Devices []struct {
		DeviceID string `json:"deviceID"`
	} `json:"devices"`
}

type ConfigDevice struct {
	DeviceID string `json:"deviceID"`
	Name     string `json:"name"`
}

// DBStatus is /rest/db/status — local state of a folder on this node.
type DBStatus struct {
	State       string `json:"state"` // idle | scanning | syncing | error
	GlobalBytes int64  `json:"globalBytes"`
	NeedBytes   int64  `json:"needBytes"`
	NeedItems   int64  `json:"needItems"`
	Errors      int    `json:"errors"`
}

type FolderError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type folderErrorsResp struct {
	Errors []FolderError `json:"errors"`
}

type SystemError struct {
	When    string `json:"when"`
	Message string `json:"message"`
}

type systemErrorsResp struct {
	Errors []SystemError `json:"errors"`
}

// --- endpoint methods ---

func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.get(ctx, "/rest/system/version", nil, &v)
	return v, err
}

func (c *Client) SystemStatus(ctx context.Context) (SystemStatus, error) {
	var s SystemStatus
	err := c.get(ctx, "/rest/system/status", nil, &s)
	return s, err
}

func (c *Client) Config(ctx context.Context) (Config, error) {
	var cfg Config
	err := c.get(ctx, "/rest/config", nil, &cfg)
	return cfg, err
}

func (c *Client) DBStatus(ctx context.Context, folder string) (DBStatus, error) {
	var s DBStatus
	err := c.get(ctx, "/rest/db/status", url.Values{"folder": {folder}}, &s)
	return s, err
}

func (c *Client) FolderErrors(ctx context.Context, folder string) ([]FolderError, error) {
	var r folderErrorsResp
	err := c.get(ctx, "/rest/folder/errors", url.Values{"folder": {folder}}, &r)
	return r.Errors, err
}

func (c *Client) SystemErrors(ctx context.Context) ([]SystemError, error) {
	var r systemErrorsResp
	err := c.get(ctx, "/rest/system/error", nil, &r)
	return r.Errors, err
}
