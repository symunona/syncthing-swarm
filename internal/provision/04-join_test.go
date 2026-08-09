package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

const swarmFixture = `listen: :8888
pollSeconds: 10
nodes:
  - name: pandora        # this machine (hub) — local syncthing
    url: http://127.0.0.1:8384
    apikey: AAA
    root: /home/symunona/syncthing

  - name: fiona          # rpi, tailscale
    url: http://100.86.131.51:8384
    mount: /media/hdd    # root's drive belongs here
    apikey: OLDKEY
    root: /media/hdd/syncthing
    ssh: fiona           # for disk stats (df over ssh)

  - name: taskbot        # tailscale, ssh :2222
    url: http://100.96.124.62:8384
    apikey: BBB
`

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	if err := os.WriteFile(path, []byte(swarmFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func rebuiltFiona() config.Node {
	return config.Node{
		Name:   "fiona",
		URL:    "http://100.86.131.51:8384",
		APIKey: "NEWKEY",
		Root:   "/srv/data/syncthing",
		Mount:  "/srv/data",
		SSH:    "fiona",
	}
}

// A rebuilt box keeps its name and loses everything else. The diff is what the
// user is asked to approve, so it must name every field that actually moves and
// nothing that does not.
func TestDiffNodeReportsChangedFieldsOnly(t *testing.T) {
	path := writeFixture(t)
	changes, exists, err := DiffNode(path, rebuiltFiona())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("fiona is in the fixture but DiffNode says it is not")
	}
	got := map[string]FieldChange{}
	for _, c := range changes {
		got[c.Field] = c
	}
	for field, want := range map[string]string{
		"apikey": "NEWKEY",
		"root":   "/srv/data/syncthing",
		"mount":  "/srv/data",
	} {
		if got[field].New != want {
			t.Errorf("%s: New = %q, want %q", field, got[field].New, want)
		}
	}
	if got["mount"].Old != "/media/hdd" {
		t.Errorf("mount: Old = %q, want /media/hdd", got["mount"].Old)
	}
	for _, unchanged := range []string{"url", "ssh", "name"} {
		if _, ok := got[unchanged]; ok {
			t.Errorf("%s reported as changed but it did not move", unchanged)
		}
	}
}

// swarm.yaml is a hand-maintained cred store. Round-tripping it through the
// marshaller would eat every comment in the file, which is why the original
// AppendNode appended text — the rewrite must respect the same rule.
func TestUpsertNodeRewritesInPlaceAndKeepsComments(t *testing.T) {
	path := writeFixture(t)
	if err := UpsertNode(path, rebuiltFiona()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	if strings.Contains(s, "OLDKEY") || strings.Contains(s, "/media/hdd/syncthing") {
		t.Error("stale values survived the upsert")
	}
	if !strings.Contains(s, "apikey: NEWKEY") || !strings.Contains(s, "root: /srv/data/syncthing") {
		t.Error("new values not written")
	}
	if !strings.Contains(s, "# rpi, tailscale") || !strings.Contains(s, "# for disk stats (df over ssh)") {
		t.Error("comments were eaten")
	}
	if !strings.Contains(s, "apikey: AAA") || !strings.Contains(s, "apikey: BBB") {
		t.Error("another node was damaged")
	}
	// It must still parse, and fiona must be exactly one node.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("upserted file no longer loads: %v", err)
	}
	n := 0
	for _, node := range cfg.Nodes {
		if node.Name == "fiona" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("fiona appears %d times", n)
	}
}

// The absent case is the old AppendNode behaviour, unchanged.
func TestUpsertNodeAppendsWhenAbsent(t *testing.T) {
	path := writeFixture(t)
	err := UpsertNode(path, config.Node{
		Name: "rue", URL: "http://100.1.2.3:8384", APIKey: "CCC",
		Root: "/mnt/hdd/syncthing", Mount: "/mnt/hdd", SSH: "rue",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nodes[len(cfg.Nodes)-1].Name != "rue" {
		t.Errorf("rue not appended: %+v", cfg.Nodes)
	}
}
