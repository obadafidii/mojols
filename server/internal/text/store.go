package text

import (
	"fmt"
	"sort"
	"sync"
)

// Document is an open text document as the editor sees it.
type Document struct {
	URI        string
	LanguageID string
	Version    int32
	Text       string
}

// Store holds every document the editor has open. It is the source of truth
// mojols replays into a fresh backend after a restart, so it must track the
// editor exactly. It is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	enc  Encoding
	docs map[string]*Document
}

// NewStore returns an empty store using UTF-16 positions.
func NewStore() *Store { return &Store{docs: make(map[string]*Document)} }

// SetEncoding sets the negotiated position encoding.
func (s *Store) SetEncoding(enc Encoding) {
	s.mu.Lock()
	s.enc = enc
	s.mu.Unlock()
}

// Encoding returns the negotiated position encoding.
func (s *Store) Encoding() Encoding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enc
}

// Open records a didOpen. Reopening an open document replaces it.
func (s *Store) Open(d Document) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := d
	s.docs[d.URI] = &cp
}

// Change applies a didChange. On error the document is left unchanged.
func (s *Store) Change(uri string, version int32, changes []ContentChange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[uri]
	if !ok {
		return fmt.Errorf("didChange for document that is not open: %s", uri)
	}
	next, err := Apply(d.Text, changes, s.enc)
	if err != nil {
		return fmt.Errorf("%s: %w", uri, err)
	}
	d.Text = next
	d.Version = version
	return nil
}

// Close records a didClose.
func (s *Store) Close(uri string) {
	s.mu.Lock()
	delete(s.docs, uri)
	s.mu.Unlock()
}

// Get returns a copy of one document.
func (s *Store) Get(uri string) (Document, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.docs[uri]
	if !ok {
		return Document{}, false
	}
	return *d, true
}

// Snapshot returns copies of all open documents, sorted by URI.
func (s *Store) Snapshot() []Document {
	s.mu.RLock()
	out := make([]Document, 0, len(s.docs))
	for _, d := range s.docs {
		out = append(out, *d)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

// Len returns the number of open documents.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}
