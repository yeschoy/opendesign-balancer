package server

// The import log: one record per import call — the open endpoint once its
// import key checked out, and the console's batch import — with the
// caller's IP, the time and how many accounts it carried. It lives in
// DATA_DIR/import-log.json, newest last, capped at maxImportLog records.
// Failed authentications are not recorded (the rate limiter handles them),
// so nobody without a key can fill the log.

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

const maxImportLog = 1000

type importRecord struct {
	ID          string    `json:"id"`
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

// add appends rec, dropping the oldest past maxImportLog. A failed write is
// only logged: the import itself already happened.
func (l *importLog) add(rec importRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, rec)
	if n := len(l.items) - maxImportLog; n > 0 {
		l.items = append([]importRecord(nil), l.items[n:]...)
	}
	if err := fsutil.WriteJSONCompactAtomic(l.path, importLogFile{Version: 1, Items: l.items}); err != nil {
		log.Printf("import log: save: %v", err)
	}
}

// recent returns up to limit records, newest first.
func (l *importLog) recent(limit int) []importRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]importRecord, 0, min(limit, len(l.items)))
	for i := len(l.items) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, l.items[i])
	}
	return out
}

// recordImport stamps rec with an id, the time and the caller's address and
// appends it to the import log.
func (s *Server) recordImport(r *http.Request, rec importRecord) {
	rec.ID = config.NewID("imp_")
	rec.At = time.Now().UTC()
	rec.IP, rec.Peer = requestIP(r)
	s.imports.add(rec)
}

func (s *Server) apiImportLog(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, maxImportLog)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.imports.recent(limit)})
}
