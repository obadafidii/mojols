package verify

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/obadafidii/mojols/server/internal/fakelsp"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakelsp.EnvMarker) == "1" {
		os.Exit(fakelsp.Run(os.Stdin, os.Stdout, fakelsp.OptionsFromEnv()))
	}
	os.Exit(m.Run())
}

func newVerifier(t *testing.T, env ...string) *Verifier {
	t.Helper()
	logOut := os.Stderr
	if os.Getenv("MOJOLS_TEST_LOG") != "1" {
		logOut, _ = os.Open(os.DevNull)
	}
	v := New(Options{
		Logger:   slog.New(slog.NewTextHandler(logOut, nil)),
		IdleStop: 200 * time.Millisecond,
		Command: func() (Command, error) {
			return Command{Path: os.Args[0], Args: []string{"-test.run=^$"}, Env: append([]string{fakelsp.EnvMarker + "=1"}, env...)}, nil
		},
	})
	t.Cleanup(v.Close)
	return v
}

const src = "def f(x: Thing):\n    x.size()\n    x.other\n"

func TestCheckAndCache(t *testing.T) {
	v := newVerifier(t, "MOJOLS_FAKE_DEFS=size=/ws/thing.mojo:4:8")
	size := strings.Index(src, "size")
	other := strings.Index(src, "other")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := v.Check(ctx, "/ws/main.mojo", []byte(src), []int{size, other}); err != nil {
		t.Fatal(err)
	}
	answers, complete, failed := v.Lookup("/ws/main.mojo", []byte(src), []int{size, other})
	if !complete || failed {
		t.Fatalf("complete=%v failed=%v", complete, failed)
	}
	if l := answers[size].Loc; l == nil || l.Path != "/ws/thing.mojo" || l.Range.Start.Line != 4 || l.Range.Start.Character != 8 {
		t.Errorf("size: %+v", answers[size].Loc)
	}
	if answers[other].Loc != nil {
		t.Errorf("other should lead nowhere: %+v", answers[other].Loc)
	}
	queries := v.Stats().Queries
	// The same version again: answered from the cache.
	if err := v.Check(ctx, "/ws/main.mojo", []byte(src), []int{size}); err != nil {
		t.Fatal(err)
	}
	if v.Stats().Queries != queries {
		t.Errorf("a cached version was checked again")
	}
	// A new version is checked anew.
	edited := []byte(strings.Replace(src, "x.other", "x.other2", 1))
	if _, complete, _ := v.Lookup("/ws/main.mojo", edited, []int{size}); complete {
		t.Errorf("an edited version must not use the old answers")
	}
}

func TestIdleStopAndRestart(t *testing.T) {
	v := newVerifier(t, "MOJOLS_FAKE_DEFS=size=/ws/thing.mojo:4:8")
	ctx := context.Background()
	v.Check(ctx, "/ws/a.mojo", []byte(src), []int{strings.Index(src, "size")})
	deadline := time.Now().Add(5 * time.Second)
	for v.Stats().Running && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if v.Stats().Running {
		t.Fatal("verifier still running after it went idle")
	}
	// Work again: it starts again.
	if err := v.Check(ctx, "/ws/b.mojo", []byte(src), []int{strings.Index(src, "size")}); err != nil {
		t.Fatal(err)
	}
	if a, complete, _ := v.Lookup("/ws/b.mojo", []byte(src), []int{strings.Index(src, "size")}); !complete || a[strings.Index(src, "size")].Loc == nil {
		t.Errorf("after restart: complete=%v %+v", complete, a)
	}
}

func TestCrashMarksFileFailed(t *testing.T) {
	v := newVerifier(t, "MOJOLS_FAKE_CRASH_ON=textDocument/definition")
	off := strings.Index(src, "size")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v.Check(ctx, "/ws/main.mojo", []byte(src), []int{off})
	_, _, failed := v.Lookup("/ws/main.mojo", []byte(src), []int{off})
	if !failed || v.Stats().Failures != 1 {
		t.Errorf("failed=%v stats=%+v", failed, v.Stats())
	}
	// Not retried for the same version.
	v.Check(ctx, "/ws/main.mojo", []byte(src), []int{off})
	if v.Stats().Failures != 1 {
		t.Errorf("a failed version was retried: %+v", v.Stats())
	}
}

func TestDeadlineReturnsEarly(t *testing.T) {
	v := newVerifier(t, "MOJOLS_FAKE_PARSE_MS=2000")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := v.Check(ctx, "/ws/slow.mojo", []byte(src), []int{strings.Index(src, "size")})
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("Check should give up at its deadline: err=%v after %v", err, time.Since(start))
	}
	// It finishes in the background.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, complete, _ := v.Lookup("/ws/slow.mojo", []byte(src), []int{strings.Index(src, "size")}); complete {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("background verification never finished")
}
