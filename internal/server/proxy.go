package server

import (
	"io"
	"net/http"
	"strings"
)

// allowedREST is the whitelist of read-only syncthing REST paths the relay will
// forward. Anything not listed (config writes, restart, shutdown, pause…) is
// rejected — the relay is read-only until phase 2 adds guarded actions.
var allowedREST = map[string]bool{
	"rest/config/folders": true, // folder paths per node (no secrets; NOT rest/config, which leaks apikey)
	"rest/db/browse":      true,
	"rest/db/status":      true,
	"rest/db/completion":  true,
	"rest/db/file":        true,
	"rest/db/need":        true,
	"rest/folder/errors":  true,
	// files that exist ONLY on this node (receive-only folders). These are usually
	// the CAUSE of a pull error — "directory not empty" means local-only files are
	// blocking a deletion — so without this the UI can show the symptom but not
	// the reason. Read-only, no secrets.
	"rest/db/localchanged":    true,
	"rest/system/log":         true,
	"rest/system/connections": true,
	"rest/system/version":     true,
	"rest/system/status":      true,
	"rest/stats/folder":       true,
	"rest/stats/device":       true,
}

// handleNodeProxy forwards GET /api/node/{name}/rest/... to that node's
// syncthing REST API, injecting its X-API-Key. Only whitelisted paths pass.
func (s *Server) handleNodeProxy(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node(r.PathValue("name"))
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	path := r.PathValue("path") // e.g. "rest/db/browse"
	if !allowedREST[path] {
		http.Error(w, "path not allowed by relay: "+path, http.StatusForbidden)
		return
	}

	target := strings.TrimRight(node.URL, "/") + "/" + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("X-API-Key", node.APIKey)

	resp, err := s.proxy.Do(req)
	if err != nil {
		http.Error(w, "node unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*") // dev: vite on another port
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
