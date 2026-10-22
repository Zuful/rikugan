package escalation

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// What the output filters put in place of what they remove.
const (
	CommandRemoved = "(command removed)"
	LinkRemoved    = "(link removed)"
	SecretMasked   = "(secret masked)"
)

var (
	// codeBlock is a fenced block: removed whole, whatever it holds.
	codeBlock = regexp.MustCompile("(?s)```.*?(```|$)")
	link      = regexp.MustCompile(`(?i)\b(?:https?|ftp|file)://\S+`)
	// commandLike matches text that reads as a command someone could
	// paste: a mutating CLI, a download, a pipe into a shell, a command
	// substitution, destructive SQL. A false positive costs a sentence;
	// a missed command can cost a namespace.
	commandLike = regexp.MustCompile(`(?i)` + strings.Join([]string{
		`\b(?:kubectl|oc|helm|kustomize|terraform|ansible|aws|gcloud|az|doctl)\s+\S`,
		`\bdocker(?:\s+compose|-compose)?\s+(?:run|exec|rm|rmi|kill|stop|restart|up|down|pull|push|build|system|volume|network|prune|cp)\b`,
		`\b(?:curl|wget|nc|ncat|socat|ssh|scp|rsync)\s+\S`,
		`\b(?:sudo|chmod|chown|chattr|mkfs\S*|shred|systemctl|pkill|killall|reboot|shutdown|crontab|iptables|nft)\s+\S`,
		`\b(?:rm|kill)\s+-`,
		`\|\s*(?:sudo\s+)?(?:ba|z|da|k|c|tc|fi)?sh\b`,
		`\b(?:bash|sh|zsh|python3?|perl|ruby|node)\s+-c\b`,
		`\beval\s`,
		`\$\(`,
		"`[^`]*\\s[^`]*`", // inline code with arguments; a lone `error_rate` stays
		`\b(?:drop|truncate)\s+(?:table|database|schema)\b`,
		`\bdelete\s+from\b`,
		`\b(?:psql|mysql|redis-cli|mongo|mongosh)\s+\S`,
	}, "|"))
	secrets = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`),
		regexp.MustCompile(`(?i)\b(?:authorization|api[_-]?key|token|secret|password|passwd|pwd)\s*[:=]\s*[^\s(]\S*`), // a value not already masked
		regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\bxox[abprse]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?s:.*?)(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	}
	// token is a run long enough to be a key; masked when it also looks
	// random (see looksRandom).
	token = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// Clean prepares agent text for the team: fenced blocks and lines that
// read as commands are replaced by CommandRemoved, links by LinkRemoved,
// secrets by SecretMasked, and the result is cut to max runes.
func Clean(text string, max int) string {
	text = codeBlock.ReplaceAllString(text, CommandRemoved)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if commandLike.MatchString(l) {
			lines[i] = CommandRemoved
		}
	}
	text = strings.Join(lines, "\n")
	text = link.ReplaceAllString(text, LinkRemoved)
	text = MaskSecrets(text)
	return cut(strings.TrimSpace(text), max)
}

// MaskSecrets replaces the secrets text seems to carry: bearer tokens,
// key=value credentials, the formats of well-known keys, JWTs, private
// keys, and long random-looking strings (a commit hash, all hex, is
// kept).
func MaskSecrets(text string) string {
	for _, re := range secrets {
		text = re.ReplaceAllString(text, SecretMasked)
	}
	return token.ReplaceAllStringFunc(text, func(s string) string {
		if looksRandom(s) {
			return SecretMasked
		}
		return s
	})
}

// looksRandom tells a key from an identifier: mixed case and digits,
// and a high per-character entropy. Hex digests have no upper case.
func looksRandom(s string) bool {
	var upper, lower, digit bool
	counts := map[rune]int{}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
		counts[r]++
	}
	if !upper || !lower || !digit {
		return false
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h >= 4.0
}

func cut(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}
