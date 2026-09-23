package slashcmd

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const getCommandsFixture = `{"type":"response","command":"get_commands","success":true,"data":{"commands":[` +
	`{"name":"cpa","description":"CPA status","source":"extension","syntax":"slash","sourceInfo":{"scope":"user"}},` +
	`{"name":"fix-tests","description":"Fix failing tests","source":"prompt","syntax":"slash","sourceInfo":{"scope":"project"}},` +
	`{"name":"skill:brave","description":"Search","source":"skill","syntax":"dollar"},` +
	`{"name":"plan","description":"Shadowed extension","source":"extension","syntax":"slash"},` +
	`{"name":"bad name!","description":"invalid","source":"extension","syntax":"slash"}` +
	`]}}`

func TestParseOMOGetCommandsKeepsSlashRowsOnly(t *testing.T) {
	commands, err := parseOMOGetCommands([]byte("noise before json\n" + getCommandsFixture + "\n"))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	got := map[string]Command{}
	for _, command := range commands {
		got[command.Command] = command
	}
	if got["/cpa"].Source != "personal" || got["/cpa"].Description != "CPA status" {
		t.Fatalf("/cpa = %#v, want a personal row", got["/cpa"])
	}
	if got["/fix-tests"].Source != "project" {
		t.Fatalf("/fix-tests = %#v, want a project row", got["/fix-tests"])
	}
	if _, ok := got["/skill:brave"]; ok {
		t.Fatal("dollar-syntax skill rows must not become slash commands")
	}
	if _, ok := got["/bad name!"]; ok {
		t.Fatal("invalid command names must be dropped")
	}
}

func TestParseOMOGetCommandsRejectsFailedResponse(t *testing.T) {
	_, err := parseOMOGetCommands([]byte(`{"type":"response","command":"get_commands","success":false,"error":"x"}` + "\n"))
	if err == nil {
		t.Fatal("a failed get_commands response must be an error")
	}
}

func TestMergeLiveCommandsAppendsWithoutShadowingExisting(t *testing.T) {
	base := Catalog{Commands: []Command{
		{Command: "/plan", Description: "builtin plan", Source: "builtin"},
		{Command: "/skill:brave", Description: "skill", Source: "personal"},
	}}
	live := []Command{
		{Command: "/plan", Description: "Shadowed extension", Source: "extension"},
		{Command: "/cpa", Description: "CPA status", Source: "extension"},
	}
	merged := MergeLiveCommands(base, live)
	if len(merged.Commands) != 3 {
		t.Fatalf("merged = %d commands, want 3: %#v", len(merged.Commands), merged.Commands)
	}
	if merged.Commands[0].Description != "builtin plan" {
		t.Fatalf("existing /plan was replaced: %#v", merged.Commands[0])
	}
	if merged.Commands[2].Command != "/cpa" {
		t.Fatalf("live /cpa not appended last: %#v", merged.Commands)
	}
}

func TestMergeLiveCommandsRespectsEntryCap(t *testing.T) {
	base := Catalog{}
	for i := 0; i < maxEntries; i++ {
		base.Commands = append(base.Commands, Command{Command: "/c" + strconv.Itoa(i), Source: "builtin"})
	}
	merged := MergeLiveCommands(base, []Command{{Command: "/cpa", Source: "extension"}})
	if len(merged.Commands) != maxEntries || !merged.Truncated {
		t.Fatalf("merged = %d truncated=%v, want %d truncated", len(merged.Commands), merged.Truncated, maxEntries)
	}
}

func TestLiveCommandCacheReusesResultPerCwd(t *testing.T) {
	var calls atomic.Int32
	cache := NewLiveCommandCache(time.Minute, time.Minute, func(ctx context.Context, cwd string) ([]Command, error) {
		calls.Add(1)
		return []Command{{Command: "/cpa", Source: "extension"}}, nil
	})
	for i := 0; i < 3; i++ {
		if got := cache.Get(context.Background(), "/repo"); len(got) != 1 {
			t.Fatalf("Get = %#v, want one command", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want 1 within TTL", calls.Load())
	}
	cache.Get(context.Background(), "/other")
	if calls.Load() != 2 {
		t.Fatalf("fetch calls = %d, want a separate fetch per cwd", calls.Load())
	}
}

func TestLiveCommandCacheRemembersFailureForFailureTTL(t *testing.T) {
	var calls atomic.Int32
	var reported atomic.Int32
	cache := NewLiveCommandCache(time.Minute, time.Hour, func(ctx context.Context, cwd string) ([]Command, error) {
		calls.Add(1)
		return nil, errors.New("omo unavailable")
	})
	cache.OnError(func(cwd string, err error) { reported.Add(1) })
	for i := 0; i < 3; i++ {
		if got := cache.Get(context.Background(), "/repo"); got != nil {
			t.Fatalf("failed fetch returned %#v, want nil", got)
		}
	}
	if calls.Load() != 1 || reported.Load() != 1 {
		t.Fatalf("fetches=%d reports=%d, want one of each within the failure TTL", calls.Load(), reported.Load())
	}
}

func TestLiveCommandCacheRetriesAfterFailureTTL(t *testing.T) {
	var calls atomic.Int32
	cache := NewLiveCommandCache(time.Minute, 0, func(ctx context.Context, cwd string) ([]Command, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("omo unavailable")
		}
		return []Command{{Command: "/cpa", Source: "personal"}}, nil
	})
	cache.Get(context.Background(), "/repo")
	if got := cache.Get(context.Background(), "/repo"); len(got) != 1 {
		t.Fatalf("retry after an expired failure = %#v, want one command", got)
	}
}

func TestLiveCommandCacheSharesOneFetchAcrossConcurrentCallers(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	cache := NewLiveCommandCache(time.Minute, time.Minute, func(ctx context.Context, cwd string) ([]Command, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return []Command{{Command: "/cpa", Source: "personal"}}, nil
	})
	const callers = 16
	var wg sync.WaitGroup
	results := make([][]Command, callers)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = cache.Get(context.Background(), "/repo")
	}()
	<-entered
	for i := 1; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = cache.Get(context.Background(), "/repo")
		}(i)
	}
	// Every waiter must be parked on the in-flight call before it finishes.
	waitFor(t, func() bool { return cache.waiters("/repo") == callers-1 })
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 for %d concurrent callers", calls.Load(), callers)
	}
	for i, got := range results {
		if len(got) != 1 {
			t.Fatalf("caller %d got %#v, want the shared result", i, got)
		}
	}
}

func TestLiveCommandCacheWaiterHonoursItsOwnContext(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	cache := NewLiveCommandCache(time.Minute, time.Minute, func(ctx context.Context, cwd string) ([]Command, error) {
		close(entered)
		<-release
		return nil, nil
	})
	defer close(release)
	go cache.Get(context.Background(), "/repo")
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := cache.Get(ctx, "/repo"); got != nil {
		t.Fatalf("cancelled waiter got %#v, want nil", got)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOMORPCFetcherReportsMissingBinary(t *testing.T) {
	fetch := OMORPCFetcher("", time.Second)
	if _, err := fetch(context.Background(), t.TempDir()); err == nil {
		t.Fatal("an empty binary must be an error")
	}
}

func TestOMORPCFetcherParsesScriptedRPCResponse(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-omo")
	body := "#!/bin/sh\nread request\nprintf '%s\\n' \"$request\"\n" +
		"printf '%s\\n' '{\"type\":\"ack\",\"command\":\"get_commands\"}'\n" +
		"printf '%s\\n' '" + getCommandsFixture + "'\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// The script stays alive for 30s after answering; a fetch that waited for
	// exit would hit the 20s budget and fail instead of returning commands.
	commands, err := OMORPCFetcher(script, 20*time.Second)(context.Background(), dir)
	if err != nil {
		t.Fatalf("fetch error: %v", err)
	}
	found := false
	for _, command := range commands {
		found = found || command.Command == "/cpa"
	}
	if !found {
		t.Fatalf("commands = %#v, want /cpa", commands)
	}
}

func TestOMORPCFetcherTimesOutSilentBinary(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "silent-omo")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := OMORPCFetcher(script, 300*time.Millisecond)(context.Background(), dir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the timeout to end the fetch", err)
	}
}

func TestOMORPCFetcherReportsOversizedLine(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "huge-omo")
	body := "#!/bin/sh\nread request\nhead -c 5000000 /dev/zero | tr '\\0' 'x'\necho\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := OMORPCFetcher(script, 20*time.Second)(context.Background(), dir)
	if err == nil || !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong surfaced", err)
	}
}
