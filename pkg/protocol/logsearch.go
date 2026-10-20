package protocol

import (
	"slices"
	"strings"
	"time"
)

// The logs.search op (KM-57) searches the logs of every pod of an app in one
// request: the cluster agent reads each container's log with pods/log (the
// same read-only right as logs), keeps the lines that match the text, the
// levels and the time window, joins stack traces to the line that started
// them, and returns the matches merged by time, each tagged with its pod and
// container. It makes no kubelet call and changes nothing.

// Log search bounds.
const (
	// MaxLogSearchPods bounds the pods one search reads; more are listed in
	// the notes ("ask for one component").
	MaxLogSearchPods = 30
	// DefaultLogSearchLines and MaxLogSearchLines bound the matched lines
	// returned; the newest are kept.
	DefaultLogSearchLines = 500
	MaxLogSearchLines     = 2000
	// DefaultLogSearchSince is the window when none is given; the longest
	// window is MaxLogSearchSince.
	DefaultLogSearchSince = time.Hour
	MaxLogSearchSince     = 7 * 24 * time.Hour
	// MaxLogSearchText bounds the search text.
	MaxLogSearchText = 200
)

// Log levels, as the search normalises them. A line whose level cannot be
// told has LevelUnknown.
const (
	LevelError   = "error" // error, fatal, panic, critical, and stack traces
	LevelWarn    = "warn"
	LevelInfo    = "info"
	LevelDebug   = "debug" // debug and trace
	LevelUnknown = "unknown"
)

// LogLevels are the levels in order, worst first.
var LogLevels = []string{LevelError, LevelWarn, LevelInfo, LevelDebug, LevelUnknown}

// LogSearchRequest selects the pods and the lines.
type LogSearchRequest struct {
	// App is an app ID from the app map ("argocd/url-shortener",
	// "sre-lab-app/redis"): every pod of its components.
	App string `json:"app"`
	// Component narrows the search to one component of the app, by its
	// graph node ID ("Deployment/sre-lab-app/url-shortener").
	Component string `json:"component,omitempty"`
	// Text keeps the entries that contain it, ignoring case, in the line or
	// its stack trace. Empty keeps every line.
	Text string `json:"text,omitempty"`
	// Levels keeps the entries with these levels (LogLevels); empty keeps
	// every level.
	Levels []string `json:"levels,omitempty"`
	// Since is the start of the window; zero means DefaultLogSearchSince
	// before Until (or now). Until is the end; zero means now.
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`
	// Previous also reads the previous run of every container that has
	// restarted: the lines before a crash are often the answer.
	Previous bool `json:"previous,omitempty"`
	// MaxLines bounds the matched lines returned (newest kept); 0 means
	// DefaultLogSearchLines.
	MaxLines int `json:"maxLines,omitempty"`
}

func (s *LogSearchRequest) validate() error {
	if s == nil || s.App == "" {
		return errorf(CodeBadRequest, "request logs.search: needs an app")
	}
	if len(s.App) > maxAppIDLen || strings.ContainsAny(s.App, " \t\n") || len(s.Component) > maxAppIDLen || strings.ContainsAny(s.Component, " \t\n") {
		return errorf(CodeBadRequest, "request logs.search: invalid app or component")
	}
	if len(s.Text) > MaxLogSearchText || strings.ContainsAny(s.Text, "\n\r") {
		return errorf(CodeBadRequest, "request logs.search: text must be one line of at most %d bytes", MaxLogSearchText)
	}
	for _, l := range s.Levels {
		if !slices.Contains(LogLevels, l) {
			return errorf(CodeBadRequest, "request logs.search: unknown level %q (use %s)", l, strings.Join(LogLevels, ", "))
		}
	}
	if s.MaxLines < 0 || s.MaxLines > MaxLogSearchLines {
		return errorf(CodeBadRequest, "request logs.search: maxLines must be 0..%d", MaxLogSearchLines)
	}
	if !s.Since.IsZero() && !s.Until.IsZero() && !s.Since.Before(s.Until) {
		return errorf(CodeBadRequest, "request logs.search: since must be before until")
	}
	if !s.Since.IsZero() && !s.Until.IsZero() && s.Until.Sub(s.Since) > MaxLogSearchSince {
		return errorf(CodeBadRequest, "request logs.search: the window is at most %s", MaxLogSearchSince)
	}
	return nil
}

// LogSearchResult is the response object of a logs.search request.
type LogSearchResult struct {
	App   string    `json:"app"`
	Since time.Time `json:"since"` // the window searched (UTC)
	Until time.Time `json:"until"`
	// Entries are the matches, oldest first, merged across pods by the
	// kubelet's timestamp.
	Entries []LogEntry `json:"entries"`
	// Levels counts the entries that match the text and the window, by
	// level, before the level filter: how many errors there are while
	// looking at warnings.
	Levels map[string]int `json:"levels"`
	// Matched is the number of entries that matched; more than
	// len(Entries) when older ones were cut (Truncated).
	Matched   int  `json:"matched"`
	Truncated bool `json:"truncated,omitempty"`
	// Sources are the containers read, in pod order.
	Sources []LogSource `json:"sources"`
	// Notes say what was not read: pods cut, logs cut, containers that
	// failed.
	Notes []string `json:"notes,omitempty"`
}

// LogEntry is one log line, with the stack trace or continuation lines
// that belong to it.
type LogEntry struct {
	Time      time.Time `json:"time"` // the kubelet's timestamp (UTC)
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Previous  bool      `json:"previous,omitempty"` // from the previous run
	Level     string    `json:"level"`
	Text      string    `json:"text"`
	// Stack holds the lines that continue this one: a Java, Python, Go
	// or Node.js stack trace, or an indented multi-line message.
	Stack []string `json:"stack,omitempty"`
}

// LogSource is one container log that was read.
type LogSource struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Previous  bool   `json:"previous,omitempty"`
	Lines     int    `json:"lines"`   // entries read in the window
	Matched   int    `json:"matched"` // entries that matched
	// Truncated is true when the window held more than the agent reads per
	// container (the newest 10,000 lines, 1 MiB): its oldest part was not
	// searched.
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}
