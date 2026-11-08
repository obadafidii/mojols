package proxy

// Crash attribution and quarantine.
//
// A file that crashes mojo-lsp-server would crash every restarted backend
// too, because mojols replays it, and drag the whole session into degraded
// mode. That is exactly what a stdlib file from a mismatched checkout did on
// a real toolchain: five segfaults, then no features for any file.
//
// So mojols tracks which documents the backend has finished with: it
// published diagnostics for them since their last change. When the backend
// dies, every document it had not finished is a suspect. After the restart,
// suspects are reopened one at a time. A crash while exactly one suspect is
// open convicts that file: it is kept away from the backend, requests on it
// fail fast, and every other file keeps full features. Saving different
// content gives it another chance.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"slices"
	"sort"
	"time"

	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
	"github.com/tdadadavid/mojols/server/internal/text"
)

// docHealth tracks one document's latest text in the current backend.
type docHealth struct {
	gen       int  // generation the latest text was sent to
	diagnosed bool // the backend published diagnostics for it since then
}

type quarantined struct {
	hash  string // content that crashed the backend
	since time.Time
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

func docURIOf(params json.RawMessage) string {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return p.TextDocument.URI
}

// blockedLocked reports whether uri is kept away from the backend.
func (s *Server) blockedLocked(uri string) (isQuarantined, onProbation bool) {
	if uri == "" {
		return false, false
	}
	_, isQuarantined = s.quarantine[uri]
	onProbation = s.probing == uri || slices.Contains(s.probeQueue, uri)
	return
}

// sendDidOpenLocked opens a document's current text in the backend.
func (s *Server) sendDidOpenLocked(d text.Document) {
	n, _ := jsonrpc.NewNotification("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": d.URI, "languageId": d.LanguageID, "version": d.Version, "text": d.Text},
	})
	s.sendBackendLocked(n)
	s.health[d.URI] = &docHealth{gen: s.be.gen}
	s.resident[d.URI] = time.Now()
}

// onDiagnosedLocked records that the backend finished with uri.
func (s *Server) onDiagnosedLocked(gen int, uri string) {
	if h := s.health[uri]; h != nil && h.gen == gen {
		h.diagnosed = true
	}
	if s.probing == uri && s.be != nil && s.be.gen == gen {
		s.log.Info("suspect document parsed cleanly", "uri", uri)
		s.endProbeLocked()
	}
}

func (s *Server) endProbeLocked() {
	s.probing = ""
	if s.probeTimer != nil {
		s.probeTimer.Stop()
		s.probeTimer = nil
	}
	s.startNextProbeLocked()
}

// startNextProbeLocked opens the next suspect alone.
func (s *Server) startNextProbeLocked() {
	if !s.ready || s.be == nil || s.probing != "" {
		return
	}
	for len(s.probeQueue) > 0 {
		uri := s.probeQueue[0]
		s.probeQueue = s.probeQueue[1:]
		d, ok := s.docs.Get(uri)
		if !ok {
			continue // closed while waiting
		}
		s.log.Info("probing suspect document alone", "uri", uri)
		s.sendDidOpenLocked(d)
		s.probing = uri
		gen := s.be.gen
		s.probeTimer = time.AfterFunc(s.opts.ProbeTimeout, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.probing == uri && s.be != nil && s.be.gen == gen {
				// No diagnostics, but no crash either: let it through.
				s.log.Warn("suspect document produced no diagnostics in time; releasing it", "uri", uri)
				s.endProbeLocked()
			}
		})
		return
	}
}

// attributeCrashLocked runs when generation gen dies. It convicts the
// document being probed, if any, and queues the remaining suspects for
// one-by-one probing after the restart. It returns the convicted URI.
func (s *Server) attributeCrashLocked(gen int) string {
	convicted := ""
	if s.probing != "" {
		convicted = s.probing
		if d, ok := s.docs.Get(convicted); ok {
			s.quarantine[convicted] = &quarantined{hash: contentHash(d.Text), since: time.Now()}
		}
		s.probing = ""
		if s.probeTimer != nil {
			s.probeTimer.Stop()
			s.probeTimer = nil
		}
	}
	suspects := map[string]bool{}
	for _, uri := range s.probeQueue {
		suspects[uri] = true
	}
	for uri, h := range s.health {
		if h.gen == gen && !h.diagnosed && uri != convicted {
			suspects[uri] = true
		}
	}
	s.probeQueue = s.probeQueue[:0]
	for uri := range suspects {
		if _, open := s.docs.Get(uri); open {
			s.probeQueue = append(s.probeQueue, uri)
		}
	}
	sort.Strings(s.probeQueue)
	s.health = map[string]*docHealth{}
	if len(s.probeQueue) > 0 {
		s.log.Info("crash suspects will be reopened one at a time", "suspects", s.probeQueue)
	}
	return convicted
}

// releaseLocked gives a quarantined document another chance.
func (s *Server) releaseLocked(uri string) {
	delete(s.quarantine, uri)
	go s.write(quarantineDiagnostics(uri, false))
	if !slices.Contains(s.probeQueue, uri) {
		s.probeQueue = append(s.probeQueue, uri)
	}
	s.log.Info("quarantined document changed; probing it again", "uri", uri)
	s.startNextProbeLocked()
}

// handleBlockedSyncLocked applies quarantine rules to a document sync
// notification. It returns true if the message must not reach the backend.
func (s *Server) handleBlockedSyncLocked(method, uri string) bool {
	q, isQuarantined := s.quarantine[uri]
	switch method {
	case "textDocument/didOpen":
		if isQuarantined {
			if d, ok := s.docs.Get(uri); ok && contentHash(d.Text) != q.hash {
				s.releaseLocked(uri) // reopened with different content
			} else {
				go s.write(quarantineDiagnostics(uri, true))
			}
			return true
		}
	case "textDocument/didSave":
		if isQuarantined {
			if d, ok := s.docs.Get(uri); ok && contentHash(d.Text) != q.hash {
				s.releaseLocked(uri)
			}
			return true
		}
	case "textDocument/didClose":
		if isQuarantined {
			return true // the backend never had it; keep the verdict for a reopen
		}
		if i := slices.Index(s.probeQueue, uri); i >= 0 {
			s.probeQueue = slices.Delete(s.probeQueue, i, i+1)
			return true
		}
		if s.probing == uri {
			s.sendDidCloseLocked(uri)
			s.endProbeLocked()
			return true
		}
		return false
	}
	switch {
	case isQuarantined:
		return true
	case s.probing == uri:
		return false // the backend has it open: keep it in sync
	default:
		return slices.Contains(s.probeQueue, uri)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func fileName(uri string) string { return path.Base(uri) }

// quarantineDiagnostics tells the user, in the file itself, why it has no
// language features. An empty list clears it.
func quarantineDiagnostics(uri string, on bool) *jsonrpc.Message {
	diags := []any{}
	if on {
		diags = append(diags, map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 0}},
			"severity": 2,
			"source":   "mojols",
			"message":  "mojo-lsp-server crashes on this file, so mojols has turned language features off for it. Other files are unaffected. Save a change to retry.",
		})
	}
	n, _ := jsonrpc.NewNotification("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": diags})
	return n
}
