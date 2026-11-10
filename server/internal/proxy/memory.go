package proxy

// Memory: residency cap and planned recycling.
//
// Measured on Mojo 1.1.0 (scripts/memory_probe.py): every document open in
// mojo-lsp-server costs about 31 MB, because each gets its own parser
// context that re-parses the stdlib (40 open: 1,252 MB). Closing them only
// returns half (40 closed: 580 to 680 MB, stable across cycles), so the
// process keeps its high-water mark.
//
// Two mechanisms follow from that:
//
//  1. Residency: at most MaxResidentDocs documents are open in the backend.
//     Every editor document stays tracked in mojols; the least recently
//     used ones are closed in the backend and reopened, with their current
//     text, the moment a request or edit touches them. Their diagnostics in
//     the editor are kept: the backend's "cleared on close" message is
//     swallowed.
//  2. Recycling: when the backend's resident memory passes MemoryLimitMB,
//     mojols lets in-flight requests finish, stops it, and starts a fresh
//     one with only the most recently used documents. New requests wait
//     instead of failing, and it does not count as a crash.

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
)

// SampleRSS returns a process's resident memory in bytes using ps, which
// exists on macOS and Linux.
func SampleRSS(pid int) (int64, error) {
	if runtime.GOOS == "windows" {
		return 0, fmt.Errorf("memory sampling is not supported on windows")
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}

func (s *Server) residentCap() int {
	if s.opts.MaxResidentDocs < 0 {
		return int(^uint(0) >> 1)
	}
	return s.opts.MaxResidentDocs
}

// ensureResidentLocked opens uri in the backend if it is open in the editor
// but not in the backend. It reports whether uri is resident afterwards.
func (s *Server) ensureResidentLocked(uri string) bool {
	if _, ok := s.resident[uri]; ok {
		s.resident[uri] = time.Now()
		return true
	}
	if !s.ready || s.be == nil {
		return false
	}
	if q, p := s.blockedLocked(uri); q || p {
		return false
	}
	d, ok := s.docs.Get(uri)
	if !ok {
		return false
	}
	s.sendDidOpenLocked(d)
	s.evictLocked(uri)
	return true
}

// sendDidCloseLocked closes uri in the backend.
func (s *Server) sendDidCloseLocked(uri string) {
	s.sendBackendLocked(&jsonrpc.Message{Method: "textDocument/didClose", Params: mustJSON(map[string]any{"textDocument": map[string]any{"uri": uri}})})
	delete(s.resident, uri)
	delete(s.health, uri)
}

// evictLocked closes the least recently used documents in the backend until
// at most the cap remain. keep and the document being probed are never
// evicted.
func (s *Server) evictLocked(keep string) {
	limit := s.residentCap()
	for len(s.resident) > limit {
		victim := ""
		var oldest time.Time
		for uri, used := range s.resident {
			if uri == keep || uri == s.probing {
				continue
			}
			if victim == "" || used.Before(oldest) {
				victim, oldest = uri, used
			}
		}
		if victim == "" {
			return
		}
		s.sendDidCloseLocked(victim)
		s.evictions++
		s.log.Debug("closed least recently used document in the backend", "uri", victim, "resident", len(s.resident))
	}
}

// replayOrderLocked returns the open, unblocked documents most recently
// used first, and how many of them to reopen in a fresh backend.
func (s *Server) replayOrderLocked() []string {
	var uris []string
	for _, d := range s.docs.Snapshot() {
		if q, p := s.blockedLocked(d.URI); !q && !p {
			uris = append(uris, d.URI)
		}
	}
	sort.SliceStable(uris, func(i, j int) bool { return s.lastUse[uris[i]].After(s.lastUse[uris[j]]) })
	if limit := s.residentCap(); len(uris) > limit {
		uris = uris[:limit]
	}
	return uris
}

// watchMemory samples the backend's memory until stop is closed.
func (s *Server) watchMemory(stop <-chan struct{}) {
	if s.opts.MemoryLimitMB < 0 {
		return
	}
	t := time.NewTicker(s.opts.MemoryCheck)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.checkMemory()
		}
	}
}

func (s *Server) checkMemory() {
	s.mu.Lock()
	if !s.ready || s.be == nil || s.be.cmd.Process == nil || s.recycling || s.shutdown {
		s.mu.Unlock()
		return
	}
	pid := s.be.cmd.Process.Pid
	gen := s.be.gen
	s.mu.Unlock()

	rss, err := s.opts.SampleRSS(pid)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.be == nil || s.be.gen != gen {
		s.mu.Unlock()
		return
	}
	s.backendRSS = rss
	limit := int64(s.opts.MemoryLimitMB) << 20
	over := limit > 0 && rss > limit
	cooling := time.Since(s.lastRecycle) < s.opts.RecycleCooldown
	s.mu.Unlock()
	if over && cooling {
		s.log.Warn("backend over its memory limit again soon after a recycle; lower --max-resident-docs or raise --memory-limit-mb",
			"rssMB", rss>>20, "limitMB", s.opts.MemoryLimitMB)
		return
	}
	if over {
		s.recycle(fmt.Sprintf("backend uses %d MB, over the %d MB limit", rss>>20, s.opts.MemoryLimitMB))
	}
}

// recycle replaces the backend with a fresh process, without failing any
// request: new requests queue, in-flight ones are given time to finish.
func (s *Server) recycle(reason string) {
	s.mu.Lock()
	if !s.ready || s.be == nil || s.recycling || s.shutdown || s.exited {
		s.mu.Unlock()
		return
	}
	s.recycling = true
	s.ready = false // new requests and notifications queue until the new backend is ready
	be := s.be
	s.mu.Unlock()
	s.log.Info("recycling backend", "reason", reason, "generation", be.gen)

	deadline := time.Now().Add(s.opts.RecycleDrain)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		inFlight := 0
		for _, p := range s.pending {
			if p.gen == be.gen && p.internal == nil {
				inFlight++
			}
		}
		s.mu.Unlock()
		if inFlight == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	s.plannedGen = be.gen
	s.mu.Unlock()
	be.stop(s.opts.ShutdownGrace) // its exit runs backendFailed, which restarts at once
}
