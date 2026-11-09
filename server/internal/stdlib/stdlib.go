// Package stdlib provides the Mojo standard library's sources matching the
// installed toolchain, so mojols can resolve names into it: go to
// definition, type hierarchies and references reach `std`, which
// mojo-lsp-server only knows in compiled form.
//
// Sources come from a local directory if given, else from a cache, else
// from GitHub (modular/modular at the release tag of the installed
// version): the tree API lists the files (a handful of requests) and
// raw.githubusercontent.com serves them. Downloads are size-capped,
// path-checked, and written atomically, so a cache entry is either
// complete or absent.
package stdlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Limits on a download, far above the real stdlib (about 250 files, 6 MB).
const (
	maxFiles     = 2000
	maxFileBytes = 4 << 20
	maxTotal     = 80 << 20
)

// Version is an installed Mojo toolchain's version.
type Version struct {
	Number string // "1.1.0", or "1.3.0.dev2026110805"
	Commit string // as printed, may be empty
}

var versionRe = regexp.MustCompile(`Mojo\s+([0-9][0-9A-Za-z.\-+]*)(?:\s+\(([0-9a-f]+)\))?`)

// DetectVersion runs the `mojo` binary next to the given mojo-lsp-server
// (or on PATH) with --version.
func DetectVersion(ctx context.Context, serverPath string) (Version, error) {
	candidates := []string{filepath.Join(filepath.Dir(serverPath), "mojo")}
	if p, err := exec.LookPath("mojo"); err == nil {
		candidates = append(candidates, p)
	}
	var lastErr error
	for _, c := range candidates {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, err := exec.CommandContext(ctx, c, "--version").Output()
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if v, ok := ParseVersion(string(out)); ok {
			return v, nil
		}
		lastErr = fmt.Errorf("unrecognized version output %q", strings.TrimSpace(string(out)))
	}
	return Version{}, fmt.Errorf("cannot tell the Mojo version: %w", lastErr)
}

// ParseVersion reads `mojo --version` output ("Mojo 1.1.0 (8189361e)").
func ParseVersion(out string) (Version, bool) {
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return Version{}, false
	}
	return Version{Number: m[1], Commit: m[2]}, true
}

// Dev reports whether v is a nightly build (no release tag).
func (v Version) Dev() bool { return strings.Contains(v.Number, "dev") }

// Options configure Ensure.
type Options struct {
	// CacheDir holds downloaded sources (<CacheDir>/<version>/std).
	CacheDir string
	// API and Raw are the GitHub endpoints (overridable for tests).
	API, Raw string
	Repo     string // "modular/modular"
	Client   *http.Client
	Logger   *slog.Logger
	// Token, if set, authenticates API requests (raises rate limits).
	Token string
}

func (o *Options) defaults() {
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	if o.Raw == "" {
		o.Raw = "https://raw.githubusercontent.com"
	}
	if o.Repo == "" {
		o.Repo = "modular/modular"
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 60 * time.Second}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.CacheDir == "" {
		if d, err := os.UserCacheDir(); err == nil {
			o.CacheDir = filepath.Join(d, "mojols", "stdlib")
		}
	}
}

// Cached returns the cache directory holding complete sources for v, or
// "" if they are not downloaded.
func Cached(v Version) string {
	var o Options
	o.defaults()
	dir := filepath.Join(o.CacheDir, sanitize(v.Number))
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return dir
	}
	return ""
}

// Root is a directory containing a `std` package: what import resolution
// takes as a library root.
type Root string

// Ensure returns a directory holding the stdlib sources for v, downloading
// them into the cache if they are not there yet.
func Ensure(ctx context.Context, v Version, opts Options) (Root, error) {
	opts.defaults()
	if opts.CacheDir == "" {
		return "", errors.New("no cache directory")
	}
	dir := filepath.Join(opts.CacheDir, sanitize(v.Number))
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return Root(dir), nil
	}
	refs := []string{"refs/tags/mojo/v" + v.Number, "refs/tags/modular/v" + v.Number}
	if v.Commit != "" {
		refs = append(refs, v.Commit)
	}
	if v.Dev() {
		refs = append(refs, "main") // best effort for nightlies
	}
	var lastErr error
	for _, ref := range refs {
		sha, err := opts.commit(ctx, ref)
		if err != nil {
			lastErr = err
			continue
		}
		if err := opts.download(ctx, sha, dir); err != nil {
			return "", err
		}
		opts.Logger.Info("stdlib sources downloaded", "version", v.Number, "ref", ref, "commit", sha, "dir", dir)
		return Root(dir), nil
	}
	return "", fmt.Errorf("no stdlib sources for Mojo %s on GitHub: %w", v.Number, lastErr)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == ' ' {
			return '_'
		}
		return r
	}, s)
}

func (o *Options) get(ctx context.Context, url string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mojols")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if o.Token != "" && strings.HasPrefix(url, o.API) {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

func (o *Options) getJSON(ctx context.Context, url string, out any) error {
	resp, err := o.get(ctx, url, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// commit resolves a ref (tag, branch or sha) to a commit sha.
func (o *Options) commit(ctx context.Context, ref string) (string, error) {
	var c struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := o.getJSON(ctx, fmt.Sprintf("%s/repos/%s/commits/%s", o.API, o.Repo, strings.TrimPrefix(ref, "refs/tags/")), &c); err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c.SHA) {
		return "", fmt.Errorf("bad commit for %s", ref)
	}
	return c.SHA, nil
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

type tree struct {
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// stdPaths are where the std package lives in the repository, newest first.
var stdPaths = [][]string{{"Mojo", "stdlib", "std"}, {"mojo", "stdlib", "std"}, {"mojo", "stdlib", "stdlib"}}

// download fetches the std package at commit sha into dir.
func (o *Options) download(ctx context.Context, sha, dir string) error {
	var files []treeEntry
	var prefix string
	for _, p := range stdPaths {
		sub, err := o.subtree(ctx, sha, p)
		if err != nil {
			continue
		}
		var t tree
		if err := o.getJSON(ctx, fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", o.API, o.Repo, sub), &t); err != nil {
			return err
		}
		if t.Truncated {
			return errors.New("the stdlib file listing was truncated")
		}
		for _, e := range t.Tree {
			if e.Type == "blob" && strings.HasSuffix(e.Path, ".mojo") {
				files = append(files, e)
			}
		}
		prefix = strings.Join(p, "/")
		break
	}
	if prefix == "" {
		return errors.New("no std package found in the repository at that version")
	}
	if len(files) == 0 || len(files) > maxFiles {
		return fmt.Errorf("unexpected stdlib file count %d", len(files))
	}
	var total int64
	for _, f := range files {
		if !safeRel(f.Path) || f.Size > maxFileBytes {
			return fmt.Errorf("refusing stdlib file %q (%d bytes)", f.Path, f.Size)
		}
		total += f.Size
	}
	if total > maxTotal {
		return fmt.Errorf("stdlib too large (%d bytes)", total)
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	jobs := make(chan treeEntry)
	errs := make(chan error, len(files))
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				errs <- o.fetch(ctx, sha, prefix, f, filepath.Join(tmp, "std"))
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	manifest := fmt.Sprintf("commit %s\nfiles %d\nbytes %d\nfetched %s\n", sha, len(files), total, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(tmp, ".complete"), []byte(manifest), 0o644); err != nil {
		return err
	}
	os.RemoveAll(dir) // a partial leftover from an older mojols
	return os.Rename(tmp, dir)
}

// subtree walks from the commit's root tree down a path of directories.
func (o *Options) subtree(ctx context.Context, sha string, parts []string) (string, error) {
	cur := sha
	for _, part := range parts {
		var t tree
		if err := o.getJSON(ctx, fmt.Sprintf("%s/repos/%s/git/trees/%s", o.API, o.Repo, cur), &t); err != nil {
			return "", err
		}
		next := ""
		for _, e := range t.Tree {
			if e.Path == part && e.Type == "tree" {
				next = e.SHA
			}
		}
		if next == "" {
			return "", fmt.Errorf("%s not found", part)
		}
		cur = next
	}
	return cur, nil
}

// safeRel reports whether p is a clean relative path inside the package.
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	c := path.Clean(p)
	return c == p && !strings.HasPrefix(c, "../") && c != ".."
}

// fetch downloads one file, retrying transient failures.
func (o *Options) fetch(ctx context.Context, sha, prefix string, f treeEntry, into string) error {
	url := fmt.Sprintf("%s/%s/%s/%s/%s", o.Raw, o.Repo, sha, prefix, f.Path)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		resp, err := o.get(ctx, url, "")
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if len(body) > maxFileBytes {
			return fmt.Errorf("%s is too large", f.Path)
		}
		dst := filepath.Join(into, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		// Read-only: these are reference sources, not for editing.
		return os.WriteFile(dst, body, 0o444)
	}
	return lastErr
}
