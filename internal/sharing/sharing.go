// Package sharing implements one-click folder share / unshare across the swarm.
// Used by both the swarmd HTTP handlers and the stc CLI so the two never drift.
//
// Share:   add target device to the source folder + create the folder on the
//          target at <root>/<label> (or an explicit path). Files sync normally.
// Unshare: remove target device from the source folder + delete the folder from
//          the target's config. NEVER deletes files on disk.
package sharing

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// Result reports what a share did.
type Result struct {
	Folder     string `json:"folder"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	TargetPath string `json:"targetPath"`
	Note       string `json:"note,omitempty"`
}

// Share shares folderID (given by id OR label, resolved on the source) from the
// source node to the target node. pathOverride wins over the target's root dir.
func Share(ctx context.Context, cfg *config.Config, folderRef, sourceName, targetName, pathOverride string) (*Result, error) {
	src, tgt, err := endpoints(cfg, sourceName, targetName)
	if err != nil {
		return nil, err
	}
	srcCli := stclient.New(src.URL, src.APIKey)
	tgtCli := stclient.New(tgt.URL, tgt.APIKey)

	srcFolder, id, err := resolveFolder(ctx, srcCli, folderRef)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", src.Name, err)
	}
	label, _ := srcFolder["label"].(string)
	if label == "" {
		label = id
	}

	targetPath := pathOverride
	if targetPath == "" {
		if tgt.Root == "" {
			return nil, fmt.Errorf("target %q has no root dir set (add `root:` in swarm.yaml or pass a path)", tgt.Name)
		}
		targetPath = path.Join(tgt.Root, label)
	}

	srcID, err := srcCli.SystemStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("source %q status: %w", src.Name, err)
	}
	tgtID, err := tgtCli.SystemStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("target %q status: %w", tgt.Name, err)
	}

	// devices must know each other (usually already do — they share the swarm)
	if err := ensureDevice(ctx, srcCli, tgtID.MyID, tgt.Name); err != nil {
		return nil, fmt.Errorf("teach %q about %q: %w", src.Name, tgt.Name, err)
	}
	if err := ensureDevice(ctx, tgtCli, srcID.MyID, src.Name); err != nil {
		return nil, fmt.Errorf("teach %q about %q: %w", tgt.Name, src.Name, err)
	}

	// 1) source offers the folder to the target
	if addDevice(srcFolder, tgtID.MyID) {
		if err := srcCli.PutFolder(ctx, srcFolder); err != nil {
			return nil, fmt.Errorf("update folder on %q: %w", src.Name, err)
		}
	}

	// 2) target accepts: create the folder (or ensure the link if it exists)
	res := &Result{Folder: id, Source: src.Name, Target: tgt.Name, TargetPath: targetPath}
	tgtFolder, found, err := tgtCli.GetFolder(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read folder on %q: %w", tgt.Name, err)
	}
	if found {
		res.TargetPath, _ = tgtFolder["path"].(string)
		res.Note = "target already had this folder; ensured share link"
		changed := addDevice(tgtFolder, srcID.MyID)
		changed = addDevice(tgtFolder, tgtID.MyID) || changed
		if changed {
			if err := tgtCli.PutFolder(ctx, tgtFolder); err != nil {
				return nil, fmt.Errorf("update folder on %q: %w", tgt.Name, err)
			}
		}
		return res, nil
	}
	ftype, _ := srcFolder["type"].(string)
	if ftype == "" {
		ftype = "sendreceive"
	}
	newFolder := stclient.Folder{
		"id":    id,
		"label": label,
		"path":  targetPath,
		"type":  ftype,
		"devices": []any{
			map[string]any{"deviceID": srcID.MyID},
			map[string]any{"deviceID": tgtID.MyID},
		},
	}
	if err := tgtCli.PutFolder(ctx, newFolder); err != nil {
		return nil, fmt.Errorf("create folder on %q: %w", tgt.Name, err)
	}
	return res, nil
}

// Unshare stops sharing folderRef between source and target. It removes the
// target device from the source folder and deletes the folder from the target
// config. It NEVER deletes files on disk.
func Unshare(ctx context.Context, cfg *config.Config, folderRef, sourceName, targetName string) (*Result, error) {
	src, tgt, err := endpoints(cfg, sourceName, targetName)
	if err != nil {
		return nil, err
	}
	srcCli := stclient.New(src.URL, src.APIKey)
	tgtCli := stclient.New(tgt.URL, tgt.APIKey)

	srcFolder, id, err := resolveFolder(ctx, srcCli, folderRef)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", src.Name, err)
	}
	tgtID, err := tgtCli.SystemStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("target %q status: %w", tgt.Name, err)
	}

	if removeDevice(srcFolder, tgtID.MyID) {
		if err := srcCli.PutFolder(ctx, srcFolder); err != nil {
			return nil, fmt.Errorf("update folder on %q: %w", src.Name, err)
		}
	}
	if err := tgtCli.DeleteFolder(ctx, id); err != nil {
		return nil, fmt.Errorf("remove folder on %q: %w", tgt.Name, err)
	}
	return &Result{Folder: id, Source: src.Name, Target: tgt.Name, Note: "files on disk kept"}, nil
}

// --- helpers ---

func endpoints(cfg *config.Config, sourceName, targetName string) (src, tgt *config.Node, err error) {
	if sourceName == "" {
		if l := cfg.LocalNode(); l != nil {
			sourceName = l.Name
		}
	}
	src = cfg.Node(sourceName)
	tgt = cfg.Node(targetName)
	if src == nil {
		return nil, nil, fmt.Errorf("unknown source node %q", sourceName)
	}
	if tgt == nil {
		return nil, nil, fmt.Errorf("unknown target node %q", targetName)
	}
	if src.Name == tgt.Name {
		return nil, nil, fmt.Errorf("source and target are the same node (%q)", src.Name)
	}
	return src, tgt, nil
}

// resolveFolder finds a folder on a node by id or (case-insensitive) label.
func resolveFolder(ctx context.Context, cli *stclient.Client, ref string) (stclient.Folder, string, error) {
	if f, found, err := cli.GetFolder(ctx, ref); err != nil {
		return nil, "", err
	} else if found {
		return f, ref, nil
	}
	// not an id — try matching a label
	var folders []stclient.Folder
	if err := cli.ConfigFolders(ctx, &folders); err != nil {
		return nil, "", err
	}
	for _, f := range folders {
		if label, _ := f["label"].(string); strings.EqualFold(label, ref) {
			id, _ := f["id"].(string)
			return f, id, nil
		}
	}
	return nil, "", fmt.Errorf("no folder with id or label %q", ref)
}

func ensureDevice(ctx context.Context, cli *stclient.Client, deviceID, name string) error {
	has, err := cli.HasDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	return cli.PutDevice(ctx, stclient.Device{"deviceID": deviceID, "name": name})
}

// addDevice adds deviceID to folder["devices"] if missing; returns true if changed.
func addDevice(f stclient.Folder, deviceID string) bool {
	devs, _ := f["devices"].([]any)
	for _, d := range devs {
		if m, ok := d.(map[string]any); ok {
			if id, _ := m["deviceID"].(string); id == deviceID {
				return false
			}
		}
	}
	f["devices"] = append(devs, map[string]any{"deviceID": deviceID})
	return true
}

// removeDevice drops deviceID from folder["devices"]; returns true if changed.
func removeDevice(f stclient.Folder, deviceID string) bool {
	devs, _ := f["devices"].([]any)
	out := devs[:0]
	changed := false
	for _, d := range devs {
		if m, ok := d.(map[string]any); ok {
			if id, _ := m["deviceID"].(string); id == deviceID {
				changed = true
				continue
			}
		}
		out = append(out, d)
	}
	if changed {
		f["devices"] = out
	}
	return changed
}
