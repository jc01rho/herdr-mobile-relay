package slashcmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var response omoGetCommandsResponse
		if json.Unmarshal(line, &response) != nil {
			continue
		}
		if response.Type != "response" || response.Command != "get_commands" {
			continue
		}
		if !response.Success {
			return nil, errors.New("omo get_commands failed")
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
				Source:      row.Source,
			})
		}
		return commands, nil
	}
	return nil, errors.New("omo get_commands response not found")
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

// LiveCommandCache keeps one successful live listing per working directory.
// Failures are not cached so the next palette open retries.
type LiveCommandCache struct {
	ttl     time.Duration
	fetch   LiveCommandFetcher
	mu      sync.Mutex
	entries map[string]liveEntry
}

func NewLiveCommandCache(ttl time.Duration, fetch LiveCommandFetcher) *LiveCommandCache {
	return &LiveCommandCache{ttl: ttl, fetch: fetch, entries: make(map[string]liveEntry)}
}

func (c *LiveCommandCache) Get(ctx context.Context, cwd string) []Command {
	c.mu.Lock()
	if entry, ok := c.entries[cwd]; ok && time.Now().Before(entry.expires) {
		c.mu.Unlock()
		return entry.commands
	}
	c.mu.Unlock()

	commands, err := c.fetch(ctx, cwd)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	c.entries[cwd] = liveEntry{commands: commands, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return commands
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
			line := scanner.Bytes()
			if !bytes.Contains(line, []byte(`"get_commands"`)) {
				continue
			}
			return parseOMOGetCommands(append(append([]byte(nil), line...), '\n'))
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
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
