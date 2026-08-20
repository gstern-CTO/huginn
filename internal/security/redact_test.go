package security

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactorCatchesCredentialShapes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		// leak is the substring that must not survive.
		leak string
	}{
		{"aws access key", `aws_key = "AKIAIOSFODNN7EXAMPLE"`, "AKIAIOSFODNN7EXAMPLE"},
		{"aws temporary key", `key: ASIAIOSFODNN7EXAMPLE`, "ASIAIOSFODNN7EXAMPLE"},
		{"aws secret", `aws_secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		{"github classic token", `token: ghp_016C7f6a9B2c3D4e5F6a7B8c9D0e1F2a3B4c5D`, "ghp_016C7f6a9B2c3D4e5F6a7B8c9D0e1F2a3B4c5D"},
		{"github fine-grained pat", `GITHUB_TOKEN=github_pat_11ABCDEFG0abcdefghijkl_KwjQ2h3Ff8sT5nVxYzAbCdEfGh`, "github_pat_11ABCDEFG0abcdefghijkl"},
		{"anthropic key", `key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345"`, "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345"},
		{"openai key", `OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH`, "sk-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH"},
		{"slack token", `xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx`, "xoxb-123456789012"},
		{"stripe key", `sk_live_abcdefghijklmnopqrstuvwx`, "sk_live_abcdefghijklmnopqrstuvwx"},
		{"google api key", `AIzaSyA1234567890abcdefghijklmnopqrstuvw`, "AIzaSyA1234567890abcdefghijklmnopqrstuvw"},
		{"databricks token", `DATABRICKS_TOKEN=dapi1234567890abcdef1234567890abcdef`, "dapi1234567890abcdef1234567890abcdef"},
		{"jwt", `Authorization: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk`, "eyJhbGciOiJIUzI1NiIs"},
		{"bearer header", `Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345`, "abcdefghijklmnopqrstuvwxyz012345"},
		{"basic auth", `Authorization: Basic dXNlcjpzdXBlcnNlY3JldA==`, "dXNlcjpzdXBlcnNlY3JldA"},
		{"url credentials", `postgres://admin:hunter2secret@db.internal:5432/app`, "hunter2secret"},
		{"generic password", `password: "correct horse battery"`, "correct horse battery"},
		{"generic api key", `api_key = 'abcd1234efgh5678ijkl'`, "abcd1234efgh5678ijkl"},
		{"npm token", `//registry.npmjs.org/:_authToken=npm_abcdefghijklmnopqrstuvwxyz0123456789`, "npm_abcdefghijklmnopqrstuvwxyz0123456789"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, count := DefaultRedactor.Redact(tc.input)
			require.NotContains(t, out, tc.leak, "secret survived redaction")
			require.Contains(t, out, redactedMarker)
			require.Positive(t, count, "the redaction count must be reported to the caller")
		})
	}
}

func TestRedactorHandlesPrivateKeyBlocks(t *testing.T) {
	input := `before
-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAx7Vv0mS9nQ0YQ8pF3Kk2lLmN1oP4qR5sT6uV7wX8yZ9aB0cD
1eF2gH3iJ4kL5mN6oP7qR8sT9uV0wX1yZ2aB3cD4eF5gH6iJ7kL8mN9oP0qR1sT
-----END RSA PRIVATE KEY-----
after`

	out, count := DefaultRedactor.Redact(input)
	require.NotContains(t, out, "MIIEowIBAAKCAQEA")
	require.NotContains(t, out, "BEGIN RSA PRIVATE KEY")
	require.Contains(t, out, "before")
	require.Contains(t, out, "after")
	require.Equal(t, 1, count)
}

// Redaction must not destroy ordinary source code. A false positive here is
// silently corrupted content, which is worse than an obvious failure.
func TestRedactorLeavesOrdinaryCodeIntact(t *testing.T) {
	safe := []string{
		`func GetToken(ctx context.Context) (string, error) {`,
		`// The password field is validated by the caller.`,
		`const commitSHA = "e83c5163316f89bfbde7d9ab23ca2e25604af290"`, // 40 hex: a git SHA
		`sha256 = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"`,
		`import "github.com/google/go-github/v66/github"`,
		`var errNotFound = errors.New("not found")`,
		`x := strings.Repeat("abcdefghij", 8)`,
		`type ConfigurationParameterCollection struct{}`,
	}
	for _, line := range safe {
		out, count := DefaultRedactor.Redact(line)
		require.Equal(t, line, out, "redactor altered ordinary code")
		require.Zero(t, count)
	}
}

// Hex digests top out at 4.0 bits of entropy per character, below the 4.5
// threshold, so they survive while base64-shaped secrets do not.
func TestHighEntropyDetection(t *testing.T) {
	require.False(t, isHighEntropySecret("e83c5163316f89bfbde7d9ab23ca2e25604af290"), "git SHA must survive")
	require.False(t, isHighEntropySecret(strings.Repeat("a", 60)), "a repeated character is not a secret")
	require.False(t, isHighEntropySecret("this_is_a_very_long_snake_case_identifier_name_here"), "identifiers must survive")
	require.False(t, isHighEntropySecret("short"), "short strings are never secrets")

	require.True(t, isHighEntropySecret("Xk9mQ2pL7vR4tY6wZ1nB8cV3aS5dF0gH2jK4lM6nP8qR0sT2uW4x"))
}

func TestRedactorIsIdempotent(t *testing.T) {
	input := `token: ghp_016C7f6a9B2c3D4e5F6a7B8c9D0e1F2a3B4c5D`

	once, firstCount := DefaultRedactor.Redact(input)
	twice, secondCount := DefaultRedactor.Redact(once)

	require.Equal(t, once, twice, "re-redacting must not change already-clean text")
	require.Positive(t, firstCount)
	require.Zero(t, secondCount, "already-redacted text must not inflate the count")
}

func TestRedactAllAccumulatesCount(t *testing.T) {
	in := []string{
		`AKIAIOSFODNN7EXAMPLE`,
		`nothing secret here`,
		`ghp_016C7f6a9B2c3D4e5F6a7B8c9D0e1F2a3B4c5D`,
	}
	out, total := DefaultRedactor.RedactAll(in)
	require.Len(t, out, 3)
	require.Equal(t, 2, total)
	require.Equal(t, "nothing secret here", out[1])
}

// Patterns are compiled once at initialisation, not per call: the redactor runs
// over every byte a research session returns.
func BenchmarkRedactorOnSourceFile(b *testing.B) {
	content := strings.Repeat("func handleRequest(w http.ResponseWriter, r *http.Request) error {\n\treturn nil\n}\n", 200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = DefaultRedactor.Redact(content)
	}
}
