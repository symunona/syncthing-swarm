// Package config loads the swarm cred store (swarm.yaml).
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Node is one managed syncthing instance (a "column" in the matrix).
type Node struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`             // GUI/REST base, e.g. http://100.x.y.z:8384
	APIKey string `yaml:"apikey"`          // per-node X-API-Key
	Root   string `yaml:"root,omitempty"`  // base dir for new shared folders: <root>/<label>
	Local  bool   `yaml:"local,omitempty"` // the machine we share FROM (defaults to the 127.0.0.1 node)
	SSH    string `yaml:"ssh,omitempty"`   // ssh destination+opts for disk stats, e.g. "-p 2222 taskbot" (empty = local)

	// Mount is where Root's drive is EXPECTED to be mounted, e.g. /mnt/hdd.
	// Optional; only meaningful when the root lives on a separate drive.
	//
	// It exists to catch a silent failure. When an external drive dies its
	// mountpoint usually survives as an empty directory on the boot media, so
	// `df /mnt/hdd/syncthing` cheerfully reports the SD CARD — and the dashboard
	// draws a healthy disk bar for a drive that is gone. Knowing where the drive
	// belongs lets us call that what it is: a df that resolves anywhere else
	// means the drive is not mounted.
	//
	// `stc bootstrap` sets this when it provisions a node onto a drive.
	Mount string `yaml:"mount,omitempty"`
}

// IsLocal reports whether this node runs on the same host as swarmd (so disk
// stats come from a local df rather than ssh).
func (n *Node) IsLocal() bool {
	return n.Local || strings.Contains(n.URL, "127.0.0.1") || strings.Contains(n.URL, "localhost")
}

// Config is the whole swarm.yaml.
type Config struct {
	Listen      string `yaml:"listen"`      // dashboard bind addr
	PollSeconds int    `yaml:"pollSeconds"` // poll interval
	Nodes       []Node `yaml:"nodes"`
}

// Load reads and validates swarm.yaml from path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8888"
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 15
	}
	if len(c.Nodes) == 0 {
		return nil, fmt.Errorf("%s has no nodes", path)
	}
	seen := map[string]bool{}
	for i, n := range c.Nodes {
		if n.Name == "" || n.URL == "" || n.APIKey == "" {
			return nil, fmt.Errorf("node[%d]: name, url, apikey all required", i)
		}
		if seen[n.Name] {
			return nil, fmt.Errorf("duplicate node name %q", n.Name)
		}
		seen[n.Name] = true
	}
	return &c, nil
}

// Node returns the node with the given name, or nil.
func (c *Config) Node(name string) *Node {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			return &c.Nodes[i]
		}
	}
	return nil
}

// LocalNode is the machine we share folders FROM: the node flagged local, else
// the one bound to loopback (the hub itself), else the first node.
func (c *Config) LocalNode() *Node {
	for i := range c.Nodes {
		if c.Nodes[i].Local {
			return &c.Nodes[i]
		}
	}
	for i := range c.Nodes {
		if strings.Contains(c.Nodes[i].URL, "127.0.0.1") || strings.Contains(c.Nodes[i].URL, "localhost") {
			return &c.Nodes[i]
		}
	}
	if len(c.Nodes) > 0 {
		return &c.Nodes[0]
	}
	return nil
}
