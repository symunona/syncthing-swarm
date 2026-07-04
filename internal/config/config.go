// Package config loads the swarm cred store (swarm.yaml).
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Node is one managed syncthing instance (a "column" in the matrix).
type Node struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`    // GUI/REST base, e.g. http://100.x.y.z:8384
	APIKey string `yaml:"apikey"` // per-node X-API-Key
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
