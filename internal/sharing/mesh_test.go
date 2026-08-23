package sharing

import (
	"reflect"
	"sort"
	"testing"
)

// The exact shape of the incident this package exists to fix: pandora shared
// dropx to fiona, then separately to taskbot. pandora ends up knowing all
// three; fiona and taskbot each only know pandora and themselves. All three
// must converge on the full three-device set.
func TestMeshDevicesConvergesPairwiseTopology(t *testing.T) {
	present := map[string][]string{
		"pandora": {"P", "F", "T"},
		"fiona":   {"P", "F"},
		"taskbot": {"P", "T"},
	}
	got := meshDevices(present)

	want := []string{"F", "P", "T"} // sorted union
	for _, node := range []string{"pandora", "fiona", "taskbot"} {
		r, ok := got[node]
		if !ok {
			t.Fatalf("node %q missing from result", node)
		}
		if !reflect.DeepEqual(r.Devices, want) {
			t.Errorf("%s: Devices = %v, want %v", node, r.Devices, want)
		}
	}

	// pandora already had everyone: nothing to do, no needless PutFolder.
	if got["pandora"].Changed {
		t.Error("pandora already had the full set — must report Changed=false")
	}
	// fiona and taskbot are each missing one device.
	if !got["fiona"].Changed {
		t.Error("fiona is missing taskbot — must report Changed=true")
	}
	if !got["taskbot"].Changed {
		t.Error("taskbot is missing fiona — must report Changed=true")
	}
}

// A node that does not hold the folder at all must never appear in the
// result — meshDevices deciding otherwise would be the one bug that turns
// "repair" into "create a folder somewhere it was never shared".
func TestMeshDevicesNeverInventsANodeThatLacksTheFolder(t *testing.T) {
	present := map[string][]string{
		"pandora": {"P", "F"},
		"fiona":   {"P", "F"},
		// "taskbot" intentionally absent: it does not have this folder.
	}
	got := meshDevices(present)

	if _, ok := got["taskbot"]; ok {
		t.Error("taskbot has no key in `present` (no folder there) but appeared in the result")
	}
	if len(got) != 2 {
		t.Errorf("result has %d entries, want exactly the 2 nodes that hold the folder", len(got))
	}
}

// A node that already has everyone converges to unchanged — pinned separately
// from the three-node test above so a future refactor that only fixes the
// "missing" case can't quietly break the "nothing to do" case.
func TestMeshDevicesReportsUnchangedWhenAlreadyComplete(t *testing.T) {
	present := map[string][]string{
		"a": {"X", "Y", "Z"},
		"b": {"X", "Y", "Z"},
	}
	got := meshDevices(present)
	if got["a"].Changed || got["b"].Changed {
		t.Errorf("both nodes already hold the full union — want Changed=false on both, got a=%v b=%v",
			got["a"].Changed, got["b"].Changed)
	}
}

// Duplicate deviceIDs in the source data (a folder's "devices" list should
// never have them, but nothing enforces that on the way in) must not produce
// duplicates in the target set, and the result must be deterministic —
// re-running on the same input twice gives the identical, sorted list.
func TestMeshDevicesDedupesAndIsDeterministic(t *testing.T) {
	present := map[string][]string{
		"a": {"X", "X", "Y"},
		"b": {"Y", "Z", "Z", "Z"},
	}

	first := meshDevices(present)
	second := meshDevices(present)

	for _, node := range []string{"a", "b"} {
		devs := first[node].Devices
		seen := map[string]bool{}
		for _, d := range devs {
			if seen[d] {
				t.Fatalf("%s: duplicate deviceID %q in %v", node, d, devs)
			}
			seen[d] = true
		}
		if !sort.StringsAreSorted(devs) {
			t.Errorf("%s: Devices = %v, not sorted", node, devs)
		}
		if !reflect.DeepEqual(first[node], second[node]) {
			t.Errorf("%s: not deterministic — first=%v second=%v", node, first[node], second[node])
		}
	}

	wantUnion := []string{"X", "Y", "Z"}
	if !reflect.DeepEqual(first["a"].Devices, wantUnion) {
		t.Errorf("union = %v, want %v", first["a"].Devices, wantUnion)
	}
}
