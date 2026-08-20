package security

import (
	"math"
	"regexp"
	"strings"
	"unicode"
)

const redactedMarker = "[REDACTED]"

// secretAssignment is the shared prefix for the generic `<credential> = value`
// detectors: a credential-ish key name followed by an assignment operator.
const secretAssignment = `(?i)\b(?:api[_-]?key|apikey|secret[_-]?key|secret|client[_-]?secret|access[_-]?token|auth[_-]?token|private[_-]?token|password|passwd|pwd|credential)\b\s*[:=]\s*`

// secretPattern is one named detector. Patterns are compiled exactly once, at
// package initialisation, into the package-level redactor. Compiling a regex
// costs milliseconds; running a compiled one costs microseconds, and a research
// session scans a great deal of content (WEAKNESSES.md #3).
type secretPattern struct {
	name string
	re   *regexp.Regexp
	// group, when non-zero, restricts replacement to that submatch so the
	// surrounding context (e.g. `api_key = `) survives in the output.
	group int
}

// DefaultRedactor is built at init and shared by every tool. It is immutable
// after construction and therefore safe for concurrent use.
var DefaultRedactor = newRedactor()

// Redactor replaces credential-shaped substrings with a marker and reports how
// many replacements it made.
type Redactor struct {
	patterns []secretPattern
	// entropy scanning is a separate pass over token-shaped candidates.
	candidate *regexp.Regexp
}

func newRedactor() *Redactor {
	p := func(name, expr string, group int) secretPattern {
		return secretPattern{name: name, re: regexp.MustCompile(expr), group: group}
	}
	return &Redactor{
		patterns: []secretPattern{
			// Private key blocks first: they are multi-line and would
			// otherwise be partially matched by narrower patterns.
			p("private-key-block", `(?s)-----BEGIN[ A-Z0-9]*PRIVATE KEY-----.*?-----END[ A-Z0-9]*PRIVATE KEY-----`, 0),

			p("aws-access-key", `\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`, 0),
			p("aws-secret-key", `(?i)\baws_?secret_?access_?key\b\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})["']?`, 1),
			p("aws-session-token", `(?i)\baws_?session_?token\b\s*[:=]\s*["']?([A-Za-z0-9/+=]{100,})["']?`, 1),

			p("github-token", `\bgh[pousr]_[A-Za-z0-9]{36,255}\b`, 0),
			p("github-pat", `\bgithub_pat_[A-Za-z0-9_]{22,255}\b`, 0),

			p("anthropic-key", `\bsk-ant-[A-Za-z0-9\-_]{20,}\b`, 0),
			p("openai-project-key", `\bsk-proj-[A-Za-z0-9\-_]{20,}\b`, 0),
			p("openai-key", `\bsk-[A-Za-z0-9]{32,}\b`, 0),

			p("slack-token", `\bxox[baprs]-[A-Za-z0-9-]{10,}\b`, 0),
			p("stripe-key", `\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`, 0),
			p("google-api-key", `\bAIza[0-9A-Za-z\-_]{35}\b`, 0),
			p("databricks-token", `\bdapi[0-9a-f]{32}(?:-\d+)?\b`, 0),
			p("npm-token", `\bnpm_[A-Za-z0-9]{36}\b`, 0),

			// A GCP service-account JSON is identified by its type marker;
			// redact the private_key_id / private_key fields it carries.
			p("gcp-service-account", `(?i)"type"\s*:\s*"service_account"`, 0),
			p("gcp-private-key-id", `(?i)"private_key_id"\s*:\s*"([A-Za-z0-9]{16,})"`, 1),

			p("jwt", `\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`, 0),
			p("bearer-token", `(?i)\b(?:bearer|token)\s+([A-Za-z0-9\-._~+/]{20,}={0,2})`, 1),
			p("basic-auth-header", `(?i)\bbasic\s+([A-Za-z0-9+/]{16,}={0,2})`, 1),

			// URLs that embed credentials: scheme://user:secret@host
			p("url-credentials", `(?i)\b[a-z][a-z0-9+.-]*://[^\s:/@]+:([^\s/@]{3,})@`, 1),

			// Generic `secret = "..."` assignments across config formats. The
			// quoted forms come first and match through spaces, because a
			// passphrase is frequently several words; the unquoted form then
			// catches the bare-token case.
			p("generic-assignment-double-quoted", secretAssignment+`"([^"\n]{4,})"`, 1),
			p("generic-assignment-single-quoted", secretAssignment+`'([^'\n]{4,})'`, 1),
			p("generic-assignment", secretAssignment+`([^\s"',;]{8,})`, 1),
		},
		// Entropy candidates: long unbroken runs of token-ish characters.
		candidate: regexp.MustCompile(`[A-Za-z0-9+/_\-]{40,}={0,2}`),
	}
}

// Redact scrubs s and returns the cleaned text plus the number of secrets
// replaced. The count is surfaced in response metadata so the caller knows
// redaction happened rather than silently receiving altered content.
func (r *Redactor) Redact(s string) (string, int) {
	if s == "" {
		return s, 0
	}
	count := 0
	out := s
	for _, pat := range r.patterns {
		pat := pat
		out = pat.re.ReplaceAllStringFunc(out, func(match string) string {
			// Already-redacted text must not be counted twice.
			if strings.Contains(match, redactedMarker) {
				return match
			}
			if pat.group == 0 {
				count++
				return redactedMarker
			}
			idx := pat.re.FindStringSubmatchIndex(match)
			if idx == nil || len(idx) <= 2*pat.group+1 || idx[2*pat.group] < 0 {
				return match
			}
			start, end := idx[2*pat.group], idx[2*pat.group+1]
			count++
			return match[:start] + redactedMarker + match[end:]
		})
	}

	// High-entropy pass. Deliberately conservative: only long candidates whose
	// Shannon entropy exceeds a hex string's theoretical maximum (4.0 bits per
	// character) qualify, so git SHAs, checksums and hex digests are preserved
	// while base64-shaped credentials are caught.
	out = r.candidate.ReplaceAllStringFunc(out, func(match string) string {
		if strings.Contains(match, redactedMarker) || !isHighEntropySecret(match) {
			return match
		}
		count++
		return redactedMarker
	})

	return out, count
}

// RedactAll scrubs a slice in place-ish, returning the cleaned copies and the
// total replacement count.
func (r *Redactor) RedactAll(in []string) ([]string, int) {
	out := make([]string, len(in))
	total := 0
	for i, s := range in {
		cleaned, n := r.Redact(s)
		out[i] = cleaned
		total += n
	}
	return out, total
}

// isHighEntropySecret decides whether a token-shaped string looks like a
// credential rather than an identifier, hash, or chunk of encoded data that a
// developer legitimately wants to see.
func isHighEntropySecret(s string) bool {
	if len(s) < 40 {
		return false
	}
	var digits, upper, lower, special int
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsUpper(r):
			upper++
		case unicode.IsLower(r):
			lower++
		default:
			special++
		}
	}
	// Require genuine mixed case plus digits: pure-hex digests, base64 of
	// ASCII text, and snake_case identifiers all fail this test.
	if digits == 0 || upper == 0 || lower == 0 {
		return false
	}
	// Long identifier-ish runs separated by underscores or dashes are names,
	// not secrets.
	if special > len(s)/4 {
		return false
	}
	return shannonEntropy(s) > 4.5
}

// shannonEntropy returns the per-character entropy of s in bits.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]int
	n := 0
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
		n++
	}
	entropy := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / float64(n)
		entropy -= p * math.Log2(p)
	}
	return entropy
}
