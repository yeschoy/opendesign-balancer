// Package logstore is the JSONL request and error log: segment files under
// DATA_DIR/logs (5 MiB each, 0600) plus an in-memory ring of the newest
// records for queries. Writes go through one async queue and fail open: a
// record that can't be written is dropped and counted, model traffic never
// waits for the disk.
package logstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	segmentBytes = 5 << 20
	maxRecord    = 64 << 10
	// detail records carry bodies, so they may be larger
	maxDetailRecord = 32 << 20
	// the details ring holds whole bodies in memory: bound it by size, not just count
	detailRingBytes = 256 << 20
	// queries read the in-memory ring, so the files may trail it this long
	flushEvery = time.Second
)

type Limits struct {
	RetentionDays int
	MaxRecords    map[string]int
	MaxTotalBytes int64
}

type record struct {
	seq  int64
	ts   time.Time
	data json.RawMessage
	keys map[string]string
}

type kind struct {
	name    string
	filters []string
	mu      sync.RWMutex
	ring    []record
	head    int // next write position once full
	seq     int64
	file    *os.File
	w       *bufio.Writer // over file; flushed every flushEvery
	size    int64
}

func (k *kind) push(r record, max int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seq++
	r.seq = k.seq
	if len(k.ring) > max {
		// the limit shrank: keep the newest max
		k.ring = k.ordered()[len(k.ring)-max:]
		k.head = 0
	}
	if len(k.ring) < max {
		k.ring = append(k.ring, r)
	} else {
		k.ring[k.head] = r
		k.head = (k.head + 1) % max
	}
	if k.name == "details" {
		k.trimBytes(detailRingBytes)
	}
}

// trimBytes drops the oldest records while the ring holds more than budget
// bytes, always keeping the newest (caller holds the lock).
func (k *kind) trimBytes(budget int) {
	total := 0
	for i := range k.ring {
		total += len(k.ring[i].data)
	}
	if total <= budget {
		return
	}
	all := k.ordered()
	drop := 0
	for total > budget && drop < len(all)-1 {
		total -= len(all[drop].data)
		drop++
	}
	k.ring, k.head = append([]record(nil), all[drop:]...), 0
}

// ordered is the ring oldest first (caller holds the lock).
func (k *kind) ordered() []record {
	if k.head == 0 {
		return append([]record(nil), k.ring...)
	}
	return append(append([]record(nil), k.ring[k.head:]...), k.ring[:k.head]...)
}

type job struct {
	k     *kind
	line  []byte
	clear bool
	done  chan struct{}
}

type Store struct {
	dir     string
	limits  func() Limits
	kinds   map[string]*kind
	ch      chan job
	dropped atomic.Int64
	ready   atomic.Bool
	closed  chan struct{}
}

// Filters are the query parameters each kind accepts besides limit/cursor/since/until.
var Filters = map[string][]string{
	"requests": {"requestId", "accountId", "status", "result", "model", "errorCategory", "bindingResult", "clientKeyId", "stream", "protocol", "affinityKeyType", "affinityConfidence"},
	"errors":   {"requestId", "accountId", "status", "category", "ruleId", "model", "clientKeyId"},
	"details":  {"requestId", "accountId", "status", "model", "protocol", "clientKeyId"},
}

func recordLimit(kindName string) int {
	if kindName == "details" {
		return maxDetailRecord
	}
	return maxRecord
}

func Open(dir string, limits func() Limits) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, limits: limits, kinds: map[string]*kind{}, ch: make(chan job, 8192), closed: make(chan struct{})}
	for name, f := range Filters {
		s.kinds[name] = &kind{name: name, filters: f}
	}
	go s.writer()
	go s.recover()
	return s, nil
}

func (s *Store) Dropped() int64 { return s.dropped.Load() }
func (s *Store) Ready() bool    { return s.ready.Load() }

func keyString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

func extractKeys(data []byte, filters []string) (map[string]string, time.Time, bool) {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil, time.Time{}, false
	}
	keys := map[string]string{}
	for _, f := range filters {
		if s, ok := keyString(m[f]); ok {
			keys[f] = s
		}
	}
	ts, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(m["ts"]))
	return keys, ts, true
}

// Keyed is a record that gives its filter keys itself, so Append doesn't
// decode what it just encoded. The keys must equal what extractKeys reads.
type Keyed interface {
	LogKeys() map[string]string
}

// Append queues rec (already a strict projection) for kind.
func (s *Store) Append(kindName string, ts time.Time, rec any) {
	k := s.kinds[kindName]
	if k == nil {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil || len(data) > recordLimit(kindName) {
		s.dropped.Add(1)
		return
	}
	var keys map[string]string
	if kr, ok := rec.(Keyed); ok {
		keys = kr.LogKeys()
	} else {
		keys, _, _ = extractKeys(data, k.filters)
	}
	lim := s.limits()
	k.push(record{ts: ts, data: data, keys: keys}, max(lim.MaxRecords[kindName], 1))
	select {
	case s.ch <- job{k: k, line: append(data, '\n')}:
	default:
		s.dropped.Add(1)
	}
}

func (s *Store) segmentName(k *kind) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s-%s-%d.jsonl", k.name, time.Now().UTC().Format("20060102-150405"), time.Now().UnixNano()%1e6))
}

func (s *Store) writer() {
	enforce := time.NewTicker(10 * time.Minute)
	defer enforce.Stop()
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	for {
		select {
		case j, ok := <-s.ch:
			if !ok {
				for _, k := range s.kinds {
					s.closeFile(k)
				}
				close(s.closed)
				return
			}
			if j.clear {
				s.clearFiles(j.k)
				close(j.done)
				continue
			}
			if err := s.write(j.k, j.line); err != nil {
				s.dropped.Add(1)
			}
		case <-enforce.C:
			s.enforce()
		case <-flush.C:
			for _, k := range s.kinds {
				if k.w != nil && k.w.Buffered() > 0 && k.w.Flush() != nil {
					s.dropped.Add(1)
				}
			}
		}
	}
}

// closeFile flushes and closes k's current segment (writer goroutine).
func (s *Store) closeFile(k *kind) {
	if k.file == nil {
		return
	}
	if k.w.Flush() != nil {
		s.dropped.Add(1)
	}
	k.file.Close()
	k.file, k.w = nil, nil
}

func (s *Store) write(k *kind, line []byte) error {
	if k.file != nil && k.size+int64(len(line)) > segmentBytes {
		s.closeFile(k)
		s.enforce()
	}
	if k.file == nil {
		f, err := os.OpenFile(s.segmentName(k), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		k.file, k.w, k.size = f, bufio.NewWriterSize(f, 64<<10), 0
	}
	n, err := k.w.Write(line)
	k.size += int64(n)
	return err
}

type segFile struct {
	path string
	kind string
	mod  time.Time
	size int64
}

func (s *Store) segments() []segFile {
	entries, _ := os.ReadDir(s.dir)
	var out []segFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		kindName, _, ok := strings.Cut(name, "-")
		if _, known := s.kinds[kindName]; !ok || !known {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, segFile{path: filepath.Join(s.dir, name), kind: kindName, mod: info.ModTime(), size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func (s *Store) isCurrent(path string) bool {
	for _, k := range s.kinds {
		if k.file != nil && k.file.Name() == path {
			return true
		}
	}
	return false
}

// enforce deletes segments past retention, then the oldest while the total
// is over the cap (runs on the writer goroutine).
func (s *Store) enforce() {
	lim := s.limits()
	segs := s.segments()
	cut := time.Now().Add(-time.Duration(lim.RetentionDays) * 24 * time.Hour)
	var total int64
	var keep []segFile
	for _, f := range segs {
		if f.mod.Before(cut) && !s.isCurrent(f.path) {
			os.Remove(f.path)
			continue
		}
		total += f.size
		keep = append(keep, f)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].mod.Before(keep[j].mod) })
	// detail segments are large: keep them to half the cap so they can't push the request log out
	var detailTotal int64
	for _, f := range keep {
		if f.kind == "details" {
			detailTotal += f.size
		}
	}
	for i, f := range keep {
		if detailTotal <= lim.MaxTotalBytes/2 {
			break
		}
		if f.kind == "details" && !s.isCurrent(f.path) {
			os.Remove(f.path)
			detailTotal -= f.size
			total -= f.size
			keep[i].path = ""
		}
	}
	for _, f := range keep {
		if total <= lim.MaxTotalBytes {
			break
		}
		if f.path == "" || s.isCurrent(f.path) {
			continue
		}
		os.Remove(f.path)
		total -= f.size
	}
}

func (s *Store) clearFiles(k *kind) {
	s.closeFile(k)
	for _, f := range s.segments() {
		if f.kind == k.name {
			os.Remove(f.path)
		}
	}
}

// recover loads the newest records of each kind from disk in the
// background; queries answer 503 until it is done.
func (s *Store) recover() {
	defer s.ready.Store(true)
	lim := s.limits()
	segs := s.segments()
	for name, k := range s.kinds {
		max := max(lim.MaxRecords[name], 1)
		var loaded []record
		// newest segments first until enough
		for i := len(segs) - 1; i >= 0 && len(loaded) < max; i-- {
			if segs[i].kind != name {
				continue
			}
			var recs []record
			f, err := os.Open(segs[i].path)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64<<10), recordLimit(name)+1024)
			for sc.Scan() {
				line := append([]byte(nil), sc.Bytes()...)
				keys, ts, ok := extractKeys(line, k.filters)
				if ok {
					recs = append(recs, record{ts: ts, data: line, keys: keys})
				}
			}
			f.Close()
			loaded = append(recs, loaded...)
		}
		if len(loaded) > max {
			loaded = loaded[len(loaded)-max:]
		}
		k.mu.Lock()
		// records appended while recovering are newer: keep them after
		live := k.ordered()
		all := append(loaded, live...)
		if len(all) > max {
			all = all[len(all)-max:]
		}
		k.ring, k.head = nil, 0
		for i := range all {
			all[i].seq = int64(i + 1)
		}
		k.ring = all
		k.seq = int64(len(all))
		if len(k.ring) == max {
			k.head = 0
		}
		k.mu.Unlock()
	}
}

var ErrNotReady = errors.New("logs are still being recovered")

type Page struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// Query returns kind's records newest first. filters were validated by the caller.
func (s *Store) Query(kindName string, filters map[string]string, since, until time.Time, limit int, cursor int64) (Page, error) {
	if !s.ready.Load() {
		return Page{}, ErrNotReady
	}
	k := s.kinds[kindName]
	if k == nil {
		return Page{}, fmt.Errorf("unknown log kind")
	}
	page := Page{Items: []json.RawMessage{}}
	var lastSeq int64
	// walk the ring newest first under the read lock: records are immutable,
	// so nothing is copied (the ring can hold 50k)
	k.mu.RLock()
	defer k.mu.RUnlock()
	n := len(k.ring)
	for i := n - 1; i >= 0; i-- {
		r := &k.ring[(k.head+i)%n]
		if cursor > 0 && r.seq >= cursor {
			continue
		}
		if !since.IsZero() && r.ts.Before(since) {
			continue
		}
		if !until.IsZero() && r.ts.After(until) {
			continue
		}
		ok := true
		for f, v := range filters {
			if r.keys[f] != v {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if len(page.Items) == limit {
			page.NextCursor = strconv.FormatInt(lastSeq, 10)
			break
		}
		page.Items = append(page.Items, r.data)
		lastSeq = r.seq
	}
	return page, nil
}

// Clear empties one kind, memory and files.
func (s *Store) Clear(kindName string) error {
	k := s.kinds[kindName]
	if k == nil {
		return fmt.Errorf("unknown log kind")
	}
	k.mu.Lock()
	k.ring, k.head = nil, 0
	k.mu.Unlock()
	done := make(chan struct{})
	s.ch <- job{k: k, clear: true, done: done}
	<-done
	return nil
}

// Close flushes the queue and closes files.
func (s *Store) Close() {
	close(s.ch)
	<-s.closed
}
