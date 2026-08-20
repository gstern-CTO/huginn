package security

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// allowedBinaries is the complete set of executables this server may spawn.
// Anything absent is rejected before a process is created. The list is
// deliberately short and contains only read-only research tooling.
var allowedBinaries = map[string]bool{
	// search and navigation
	"rg":   true,
	"find": true,
	"git":  true,
	"gh":   true,
	// language servers
	"gopls":                      true,
	"pyright-langserver":         true,
	"rust-analyzer":              true,
	"typescript-language-server": true,
	"clangd":                     true,
	"jdtls":                      true,
	"solargraph":                 true,
	"intelephense":               true,
	"lua-language-server":        true,
}

// gitReadOnlySubcommands bounds what git may do. The server is read-only
// everywhere, so mutating porcelain is not reachable even though git itself is
// on the binary allowlist.
var gitReadOnlySubcommands = map[string]bool{
	"log": true, "show": true, "diff": true, "blame": true, "status": true,
	"ls-files": true, "ls-tree": true, "rev-parse": true, "grep": true,
	"describe": true, "branch": true, "remote": true, "config": true,
}

// ErrBinaryNotAllowed is returned before any process is created.
var ErrBinaryNotAllowed = errors.New("binary is not on the allowlist")

// CommandResult carries the outcome of a subprocess run.
type CommandResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	Truncated  bool
	OutputSize int
}

// Runner executes external binaries safely.
//
// Arguments are always passed as an explicit array — there is no code path in
// this server that builds a shell string, so shell metacharacters in a
// user-supplied query are inert data rather than syntax.
type Runner struct {
	maxOutput int

	mu       sync.Mutex
	pathOnce map[string]string // resolved absolute paths, cached per binary
}

func NewRunner(maxOutput int) *Runner {
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutput
	}
	return &Runner{maxOutput: maxOutput, pathOnce: map[string]string{}}
}

// Lookup resolves a whitelisted binary to its absolute path, or reports that it
// is missing. Callers use this to produce install hints before attempting a run.
func (r *Runner) Lookup(name string) (string, error) {
	if err := checkBinaryAllowed(name); err != nil {
		return "", err
	}
	r.mu.Lock()
	if cached, ok := r.pathOnce[name]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.pathOnce[name] = resolved
	r.mu.Unlock()
	return resolved, nil
}

// Available reports whether a whitelisted binary is installed.
func (r *Runner) Available(name string) bool {
	_, err := r.Lookup(name)
	return err == nil
}

// Run executes name with args, capping captured output. dir, when non-empty,
// becomes the working directory and must already have passed path validation.
func (r *Runner) Run(ctx context.Context, name string, args []string, dir string) (*CommandResult, error) {
	if err := checkBinaryAllowed(name); err != nil {
		return nil, err
	}
	if name == "git" && len(args) > 0 && !gitReadOnlySubcommands[args[0]] {
		return nil, fmt.Errorf("%w: git %s is not a read-only subcommand", ErrBinaryNotAllowed, args[0])
	}
	resolved, err := r.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed: %w", name, err)
	}

	// exec.CommandContext with an explicit argv: no shell is involved.
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Dir = dir
	// A minimal environment: the child inherits nothing that could leak
	// credentials from this process into a subprocess's view.
	cmd.Env = minimalEnv()

	stdout := &cappedBuffer{limit: r.maxOutput}
	stderr := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	res := &CommandResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		Truncated:  stdout.truncated,
		OutputSize: stdout.written,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// A non-zero exit is data, not a failure: ripgrep exits 1 when it
			// finds nothing. Callers interpret the code.
			return res, nil
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, runErr
	}
	return res, nil
}

// Process is a long-lived child process with pipes attached, used for language
// servers. It goes through the same allowlist as one-shot commands.
type Process struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

// Start launches a whitelisted binary with stdio pipes. The caller owns the
// process lifetime and must call Cmd.Wait or kill it.
func (r *Runner) Start(ctx context.Context, name string, args []string, dir string) (*Process, error) {
	if err := checkBinaryAllowed(name); err != nil {
		return nil, err
	}
	resolved, err := r.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed: %w", name, err)
	}

	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Dir = dir
	cmd.Env = minimalEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Process{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}, nil
}

func checkBinaryAllowed(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty binary name", ErrBinaryNotAllowed)
	}
	// Reject anything that tries to name a path rather than a binary: only
	// bare names resolved through PATH are permitted.
	if strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
		return fmt.Errorf("%w: %q must be a bare binary name, not a path", ErrBinaryNotAllowed, name)
	}
	if strings.ContainsAny(name, " \t\n;|&$`") {
		return fmt.Errorf("%w: %q contains shell metacharacters", ErrBinaryNotAllowed, name)
	}
	if !allowedBinaries[name] {
		return fmt.Errorf("%w: %q", ErrBinaryNotAllowed, name)
	}
	return nil
}

// minimalEnv builds the child environment. PATH is needed to run the binary;
// HOME is needed by language servers for their caches. Nothing else — in
// particular no tokens — is passed down.
func minimalEnv() []string {
	keep := []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "XDG_CACHE_HOME", "GOPATH", "GOMODCACHE", "GOCACHE"}
	env := make([]string, 0, len(keep))
	for _, key := range keep {
		if val, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+val)
		}
	}
	return env
}

// cappedBuffer accumulates output up to a limit and then discards the rest,
// so a runaway subprocess cannot exhaust memory.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	written   int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.written += len(p)
	if c.buf.Len() >= c.limit {
		c.truncated = true
		return len(p), nil // absorb without storing
	}
	room := c.limit - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }

var _ io.Writer = (*cappedBuffer)(nil)

// DefaultMaxOutput bounds how much subprocess output is captured.
const DefaultMaxOutput = 8 << 20
