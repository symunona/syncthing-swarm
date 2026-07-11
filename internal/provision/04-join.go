package provision

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// JoinResult reports what joining the swarm did.
type JoinResult struct {
	Node       string
	DeviceID   string
	AddedTo    []string // nodes that now know this device
	AlreadyHad []string
	Failed     map[string]string // node -> error; partial mesh is incomplete, not corrupt
	YamlPath   string
}

// AppendNode adds the new node to swarm.yaml.
//
// Appends TEXT rather than round-tripping through yaml.v3, which would eat every
// comment in the file. swarm.yaml is a hand-maintained cred store; keeping the
// user's comments matters more than the elegance of the marshaller.
func AppendNode(path string, n config.Node) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.Contains(string(raw), "name: "+n.Name+"\n") ||
		strings.Contains(string(raw), "name: "+n.Name+" ") {
		return fmt.Errorf("swarm.yaml already has a node called %q", n.Name)
	}

	var b strings.Builder
	if !strings.HasSuffix(string(raw), "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n  - name: %s\n", n.Name)
	fmt.Fprintf(&b, "    url: %s\n", n.URL)
	fmt.Fprintf(&b, "    apikey: %s\n", n.APIKey)
	if n.Root != "" {
		fmt.Fprintf(&b, "    root: %s\n", n.Root)
	}
	if n.Mount != "" {
		// arms the DRIVE MISSING alarm: a df that resolves anywhere else means the
		// drive is gone, not that the disk is healthy
		fmt.Fprintf(&b, "    mount: %s\n", n.Mount)
	}
	if n.SSH != "" {
		fmt.Fprintf(&b, "    ssh: %s\n", n.SSH)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(b.String())
	return err
}

// MeshDevice teaches every node in the swarm about the new device, and the new
// device about every node. It does NOT touch a single folder: sharing is a
// separate, deliberate act.
//
// Addresses are set explicitly to tcp://<tailnet-ip>:22000 rather than left
// "dynamic": syncthing's global discovery does not reliably find tailnet peers,
// and the hub already knows every node's address from swarm.yaml.
func MeshDevice(ctx context.Context, cfg *config.Config, newNode config.Node, newID string) *JoinResult {
	res := &JoinResult{Node: newNode.Name, DeviceID: newID, Failed: map[string]string{}}
	newCli := stclient.New(newNode.URL, newNode.APIKey)

	for i := range cfg.Nodes {
		peer := cfg.Nodes[i]
		if peer.Name == newNode.Name {
			continue
		}
		peerCli := stclient.New(peer.URL, peer.APIKey)

		st, err := peerCli.SystemStatus(ctx)
		if err != nil {
			res.Failed[peer.Name] = "unreachable: " + err.Error()
			continue
		}

		// peer learns the new device
		had, err := peerCli.HasDevice(ctx, newID)
		if err != nil {
			res.Failed[peer.Name] = err.Error()
			continue
		}
		if had {
			res.AlreadyHad = append(res.AlreadyHad, peer.Name)
		} else {
			d := stclient.Device{
				"deviceID":  newID,
				"name":      newNode.Name,
				"addresses": []any{syncAddr(newNode.URL)},
			}
			if err := peerCli.PutDevice(ctx, d); err != nil {
				res.Failed[peer.Name] = "add device: " + err.Error()
				continue
			}
			res.AddedTo = append(res.AddedTo, peer.Name)
		}

		// the new device learns the peer
		if has, err := newCli.HasDevice(ctx, st.MyID); err == nil && !has {
			d := stclient.Device{
				"deviceID":  st.MyID,
				"name":      peer.Name,
				"addresses": []any{syncAddr(peer.URL)},
			}
			if err := newCli.PutDevice(ctx, d); err != nil {
				res.Failed[peer.Name] = "teach new node about " + peer.Name + ": " + err.Error()
			}
		}
	}
	return res
}

// syncAddr turns a GUI URL (http://100.x.y.z:8384) into a BEP sync address
// (tcp://100.x.y.z:22000). Falls back to "dynamic" if the host can't be read.
func syncAddr(guiURL string) string {
	u, err := url.Parse(guiURL)
	if err != nil || u.Hostname() == "" {
		return "dynamic"
	}
	return "tcp://" + u.Hostname() + ":22000"
}
