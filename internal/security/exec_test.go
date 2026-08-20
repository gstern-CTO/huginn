package security

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunnerRejectsBinariesOffTheAllowlist(t *testing.T) {
	runner := NewRunner(1 << 20)

	for _, name := range []string{"bash", "sh", "curl", "python3", "rm", "env"} {
		_, err := runner.Run(context.Background(), name, []string{"--version"}, "")
		require.ErrorIs(t, err, ErrBinaryNotAllowed, "expected %s to be rejected", name)
	}
}

// The allowlist must be checked against a bare name, so neither an absolute
// path nor a traversal can smuggle in a different executable.
func TestRunnerRejectsPathsAndShellSyntaxAsBinaryName(t *testing.T) {
	runner := NewRunner(1 << 20)

	for _, name := range []string{
		"/bin/sh",
		"./rg",
		"../../bin/rg",
		"rg; rm -rf /",
		"rg && curl evil.example",
		"rg | tee /tmp/x",
		"",
	} {
		_, err := runner.Run(context.Background(), name, nil, "")
		require.ErrorIs(t, err, ErrBinaryNotAllowed, "expected %q to be rejected", name)
	}
}

// Arguments are passed as an array, never interpolated into a shell string, so
// metacharacters inside a search pattern are inert data.
func TestRunnerTreatsShellMetacharactersInArgsAsData(t *testing.T) {
	runner := NewRunner(1 << 20)
	if !runner.Available("rg") {
		t.Skip("ripgrep is not installed")
	}

	dir := t.TempDir()
	canary := filepath.Join(dir, "canary.txt")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "code.txt"), []byte("harmless content\n"), 0o644))

	// If any of these were passed through a shell, the canary would appear.
	injections := []string{
		"x; touch " + canary,
		"$(touch " + canary + ")",
		"`touch " + canary + "`",
		"x && touch " + canary,
	}
	for _, pattern := range injections {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := runner.Run(ctx, "rg", []string{"--fixed-strings", "--", pattern, dir}, dir)
		cancel()
		require.NoError(t, err)

		_, statErr := os.Stat(canary)
		require.True(t, os.IsNotExist(statErr), "command injection succeeded for %q", pattern)
	}
}

func TestRunnerRejectsMutatingGitSubcommands(t *testing.T) {
	runner := NewRunner(1 << 20)
	if !runner.Available("git") {
		t.Skip("git is not installed")
	}

	for _, sub := range []string{"push", "commit", "clean", "reset", "checkout"} {
		_, err := runner.Run(context.Background(), "git", []string{sub}, t.TempDir())
		require.ErrorIs(t, err, ErrBinaryNotAllowed, "expected git %s to be rejected", sub)
	}
}

func TestRunnerCapsOutput(t *testing.T) {
	buf := &cappedBuffer{limit: 10}

	n, err := buf.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)
	require.Equal(t, 16, n, "the writer must report a full write so the child is not blocked")
	require.Equal(t, "0123456789", buf.String())
	require.True(t, buf.truncated)

	// Further writes are absorbed rather than stored.
	_, err = buf.Write([]byte("more"))
	require.NoError(t, err)
	require.Equal(t, "0123456789", buf.String())
}

// The child environment must not carry this process's credentials.
func TestMinimalEnvExcludesSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_thisisasecrettokenvalue000000000000")
	t.Setenv("DATABRICKS_TOKEN", "dapi00000000000000000000000000000000")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "abcdefghijklmnopqrstuvwxyz0123456789ABCD")

	env := strings.Join(minimalEnv(), "\n")
	require.NotContains(t, env, "GITHUB_TOKEN")
	require.NotContains(t, env, "DATABRICKS_TOKEN")
	require.NotContains(t, env, "AWS_SECRET_ACCESS_KEY")
	require.Contains(t, env, "PATH=")
}

func TestRunnerNonZeroExitIsNotAnError(t *testing.T) {
	runner := NewRunner(1 << 20)
	if !runner.Available("rg") {
		t.Skip("ripgrep is not installed")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644))

	// ripgrep exits 1 when nothing matches: an empty result, not a failure.
	res, err := runner.Run(context.Background(), "rg", []string{"--", "no-such-content-anywhere", dir}, dir)
	require.NoError(t, err)
	require.Equal(t, 1, res.ExitCode)
	require.Empty(t, strings.TrimSpace(res.Stdout))
}
