package slashcmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OMO (senpi) registers extension commands at runtime through
// pi.registerCommand, so no file on disk lists them. Its RPC mode answers
// get_commands with the live command surface, which is the only complete
// source for extension and prompt commands.

const maxLiveOutput = 4 * 1024 * 1024

type omoCommandRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Syntax      string `json:"syntax"`
	SourceInfo  struct {
		Scope string `json:"scope"`
	} `json:"sourceInfo"`
}

// wireSource maps an omo row onto the palette's source enum
// (builtin | personal | project). Project-scoped resources are project rows;
// everything else the user installed is personal.
func wireSource(row omoCommandRow) string {
	if row.SourceInfo.Scope == "project" {
		return "project"
	}
	return "personal"
}

type omoGetCommandsResponse struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Data    struct {
		Commands []omoCommandRow `json:"commands"`
	} `json:"data"`
}

// parseOMOGetCommands extracts slash-invocable extension and prompt rows from
// an RPC transcript. Skill rows are dropped: the native discovery already
// renders them as /skill:<name>.
func parseOMOGetCommands(output []byte) ([]Command, error) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), maxLiveOutput)
	for scanner.Scan() {
		commands, matched, err := parseOMOGetCommandsLine(scanner.Bytes())
		if matched {
			return commands, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("omo get_commands response not found")
}

// parseOMOGetCommandsLine reports matched=true only for a line that parses as
// the get_commands response; every other line (events, acks, echoes, logs) is
// skipped so the caller keeps reading.
func parseOMOGetCommandsLine(raw []byte) ([]Command, bool, error) {
	line := bytes.TrimSpace(raw)
	if len(line) == 0 || line[0] != '{' {
		return nil, false, nil
	}
	var response omoGetCommandsResponse
	if json.Unmarshal(line, &response) != nil {
		return nil, false, nil
	}
	if response.Type != "response" || response.Command != "get_commands" {
		return nil, false, nil
	}
	if !response.Success {
		return nil, true, errors.New("omo get_commands failed")
	}
	commands := make([]Command, 0, len(response.Data.Commands))
	for _, row := range response.Data.Commands {
		if row.Syntax != "slash" || (row.Source != "extension" && row.Source != "prompt") {
			continue
		}
		if !commandNamePattern.MatchString(row.Name) {
			continue
		}
		description := compact(row.Description, 240)
		if description == "" {
			description = "/" + row.Name
		}
		commands = append(commands, Command{
			Command:     "/" + row.Name,
			Description: description,
			Source:      wireSource(row),
		})
	}
	return commands, true, nil
}

// MergeLiveCommands appends live rows that the catalog does not already hold.
// Existing rows win so builtins and discovered skills keep their precedence.
func MergeLiveCommands(catalog Catalog, live []Command) Catalog {
	seen := make(map[string]bool, len(catalog.Commands))
	commands := append([]Command(nil), catalog.Commands...)
	for _, command := range commands {
		seen[command.Command] = true
	}
	truncated := catalog.Truncated
	for _, command := range live {
		if seen[command.Command] {
			continue
		}
		if len(commands) >= maxEntries {
			truncated = true
			break
		}
		seen[command.Command] = true
		commands = append(commands, command)
	}
	return Catalog{Commands: commands, Truncated: truncated}
}

// LiveCommandFetcher loads live commands for one working directory.
type LiveCommandFetcher func(ctx context.Context, cwd string) ([]Command, error)

type liveEntry struct {
	commands []Command
	expires  time.Time
}

type liveCall struct {
	done     chan struct{}
	commands []Command
	waiters  int
}

// LiveCommandCache keeps one live listing per working directory. Concurrent
// requests for the same cwd share one fetch, and a failure is remembered for
// failureTTL so a hung omo cannot spawn one process per palette request.
type LiveCommandCache struct {
	ttl        time.Duration
	failureTTL time.Duration
	fetch      LiveCommandFetcher
	onError    func(cwd string, err error)
	mu         sync.Mutex
	entries    map[string]liveEntry
	inflight   map[string]*liveCall
}

func NewLiveCommandCache(ttl, failureTTL time.Duration, fetch LiveCommandFetcher) *LiveCommandCache {
	return &LiveCommandCache{
		ttl:        ttl,
		failureTTL: failureTTL,
		fetch:      fetch,
		entries:    make(map[string]liveEntry),
		inflight:   make(map[string]*liveCall),
	}
}

// OnError registers a callback for failed fetches, called once per fetch.
func (c *LiveCommandCache) OnError(callback func(cwd string, err error)) {
	c.mu.Lock()
	c.onError = callback
	c.mu.Unlock()
}

// Get returns the cached or freshly fetched commands for cwd, or nil when the
// listing is unavailable. A caller that joins an in-flight fetch returns when
// that fetch finishes or its own ctx ends, whichever is first.
func (c *LiveCommandCache) Get(ctx context.Context, cwd string) []Command {
	c.mu.Lock()
	if entry, ok := c.entries[cwd]; ok && time.Now().Before(entry.expires) {
		c.mu.Unlock()
		return entry.commands
	}
	if call, ok := c.inflight[cwd]; ok {
		call.waiters++
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.commands
		case <-ctx.Done():
			return nil
		}
	}
	call := &liveCall{done: make(chan struct{})}
	c.inflight[cwd] = call
	c.mu.Unlock()

	commands, err := c.fetch(ctx, cwd)

	c.mu.Lock()
	delete(c.inflight, cwd)
	onError := c.onError
	if err != nil {
		commands = nil
		c.entries[cwd] = liveEntry{expires: time.Now().Add(c.failureTTL)}
	} else {
		c.entries[cwd] = liveEntry{commands: commands, expires: time.Now().Add(c.ttl)}
	}
	call.commands = commands
	close(call.done)
	c.mu.Unlock()
	if err != nil && onError != nil {
		onError(cwd, err)
	}
	return commands
}

// waiters is a test hook reporting how many callers joined cwd's in-flight fetch.
func (c *LiveCommandCache) waiters(cwd string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call, ok := c.inflight[cwd]; ok {
		return call.waiters
	}
	return 0
}

// OMORPCFetcher runs `<omo> --mode rpc --no-session` in cwd, sends one
// get_commands request, and parses the response. The process is bounded by
// ctx and killed once the response arrives.
func OMORPCFetcher(binary string, timeout time.Duration) LiveCommandFetcher {
	return func(ctx context.Context, cwd string) ([]Command, error) {
		if binary == "" {
			return nil, errors.New("omo executable unavailable")
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--mode", "rpc", "--no-session")
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			cmd.Dir = cwd
		}
		cmd.Env = omoCommandEnv(os.Environ())
		cmd.Stdin = strings.NewReader(`{"id":"relay-commands","type":"get_commands"}` + "\n")
		// omo runs its engine in child processes that inherit stdout, so only
		// signalling the whole process group releases the pipe on timeout.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return killProcessGroup(cmd) }
		cmd.WaitDelay = time.Second
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		defer func() {
			_ = killProcessGroup(cmd)
			_ = cmd.Wait()
		}()
		stop := context.AfterFunc(ctx, func() { _ = killProcessGroup(cmd) })
		defer stop()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLiveOutput)
		for scanner.Scan() {
			commands, matched, err := parseOMOGetCommandsLine(scanner.Bytes())
			if matched {
				return commands, err
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read omo output: %w", err)
		}
		return nil, errors.New("omo exited without a get_commands response")
	}
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// omoCommandEnv drops relay-private settings so the spawned agent sees the
// user's normal environment.
func omoCommandEnv(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, entry := range environ {
		if strings.HasPrefix(entry, "HERDR_RELAY_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}
