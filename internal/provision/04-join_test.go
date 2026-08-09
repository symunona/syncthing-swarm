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

// taskbot is the LAST node in swarmFixture and has no `mount` line at all.
// Appending a field an existing node never had is a different code path from
// rewriting one it already has (TestUpsertNodeRewritesInPlaceAndKeepsComments
// never exercises it: fiona already carries every field nodeFields knows
// about, so that test's `want` map empties out entirely in the rewrite loop
// and the append branch never runs). Being the LAST node means there is no
// following "- name:" line to bound the block, so the insertion point has to
// be derived from strings.Split's own trailing-newline artifact rather than a
// real boundary — the naive version of this (insert at the raw `end` the
// brief's nodeBlock returns) drops the file's trailing newline and leaves the
// new field stranded after a stray blank line, outside where a reader would
// expect it.
func TestUpsertNodeAppendsMissingFieldOnLastNode(t *testing.T) {
	path := writeFixture(t)
	if err := UpsertNode(path, config.Node{Name: "taskbot", Mount: "/mnt/taskbot"}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	// The new field must sit directly under taskbot's last existing field,
	// with the file's original trailing newline intact — not after a stray
	// blank line, and not swallowed.
	if !strings.Contains(s, "apikey: BBB\n    mount: /mnt/taskbot\n") {
		t.Errorf("mount did not land inside taskbot's block with the trailing newline preserved:\n%s", s)
	}
	if strings.Contains(s, "apikey: BBB\n\n") {
		t.Error("a blank line was introduced between taskbot's existing fields and the appended one")
	}
	// The other nodes, including their comments, must be untouched.
	if !strings.Contains(s, "# rpi, tailscale") || !strings.Contains(s, "# for disk stats (df over ssh)") {
		t.Error("an unrelated node's comments were disturbed by the append")
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("file no longer parses after appending to the last node: %v\n%s", err, s)
	}
	if len(cfg.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %+v", len(cfg.Nodes), cfg.Nodes)
	}
	got := cfg.Node("taskbot")
	if got == nil || got.Mount != "/mnt/taskbot" {
		t.Errorf("taskbot.Mount = %+v, want /mnt/taskbot", got)
	}
}

// A fixture where the node missing a field sits in the MIDDLE, with the
// fixture's own blank-line convention separating every entry, to pin down
// that an appended field lands beside its own block's fields and not in the
// blank-line gap that precedes the next node — the naive insertion point (the
// brief's raw `end`, which is nodeBlock's next "- name:" line) sits on the
// far side of that gap, not before it.
const middleAppendFixture = `listen: :8888
pollSeconds: 10
nodes:
  - name: alpha
    url: http://10.0.0.1:8384
    apikey: AAA

  - name: beta
    url: http://10.0.0.2:8384
    apikey: BBB

  - name: gamma
    url: http://10.0.0.3:8384
    apikey: CCC
`

func TestUpsertNodeAppendsMissingFieldOnMiddleNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	if err := os.WriteFile(path, []byte(middleAppendFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpsertNode(path, config.Node{Name: "beta", Mount: "/mnt/beta"}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	// The new field belongs right under beta's own last field, and the blank
	// separator before gamma must survive immediately after it — not before.
	if !strings.Contains(s, "apikey: BBB\n    mount: /mnt/beta\n\n  - name: gamma") {
		t.Errorf("mount did not land inside beta's block ahead of the gamma separator:\n%s", s)
	}
	if strings.Contains(s, "apikey: BBB\n\n    mount:") {
		t.Error("mount landed in the blank-line gap before the next node instead of beside beta's own fields")
	}
	// alpha's separator (and alpha itself) must be untouched too.
	if !strings.Contains(s, "apikey: AAA\n\n  - name: beta") {
		t.Error("the separator/structure around an unrelated node changed")
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("file no longer parses after appending to a middle node: %v\n%s", err, s)
	}
	if len(cfg.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %+v", len(cfg.Nodes), cfg.Nodes)
	}
	if got := cfg.Node("beta"); got == nil || got.Mount != "/mnt/beta" {
		t.Errorf("beta.Mount = %+v, want /mnt/beta", got)
	}
	if got := cfg.Node("alpha"); got == nil || got.APIKey != "AAA" {
		t.Errorf("alpha damaged by beta's upsert: %+v", got)
	}
	if got := cfg.Node("gamma"); got == nil || got.APIKey != "CCC" {
		t.Errorf("gamma damaged by beta's upsert: %+v", got)
	}
}

// YAML only starts a comment at a '#' preceded by whitespace (or at the very
// start of the line) — never at a bare '#' stuck inside a scalar. delta's
// apikey below has one glued to the middle of it, the way a hex/base64 API
// key could. Upserting a DIFFERENT field (mount) on the same node still walks
// past this line — nodeFields always includes apikey in `want` whenever the
// caller supplies one, even unchanged, so the rewrite loop reprocesses it —
// and the value must survive that pass untouched, not get truncated at the
// '#' or leave a stray fragment of itself behind as a fake comment.
const hashApikeyFixture = `listen: :8888
pollSeconds: 10
nodes:
  - name: delta
    url: http://10.0.0.9:8384
    apikey: ABC#DEF
    mount: /mnt/old
`

func TestUpsertNodePreservesApikeyContainingHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	if err := os.WriteFile(path, []byte(hashApikeyFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	err := UpsertNode(path, config.Node{
		Name: "delta", APIKey: "ABC#DEF", Mount: "/mnt/new",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	if !strings.Contains(s, "apikey: ABC#DEF\n") {
		t.Errorf("apikey containing '#' was corrupted:\n%s", s)
	}
	if !strings.Contains(s, "mount: /mnt/new") {
		t.Errorf("mount was not updated:\n%s", s)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("file no longer parses: %v\n%s", err, s)
	}
	got := cfg.Node("delta")
	if got == nil || got.APIKey != "ABC#DEF" {
		t.Errorf("delta.APIKey = %+v, want ABC#DEF intact", got)
	}
	if got == nil || got.Mount != "/mnt/new" {
		t.Errorf("delta.Mount = %+v, want /mnt/new", got)
	}
}
