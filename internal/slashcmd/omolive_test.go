package slashcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const getCommandsFixture = `{"type":"response","command":"get_commands","success":true,"data":{"commands":[` +
	`{"name":"cpa","description":"CPA status","source":"extension","syntax":"slash"},` +
	`{"name":"fix-tests","description":"Fix failing tests","source":"prompt","syntax":"slash"},` +
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
	if got["/cpa"].Source != "extension" || got["/cpa"].Description != "CPA status" {
		t.Fatalf("/cpa = %#v, want extension row", got["/cpa"])
	}
	if got["/fix-tests"].Source != "prompt" {
		t.Fatalf("/fix-tests = %#v, want prompt row", got["/fix-tests"])
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
		base.Commands = append(base.Commands, Command{Command: "/c" + time.Duration(i).String(), Source: "builtin"})
	}
	merged := MergeLiveCommands(base, []Command{{Command: "/cpa", Source: "extension"}})
	if len(merged.Commands) != maxEntries || !merged.Truncated {
		t.Fatalf("merged = %d truncated=%v, want %d truncated", len(merged.Commands), merged.Truncated, maxEntries)
	}
}

func TestLiveCommandCacheReusesResultPerCwd(t *testing.T) {
	var calls atomic.Int32
	cache := NewLiveCommandCache(time.Minute, func(ctx context.Context, cwd string) ([]Command, error) {
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

func TestLiveCommandCacheReturnsNothingOnFailureAndRetries(t *testing.T) {
	var calls atomic.Int32
	cache := NewLiveCommandCache(time.Minute, func(ctx context.Context, cwd string) ([]Command, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("omo unavailable")
		}
		return []Command{{Command: "/cpa", Source: "extension"}}, nil
	})
	if got := cache.Get(context.Background(), "/repo"); got != nil {
		t.Fatalf("failed fetch returned %#v, want nil", got)
	}
	if got := cache.Get(context.Background(), "/repo"); len(got) != 1 {
		t.Fatalf("retry after failure = %#v, want one command", got)
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
	body := "#!/bin/sh\nread request\nprintf '%s\\n' '{\"type\":\"event\"}'\nprintf '%s\\n' '" + getCommandsFixture + "'\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	commands, err := OMORPCFetcher(script, 5*time.Second)(context.Background(), dir)
	if err != nil {
		t.Fatalf("fetch error: %v", err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("fetcher waited for process exit instead of returning on the response")
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
	started := time.Now()
	if _, err := OMORPCFetcher(script, 300*time.Millisecond)(context.Background(), dir); err == nil {
		t.Fatal("a silent binary must time out with an error")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("timeout did not bound the fetch")
	}
}
