// mesh.go: full-mesh device membership for a folder that already exists on
// more than one node.
//
// INCIDENT: Share used to be strictly pairwise. `stc share dropx fiona`
// followed by `stc share dropx taskbot` left the folder's device list looking
// like {pandora,fiona,taskbot} on pandora, {pandora,fiona} on fiona, and
// {pandora,taskbot} on taskbot. Syncthing only syncs a folder between two
// devices when BOTH sides list each other IN THAT FOLDER's device set — the
// device mesh (which every node knows about which) is a separate thing from
// folder membership entirely — so fiona and taskbot, despite being meshed as
// devices and sitting on a fast direct tailnet link, never exchanged dropx
// with each other. Every byte routed through pandora instead. pandora is the
// human's laptop; fiona and taskbot are always-on servers. So whenever the
// laptop was shut, the two servers that actually stay up around the clock
// could not converge with each other at all. Nobody chose that topology — it
// fell out of calling a two-device folder API twice.
//
// The fix: make a folder's device list the SAME SET on every node that holds
// it — the union of whatever is there today. Share now does this after its
// pairwise step (unless -pairwise opts out), and `stc remesh` applies the
// same fix to folders that were shared before this existed.
package sharing

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// meshResult is one node's target folder membership once meshed.
type meshResult struct {
	Devices []string // full target set for this node's folder: sorted, deduped
	Changed bool     // true if reaching Devices requires adding something
}

// meshDevices computes, for every node that ALREADY holds a folder, the full
// device set it should carry once meshed — the union of every device ID seen
// on any node's copy of that folder — and whether reaching it takes a change.
//
// present is keyed by node name: the current deviceIDs on that node's copy of
// the folder, as read off its "devices" list. A node absent from present does
// not have the folder at all and is NEVER added to the result — this is the
// one piece of full-mesh sharing that decides who is eligible to be widened,
// and remesh must never be the reason a folder gets created somewhere it
// wasn't already shared to. Keeping that decision in one pure function, apart
// from the HTTP calls that carry it out, is what makes it cheap to pin hard
// with a test instead of trusting it by inspection.
//
// Every node present's device set is, by construction, a subset of the union
// (the union is built FROM those same sets), so "changed" is decidable purely
// by set size: equal size means equal sets, no shorter comparison needed.
func meshDevices(present map[string][]string) map[string]meshResult {
	union := map[string]bool{}
	for _, devs := range present {
		for _, d := range devs {
			union[d] = true
		}
	}
	target := make([]string, 0, len(union))
	for d := range union {
		target = append(target, d)
	}
	sort.Strings(target)

	out := make(map[string]meshResult, len(present))
	for node, devs := range present {
		have := map[string]bool{}
		for _, d := range devs {
			have[d] = true
		}
		out[node] = meshResult{Devices: target, Changed: len(have) < len(target)}
	}
	return out
}

// RemeshResult reports what remeshing one folder did, node by node.
type RemeshResult struct {
	Folder  string
	Added   map[string][]string // node name -> device IDs newly added there
	Skipped []string            // nodes reachable but WITHOUT this folder — left alone, never created
	Failed  map[string]string   // node name -> error; partial mesh is incomplete, not corrupt
}

// Remesh applies full-mesh membership to a folder that ALREADY exists on at
// least one node, without ever creating it anywhere new. This is the repair
// path for folders shared before mesh-by-default existed (or with
// -pairwise): it widens each node's device list to the union, the same thing
// the mesh step inside Share does automatically for new shares, so a folder
// stuck in the old pairwise topology can be fixed without re-sharing it.
func Remesh(ctx context.Context, cfg *config.Config, folderRef string) (*RemeshResult, error) {
	id, err := resolveRemeshTarget(ctx, cfg, folderRef)
	if err != nil {
		return nil, err
	}
	return remeshFolder(ctx, cfg, id), nil
}

// RemeshAll applies Remesh to every folder ID visible anywhere in the swarm —
// `stc remesh -all`. discoveryFailed reports nodes that could not even be
// asked what folders they hold: unlike remeshFolder's per-folder Failed (which
// still shows up once per folder that DOES get found elsewhere), a node down
// for this discovery pass could be the ONLY place a folder is known, in which
// case that folder never enters the sweep at all and would otherwise vanish
// from the report with no trace — the exact "misleading done" this whole
// feature is required not to produce for an unreachable node.
func RemeshAll(ctx context.Context, cfg *config.Config) (results map[string]*RemeshResult, discoveryFailed map[string]string) {
	ids := map[string]bool{}
	discoveryFailed = map[string]string{}
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		cli := stclient.New(n.URL, n.APIKey)
		var folders []stclient.Folder
		if err := cli.ConfigFolders(ctx, &folders); err != nil {
			discoveryFailed[n.Name] = err.Error()
			continue
		}
		for _, f := range folders {
			if id, _ := f["id"].(string); id != "" {
				ids[id] = true
			}
		}
	}
	results = make(map[string]*RemeshResult, len(ids))
	for id := range ids {
		results[id] = remeshFolder(ctx, cfg, id)
	}
	return results, discoveryFailed
}

// resolveRemeshTarget finds the canonical folder ID for ref by asking each
// managed node in turn — ref may be an ID (matches directly on any node that
// has it) or a label (case-insensitive, and only some nodes may show it). The
// first node that recognizes ref by either name wins; remeshFolder then
// re-derives full membership fresh from every node, so which node happened to
// answer first does not bias the result.
func resolveRemeshTarget(ctx context.Context, cfg *config.Config, ref string) (string, error) {
	var errs []string
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		cli := stclient.New(n.URL, n.APIKey)
		if _, id, err := resolveFolder(ctx, cli, ref); err == nil {
			return id, nil
		} else {
			errs = append(errs, fmt.Sprintf("%s: %v", n.Name, err))
		}
	}
	return "", fmt.Errorf("folder %q not found on any reachable node:\n  %s", ref, strings.Join(errs, "\n  "))
}

// remeshFolder is the shared core behind Remesh, RemeshAll, and the mesh step
// inside Share: gather folderID's device list from every node that has it,
// compute the union with meshDevices, and PutFolder only where that union
// actually changes something. An unreachable node is recorded in Failed and
// otherwise ignored — the same "partial mesh is incomplete, not corrupt"
// convention MeshDevice uses in internal/provision/04-join.go — because one
// down box must never stop the rest of the swarm from converging.
func remeshFolder(ctx context.Context, cfg *config.Config, folderID string) *RemeshResult {
	res := &RemeshResult{Folder: folderID, Added: map[string][]string{}, Failed: map[string]string{}}

	type holder struct {
		cli    *stclient.Client
		folder stclient.Folder
	}
	present := map[string][]string{}
	holders := map[string]holder{}

	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		cli := stclient.New(n.URL, n.APIKey)
		f, found, err := cli.GetFolder(ctx, folderID)
		if err != nil {
			res.Failed[n.Name] = err.Error()
			continue
		}
		if !found {
			// Not corrupt, not missing — this node was simply never part of
			// this share. Recording it as Skipped (rather than silently
			// dropping it) is what lets `stc remesh` say "nothing alarming"
			// instead of nothing at all.
			res.Skipped = append(res.Skipped, n.Name)
			continue
		}
		devs, _ := f["devices"].([]any)
		var ids []string
		for _, d := range devs {
			if m, ok := d.(map[string]any); ok {
				if id, _ := m["deviceID"].(string); id != "" {
					ids = append(ids, id)
				}
			}
		}
		present[n.Name] = ids
		holders[n.Name] = holder{cli: cli, folder: f}
	}

	for name, want := range meshDevices(present) {
		if !want.Changed {
			continue
		}
		h := holders[name]
		var added []string
		for _, d := range want.Devices {
			// addDevice is the same idempotent helper Share uses to add the
			// target device to the source folder — reused here rather than
			// reimplemented, and it is also the guarantee that this loop can
			// only ever ADD an entry, never touch the ones already there.
			if addDevice(h.folder, d) {
				added = append(added, d)
			}
		}
		if len(added) == 0 {
			// meshDevices said Changed but nothing new landed — should not
			// happen given Changed is derived from the same have/target sets
			// addDevice is walking, but this is not the place to trust that
			// invariant blindly: a mismatch here must not silently PUT a
			// folder that has not actually changed.
			continue
		}
		if err := h.cli.PutFolder(ctx, h.folder); err != nil {
			res.Failed[name] = err.Error()
			continue
		}
		res.Added[name] = added
	}
	return res
}

// meshNote turns a RemeshResult into the short summary appended to a Share
// Result's Note. Empty when the mesh step found nothing to widen — the common
// case, since a share to a folder nobody else holds has nothing to mesh onto
// — because a share command that prints alarming-looking mesh output on every
// single run trains a human to stop reading it.
func meshNote(mr *RemeshResult) string {
	if len(mr.Added) == 0 && len(mr.Failed) == 0 {
		return ""
	}
	var parts []string
	if len(mr.Added) > 0 {
		names := make([]string, 0, len(mr.Added))
		for n := range mr.Added {
			names = append(names, n)
		}
		sort.Strings(names)
		parts = append(parts, "meshed onto "+strings.Join(names, ", "))
	}
	if len(mr.Failed) > 0 {
		names := make([]string, 0, len(mr.Failed))
		for n := range mr.Failed {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("mesh could not reach %s: %s", n, mr.Failed[n]))
		}
	}
	return strings.Join(parts, "; ")
}
