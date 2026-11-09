package proxy

// Crash capture.
//
// Soaks on ordinary code showed the official server crashing at the same
// sites, in the same files, run after run: a specific request on specific
// code, not a random race. To turn that into a reproduction, mojols records
// what was in flight whenever the backend dies unexpectedly: the requests,
// the exact text of their documents and of every resident document, the
// initialize parameters and the backend command. scripts/replay_crash.py
// feeds such a bundle to a bare mojo-lsp-server.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CrashBundle is everything needed to replay a backend crash.
type CrashBundle struct {
	Time        string          `json:"time"`
	Reason      string          `json:"reason"`
	Generation  int             `json:"generation"`
	Backend     Command         `json:"backend"`
	InitParams  json.RawMessage `json:"initParams"`
	InFlight    []InFlight      `json:"inFlight"`
	Documents   []BundleDoc     `json:"documents"`
	CrashOutput string          `json:"crashOutput"`
}

// InFlight is a request the backend had not answered when it died.
type InFlight struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	AgeMs  int64           `json:"ageMs"` // how long it had been at the backend
}

// BundleDoc is a document open in the backend when it died.
type BundleDoc struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int32  `json:"version"`
	Text       string `json:"text"`
	Target     bool   `json:"target"` // an in-flight request is about it
}

// crashBundleLocked snapshots generation gen just after it died. It must run
// before the pending requests and the resident set are cleared.
func (s *Server) crashBundleLocked(gen int, be *backend, reason error) *CrashBundle {
	b := &CrashBundle{
		Time:       time.Now().Format(time.RFC3339Nano),
		Reason:     reason.Error(),
		Generation: gen,
		Backend:    s.lastCmd,
		InitParams: s.initParams,
	}
	if be != nil {
		b.CrashOutput = be.crashReport()
	}
	targets := map[string]bool{}
	now := time.Now()
	for _, p := range s.pending {
		if p.gen != gen || p.internal != nil {
			continue
		}
		b.InFlight = append(b.InFlight, InFlight{Method: p.method, Params: p.msg.Params, AgeMs: now.Sub(p.sent).Milliseconds()})
		if uri := docURIOf(p.msg.Params); uri != "" {
			targets[uri] = true
		}
	}
	sort.Slice(b.InFlight, func(i, j int) bool { return b.InFlight[i].AgeMs > b.InFlight[j].AgeMs })
	seen := map[string]bool{}
	addDoc := func(uri string) {
		if seen[uri] {
			return
		}
		seen[uri] = true
		if d, ok := s.docs.Get(uri); ok {
			b.Documents = append(b.Documents, BundleDoc{URI: d.URI, LanguageID: d.LanguageID, Version: d.Version, Text: d.Text, Target: targets[uri]})
		}
	}
	for uri := range targets {
		addDoc(uri)
	}
	for uri := range s.resident {
		addDoc(uri)
	}
	sort.Slice(b.Documents, func(i, j int) bool {
		if b.Documents[i].Target != b.Documents[j].Target {
			return b.Documents[i].Target
		}
		return b.Documents[i].URI < b.Documents[j].URI
	})
	return b
}

// summary is one line per in-flight request, for the log.
func (b *CrashBundle) summary() string {
	if len(b.InFlight) == 0 {
		return "none"
	}
	var parts []string
	for _, f := range b.InFlight {
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position *struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"position"`
		}
		_ = json.Unmarshal(f.Params, &p)
		where := fileName(p.TextDocument.URI)
		if p.Position != nil {
			where += fmt.Sprintf(":%d:%d", p.Position.Line+1, p.Position.Character+1)
		}
		parts = append(parts, fmt.Sprintf("%s %s (%d ms)", f.Method, where, f.AgeMs))
	}
	return strings.Join(parts, "; ")
}

// recordCrash logs what was in flight and, with CrashDir, saves the bundle.
func (s *Server) recordCrash(b *CrashBundle) {
	s.log.Warn("requests in flight at backend crash", "generation", b.Generation, "inFlight", b.summary())
	if s.opts.CrashDir == "" {
		return
	}
	if err := os.MkdirAll(s.opts.CrashDir, 0o755); err != nil {
		s.log.Error("cannot create crash dir", "err", err)
		return
	}
	name := fmt.Sprintf("crash-%s-gen%d.json", time.Now().Format("20060102-150405.000"), b.Generation)
	path := filepath.Join(s.opts.CrashDir, name)
	data, err := json.MarshalIndent(b, "", "  ")
	if err == nil {
		err = os.WriteFile(path, data, 0o644)
	}
	if err != nil {
		s.log.Error("cannot save crash bundle", "err", err)
		return
	}
	s.log.Warn("saved crash bundle", "path", path, "documents", len(b.Documents), "inFlight", len(b.InFlight))
}
