package server

// The import log: one record per import call — the open endpoint once its
// import key checked out, and the console's batch import — with the
// caller's IP, the time and how many accounts it carried. It lives in
// DATA_DIR/import-log.json, newest last, capped at maxImportLog records.
// Failed authentications are not recorded (the rate limiter handles them),
// so nobody without a key can fill the log.
//
// Every record carries the pool (provider) it imported into, the console
// shows one pool at a time, and the cap is applied per pool: a busy River or
// Ollama pool must not push another pool's records out of the log.

import (
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/fsutil"
)

// maxImportLog is how many records ONE pool keeps: the console reads a pool
// at a time, so each pool's history stays as deep as it was when the log was
// one shared list.
const maxImportLog = 1000

type importRecord struct {
	ID string `json:"id"`
	// Provider is the pool the accounts joined (opendesign | river | ollama |
	// modal),
	// so each pool's console tab lists only its own imports. It is empty on a
	// record written before pools had their own tabs.
	Provider    string    `json:"provider,omitempty"`
	At          time.Time `json:"at"`
	IP          string    `json:"ip"`
	Peer        string    `json:"peer,omitempty"` // the TCP peer, when IP came from a proxy header
	Source      string    `json:"source"`         // open | console
	ImportKey   string    `json:"importKey,omitempty"`
	ImportKeyID string    `json:"importKeyId,omitempty"`
	Group       string    `json:"group,omitempty"` // console: the group it imported into
	Total       int       `json:"total"`
	Added       int       `json:"added"`
	Staged      int       `json:"staged"`
	Unchanged   int       `json:"unchanged"`
	Failed      int       `json:"failed"`
	Status      int       `json:"status"` // the HTTP status answered
	Error       string    `json:"error,omitempty"`
}

type importLogFile struct {
	Version int            `json:"version"`
	Items   []importRecord `json:"items"`
}

type importLog struct {
	mu    sync.Mutex
	path  string
	items []importRecord
}

func importLogPath(dataDir string) string { return filepath.Join(dataDir, "import-log.json") }

func loadImportLog(path string) (*importLog, error) {
	l := &importLog{path: path}
	var f importLogFile
	switch err := fsutil.ReadJSON(path, &f); {
	case errors.Is(err, fsutil.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		l.items = f.Items
	}
	return l, nil
}

// add appends rec, dropping that pool's oldest past maxImportLog (another
// pool's records are never touched). A failed write is only logged: the
// import itself already happened.
func (l *importLog) add(rec importRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, rec)
	n := 0
	for _, it := range l.items {
		if it.Provider == rec.Provider {
			n++
		}
	}
	if drop := n - maxImportLog; drop > 0 {
		kept := make([]importRecord, 0, len(l.items)-drop)
		for _, it := range l.items {
			if drop > 0 && it.Provider == rec.Provider {
				drop--
				continue
			}
			kept = append(kept, it)
		}
		l.items = kept
	}
	if err := fsutil.WriteJSONCompactAtomic(l.path, importLogFile{Version: 1, Items: l.items}); err != nil {
		log.Printf("import log: save: %v", err)
	}
}

// recent returns up to limit records of one pool, newest first. provider ""
// returns every pool's records.
func (l *importLog) recent(limit int, provider string) []importRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]importRecord, 0, min(limit, len(l.items)))
	for i := len(l.items) - 1; i >= 0 && len(out) < limit; i-- {
		if provider != "" && l.items[i].Provider != provider {
			continue
		}
		out = append(out, l.items[i])
	}
	return out
}

// recordImport stamps rec with an id, the time, the caller's pool and address
// and appends it to the import log. An empty rec.Provider is settled from the
// record's group, so a caller that only knows its group still files the
// record under the right pool.
func (s *Server) recordImport(r *http.Request, rec importRecord) {
	rec.ID = config.NewID("imp_")
	rec.At = time.Now().UTC()
	rec.IP, rec.Peer = requestIP(r)
	if rec.Provider == "" {
		rec.Provider = providerOf(s.cfg.Get().GroupPool(rec.Group))
	}
	s.imports.add(rec)
}

func (s *Server) apiImportLog(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, maxImportLog)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.imports.recent(limit, providerParam(r))})
}
