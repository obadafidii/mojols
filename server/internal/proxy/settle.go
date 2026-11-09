package proxy

// Request settling.
//
// The official server re-parses a document about 150 ms after an edit by
// replacing the whole document object, while hover, completion and
// definition resolve function bodies lazily at the cursor. A 1-hour soak on
// ordinary code crashed it 16 times, nearly all "resolving decl body" in the
// file being edited, and twice with a symbol of the prebuilt stdlib
// vanishing mid-lookup: the signature of a request racing the replacement.
//
// With SettleRequests, a request on a document waits until the backend has
// published diagnostics for the document's latest version (it has finished
// parsing that text), or SettleTimeout passes. Requests then never overlap
// a re-parse of their own document.

import "time"

// holdUntilSettledLocked parks p if its document has an edit the backend
// has not finished parsing. It reports whether p was parked.
func (s *Server) holdUntilSettledLocked(p *pending) bool {
	if !s.opts.SettleRequests {
		return false
	}
	uri := docURIOf(p.msg.Params)
	h := s.health[uri]
	if uri == "" || h == nil || h.diagnosed || h.gen != s.be.gen {
		return false
	}
	first := len(s.waiting[uri]) == 0
	s.waiting[uri] = append(s.waiting[uri], p)
	s.held++
	if first {
		gen := s.be.gen
		time.AfterFunc(s.opts.SettleTimeout, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.be != nil && s.be.gen == gen && len(s.waiting[uri]) > 0 {
				s.log.Debug("document did not settle in time; sending its requests anyway", "uri", uri)
				s.releaseWaitersLocked(uri)
			}
		})
	}
	return true
}

// releaseWaitersLocked dispatches the requests parked on uri, in order.
func (s *Server) releaseWaitersLocked(uri string) {
	ws := s.waiting[uri]
	if len(ws) == 0 {
		return
	}
	delete(s.waiting, uri)
	for _, p := range ws {
		if _, live := s.pending[p.id]; !live {
			continue // timed out or cancelled meanwhile
		}
		if s.ready && s.be != nil {
			s.dispatchLocked(p)
		} else {
			s.queue = append(s.queue, p)
		}
	}
}

// requeueWaitersLocked moves every parked request to the ready queue, for
// a backend that is going away: they go to the next one after its replay.
func (s *Server) requeueWaitersLocked() {
	for uri, ws := range s.waiting {
		for _, p := range ws {
			if _, live := s.pending[p.id]; live {
				s.queue = append(s.queue, p)
			}
		}
		delete(s.waiting, uri)
	}
}
