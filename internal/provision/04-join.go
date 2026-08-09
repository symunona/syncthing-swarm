package provision

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

// FieldChange is one scalar that an upsert would rewrite.
type FieldChange struct{ Field, Old, New string }

// nodeFields is the upsert's whole surface: the scalars bootstrap knows how to
// derive from a freshly provisioned box. `local` is deliberately absent — it is
// a human's declaration about which node the dashboard shares FROM, and the
// wizard has no business guessing it.
func nodeFields(n config.Node) []FieldChange {
	return []FieldChange{
		{Field: "url", New: n.URL},
		{Field: "apikey", New: n.APIKey},
		{Field: "root", New: n.Root},
		{Field: "mount", New: n.Mount},
		{Field: "ssh", New: n.SSH},
	}
}

// DiffNode reports what UpsertNode would change. exists is false when the node
// is new, in which case changes is nil and the upsert is a plain append.
func DiffNode(path string, n config.Node) (changes []FieldChange, exists bool, err error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, false, err
	}
	var cur *config.Node
	for i := range cfg.Nodes {
		if cfg.Nodes[i].Name == n.Name {
			cur = &cfg.Nodes[i]
			break
		}
	}
	if cur == nil {
		return nil, false, nil
	}
	old := map[string]string{
		"url": cur.URL, "apikey": cur.APIKey, "root": cur.Root,
		"mount": cur.Mount, "ssh": cur.SSH,
	}
	for _, f := range nodeFields(n) {
		// Never blank a field that swarm.yaml has and this run could not derive.
		if f.New == "" || f.New == old[f.Field] {
			continue
		}
		f.Old = old[f.Field]
		changes = append(changes, f)
	}
	return changes, true, nil
}

// UpsertNode writes the node into swarm.yaml: appended when new, rewritten in
// place when it is already there.
//
// Text surgery rather than a yaml.v3 round-trip, for the same reason the
// original AppendNode appended text: swarm.yaml is a hand-maintained cred store
// and the marshaller would eat every comment in it.
//
// The in-place case exists because a node outlives its hardware. fiona was
// rebuilt onto a new SSD and came back with a different drive path and a
// different API key, while its swarm.yaml entry still described the machine it
// used to be. Refusing to touch an existing entry — the old behaviour — meant
// the wizard could install syncthing on a box and then be unable to record the
// result.
func UpsertNode(path string, n config.Node) error {
	_, exists, err := DiffNode(path, n)
	if err != nil {
		return err
	}
	if !exists {
		return appendNode(path, n)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")

	start, end := nodeBlock(lines, n.Name)
	if start < 0 {
		return fmt.Errorf("node %q parsed from %s but its lines could not be found", n.Name, path)
	}

	want := map[string]string{}
	for _, f := range nodeFields(n) {
		if f.New != "" {
			want[f.Field] = f.New
		}
	}

	for i := start; i < end; i++ {
		key, _, comment, ok := splitScalar(lines[i])
		if !ok {
			continue
		}
		v, wanted := want[key]
		if !wanted {
			continue
		}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
		lines[i] = fmt.Sprintf("%s%s: %s%s", indent, key, v, comment)
		delete(want, key)
	}

	// Fields the old entry never had (a node provisioned before `mount` existed)
	// get appended to the block rather than dropped.
	//
	// Deviation from the brief: naively inserting at `end` breaks on both the
	// last node in the file (no trailing "- name:" to bound it — `end` lands
	// after the file's own trailing blank/newline artifact from strings.Split,
	// so the new field would land outside the node's mapping, after a stray
	// blank line, and the file's trailing newline would be silently dropped)
	// and on a middle node (the blank line conventionally separating entries
	// sits inside [start,end), so the field would land in that gap rather than
	// beside the fields it belongs with). Walking back from `end` over blank
	// lines finds the block's real last content line and inserts right after
	// it, leaving any separator/trailing blank exactly where it was.
	if len(want) > 0 {
		insertAt := end
		for insertAt > start && strings.TrimSpace(lines[insertAt-1]) == "" {
			insertAt--
		}
		// Derive the indent from an existing FIELD line in this node's block —
		// the same way the rewrite loop above derives it — instead of
		// hardcoding "    ". A hardcoded four spaces silently corrupts any
		// swarm.yaml that happens to indent its fields differently (hand
		// edits are exactly how this file drifts): the appended field lands
		// at the wrong depth, which is either a cosmetic wart if YAML still
		// parses it as a sibling, or a flat-out parse error if it doesn't.
		// splitScalar rejects the "- name:" line itself (it starts with
		// "- "), so this walk naturally lands on the first real field —
		// url, apikey, whichever the block actually has.
		indent := "    "
		for i := start; i < end; i++ {
			if key, _, _, ok := splitScalar(lines[i]); ok && key != "" {
				indent = lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
				break
			}
		}
		var add []string
		for _, f := range nodeFields(n) {
			if v, ok := want[f.Field]; ok {
				add = append(add, fmt.Sprintf("%s%s: %s", indent, f.Field, v))
			}
		}
		tail := append([]string{}, lines[insertAt:]...)
		lines = append(lines[:insertAt], append(add, tail...)...)
	}

	return atomicWriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

// atomicWriteFile replaces path's contents without ever leaving it half
// written. swarm.yaml is a credential store with no backup copy the user can
// reconstruct; an interrupt (crash, killed process, disk full) partway
// through an in-place os.WriteFile would truncate it to zero length before a
// single new byte lands. Writing to a temp file in the same directory first
// and renaming over the target makes the switch a single filesystem
// operation: readers either see the old file or the new one, never a
// half-written one. Same directory matters — os.Rename is only atomic within
// one filesystem/mount, and a cross-filesystem tmp dir would fall back to a
// non-atomic copy.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".swarm-yaml-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Flush to disk before the rename. Without this, the write can still be
	// sitting in the page cache when the rename lands: the DIRECTORY ENTRY
	// swap is durable immediately, but the file's actual bytes are not
	// guaranteed to be until fsync'd, and a power loss or crash between the
	// rename and the eventual writeback can leave the new name pointing at
	// zero or partial bytes on some filesystems/mount options — the exact
	// half-written state this function exists to prevent, just moved one
	// step later.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// os.CreateTemp makes the file 0600 already, but this store holds live
	// API keys, so the mode is asserted explicitly rather than trusted.
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// nodeBlock finds the line range of one node's YAML block: from its "- name:"
// line up to (not including) the next list item at the same indent, or EOF.
func nodeBlock(lines []string, name string) (start, end int) {
	start = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "- name:") {
			continue
		}
		if start >= 0 {
			return start, i
		}
		// "- name: fiona          # rpi" -> "fiona"
		v := strings.TrimSpace(strings.TrimPrefix(t, "- name:"))
		if i := strings.Index(v, "#"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		// YAML lets a scalar be quoted or bare — "- name: fiona" and
		// "- name: \"fiona\"" name the same node. Comparing the raw text
		// meant a quoted entry never matched `name` at all: nodeBlock
		// returned start=-1, UpsertNode's "its lines could not be found"
		// error fired on a node DiffNode had just found two lines above it
		// (DiffNode goes through config.Load/yaml.v3, which does unquote),
		// and the failure landed after syncthing was already installed and
		// running on the box — installed, but never recorded.
		v = strings.Trim(v, `"'`)
		if v == name {
			start = i
		}
	}
	if start < 0 {
		return -1, -1
	}
	return start, len(lines)
}

// splitScalar takes "    apikey: OLD    # note" apart, keeping the trailing
// comment so a rewrite does not destroy it.
//
// A '#' only starts a YAML comment when it is preceded by whitespace (or
// opens the line) — never when it's stuck to the middle of a scalar. Treating
// every '#' as a comment marker would silently truncate a credential like
// "ABC#DEF" down to "ABC" on the next rewrite, and worse, could leave a
// fragment of a REPLACED secret sitting in the file disguised as a comment.
// So the value and comment are only ever split on a '#' that is genuinely
// preceded by a space or tab.
func splitScalar(line string) (key, value, comment string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "- ") {
		return "", "", "", false
	}
	i := strings.Index(t, ":")
	if i < 0 {
		return "", "", "", false
	}
	key = strings.TrimSpace(t[:i])

	// Work from the raw, untrimmed line past the colon: whatever spacing the
	// user typed between the value and the comment is theirs to keep, since
	// swarm.yaml is hand-formatted and those comments are hand-aligned.
	leadWS := len(line) - len(strings.TrimLeft(line, " \t"))
	rest := line[leadWS+i+1:]

	commentAt := -1
	for idx := 0; idx < len(rest); idx++ {
		if rest[idx] != '#' {
			continue
		}
		if idx == 0 || rest[idx-1] == ' ' || rest[idx-1] == '\t' {
			commentAt = idx
			break
		}
	}
	if commentAt >= 0 {
		// Walk back over the whitespace run right before the '#' too, so the
		// comment carries its own lead-in spacing rather than losing it to
		// the trimmed value.
		start := commentAt
		for start > 0 && (rest[start-1] == ' ' || rest[start-1] == '\t') {
			start--
		}
		comment = rest[start:]
		rest = rest[:start]
	}
	return key, strings.TrimSpace(rest), comment, true
}

// appendNode adds a brand-new node to swarm.yaml. Renamed from the old
// exported AppendNode: UpsertNode is now the entry point, and it — not this
// function — decides whether an existing name means "refuse" or "rewrite".
//
// Appends TEXT rather than round-tripping through yaml.v3, which would eat every
// comment in the file. swarm.yaml is a hand-maintained cred store; keeping the
// user's comments matters more than the elegance of the marshaller.
//
// This is the path a brand-new node takes — the FIRST time bootstrap ever
// writes a box's credentials to swarm.yaml — so it gets the same atomic-write
// guarantee as the rewrite path below, not a plain O_APPEND. An append that
// dies partway (disk full, process killed, ssh session dropped mid-write) can
// leave a truncated "- name: rue\n    url: http://…" fragment sitting in the
// file, which is a YAML parse error the NEXT time anything reads swarm.yaml —
// including every other, already-working node's credentials sitting right
// above it.
func appendNode(path string, n config.Node) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
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

	return atomicWriteFile(path, append(raw, b.String()...), 0o600)
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
