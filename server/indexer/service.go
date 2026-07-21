package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Service ties a Source to the store: it runs the initial index, applies live
// changes as the Source reports them, and fans those changes out to subscribers
// (the SSE feed live search results ride). It is Source-agnostic — the same code
// drives the portable backend today and the native ones later.
type Service struct {
	store *Store
	src   Source
	log   func(string)

	mu    sync.Mutex
	roots map[string]*VolumeInfo // volumeID → coverage

	subMu sync.Mutex
	subs  map[chan Change]struct{}
}

func NewService(store *Store, src Source) *Service {
	return &Service{
		store: store,
		src:   src,
		log:   func(string) {},
		roots: map[string]*VolumeInfo{},
		subs:  map[chan Change]struct{}{},
	}
}

// SetLogger installs a log sink for background (non-request) diagnostics.
func (s *Service) SetLogger(log func(string)) {
	if log != nil {
		s.log = log
	}
}

// Start indexes the roots and, if the Source is realtime, watches them for changes.
// Watches are seeded before the initial walk so a change during indexing isn't
// missed. Returns once wiring is up; the background work continues under ctx.
func (s *Service) Start(ctx context.Context, roots []string) error {
	if s.src.Caps().Realtime {
		ch, err := s.src.Watch(ctx, roots)
		if err != nil {
			s.log("watch setup failed (index will be static): " + err.Error())
		} else {
			go s.consume(ctx, ch)
		}
	}
	for _, root := range roots {
		go s.indexRoot(ctx, root)
	}
	return nil
}

// IndexRoot performs a full walk of root into the index (idempotent). Exposed for
// direct/forced rescans and tests; Start calls the same path.
func (s *Service) IndexRoot(root string) error { return s.indexRoot(context.Background(), root) }

func (s *Service) indexRoot(ctx context.Context, root string) error {
	vid := volumeID(root)
	s.setVolume(vid, root, "building")

	batch, err := s.store.NewBatch()
	if err != nil {
		return err
	}
	n, walkErr := s.src.Walk(ctx, root, func(e Entry) error { return batch.Add(e) })
	if commitErr := batch.Commit(); commitErr != nil && walkErr == nil {
		walkErr = commitErr
	}
	state := "ready"
	if walkErr != nil {
		state = "degraded"
	}
	s.mu.Lock()
	if v := s.roots[vid]; v != nil {
		v.State = state
		v.LastScan = time.Now()
		v.IndexedFiles = n
	}
	s.mu.Unlock()
	s.store.recordVolume(vid, root, state)
	return walkErr
}

func (s *Service) setVolume(vid, root, state string) {
	s.mu.Lock()
	s.roots[vid] = &VolumeInfo{VolumeID: vid, Root: root, State: state}
	s.mu.Unlock()
}

// consume applies each live change to the store and fans it out to subscribers.
func (s *Service) consume(ctx context.Context, ch <-chan Change) {
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-ch:
			if !ok {
				return
			}
			s.applyChange(c)
			s.fanout(c)
		}
	}
}

func (s *Service) applyChange(c Change) {
	var err error
	switch c.Op {
	case ChangeAdded, ChangeModified:
		err = s.store.Upsert(c.Entry)
	case ChangeRemoved:
		_, err = s.store.DeleteSubtree(c.Entry.Path)
	}
	if err != nil {
		s.log("apply " + c.Op.String() + " " + c.Entry.Path + ": " + err.Error())
	}
}

// Subscribe registers a change subscriber. The returned cancel func removes and
// closes the channel; call it when the subscriber goes away (e.g. SSE disconnect).
func (s *Service) Subscribe() (<-chan Change, func()) {
	ch := make(chan Change, 64)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.subMu.Lock()
			delete(s.subs, ch)
			close(ch)
			s.subMu.Unlock()
		})
	}
}

// fanout delivers a change to every subscriber, dropping it for any whose buffer is
// full (a slow SSE client misses deltas and can re-query — never blocks the indexer).
func (s *Service) fanout(c Change) {
	s.subMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- c:
		default:
		}
	}
	s.subMu.Unlock()
}

func (s *Service) Search(q Query) (ResultPage, error) { return s.store.Search(q) }

func (s *Service) Status() (Status, error) {
	count, err := s.store.fileCount()
	if err != nil {
		return Status{}, err
	}
	s.mu.Lock()
	vols := make([]VolumeInfo, 0, len(s.roots))
	overall := "ready"
	for _, v := range s.roots {
		vols = append(vols, *v)
		if v.State == "building" {
			overall = "building"
		} else if v.State == "degraded" && overall != "building" {
			overall = "degraded"
		}
	}
	s.mu.Unlock()
	return Status{State: overall, FileCount: count, DBSizeByte: s.store.dbSizeBytes(), Volumes: vols}, nil
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

// Handler returns the indexer's local HTTP surface. Core proxies these under
// /_api/v1/; nothing here is exposed to the app directly.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /subscribe", s.handleSubscribe)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	return mux
}

func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	page, err := s.Search(parseQuery(r))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, page)
}

func (s *Service) handleStatus(w http.ResponseWriter, _ *http.Request) {
	st, err := s.Status()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, st)
}

// handleSubscribe streams live index changes as Server-Sent Events. The consumer
// (the Search UI) live-updates open result sets from these deltas.
func (s *Service) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := s.Subscribe()
	defer cancel()

	// A heartbeat keeps intermediaries from timing the idle stream out.
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case c, ok := <-ch:
			if !ok {
				return
			}
			w.Write([]byte("data: "))
			enc.Encode(c) // Encode appends a newline
			w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

func parseQuery(r *http.Request) Query {
	v := r.URL.Query()
	q := Query{
		Text:      v.Get("q"),
		Scope:     v.Get("scope"),
		Match:     MatchMode(v.Get("match")),
		Sort:      SortField(v.Get("sort")),
		Desc:      v.Get("desc") == "1" || v.Get("desc") == "true",
		DirsOnly:  v.Get("dirsOnly") == "1",
		FilesOnly: v.Get("filesOnly") == "1",
		Limit:     atoi(v.Get("limit")),
		Offset:    atoi(v.Get("offset")),
		MinSize:   atoi64(v.Get("minSize")),
		MaxSize:   atoi64(v.Get("maxSize")),
	}
	if t := v.Get("type"); t != "" {
		q.Types = strings.Split(t, ",")
	}
	return q
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func atoi(s string) int     { n, _ := strconv.Atoi(s); return n }
func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
