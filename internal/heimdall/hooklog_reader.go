package heimdall

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HookLogEntry is a parsed line from hooks.log (see formatHookLogLine in
// hooklog.go for the producer). Missing keys in Fields means the producer
// did not emit them on that line — callers should treat absence as zero.
type HookLogEntry struct {
	Timestamp time.Time
	Level     string
	Event     string
	Session   string // convenience mirror of Fields["session"]; empty if absent
	Fields    map[string]string
}

// parseHookLogLine parses a single hooks.log line of the form:
//
//	<rfc3339> <LEVEL> event=<name> [k=v ...]
//
// Values that contain whitespace, double-quotes, backslashes, or control
// chars are serialized by the producer (hooklog.go redactLogString) as a
// Go-quoted literal: `k="v with spaces"`, `k="he said \"hi\""`, etc. This
// parser pairs that with strconv.Unquote so the round-trip is lossless.
//
// Returns (_, false) on a hard parse failure — empty line, unparseable
// timestamp, or a line missing the `event=` token. A MALFORMED value
// (unterminated quote) degrades gracefully: the parser logs a warning
// via the stdlib log package and keeps whatever was parsed up to the
// malformed token, rather than dropping the whole line. Never panics.
func parseHookLogLine(line string) (HookLogEntry, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return HookLogEntry{}, false
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 {
		return HookLogEntry{}, false
	}
	ts, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return HookLogEntry{}, false
	}
	entry := HookLogEntry{
		Timestamp: ts,
		Level:     parts[1],
		Fields:    map[string]string{},
	}

	for _, kv := range splitKVTokens(parts[2]) {
		eq := strings.IndexByte(kv.raw, '=')
		if eq < 0 {
			continue
		}
		k := kv.raw[:eq]
		rawV := kv.raw[eq+1:]
		v := unquoteValue(rawV)
		if k == "event" && entry.Event == "" {
			entry.Event = v
			continue
		}
		if k == "session" {
			entry.Session = v
		}
		entry.Fields[k] = v
	}
	if entry.Event == "" {
		return HookLogEntry{}, false
	}
	return entry, true
}

// kvToken is one whitespace-separated `k=v` chunk from a hooks.log line,
// with quoted values kept intact so the caller can split on the FIRST `=`
// rather than every `=` (values can legitimately contain `=`).
type kvToken struct {
	raw string
}

// splitKVTokens splits the tail of a hooks.log line (everything after
// `<ts> <LEVEL> `) into its kv tokens, respecting strconv.Quote-style
// quoted values. Rules:
//   - unquoted run: bytes up to the next space.
//   - quoted run: `"..."` where inner `\"` and `\\` are escaped. We scan
//     until the matching unescaped `"` and include both quote bytes in
//     the returned token so downstream strconv.Unquote has a well-formed
//     input.
//   - unterminated quote: capture everything to end-of-string as the
//     malformed token's value, log a warning, and stop splitting. The
//     caller keeps fields parsed before the malformed token intact.
func splitKVTokens(s string) []kvToken {
	var out []kvToken
	i := 0
	n := len(s)
	for i < n {
		// Skip any run of spaces between tokens.
		for i < n && s[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}
		start := i
		// Walk forward until we either hit the value's opening quote or
		// end-of-token. A token has the shape `k=...`; we need to scan
		// the key (up to `=`) without any quote handling.
		for i < n && s[i] != '=' && s[i] != ' ' {
			i++
		}
		// Missing `=` — emit whatever we have, let parseHookLogLine drop it.
		if i >= n || s[i] == ' ' {
			out = append(out, kvToken{raw: s[start:i]})
			continue
		}
		// Found `=`; now scan the value.
		i++ // past '='
		if i < n && s[i] == '"' {
			// Quoted value. Walk until the matching unescaped `"`.
			vStart := i
			i++ // past opening quote
			terminated := false
			for i < n {
				if s[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if s[i] == '"' {
					i++ // include closing quote
					terminated = true
					break
				}
				i++
			}
			if !terminated {
				// Malformed: unterminated quote. Best-effort — capture
				// the rest of the line as this token's value and stop.
				// We prefix a synthetic closing quote so strconv.Unquote
				// still accepts the payload; the caller falls back to
				// the raw inner bytes if unquote fails.
				log.Printf("hooklog: unterminated quoted value in line fragment %q; degrading", s[start:])
				out = append(out, kvToken{raw: s[start:vStart] + s[vStart:n]})
				return out
			}
			out = append(out, kvToken{raw: s[start:i]})
			continue
		}
		// Unquoted value — scan to next space.
		for i < n && s[i] != ' ' {
			i++
		}
		out = append(out, kvToken{raw: s[start:i]})
	}
	return out
}

// unquoteValue decodes a value that may have been emitted by strconv.Quote.
// Unquoted simple values pass through unchanged. A value starting with `"`
// is fed to strconv.Unquote; if that fails (malformed input, unterminated
// quote captured by splitKVTokens) we fall back to stripping the leading
// quote and returning the raw inner bytes — this preserves the behavior
// that a malformed line doesn't drop fields.
func unquoteValue(raw string) string {
	if len(raw) < 2 || raw[0] != '"' {
		return raw
	}
	if v, err := strconv.Unquote(raw); err == nil {
		return v
	}
	// Degraded path: trim the leading quote and any trailing quote.
	v := raw[1:]
	if last := len(v) - 1; last >= 0 && v[last] == '"' {
		v = v[:last]
	}
	return v
}

// ReadHookLogOpts controls ReadHookLog. Empty fields mean "no filter".
type ReadHookLogOpts struct {
	Path    string
	Session string
	Event   string
	Since   time.Time
	Until   time.Time
}

// ReadHookLog reads hooks.log at Path, applies filters, and returns entries
// in chronological order. A missing path returns an empty slice and no
// error — hooks.log is created lazily, so callers shouldn't treat absence
// as a hard failure.
//
// For byte-level streaming (e.g. `hooks tail -f`), use OpenHookLog instead.
func ReadHookLog(opts ReadHookLogOpts) ([]HookLogEntry, error) {
	if opts.Path == "" {
		opts.Path = HookLogPath()
	}
	f, err := os.Open(opts.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open hooks.log: %w", err)
	}
	defer f.Close()

	var out []HookLogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		entry, ok := parseHookLogLine(sc.Text())
		if !ok {
			continue
		}
		if opts.Session != "" && entry.Session != opts.Session {
			continue
		}
		if opts.Event != "" && entry.Event != opts.Event {
			continue
		}
		if !opts.Since.IsZero() && entry.Timestamp.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && entry.Timestamp.After(opts.Until) {
			continue
		}
		out = append(out, entry)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("scan hooks.log: %w", err)
	}
	return out, nil
}

// SessionHookAggregate summarizes hook-side metrics for one session.
type SessionHookAggregate struct {
	Session string

	// FirstSeen is the earliest timestamp of any hooks.log entry carrying
	// this session id; LastSeen is the latest. Both are zero when no
	// timestamped entries were folded in.
	FirstSeen time.Time
	LastSeen  time.Time

	SessionStartEvents int
	SessionEndEvents   int

	UserPromptEvents    int
	UserPromptCacheHits int
	UserPromptSkips     int
	// UserPromptCacheTotal is the denominator for "cache hit ratio" —
	// the count of user-prompt events that actually reached the cache
	// lookup layer (stage=ok + stage=cache_hit). Degraded stages
	// (ollama_ping, embed errors, verify_hook_index, etc.) and
	// stage=skip (prompt_too_short) are excluded — those aren't
	// attempts the cache got to observe, so including them would
	// deflate the ratio misleadingly.
	UserPromptCacheTotal    int
	UserPromptBytesInjected int64

	PreToolUseEvents  int
	GuardrailVerdicts map[string]int // class -> count, e.g. {"allow": 12, "block": 0}

	PostEditEvents    int
	PostEditReindexes int

	StopEvents int
	StopBytes  int64
}

// AggregateHookLogBySession folds a flat list of hooks.log entries into a
// per-session aggregate. Entries with an empty Session are skipped (they
// represent pre-payload / doctor dry-fire lines that don't belong to a
// specific Claude Code session).
func AggregateHookLogBySession(entries []HookLogEntry) map[string]SessionHookAggregate {
	out := map[string]SessionHookAggregate{}
	for _, e := range entries {
		if e.Session == "" {
			continue
		}
		agg := out[e.Session]
		agg.Session = e.Session
		if !e.Timestamp.IsZero() {
			if agg.FirstSeen.IsZero() || e.Timestamp.Before(agg.FirstSeen) {
				agg.FirstSeen = e.Timestamp
			}
			if e.Timestamp.After(agg.LastSeen) {
				agg.LastSeen = e.Timestamp
			}
		}
		switch e.Event {
		case "session-start":
			agg.SessionStartEvents++
		case "session-end":
			agg.SessionEndEvents++
		case "user-prompt":
			agg.UserPromptEvents++
			switch e.Fields["stage"] {
			case "cache_hit":
				agg.UserPromptCacheHits++
				agg.UserPromptCacheTotal++
			case "ok":
				// Fresh retrieval that reached the cache layer —
				// counts toward the cache-hit-ratio denominator
				// but not the numerator.
				agg.UserPromptCacheTotal++
			case "skip":
				agg.UserPromptSkips++
			}
			agg.UserPromptBytesInjected += parseInt64Field(e.Fields["bytes"])
		case "pre-tool-use":
			agg.PreToolUseEvents++
			if agg.GuardrailVerdicts == nil {
				agg.GuardrailVerdicts = map[string]int{}
			}
			class := e.Fields["class"]
			if class != "" {
				agg.GuardrailVerdicts[class]++
			}
		case "post-edit":
			agg.PostEditEvents++
		case "post-edit-actor":
			if e.Fields["msg"] == "reindex_ok" {
				agg.PostEditReindexes++
			}
		case "stop":
			agg.StopEvents++
			agg.StopBytes += parseInt64Field(e.Fields["bytes"])
		}
		out[e.Session] = agg
	}
	return out
}

// SortedSessionIDs returns the session IDs from an aggregate map in stable
// alphabetical order — used by the sessions-list CLI for deterministic output.
func SortedSessionIDs(agg map[string]SessionHookAggregate) []string {
	ids := make([]string, 0, len(agg))
	for id := range agg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// MostRecentSessionID returns the session id with the latest LastSeen in the
// aggregate map, or "" if the map is empty. Ties break on lexicographically
// greater id for stability.
func MostRecentSessionID(agg map[string]SessionHookAggregate) string {
	var bestID string
	var bestSeen time.Time
	for id, a := range agg {
		if a.LastSeen.After(bestSeen) || (a.LastSeen.Equal(bestSeen) && id > bestID) {
			bestID = id
			bestSeen = a.LastSeen
		}
	}
	return bestID
}

func parseInt64Field(s string) int64 {
	if s == "" {
		return 0
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}
