package logsearch

import (
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
)

var t0 = time.Date(2026, 9, 28, 7, 12, 0, 0, time.UTC)

// stamp prefixes each line with a kubelet timestamp, one second apart.
func stamp(lines ...string) []byte {
	var b strings.Builder
	for i, l := range lines {
		b.WriteString(t0.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano) + " " + l + "\n")
	}
	return []byte(b.String())
}

var src = Source{Namespace: "sre-lab-app", Pod: "url-shortener-1", Container: "app"}

func TestStackTraces(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
		// want: each entry's first line and stack length.
		want  []string
		stack []int
		level []string
	}{
		{"java with cause", []string{
			"2026-09-28 07:12:00.001 INFO  [main] c.e.App - Started",
			"java.lang.IllegalStateException: pool exhausted",
			"\tat com.example.Pool.get(Pool.java:42)",
			"\tat com.example.Api.handle(Api.java:17)",
			"Caused by: java.net.ConnectException: Connection refused",
			"\t... 12 more",
			"2026-09-28 07:12:05.000 WARN  [main] c.e.App - retrying",
		}, []string{"2026-09-28 07:12:00.001 INFO", "java.lang.IllegalStateException", "2026-09-28 07:12:05.000 WARN"}, []int{0, 4, 0},
			[]string{"info", "error", "warn"}},
		{"python traceback ends at the exception line", []string{
			"Traceback (most recent call last):",
			`  File "/app/app.py", line 12, in handler`,
			"    conn = pool.getconn()",
			"psycopg2.OperationalError: FATAL:  sorry, too many clients already",
			"INFO:werkzeug:10.42.0.1 - - GET /health 200",
		}, []string{"Traceback", "INFO:werkzeug"}, []int{3, 0}, []string{"error", "info"}},
		{"go panic with goroutines and blank lines", []string{
			"panic: runtime error: invalid memory address or nil pointer dereference",
			"[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a2b3c]",
			"",
			"goroutine 1 [running]:",
			"main.(*Server).handle(0x0, 0xc000010000)",
			"\t/src/main.go:42 +0x1c",
			"main.main()",
			"\t/src/main.go:12 +0x25",
			"exit status 2",
			"listening on :8080",
		}, []string{"panic: runtime error", "listening on"}, []int{7, 0}, []string{"error", "unknown"}},
		{"node.js error", []string{
			"Error: connect ECONNREFUSED 10.43.0.10:5432",
			"    at TCPConnectWrap.afterConnect [as oncomplete] (node:net:1555:16)",
			`{"level":30,"msg":"server started"}`,
		}, []string{"Error: connect", `{"level":30`}, []int{1, 0}, []string{"error", "info"}},
		{"indented message is joined, not an error", []string{
			"INFO config loaded:",
			"  port: 8080",
			"  db: url-shortener-pooler",
		}, []string{"INFO config loaded"}, []int{2}, []string{"info"}},
	} {
		es := Parse(stamp(c.lines...), src)
		if len(es) != len(c.want) {
			t.Errorf("%s: %d entries, want %d: %+v", c.name, len(es), len(c.want), es)
			continue
		}
		for i, e := range es {
			if !strings.HasPrefix(e.Text, c.want[i]) || len(e.Stack) != c.stack[i] || e.Level != c.level[i] {
				t.Errorf("%s: entry %d = %q stack %d level %s; want %q stack %d level %s", c.name, i, e.Text, len(e.Stack), e.Level, c.want[i], c.stack[i], c.level[i])
			}
			if e.Pod != "url-shortener-1" || e.Namespace != "sre-lab-app" || e.Container != "app" || e.Time.IsZero() {
				t.Errorf("%s: source not kept: %+v", c.name, e)
			}
		}
	}
}

func TestLevels(t *testing.T) {
	for text, want := range map[string]string{
		`{"level":"error","msg":"db down"}`:                                                "error",
		`{"severity":"WARNING","message":"slow"}`:                                          "warn",
		`{"level":50,"msg":"pino error"}`:                                                  "error",
		`{"lvl":"dbug"}`:                                                                   "unknown",
		`time=2026-09-28T07:12:00Z level=warn msg="retry"`:                                 "warn",
		`E0928 07:12:00.000000       1 controller.go:42] sync failed`:                      "error",
		`I0928 07:12:00.000000       1 main.go:10] started`:                                "info",
		`2026-09-28 07:12:00.000 UTC [1] ERROR:  relation "urls" does not exist`:           "error",
		`2026-09-28 07:12:00.000 UTC [1] LOG:  checkpoint starting: time`:                  "info",
		`2026/09/28 07:12:00 [error] 29#29: *1 connect() failed (111: Connection refused)`: "error",
		`10.42.0.1 - - [28/Sep/2026:07:12:00 +0000] "GET / HTTP/1.1" 200 612`:              "unknown",
		`Error: ENOENT: no such file`:                                                      "error",
		`warning: deprecated flag`:                                                         "warn",
		`DEBUG sql: SELECT 1`:                                                              "debug",
		`no error found in 3 checks`:                                                       "unknown",
		`[ERR_CONNECTION] something`:                                                       "unknown",
	} {
		if got := Level(text, nil); got != want {
			t.Errorf("Level(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestParseEdges(t *testing.T) {
	// A line without a timestamp takes the previous time; a leading
	// continuation line (the log was cut) is its own entry; long lines are
	// clipped on a rune boundary; CRLF and invalid UTF-8 are cleaned.
	data := []byte("\tat cut.Frame(X.java:1)\n" + t0.Format(time.RFC3339Nano) + " " + strings.Repeat("é", 3000) + "\r\nno stamp here\n" + t0.Format(time.RFC3339Nano) + " bad \xff byte")
	es := Parse(data, src)
	if len(es) != 4 {
		t.Fatalf("%d entries: %+v", len(es), es)
	}
	if !es[0].Time.IsZero() || es[0].Text != "\tat cut.Frame(X.java:1)" {
		t.Errorf("leading continuation: %+v", es[0])
	}
	if len(es[1].Text) > MaxText+4 || !strings.HasSuffix(es[1].Text, " …") || strings.ContainsRune(es[1].Text, '�') {
		t.Errorf("clip: %d bytes, suffix %q", len(es[1].Text), es[1].Text[len(es[1].Text)-5:])
	}
	if !es[2].Time.Equal(t0) || es[2].Text != "no stamp here" {
		t.Errorf("no stamp: %+v", es[2])
	}
	if es[3].Text != "bad � byte" {
		t.Errorf("invalid UTF-8: %q", es[3].Text)
	}
	// A very long stack is capped.
	lines := []string{"java.lang.StackOverflowError: deep"}
	for range MaxStackLines + 20 {
		lines = append(lines, "\tat a.b.C.d(C.java:1)")
	}
	if es := Parse(stamp(lines...), src); len(es) != 1 || len(es[0].Stack) != MaxStackLines+1 || !strings.Contains(es[0].Stack[MaxStackLines], "not kept") {
		t.Errorf("stack cap: %d lines", len(es[0].Stack))
	}
}

func TestFilterAndMerge(t *testing.T) {
	a := Parse(stamp("INFO ok", "ERROR pool exhausted", "java.lang.RuntimeException: boom", "\tat x.Pool.get(Pool.java:1)"), src)
	b := Parse(stamp("WARN slow query"), Source{Namespace: "sre-lab-app", Pod: "url-shortener-2", Container: "app"})
	f := Filter{Text: "POOL", Levels: []string{protocol.LevelError}, Since: t0.Add(time.Second), Until: t0.Add(2 * time.Second)}
	var kept []string
	for _, e := range append(a, b...) {
		if f.InWindow(e) && f.MatchesText(e) && f.MatchesLevel(e) {
			kept = append(kept, e.Text)
		}
	}
	// "ERROR pool exhausted" (1 s) and the exception whose stack names
	// Pool (2 s); not the INFO line (0 s, outside) nor the WARN (no match).
	if strings.Join(kept, "|") != "ERROR pool exhausted|java.lang.RuntimeException: boom" {
		t.Errorf("kept %q", kept)
	}
	if !(Filter{}).MatchesText(a[0]) || !(Filter{}).MatchesLevel(a[0]) || !(Filter{Since: t0}).InWindow(a[0]) {
		t.Error("an empty filter keeps everything")
	}

	all := append(append([]protocol.LogEntry{}, b...), a...)
	Merge(all)
	var order []string
	for _, e := range all {
		order = append(order, e.Pod+":"+e.Text)
	}
	// Same time (0 s): pod 1 before pod 2.
	want := "url-shortener-1:INFO ok|url-shortener-2:WARN slow query|url-shortener-1:ERROR pool exhausted|url-shortener-1:java.lang.RuntimeException: boom"
	if got := strings.Join(order, "|"); got != want {
		t.Errorf("merge order:\n got %s\nwant %s", got, want)
	}
}
