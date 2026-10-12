package testartifact

import (
	"regexp"
	"strings"
	"unicode"
)

// Hazard classes the text-quote and label check refuses. This is the narrow
// check for the two places the contract lets agent-chosen text through; it is
// not a scanner for a whole pull request body.
const (
	HazardCredential        = "credential"
	HazardAbsolutePath      = "absolute-path"
	HazardEndpoint          = "endpoint"
	HazardProcessID         = "process-id"
	HazardRunID             = "run-id"
	HazardUUID              = "uuid"
	HazardHexToken          = "hex-token"
	HazardToken             = "token"
	HazardPrivateIdentifier = "private-identifier"
)

var hazardMatchers = []struct {
	class string
	re    *regexp.Regexp
}{
	{HazardCredential, regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|auth|cookie|credentials?)\b\s*[:=]|(?i)\bbearer\s+\S|-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{HazardAbsolutePath, regexp.MustCompile(`(?i)(^|[^a-z0-9_.~-])/(home|users|root|tmp|var|etc|opt|private|mnt|srv|usr|run|proc|volumes|data|workspace|nix)/|(^|[^a-z0-9_.-])~/|\b[a-z]:\\|file://`)},
	{HazardEndpoint, regexp.MustCompile(`(?i)\b(localhost|0\.0\.0\.0)\b|\[::1?\]|\b\d{1,3}(\.\d{1,3}){3}\b|\b[a-z][a-z0-9+.-]*://|\b[a-z][a-z0-9-]*(\.[a-z0-9-]+)*:\d{4,5}\b`)},
	{HazardProcessID, regexp.MustCompile(`(?i)\bpid\s*[:=#]?\s*\d+`)},
	{HazardRunID, regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)},
	{HazardUUID, regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)},
	{HazardHexToken, regexp.MustCompile(`(?i)\b[0-9a-f]{32,}\b`)},
	{HazardPrivateIdentifier, regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b|\b(dob|date[ _-]of[ _-]birth|pat_?num|ssn|mrn)\b`)},
}

var tokenCandidate = regexp.MustCompile(`[A-Za-z0-9+/_=-]{24,}`)

// Hazards returns the classes of value-shaped text found in s, in a fixed
// order and without duplicates. It never returns the matched text.
func Hazards(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var classes []string
	for _, m := range hazardMatchers {
		if m.re.MatchString(s) {
			classes = append(classes, m.class)
		}
	}
	for _, candidate := range tokenCandidate.FindAllString(s, -1) {
		if hasLetterAndDigit(candidate) {
			classes = append(classes, HazardToken)
			break
		}
	}
	return classes
}

func hasLetterAndDigit(s string) bool {
	var letter, digit bool
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			digit = true
		case unicode.IsLetter(r):
			letter = true
		}
	}
	return letter && digit
}
