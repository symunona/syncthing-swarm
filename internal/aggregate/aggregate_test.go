package aggregate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

func TestCompletion(t *testing.T) {
	cases := []struct {
		global, need int64
		want         float64
	}{
		{0, 0, 100},      // empty folder = synced
		{100, 0, 100},    // fully synced
		{100, 100, 0},    // nothing yet
		{100, 25, 75},    // three quarters
		{100, -5, 100},   // guard: negative need
	}
	for _, c := range cases {
		if got := completion(c.global, c.need); got != c.want {
			t.Errorf("completion(%d,%d)=%v want %v", c.global, c.need, got, c.want)
		}
	}
}

// fakeNode serves the minimal REST surface Poll touches.
func fakeNode(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/system/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"v1.27.2","os":"linux","arch":"amd64"}`))
	})
	mux.HandleFunc("/rest/system/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"myID":"DEVICE-ID-1"}`))
	})
	mux.HandleFunc("/rest/system/error", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"errors":[]}`))
	})
	mux.HandleFunc("/rest/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"folders":[{"id":"photos","label":"Photos"}],"devices":[]}`))
	})
	mux.HandleFunc("/rest/db/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"state":"idle","globalBytes":100,"needBytes":0,"needItems":0,"errors":0}`))
	})
	mux.HandleFunc("/rest/folder/errors", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"errors":[]}`))
	})
	return httptest.NewServer(mux)
}

func TestPoll(t *testing.T) {
	srv := fakeNode(t)
	defer srv.Close()
	down := "http://127.0.0.1:1" // unreachable

	cfg := &config.Config{Nodes: []config.Node{
		{Name: "up", URL: srv.URL, APIKey: "x"},
		{Name: "down", URL: down, APIKey: "x"},
	}}

	snap := Poll(context.Background(), cfg, time.Unix(0, 0))

	if len(snap.Devices) != 2 {
		t.Fatalf("want 2 device columns, got %d", len(snap.Devices))
	}
	byName := map[string]Device{}
	for _, d := range snap.Devices {
		byName[d.Name] = d
	}
	if !byName["up"].Online || byName["up"].Version != "v1.27.2" {
		t.Errorf("up node wrong: %+v", byName["up"])
	}
	if byName["down"].Online {
		t.Error("down node should be offline")
	}
	if len(snap.Folders) != 1 || snap.Folders[0].ID != "photos" {
		t.Fatalf("want folder photos, got %+v", snap.Folders)
	}
	cell := snap.Cells["photos"]["up"]
	if !cell.Present || cell.Completion != 100 || cell.State != "idle" {
		t.Errorf("photos@up cell wrong: %+v", cell)
	}
	// down node contributes no cells
	if _, ok := snap.Cells["photos"]["down"]; ok {
		t.Error("down node should contribute no cells")
	}
}
