package diskusage

import (
	"context"
	"strings"
	"testing"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

// The disk bar exists to tell you when a drive is in trouble. It used to do the
// opposite: `df <root> || df /` meant that when an external drive died, df fell
// back to the boot media and the UI drew a healthy green bar for a drive that was
// GONE. These tests pin the two ways that lie can happen.

// 1. The root path is gone entirely, on a node that declares a drive.
func TestCollectRootMissingOnDrive(t *testing.T) {
	n := config.Node{Name: "rue", Root: "/definitely/not/here", Mount: "/mnt/hdd", URL: "http://127.0.0.1:8384"}
	u := Collect(context.Background(), n)

	if !u.Missing {
		t.Fatalf("Missing = false for an absent root on a declared drive; got %+v", u)
	}
	if u.Total != 0 || u.Pct != 0 {
		t.Errorf("reported usage %d bytes / %d%% for a drive that is not there — "+
			"this is the fallback-to-/ bug: a missing drive must never draw a bar", u.Total, u.Pct)
	}
	if !strings.Contains(u.Err, "not mounted") {
		t.Errorf("Err = %q, want it to say the drive is not mounted", u.Err)
	}
}

// A root that simply hasn't been created yet, on a node with no separate drive,
// is a missing directory — NOT a dead disk. It must report honestly (no bar) but
// must not raise the drive alarm: an alarm that cries wolf is one you learn to
// ignore. (pandora is exactly this today: root /home/symunona/syncthing does not
// exist, and it has no external drive.)
func TestCollectRootMissingIsNotADeadDrive(t *testing.T) {
	n := config.Node{Name: "pandora", Root: "/definitely/not/here", URL: "http://127.0.0.1:8384"}
	u := Collect(context.Background(), n)

	if u.Missing {
		t.Errorf("raised DRIVE MISSING for a node with no drive declared: %+v", u)
	}
	if u.Err == "" {
		t.Error("silently reported nothing; a non-existent root is still worth saying out loud")
	}
	if u.Total != 0 {
		t.Errorf("drew a bar (%d bytes) for a root that does not exist", u.Total)
	}
}

// 2. The nastier one: the drive died but its mountpoint survives as an empty
// directory on the boot media, so df happily resolves /mnt/hdd to the SD card.
// The node declares where its drive belongs; a df that lands anywhere else is a
// missing drive, not a healthy one.
func TestCollectDriveUnmountedButMountpointExists(t *testing.T) {
	// /tmp exists and is (almost certainly) not its own mount matching /mnt/hdd,
	// which is exactly the shape of the failure: the path resolves, but not to
	// the drive we expect.
	n := config.Node{
		Name:  "rue",
		Root:  "/tmp",
		Mount: "/mnt/hdd", // where the drive is supposed to be
		URL:   "http://127.0.0.1:8384",
	}
	u := Collect(context.Background(), n)

	if !u.Missing {
		t.Fatalf("Missing = false: df resolved %q to a filesystem that is not %q, "+
			"which means the drive is not mounted; got %+v", n.Root, n.Mount, u)
	}
	if u.Total != 0 {
		t.Errorf("drew a bar (%d bytes) from the wrong filesystem", u.Total)
	}
}

// A node whose root really is on the main filesystem (no separate drive, so no
// Mount declared) must still report normally — the guard must not cry wolf.
func TestCollectNoSeparateDrive(t *testing.T) {
	n := config.Node{Name: "hub", Root: "/", URL: "http://127.0.0.1:8384"}
	u := Collect(context.Background(), n)

	if u.Missing {
		t.Errorf("Missing = true for a root on the main filesystem: %+v", u)
	}
	if u.Err != "" {
		t.Fatalf("unexpected error: %s", u.Err)
	}
	if u.Total == 0 {
		t.Error("no usage reported for /")
	}
}
