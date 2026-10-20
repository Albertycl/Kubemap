package protocol

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestDocsMatchTypes keeps docs/protocol.md in step with the Go types: the
// message table lists exactly Types, and each message section lists exactly
// the top-level JSON fields of its payload struct.
func TestDocsMatchTypes(t *testing.T) {
	data, err := os.ReadFile("../../docs/protocol.md")
	if err != nil {
		t.Fatal(err)
	}
	firstCol := regexp.MustCompile("^\\| `([a-zA-Z.]+)` \\|")
	section := regexp.MustCompile("^### `([a-z.]+)`$")

	var tableTypes []Type
	fields := map[Type][]string{}
	var cur Type
	inTable := false
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case line == "## Messages":
			inTable = true
			continue
		case strings.HasPrefix(line, "#"):
			inTable = false
			cur = ""
			if m := section.FindStringSubmatch(line); m != nil {
				cur = Type(m[1])
				fields[cur] = []string{}
			}
			continue
		}
		m := firstCol.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if inTable {
			tableTypes = append(tableTypes, Type(m[1]))
		} else if cur != "" {
			fields[cur] = append(fields[cur], m[1])
		}
	}

	if !slices.Equal(tableTypes, Types) {
		t.Errorf("message table lists %v, Go has %v", tableTypes, Types)
	}
	for _, typ := range Types {
		doc, ok := fields[typ]
		if !ok {
			t.Errorf("docs have no section for %s", typ)
			continue
		}
		want := jsonFields(reflect.TypeOf(newPayload(typ)).Elem())
		slices.Sort(doc)
		slices.Sort(want)
		if !slices.Equal(doc, want) {
			t.Errorf("%s: docs list fields %v, Go has %v", typ, doc, want)
		}
	}
	for typ := range fields {
		if !Known(typ) {
			t.Errorf("docs describe unknown message %s", typ)
		}
	}
}

func jsonFields(rt reflect.Type) []string {
	var out []string
	for f := range rt.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}
