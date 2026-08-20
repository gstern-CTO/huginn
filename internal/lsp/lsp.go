package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gstern-CTO/huginn/internal/metrics"
	"github.com/gstern-CTO/huginn/internal/security"
)

// ServerSpec describes how to launch a server for a language and what to
// tell the caller when it is missing.
type ServerSpec struct {
	Binary     string
	Args       []string
	LanguageID string
	Install    string
	// rootMarkers identify the project root the server should be given.
	RootMarkers []string
}

// serversByExtension maps a file extension to its language server. Adding a
// language is a table entry, not a code change.
var serversByExtension = map[string]ServerSpec{
	".go": {
		Binary: "gopls", Args: []string{"serve"}, LanguageID: "go",
		Install:     "go install golang.org/x/tools/gopls@latest",
		RootMarkers: []string{"go.work", "go.mod"},
	},
	".py": {
		Binary: "pyright-langserver", Args: []string{"--stdio"}, LanguageID: "python",
		Install:     "npm install -g pyright",
		RootMarkers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"},
	},
	".rs": {
		Binary: "rust-analyzer", LanguageID: "rust",
		Install:     "rustup component add rust-analyzer",
		RootMarkers: []string{"Cargo.toml"},
	},
	".ts": {
		Binary: "typescript-language-server", Args: []string{"--stdio"}, LanguageID: "typescript",
		Install:     "npm install -g typescript-language-server typescript",
		RootMarkers: []string{"tsconfig.json", "package.json"},
	},
	".js": {
		Binary: "typescript-language-server", Args: []string{"--stdio"}, LanguageID: "javascript",
		Install:     "npm install -g typescript-language-server typescript",
		RootMarkers: []string{"jsconfig.json", "package.json"},
	},
	".c": {
		Binary: "clangd", LanguageID: "c",
		Install:     "apt install clangd (or brew install llvm)",
		RootMarkers: []string{"compile_commands.json", "CMakeLists.txt", "Makefile"},
	},
	".cpp": {
		Binary: "clangd", LanguageID: "cpp",
		Install:     "apt install clangd (or brew install llvm)",
		RootMarkers: []string{"compile_commands.json", "CMakeLists.txt", "Makefile"},
	},
	".rb": {
		Binary: "solargraph", Args: []string{"stdio"}, LanguageID: "ruby",
		Install:     "gem install solargraph",
		RootMarkers: []string{"Gemfile", ".solargraph.yml"},
	},
	".lua": {
		Binary: "lua-language-server", LanguageID: "lua",
		Install:     "brew install lua-language-server",
		RootMarkers: []string{".luarc.json"},
	},
}

func init() {
	// Extension aliases share a server definition.
	alias := func(from, to string) {
		if srv, ok := serversByExtension[to]; ok {
			serversByExtension[from] = srv
		}
	}
	alias(".tsx", ".ts")
	alias(".mts", ".ts")
	alias(".cts", ".ts")
	alias(".jsx", ".js")
	alias(".mjs", ".js")
	alias(".cjs", ".js")
	alias(".cc", ".cpp")
	alias(".cxx", ".cpp")
	alias(".hpp", ".cpp")
	alias(".hh", ".cpp")
	alias(".h", ".c")
	alias(".pyi", ".py")
}

func ServerForFile(path string) (ServerSpec, bool) {
	srv, ok := serversByExtension[strings.ToLower(filepath.Ext(path))]
	return srv, ok
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 over stdio
// ---------------------------------------------------------------------------

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  any              `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// Client is a single language server process plus its request bookkeeping.
type Client struct {
	name   string
	root   string
	langID string
	proc   *security.Process
	logger *slog.Logger

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan rpcMessage

	openedMu sync.Mutex
	opened   map[string]int // uri -> document version

	closed atomic.Bool
	done   chan struct{}
}

func newClient(ctx context.Context, runner *security.Runner, srv ServerSpec, root string, logger *slog.Logger) (*Client, error) {
	proc, err := runner.Start(ctx, srv.Binary, srv.Args, root)
	if err != nil {
		return nil, err
	}
	c := &Client{
		name:    srv.Binary,
		root:    root,
		langID:  srv.LanguageID,
		proc:    proc,
		logger:  logger,
		pending: map[int64]chan rpcMessage{},
		opened:  map[string]int{},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	go c.drainStderr()

	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   PathToURI(c.root),
		"workspaceFolders": []map[string]any{
			{"uri": PathToURI(c.root), "name": filepath.Base(c.root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"definition":     map[string]any{"linkSupport": true},
				"references":     map[string]any{},
				"hover":          map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"synchronization": map[string]any{
					"didSave": false,
				},
			},
			"workspace": map[string]any{"workspaceFolders": true},
		},
	}
	if _, err := c.Call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("initialize %s: %w", c.name, err)
	}
	return c.notify("initialized", map[string]any{})
}

// readLoop parses Content-Length framed messages and routes responses to their
// waiting caller.
func (c *Client) readLoop() {
	defer close(c.done)
	reader := bufio.NewReaderSize(c.proc.Stdout, 64<<10)

	for {
		body, err := readLSPMessage(reader)
		if err != nil {
			if !c.closed.Load() && !errors.Is(err, io.EOF) {
				c.logger.Debug("lsp read loop ended", "server", c.name, "error", err)
			}
			c.failAllPending(fmt.Errorf("%s connection closed: %w", c.name, err))
			return
		}
		var msg rpcMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			continue // a notification: diagnostics, progress, logs
		}
		id, err := strconv.ParseInt(strings.Trim(string(*msg.ID), `"`), 10, 64)
		if err != nil {
			continue
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.pendingMu.Unlock()
		if ok {
			ch <- msg
			close(ch)
		}
	}
}

// drainStderr keeps the server's stderr pipe from filling and blocking it.
func (c *Client) drainStderr() {
	scanner := bufio.NewScanner(c.proc.Stderr)
	scanner.Buffer(make([]byte, 0, 8192), 1<<20)
	for scanner.Scan() {
		c.logger.Debug("lsp stderr", "server", c.name, "line", scanner.Text())
	}
}

func (c *Client) failAllPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, ch := range c.pending {
		ch <- rpcMessage{Error: &rpcError{Code: -1, Message: err.Error()}}
		close(ch)
		delete(c.pending, id)
	}
}

func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("%s is closed", c.name)
	}
	id := c.nextID.Add(1)
	ch := make(chan rpcMessage, 1)

	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	rawID := json.RawMessage(strconv.FormatInt(id, 10))
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: &rawID, Method: method, Params: params}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
}

func (c *Client) notify(method string, params any) error {
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(msg rpcMessage) error {
	msg.JSONRPC = "2.0"
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.proc.Stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.proc.Stdin.Write(body)
	return err
}

// ensureOpen sends textDocument/didOpen once per file. Language servers answer
// position queries only for documents they have been told about.
func (c *Client) EnsureOpen(path string) error {
	uri := PathToURI(path)
	c.openedMu.Lock()
	_, already := c.opened[uri]
	if !already {
		c.opened[uri] = 1
	}
	c.openedMu.Unlock()
	if already {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": c.langID,
			"version":    1,
			"text":       string(content),
		},
	})
}

func (c *Client) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	// A best-effort graceful shutdown; the process is killed regardless.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = c.Call(ctx, "shutdown", nil)
	_ = c.notify("exit", nil)

	_ = c.proc.Stdin.Close()
	if c.proc.Cmd.Process != nil {
		_ = c.proc.Cmd.Process.Kill()
	}
	_ = c.proc.Cmd.Wait()
}

// readLSPMessage reads one Content-Length framed JSON-RPC message.
func readLSPMessage(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if name, value, ok := strings.Cut(line, ":"); ok {
			if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					return nil, fmt.Errorf("bad Content-Length %q", value)
				}
				contentLength = n
			}
		}
	}
	if contentLength < 0 {
		return nil, errors.New("message without Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func PathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

func URIToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	if u.Scheme != "file" {
		return uri
	}
	return filepath.FromSlash(u.Path)
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager owns the pool of running language servers, keyed by binary and
// project root so a server is reused across calls within a session.
type Manager struct {
	runner  *security.Runner
	guard   *security.PathGuard
	metrics *metrics.Metrics
	logger  *slog.Logger

	mu      sync.Mutex
	clients map[string]*Client
}

func NewManager(runner *security.Runner, guard *security.PathGuard, metrics *metrics.Metrics, logger *slog.Logger) *Manager {
	return &Manager{
		runner:  runner,
		guard:   guard,
		metrics: metrics,
		logger:  logger,
		clients: map[string]*Client{},
	}
}

// clientFor returns a running server for the file, starting one if needed.
func (m *Manager) ClientFor(ctx context.Context, srv ServerSpec, path string) (*Client, error) {
	root := m.projectRoot(path, srv.RootMarkers)
	key := srv.Binary + "\x00" + root

	m.mu.Lock()
	if c, ok := m.clients[key]; ok && !c.closed.Load() {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	// Starting a server is slow (gopls indexes the module), so it happens
	// outside the lock; a duplicate start is resolved below.
	client, err := newClient(ctx, m.runner, srv, root, m.logger)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, ok := m.clients[key]; ok && !existing.closed.Load() {
		m.mu.Unlock()
		client.Close()
		return existing, nil
	}
	m.clients[key] = client
	m.metrics.SetActiveLSPServers(len(m.clients))
	m.mu.Unlock()
	return client, nil
}

// projectRoot walks up from the file looking for a language-specific marker,
// stopping at the workspace boundary so a server is never rooted outside it.
func (m *Manager) projectRoot(path string, markers []string) string {
	dir := filepath.Dir(path)
	boundary := m.guard.PrimaryRoot()
	all := append(append([]string{}, markers...), ".git")

	current := dir
	for {
		for _, marker := range all {
			if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current || !security.WithinRoot(current, boundary) {
			break
		}
		current = parent
	}
	if security.WithinRoot(dir, boundary) {
		return dir
	}
	return boundary
}

// Shutdown stops every language server. Child processes must not outlive the
// MCP server.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for key, c := range m.clients {
		clients = append(clients, c)
		delete(m.clients, key)
	}
	m.metrics.SetActiveLSPServers(0)
	m.mu.Unlock()

	for _, c := range clients {
		c.Close()
	}
}
