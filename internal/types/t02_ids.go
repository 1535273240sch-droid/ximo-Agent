package types

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync/atomic"
)

// ID prefixes. Keeping the prefix in the identifier makes event logs and crash
// dumps readable without a lookup table.
const (
	PrefixRun      = "run"
	PrefixSession  = "ses"
	PrefixTask     = "task"
	PrefixEvent    = "evt"
	PrefixToolCall = "call"
	PrefixLease    = "lease"
	PrefixActor    = "actor"
)

var idCounter atomic.Uint64

// NewID returns a lexicographically unsorted but collision-resistant
// identifier such as "run_3f9a1c8d2b4e6f70". crypto/rand is used so that IDs
// cannot be predicted from one another; the counter suffix keeps IDs unique
// even if the entropy source is unavailable and returns zeros.
func NewID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		n := idCounter.Add(1)
		for i := 0; i < 8; i++ {
			buf[i] = byte(n >> (8 * (7 - i)))
		}
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

// Redaction markers.
const redactedMarker = "***REDACTED***"

// secretPatterns are the credential markers redaction looks for. Each marks the
// *start* of a secret: everything after it, up to a delimiter, is masked.
//
// The list is deliberately conservative — it must never mask ordinary prose,
// because redaction runs over user-visible error text.
var secretPatterns = []string{
	"sk-", "sk_", "api_key=", "api-key=", "apikey=",
	"access_token=", "refresh_token=", "token=", "password=", "secret=",
	"authorization:", "authorization=", "bearer ", "basic ",
}

// authSchemes are the words that may sit between an authentication header name
// and its credential ("Authorization: Bearer <token>"). They are consumed along
// with the prefix so the credential itself is what gets masked, rather than the
// scheme name.
var authSchemes = []string{"bearer", "basic", "token", "digest"}

// secretDelimiters end a secret. Anything outside this set is treated as part of
// the credential, so a token is masked in full even when it contains hyphens or
// dots.
func isSecretDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', ',', ';', '}', ')', ']', '&', '?':
		return true
	default:
		return false
	}
}

// RedactString masks anything that looks like a credential. It is applied to
// every error message produced by types.NewError/WrapError, so secrets cannot
// leak through the error path into logs, event payloads or the UI (doc ch. 21).
func RedactString(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	for _, p := range secretPatterns {
		idx := strings.Index(lower, p)
		for idx >= 0 {
			// Everything from the marker onwards is suspect; find where the
			// credential starts.
			start := idx + len(p)
			// Consume separators, then any authentication scheme word, then
			// separators again. Without the scheme step, "Authorization: Bearer
			// <token>" would mask the two header words and leave the token in
			// clear.
			for {
				before := start
				start = skipSeparators(s, start)
				start = skipAuthScheme(s, start)
				if start == before {
					break
				}
			}
			if start >= len(s) {
				break
			}

			end := len(s)
			for i := start; i < len(s); i++ {
				if isSecretDelimiter(s[i]) {
					end = i
					break
				}
			}
			if end <= start {
				// Nothing to mask after the marker; move past it.
				next := strings.Index(lower[idx+len(p):], p)
				if next < 0 {
					break
				}
				idx = idx + len(p) + next
				continue
			}

			s = s[:start] + redactedMarker + s[end:]
			lower = strings.ToLower(s)
			searchFrom := start + len(redactedMarker)
			if searchFrom >= len(lower) {
				break
			}
			next := strings.Index(lower[searchFrom:], p)
			if next < 0 {
				break
			}
			idx = searchFrom + next
		}
	}
	return s
}

// skipSeparators advances past spaces and the punctuation that joins a header
// name to its value.
func skipSeparators(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', ':', '=', '"', '\'':
			i++
			continue
		}
		break
	}
	return i
}

// skipAuthScheme advances past an authentication scheme word, if one is present.
func skipAuthScheme(s string, i int) int {
	rest := s[i:]
	lower := strings.ToLower(rest)
	for _, scheme := range authSchemes {
		if strings.HasPrefix(lower, scheme) {
			after := i + len(scheme)
			// Only treat it as a scheme if a separator follows, so a token that
			// merely starts with those letters is not consumed as one.
			if after < len(s) && (s[after] == ' ' || s[after] == '\t' || s[after] == ':') {
				return after
			}
		}
	}
	return i
}
