// Package stclient is a thin client for one syncthing node's REST API.
// Docs: https://docs.syncthing.net/dev/rest.html
package stclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Folder is a syncthing folder config kept as a raw object so we round-trip
// every field we don't model when copying a folder between nodes.
type Folder map[string]any

// Device is a syncthing device config kept raw for the same reason.
type Device map[string]any

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

// do sends a request with an optional JSON body. found=false on 404 (so callers
// can tell "not there" from a real error). Returns error on other non-2xx.
func (c *Client) do(ctx context.Context, method, path string, body any) (found bool, err error) {
	var rdr *bytes.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return false, e
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("%s %s -> %s", method, path, resp.Status)
	}
	return true, nil
}

// GetFolder returns the folder config, found=false if the node has no such folder.
func (c *Client) GetFolder(ctx context.Context, id string) (Folder, bool, error) {
	var f Folder
	err := c.get(ctx, "/rest/config/folders/"+id, nil, &f)
	if err != nil {
		// get() turns 404 into an error string; treat "404" as not-found
		if err.Error() == "/rest/config/folders/"+id+" -> 404 Not Found" {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

// ConfigFolders lists all folders as raw objects (into out, a *[]Folder).
func (c *Client) ConfigFolders(ctx context.Context, out any) error {
	return c.get(ctx, "/rest/config/folders", nil, out)
}

// PutFolder creates or replaces a folder (must contain "id").
func (c *Client) PutFolder(ctx context.Context, f Folder) error {
	id, _ := f["id"].(string)
	_, err := c.do(ctx, http.MethodPut, "/rest/config/folders/"+id, f)
	return err
}

// DeleteFolder removes a folder from config. Does NOT delete files on disk.
func (c *Client) DeleteFolder(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/rest/config/folders/"+id, nil)
	return err
}

// HasDevice reports whether the node already knows a device ID.
func (c *Client) HasDevice(ctx context.Context, deviceID string) (bool, error) {
	return c.do(ctx, http.MethodGet, "/rest/config/devices/"+deviceID, nil)
}

// PutDevice adds/updates a device (must contain "deviceID").
func (c *Client) PutDevice(ctx context.Context, d Device) error {
	id, _ := d["deviceID"].(string)
	_, err := c.do(ctx, http.MethodPut, "/rest/config/devices/"+id, d)
	return err
}

// BrowseTop lists a folder's top-level entry names from the GLOBAL index — so
// any node that knows the folder can answer, even one that holds none of the
// files locally.
//
// This is the fingerprint used to match a directory found on a new node's disk
// against the folder it actually is. Names alone are not enough: on a real drive
// the folder labelled "Music" sits in a directory called "music", and "Music
// Resources" in "music_resources". Worse, a coincidental name match would cause
// an adoption under the WRONG folder ID — the one genuinely destructive mistake
// available here.
func (c *Client) BrowseTop(ctx context.Context, folderID string) ([]string, error) {
	var entries []struct {
		Name string `json:"name"`
	}
	q := url.Values{"folder": {folderID}, "levels": {"0"}}
	if err := c.get(ctx, "/rest/db/browse", q, &entries); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name != "" {
			names = append(names, e.Name)
		}
	}
	return names, nil
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

	// Error is the folder-level error, and it is the one that matters when a
	// drive dies: "folder marker missing (this indicates potential data loss…)".
	// It is NOT the same as /rest/folder/errors, which only carries per-file pull
	// failures — so a folder whose whole disk vanished reports zero of those.
	// Without this field the UI showed a red cell with no reason.
	Error string `json:"error"`
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
