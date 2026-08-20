package security

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

// newTestGuard builds a guard over a temporary workspace and returns both.
func newTestGuard(t *testing.T, allowed ...string) (*PathGuard, string) {
	t.Helper()
	// The temp dir is resolved because macOS returns /var, a symlink to
	// /private/var, which would otherwise make every comparison fail.
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o644))

	guard, err := NewPathGuard(root, allowed)
	require.NoError(t, err)
	return guard, root
}

func TestPathGuardAllowsPathsInsideWorkspace(t *testing.T) {
	guard, root := newTestGuard(t)

	for _, path := range []string{
		filepath.Join(root, "src", "main.go"),
		filepath.Join(root, "src", "pkg"),
		root,
		// A traversal that stays inside after cleaning is legitimate.
		filepath.Join(root, "src", "..", "src", "main.go"),
	} {
		resolved, tErr := guard.Validate(path)
		require.Nil(t, tErr, "expected %s to be allowed", path)
		require.True(t, WithinRoot(resolved, root))
	}
}

func TestPathGuardRejectsTraversalEscape(t *testing.T) {
	guard, root := newTestGuard(t)

	escapes := []string{
		filepath.Join(root, "..", "..", "etc", "passwd"),
		filepath.Join(root, "src", "..", "..", "outside.txt"),
		"/etc/passwd",
		"/",
	}
	for _, path := range escapes {
		_, tErr := guard.Validate(path)
		require.NotNil(t, tErr, "expected %s to be rejected", path)
		require.Equal(t, protocol.CodePathDenied, tErr.Code)
		require.False(t, tErr.Retryable, "a denied path must not invite a retry")
		require.NotEmpty(t, tErr.Hint)
	}
}

// A symlink inside the workspace pointing out of it is the case that a
// pre-resolution check would miss. Resolution must happen first, and the result
// must be rejected rather than clamped back inside.
func TestPathGuardRejectsSymlinkEscapeAfterResolution(t *testing.T) {
	guard, root := newTestGuard(t)

	outsideDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	secret := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("token"), 0o600))

	link := filepath.Join(root, "escape.txt")
	require.NoError(t, os.Symlink(secret, link))

	_, tErr := guard.Validate(link)
	require.NotNil(t, tErr, "a symlink out of the workspace must be rejected")
	require.Equal(t, protocol.CodePathDenied, tErr.Code)
	require.Equal(t, secret, tErr.Details["resolvedPath"],
		"the error should name the resolved target, not the link")
}

func TestPathGuardRejectsSymlinkedDirectoryEscape(t *testing.T) {
	guard, root := newTestGuard(t)

	outsideDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "data.txt"), []byte("x"), 0o600))

	require.NoError(t, os.Symlink(outsideDir, filepath.Join(root, "linkdir")))

	_, tErr := guard.Validate(filepath.Join(root, "linkdir", "data.txt"))
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodePathDenied, tErr.Code)
}

func TestPathGuardAllowsSymlinkStayingInsideWorkspace(t *testing.T) {
	guard, root := newTestGuard(t)

	target := filepath.Join(root, "src", "main.go")
	link := filepath.Join(root, "alias.go")
	require.NoError(t, os.Symlink(target, link))

	resolved, tErr := guard.Validate(link)
	require.Nil(t, tErr)
	require.Equal(t, target, resolved, "the resolved target is what callers must open")
}

func TestPathGuardHonoursAdditionalAllowedPaths(t *testing.T) {
	extra, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(extra, "shared.go"), []byte("package shared\n"), 0o644))

	guard, _ := newTestGuard(t, extra)

	resolved, tErr := guard.Validate(filepath.Join(extra, "shared.go"))
	require.Nil(t, tErr)
	require.Equal(t, filepath.Join(extra, "shared.go"), resolved)
}

func TestPathGuardBlocksCredentialFiles(t *testing.T) {
	guard, root := newTestGuard(t)

	blocked := []string{
		".env",
		".env.production",
		"server.pem",
		"private.key",
		"terraform.tfstate",
		"id_ed25519",
		".netrc",
		"credentials.json",
	}
	for _, name := range blocked {
		path := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))

		_, tErr := guard.Validate(path)
		require.NotNil(t, tErr, "expected %s to be blocked", name)
		require.Equal(t, protocol.CodePathDenied, tErr.Code)
	}
}

func TestPathGuardBlocksCredentialDirectories(t *testing.T) {
	guard, root := newTestGuard(t)

	for _, dir := range []string{".aws", ".kube", ".ssh"} {
		path := filepath.Join(root, dir, "config")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

		_, tErr := guard.Validate(path)
		require.NotNil(t, tErr, "expected %s to be blocked", path)
		require.Equal(t, protocol.CodePathDenied, tErr.Code)
	}
}

// A sibling directory sharing a name prefix must not be mistaken for a child.
func TestWithinRootRejectsPrefixSibling(t *testing.T) {
	require.False(t, WithinRoot("/workspace-other/file.go", "/workspace"))
	require.True(t, WithinRoot("/workspace/file.go", "/workspace"))
	require.True(t, WithinRoot("/workspace", "/workspace"))
	require.False(t, WithinRoot("/", "/workspace"))
}

func TestPathGuardRejectsEmptyAndNulPaths(t *testing.T) {
	guard, _ := newTestGuard(t)

	_, tErr := guard.Validate("")
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeInvalidInput, tErr.Code)

	_, tErr = guard.Validate("file\x00.go")
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeInvalidInput, tErr.Code)
}

// A path that does not exist yet still has to be located relative to the
// boundary, via its deepest existing ancestor.
func TestResolveExistingHandlesMissingLeaf(t *testing.T) {
	guard, root := newTestGuard(t)

	resolved, tErr := guard.Validate(filepath.Join(root, "src", "not-created-yet.go"))
	require.Nil(t, tErr)
	require.Equal(t, filepath.Join(root, "src", "not-created-yet.go"), resolved)

	_, tErr = guard.Validate(filepath.Join(root, "..", "nope", "still-outside.go"))
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodePathDenied, tErr.Code)
}

func TestValidateDirRejectsFiles(t *testing.T) {
	guard, root := newTestGuard(t)

	_, tErr := guard.ValidateDir(filepath.Join(root, "src", "main.go"))
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeInvalidInput, tErr.Code)

	resolved, tErr := guard.ValidateDir(filepath.Join(root, "src"))
	require.Nil(t, tErr)
	require.Equal(t, filepath.Join(root, "src"), resolved)
}
