package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

// eventHub relays syncthing's per-node event streams to browsers over SSE.
//
// It is ON-DEMAND: node long-polls exist only while at least one browser is
// subscribed. The first subscriber starts one poller goroutine per node; the
// last to leave cancels them. This keeps idle load off the fleet (the phone and
// the rpi in particular) — nothing polls when no dashboard is open.
type eventHub struct {
	cfg *config.Config

	mu      sync.Mutex
	subs    map[chan sseEvent]struct{}
	cancel  context.CancelFunc // stops the poller group; nil when idle
	pollers sync.WaitGroup
}

// sseEvent is the normalized shape we push to the browser — only the fields the
// mesh view needs, flattened out of syncthing's per-type event payloads.
type sseEvent struct {
	Node   string `json:"node"`             // managed node the event came from
	Type   string `json:"type"`             // ItemStarted|ItemFinished|StateChanged|FolderErrors
	Folder string `json:"folder,omitempty"` // folder id
	Item   string `json:"item,omitempty"`   // file path within the folder
	Action string `json:"action,omitempty"` // update|delete|metadata
	State  string `json:"state,omitempty"`  // for StateChanged: the new state
	Error  string `json:"error,omitempty"`  // first error message, if any
	Time   string `json:"time,omitempty"`
}

func newEventHub(cfg *config.Config) *eventHub {
	return &eventHub{cfg: cfg, subs: map[chan sseEvent]struct{}{}}
}

// handleEvents streams normalized node events to one browser as Server-Sent
// Events until the client disconnects.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	// An initial comment flushes headers so the browser's EventSource opens.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Heartbeat keeps proxies from closing an idle stream.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// subscribe registers a browser and, if it is the first, starts the pollers.
func (h *eventHub) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	if h.cancel == nil { // first subscriber: spin up the fleet pollers
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		for _, n := range h.cfg.Nodes {
			h.pollers.Add(1)
			go func(n config.Node) {
				defer h.pollers.Done()
				h.pollNode(ctx, n)
			}(n)
		}
	}
	return ch
}

// unsubscribe removes a browser and, if it was the last, stops the pollers.
func (h *eventHub) unsubscribe(ch chan sseEvent) {
	h.mu.Lock()
	cancel := (context.CancelFunc)(nil)
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	if len(h.subs) == 0 && h.cancel != nil {
		cancel = h.cancel
		h.cancel = nil
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
		h.pollers.Wait() // let the pollers exit before they could be restarted
	}
}

// broadcast fans an event out to every subscriber, dropping to any that has
// fallen behind rather than stalling the poller.
func (h *eventHub) broadcast(ev sseEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // slow consumer: drop this event for them
		}
	}
}

// pollNode long-polls one node's /rest/events and broadcasts the interesting
// ones. It primes `since` to the current head so a freshly-opened dashboard
// doesn't replay the whole event backlog.
func (h *eventHub) pollNode(ctx context.Context, n config.Node) {
	// A long-poll client: its timeout must exceed the syncthing poll timeout.
	client := &http.Client{Timeout: 40 * time.Second}
	const wantEvents = "ItemStarted,ItemFinished,StateChanged,FolderErrors"

	since := 0
	if id, ok := latestEventID(ctx, client, n, wantEvents); ok {
		since = id
	}
	for {
		if ctx.Err() != nil {
			return
		}
		evs, err := fetchEvents(ctx, client, n, since, wantEvents, 25, 0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Node hiccup: back off, keep `since`, retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, e := range evs {
			if e.ID > since {
				since = e.ID
			}
			if se, ok := normalizeEvent(n.Name, e); ok {
				h.broadcast(se)
			}
		}
	}
}

type rawEvent struct {
	ID   int             `json:"id"`
	Type string          `json:"type"`
	Time string          `json:"time"`
	Data json.RawMessage `json:"data"`
}

// latestEventID grabs the id of the most recent event so polling can start from
// "now" instead of replaying syncthing's whole ring buffer.
//
// It MUST prime with the same events filter it will later poll with: syncthing
// gives each distinct `events=` filter its own event subscription with its own
// id sequence (a filtered stream's head might be id 150 while the global stream
// is at 41883). Priming off the unfiltered stream yields an id from the wrong
// sequence, so the filtered poll never matches and no events ever arrive.
func latestEventID(ctx context.Context, c *http.Client, n config.Node, events string) (int, bool) {
	evs, err := fetchEvents(ctx, c, n, 0, events, 1, 1)
	if err != nil || len(evs) == 0 {
		return 0, false
	}
	return evs[len(evs)-1].ID, true
}

func fetchEvents(ctx context.Context, c *http.Client, n config.Node, since int, events string, timeoutS, limit int) ([]rawEvent, error) {
	q := url.Values{
		"since":   {strconv.Itoa(since)},
		"timeout": {strconv.Itoa(timeoutS)},
	}
	if events != "" {
		q.Set("events", events)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	target := trimSlash(n.URL) + "/rest/events?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", n.APIKey)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("events -> %s", resp.Status)
	}
	var out []rawEvent
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeEvent flattens a syncthing event into our sseEvent. ok=false for an
// event we don't forward.
func normalizeEvent(node string, e rawEvent) (sseEvent, bool) {
	se := sseEvent{Node: node, Type: e.Type, Time: e.Time}
	switch e.Type {
	case "ItemStarted", "ItemFinished":
		var d struct {
			Folder string `json:"folder"`
			Item   string `json:"item"`
			Action string `json:"action"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return se, false
		}
		se.Folder, se.Item, se.Action, se.Error = d.Folder, d.Item, d.Action, d.Error
		return se, true
	case "StateChanged":
		var d struct {
			Folder string `json:"folder"`
			To     string `json:"to"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return se, false
		}
		se.Folder, se.State = d.Folder, d.To
		return se, true
	case "FolderErrors":
		var d struct {
			Folder string `json:"folder"`
			Errors []struct {
				Path  string `json:"path"`
				Error string `json:"error"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return se, false
		}
		se.Folder = d.Folder
		if len(d.Errors) > 0 {
			se.Item = d.Errors[0].Path
			se.Error = d.Errors[0].Error
		}
		return se, true
	}
	return se, false
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
