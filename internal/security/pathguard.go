package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

// blockedPatterns are refused regardless of whether the path sits inside the
// workspace. Each entry is matched with filepath.Match against every path
// component as well as the full base name.
var blockedPatterns = []string{
	".env",
	".env.*",
	"*.pem",
	"*.key",
	"*.p12",
	"*.pfx",
	"*.jks",
	"*.keystore",
	"*.tfstate",
	"*.tfstate.*",
	"*.kdbx",
	"id_rsa",
	"id_dsa",
	"id_ecdsa",
	"id_ed25519",
	".netrc",
	".npmrc",
	".pypirc",
	".htpasswd",
	"credentials",
	"credentials.json",
	"service-account*.json",
	"secrets.y*ml",
	"*.jceks",
}

// blockedDirs are directory names that must not appear anywhere in a resolved
// path. These hold credentials for entire toolchains.
var blockedDirs = []string{
	".aws",
	".kube",
	".ssh",
	".gnupg",
	".docker",
	".azure",
	".config/gcloud",
}

// PathGuard enforces the workspace boundary for every local filesystem
// operation. It is constructed once at startup and is safe for concurrent use.
type PathGuard struct {
	// roots are the canonical (symlink-resolved) directories a path may
	// resolve into: the workspace root plus any explicitly allowed paths.
	roots []string
}

// NewPathGuard resolves the workspace root and allowed paths to their canonical
// forms. Resolving them up front means the per-request check compares two
// already-canonical paths.
func NewPathGuard(workspaceRoot string, allowed []string) (*PathGuard, error) {
	g := &PathGuard{}
	add := func(p string) error {
		if p == "" {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", p, err)
		}
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return fmt.Errorf("absolutise %s: %w", resolved, err)
		}
		g.roots = append(g.roots, filepath.Clean(abs))
		return nil
	}
	if err := add(workspaceRoot); err != nil {
		return nil, err
	}
	for _, p := range allowed {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	if len(g.roots) == 0 {
		return nil, fmt.Errorf("path guard needs at least one root")
	}
	return g, nil
}

// Validate canonicalises the requested path and confirms it lies inside an
// allowed root. It returns the resolved absolute path to use for the actual
// filesystem operation — callers must use the returned path, not the input,
// so that the check and the access refer to the same file.
//
// A path that escapes the boundary is rejected outright. It is never clamped
// back into the workspace: silently reading a different file than the one
// requested is worse than an error.
func (g *PathGuard) Validate(path string) (string, *protocol.ToolError) {
	if strings.TrimSpace(path) == "" {
		return "", protocol.ErrInvalidInput("path must not be empty")
	}
	if strings.ContainsRune(path, 0) {
		return "", protocol.ErrInvalidInput("path contains a NUL byte")
	}

	expanded, err := ExpandPath(path)
	if err != nil {
		return "", protocol.ErrInvalidInput("cannot expand path %q: %v", path, err)
	}

	// Symlink resolution is mandatory and must happen before the boundary
	// check: a symlink inside the workspace pointing at /etc/shadow is only
	// detectable after resolution. Paths that do not exist yet are resolved
	// through their deepest existing ancestor.
	resolved, err := resolveExisting(expanded)
	if err != nil {
		return "", protocol.ErrInvalidInput("cannot resolve path %q: %v", path, err)
	}

	if !g.withinRoots(resolved) {
		return "", protocol.NewError(protocol.CodePathDenied, false,
			fmt.Sprintf("Request a path inside the workspace (%s). If this path is genuinely needed, add it to ALLOWED_PATHS and restart the server.", g.roots[0]),
			"path %q resolves to %q, which is outside the allowed workspace", path, resolved).
			WithDetail("resolvedPath", resolved).
			WithDetail("allowedRoots", g.roots)
	}

	if reason, blocked := IsBlockedPath(resolved); blocked {
		return "", protocol.NewError(protocol.CodePathDenied, false,
			"This file class is blocked because it typically holds credentials. There is no override; read the code that consumes the secret instead.",
			"path %q is blocked: %s", path, reason).
			WithDetail("resolvedPath", resolved)
	}

	return resolved, nil
}

// ValidateDir is Validate plus a directory check.
func (g *PathGuard) ValidateDir(path string) (string, *protocol.ToolError) {
	resolved, tErr := g.Validate(path)
	if tErr != nil {
		return "", tErr
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", protocol.NewError(protocol.CodeNotFound, false,
			"Check the path exists; use the local file finder to locate it.",
			"cannot stat %q: %v", path, err)
	}
	if !info.IsDir() {
		return "", protocol.ErrInvalidInput("%q is a file, not a directory", path)
	}
	return resolved, nil
}

// Roots exposes the configured boundary for error messages and diagnostics.
func (g *PathGuard) Roots() []string { return append([]string(nil), g.roots...) }

// PrimaryRoot is the workspace root: the default location for operations that
// take no explicit path.
func (g *PathGuard) PrimaryRoot() string { return g.roots[0] }

func (g *PathGuard) withinRoots(path string) bool {
	for _, root := range g.roots {
		if WithinRoot(path, root) {
			return true
		}
	}
	return false
}

// withinRoot reports whether path is root or lies beneath it. It compares
// cleaned paths component-wise, so a sibling directory that merely shares a
// name prefix (/work vs /workspace) is correctly excluded.
func WithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolveExisting resolves symlinks for a path that may not exist yet, by
// resolving the deepest ancestor that does exist and re-appending the missing
// tail. Without this, validating a not-yet-created path would fail outright.
func resolveExisting(path string) (string, error) {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved), nil
	}

	var missing []string
	current := path
	for {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without finding anything.
			return path, nil
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Clean(filepath.Join(append([]string{resolved}, missing...)...)), nil
		}
		current = parent
	}
}

// isBlockedPath matches the resolved path against the credential blocklist.
func IsBlockedPath(path string) (string, bool) {
	normalised := filepath.ToSlash(path)

	for _, dir := range blockedDirs {
		needle := "/" + dir + "/"
		if strings.Contains(normalised, needle) || strings.HasSuffix(normalised, "/"+dir) {
			return "it is inside a " + dir + " directory", true
		}
	}

	base := filepath.Base(path)
	for _, pattern := range blockedPatterns {
		if ok, err := filepath.Match(pattern, base); err == nil && ok {
			return "the name matches the blocked pattern " + pattern, true
		}
	}
	return "", false
}

// ExpandPath resolves ~, expands environment variables, and returns a cleaned
// absolute path. Both the config loader and the path guard canonicalise through
// this single function so they cannot disagree about what a path means.
func ExpandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	p = os.ExpandEnv(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
