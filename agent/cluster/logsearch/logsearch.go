// Package logsearch turns raw container logs into searchable entries
// (KM-57): it splits the kubelet's timestamped lines, joins stack traces and
// indented continuation lines to the line that started them, tells each
// entry's level (error, warn, info, debug) from the common log formats, and
// filters and merges the entries of many containers. It reads nothing
// itself; agent/cluster/query fetches the logs with pods/log.
package logsearch

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"kubemap/pkg/protocol"
)

// Bounds for one entry, so a binary blob or a huge JSON line cannot fill a
// response.
const (
	MaxText       = 4000 // bytes of an entry's first line
	MaxStackLines = 100
	MaxStackLine  = 1000
)

// Source names the container a log came from.
type Source struct {
	Namespace, Pod, Container string
	Previous                  bool
}

// mode is the kind of stack trace being joined.
type mode int

const (
	modeNone mode = iota
	modePython
	modeGo
)

var (
	goFrame     = regexp.MustCompile(`^[\w./*()\[\]-]+\(.*\)$`)
	goroutine   = regexp.MustCompile(`^goroutine \d+ \[`)
	pyFinal     = regexp.MustCompile(`^[A-Za-z_][\w.]*(Error|Exception|Exit|Interrupt|Warning)\b`)
	moreFrames  = regexp.MustCompile(`^\.\.\. \d+ (more|common frames omitted)`)
	javaAt      = regexp.MustCompile(`^\s+at\s`)
	exitStatus  = regexp.MustCompile(`^exit status \d+$`)
	klog        = regexp.MustCompile(`^([IWEF])\d{4} \d\d:\d\d:\d\d`)
	logfmtLevel = regexp.MustCompile(`(?i)\b(?:level|lvl|severity|loglevel)=["']?([a-zA-Z]+)`)
	// Upper-case level words anywhere near the start (Postgres "ERROR:",
	// Python "WARNING:root:", Java "2026-… ERROR [main]"), and bracketed or
	// leading ones in any case (nginx "[error]", "Error: …").
	upperWord   = regexp.MustCompile(`\b(FATAL|PANIC|CRITICAL|CRIT|SEVERE|EMERG|ALERT|ERROR|ERR|WARNING|WARN|INFO|NOTICE|LOG|DEBUG|TRACE)\b`)
	bracketWord = regexp.MustCompile(`(?i)[\[(<]\s*(fatal|panic|critical|crit|error|err|warning|warn|info|notice|debug|trace)\s*[\])>]`)
	leadingWord = regexp.MustCompile(`(?i)^(fatal|panic|critical|error|warning|warn|info|debug|trace)\b\s*[:\-|]`)
	errorWord   = regexp.MustCompile(`^(Traceback \(most recent call last\)|panic: |fatal error: |Exception in thread |Unhandled exception|[A-Za-z_][\w.$]*(Exception|Error): )`)
)

// Parse splits one container's log, read with timestamps, into entries.
// Lines without a kubelet timestamp take the previous entry's time.
func Parse(data []byte, src Source) []protocol.LogEntry {
	var out []protocol.LogEntry
	m := modeNone
	var last time.Time
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		t, text := splitTime(string(bytes.TrimRight(line, "\r")))
		if t.IsZero() {
			t = last
		}
		last = t
		if strings.TrimSpace(text) == "" {
			continue // blank lines carry nothing to search; a Go panic has them between parts
		}
		if n := len(out); n > 0 {
			if ok, next := continues(m, text); ok {
				addStack(&out[n-1], text)
				m = next
				continue
			}
		}
		m = starts(text)
		out = append(out, protocol.LogEntry{Time: t, Namespace: src.Namespace, Pod: src.Pod, Container: src.Container,
			Previous: src.Previous, Text: clip(text, MaxText)})
	}
	for i := range out {
		out[i].Level = Level(out[i].Text, out[i].Stack)
	}
	return out
}

// splitTime separates the kubelet's RFC 3339 timestamp from the line.
func splitTime(line string) (time.Time, string) {
	sp := strings.IndexByte(line, ' ')
	if sp < 20 || sp > 40 {
		return time.Time{}, line
	}
	t, err := time.Parse(time.RFC3339Nano, line[:sp])
	if err != nil {
		return time.Time{}, line
	}
	return t.UTC(), line[sp+1:]
}

// starts says whether a new entry opens a stack trace whose lines are not
// all indented (Python's final exception line, Go's goroutine blocks).
func starts(text string) mode {
	switch {
	case strings.HasPrefix(text, "Traceback (most recent call last):"):
		return modePython
	case strings.HasPrefix(text, "panic: "), strings.HasPrefix(text, "fatal error: "), goroutine.MatchString(text):
		return modeGo
	}
	return modeNone
}

// continues says whether text belongs to the entry before it, and the mode
// after it.
func continues(m mode, text string) (bool, mode) {
	switch {
	case text[0] == ' ' || text[0] == '\t':
		return true, m
	case strings.HasPrefix(text, "Caused by: "), moreFrames.MatchString(text):
		return true, m
	case m == modePython && pyFinal.MatchString(text):
		return true, modeNone // the exception line ends a Python traceback
	case m == modeGo && (goroutine.MatchString(text) || goFrame.MatchString(text) ||
		strings.HasPrefix(text, "created by ") || strings.HasPrefix(text, "[signal ") || exitStatus.MatchString(text)):
		return true, m
	}
	return false, modeNone
}

func addStack(e *protocol.LogEntry, text string) {
	switch n := len(e.Stack); {
	case n < MaxStackLines:
		e.Stack = append(e.Stack, clip(text, MaxStackLine))
	case n == MaxStackLines:
		e.Stack = append(e.Stack, "… (more lines not kept)")
	}
}

// clip cuts s to at most max bytes, on a rune boundary.
func clip(s string, max int) string {
	if len(s) <= max {
		return strings.ToValidUTF8(s, "\uFFFD")
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.ToValidUTF8(s[:cut], "\uFFFD") + " …"
}

// Level tells an entry's level from its first line, in this order: a JSON
// log's level field (string, or pino/bunyan numbers), logfmt level=, the
// klog prefix (E0928 …), a level word (upper case anywhere near the start,
// or bracketed or leading in any case), then the shape of an error or a
// stack trace. Anything else is unknown.
func Level(text string, stack []string) string {
	if l := jsonLevel(text); l != "" {
		return l
	}
	if m := logfmtLevel.FindStringSubmatch(text); m != nil {
		if l := word(m[1]); l != "" {
			return l
		}
	}
	if m := klog.FindStringSubmatch(text); m != nil {
		return map[string]string{"I": protocol.LevelInfo, "W": protocol.LevelWarn, "E": protocol.LevelError, "F": protocol.LevelError}[m[1]]
	}
	head := text
	if len(head) > 160 {
		head = head[:160]
	}
	for _, re := range []*regexp.Regexp{bracketWord, leadingWord, upperWord} {
		if m := re.FindStringSubmatch(head); m != nil {
			if l := word(m[1]); l != "" {
				return l
			}
		}
	}
	if errorWord.MatchString(text) || isTrace(stack) {
		return protocol.LevelError
	}
	return protocol.LevelUnknown
}

// isTrace says whether continuation lines look like a stack trace rather
// than an indented message.
func isTrace(stack []string) bool {
	for _, s := range stack {
		if javaAt.MatchString(s) || strings.HasPrefix(strings.TrimSpace(s), "File \"") || goroutine.MatchString(s) || strings.HasPrefix(s, "Caused by: ") {
			return true
		}
	}
	return false
}

// word normalises a level word.
func word(w string) string {
	switch strings.ToLower(w) {
	case "fatal", "panic", "critical", "crit", "severe", "emerg", "emergency", "alert", "error", "err", "dpanic":
		return protocol.LevelError
	case "warning", "warn":
		return protocol.LevelWarn
	case "info", "information", "notice", "log":
		return protocol.LevelInfo
	case "debug", "trace", "verbose", "fine", "finer", "finest":
		return protocol.LevelDebug
	}
	return ""
}

// jsonLevel reads the level of a JSON log line.
func jsonLevel(text string) string {
	if len(text) < 2 || text[0] != '{' {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(text), &obj) != nil {
		return ""
	}
	for k, v := range obj {
		switch strings.ToLower(k) {
		case "level", "lvl", "severity", "log.level", "levelname", "loglevel", "@l":
		default:
			continue
		}
		switch v := v.(type) {
		case string:
			return word(v)
		case float64: // pino and bunyan
			switch {
			case v >= 50:
				return protocol.LevelError
			case v >= 40:
				return protocol.LevelWarn
			case v >= 30:
				return protocol.LevelInfo
			default:
				return protocol.LevelDebug
			}
		}
	}
	return ""
}

// Filter says what to keep.
type Filter struct {
	Text         string   // case-insensitive, in the line or its stack
	Levels       []string // empty: every level
	Since, Until time.Time
}

// InWindow reports whether e is inside the time window.
func (f Filter) InWindow(e protocol.LogEntry) bool {
	return !e.Time.Before(f.Since) && (f.Until.IsZero() || !e.Time.After(f.Until))
}

// MatchesText reports whether e contains the text.
func (f Filter) MatchesText(e protocol.LogEntry) bool {
	if f.Text == "" {
		return true
	}
	q := strings.ToLower(f.Text)
	if strings.Contains(strings.ToLower(e.Text), q) {
		return true
	}
	return slices.ContainsFunc(e.Stack, func(s string) bool { return strings.Contains(strings.ToLower(s), q) })
}

// MatchesLevel reports whether e has one of the levels.
func (f Filter) MatchesLevel(e protocol.LogEntry) bool {
	return len(f.Levels) == 0 || slices.Contains(f.Levels, e.Level)
}

// Merge sorts the entries of many containers by time; entries at the same
// time keep their log order, then pod and container order.
func Merge(es []protocol.LogEntry) {
	slices.SortStableFunc(es, func(a, b protocol.LogEntry) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		if a.Namespace != b.Namespace {
			return strings.Compare(a.Namespace, b.Namespace)
		}
		if a.Pod != b.Pod {
			return strings.Compare(a.Pod, b.Pod)
		}
		return strings.Compare(a.Container, b.Container)
	})
}
