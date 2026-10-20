package routes

import (
	"fmt"
	"strings"
)

// Match is one alternative of a Traefik rule: the hosts and path it needs,
// plus any other conditions. A rule with "||" gives several Matches.
type Match struct {
	Hosts []string // empty: any host
	// Path and PathKind: "PathPrefix" /api, "Path" /exact, "PathRegexp" ^/v[0-9]+.
	// Empty Path matches every path.
	Path     string
	PathKind string
	// Other lists matchers that are not host or path (Method, Headers,
	// Query, ClientIP, HostRegexp, and negated matchers), as written.
	Other []string
}

// ParseRule turns a Traefik router rule such as
//
//	Host(`shop.example.com`) && (PathPrefix(`/api`) || PathPrefix(`/v2`))
//
// into its alternatives, one Match per path through the "||"s. It supports
// the matchers of Traefik v2 and v3 (Host takes several hosts in v2),
// "&&", "||", "!" and parentheses.
func ParseRule(rule string) ([]Match, error) {
	p := &parser{toks: nil}
	if err := p.lex(rule); err != nil {
		return nil, err
	}
	dnf, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos].text)
	}
	out := make([]Match, 0, len(dnf))
	for _, conj := range dnf {
		var m Match
		for _, a := range conj {
			switch {
			case a.negated:
				m.Other = append(m.Other, "!"+a.String())
			case a.name == "Host":
				m.Hosts = append(m.Hosts, a.args...)
			case (a.name == "PathPrefix" || a.name == "Path" || a.name == "PathRegexp") && len(a.args) > 0 && m.Path == "":
				m.Path, m.PathKind = a.args[0], a.name
			default:
				m.Other = append(m.Other, a.String())
			}
		}
		out = append(out, m)
	}
	return out, nil
}

type atom struct {
	name    string
	args    []string
	negated bool
}

func (a atom) String() string {
	quoted := make([]string, len(a.args))
	for i, s := range a.args {
		quoted[i] = "`" + s + "`"
	}
	return a.name + "(" + strings.Join(quoted, ", ") + ")"
}

// dnf is an OR of ANDs of atoms.
type dnf [][]atom

func and(x, y dnf) dnf {
	var out dnf
	for _, a := range x {
		for _, b := range y {
			out = append(out, append(append([]atom{}, a...), b...))
		}
	}
	return out
}

// not negates a DNF. A negated single atom stays exact; a negated compound
// becomes one opaque condition, which is enough to show it to a human.
func not(x dnf) dnf {
	if len(x) == 1 && len(x[0]) == 1 {
		a := x[0][0]
		a.negated = !a.negated
		return dnf{{a}}
	}
	return dnf{{{name: "Not", args: []string{"compound"}, negated: false}}}
}

// maxAlternatives caps the "||" expansion so a hostile rule cannot blow up.
const maxAlternatives = 64

type token struct {
	kind string // ident, str, "(", ")", ",", "&&", "||", "!"
	text string
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) lex(s string) error {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')' || c == ',' || c == '!':
			p.toks = append(p.toks, token{string(c), string(c)})
			i++
		case strings.HasPrefix(s[i:], "&&") || strings.HasPrefix(s[i:], "||"):
			p.toks = append(p.toks, token{s[i : i+2], s[i : i+2]})
			i += 2
		case c == '`' || c == '"' || c == '\'':
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return fmt.Errorf("unterminated string at %d", i)
			}
			p.toks = append(p.toks, token{"str", s[i+1 : i+1+end]})
			i += end + 2
		case isIdent(c):
			j := i
			for j < len(s) && isIdent(s[j]) {
				j++
			}
			p.toks = append(p.toks, token{"ident", s[i:j]})
			i = j
		default:
			return fmt.Errorf("unexpected %q at %d", c, i)
		}
	}
	return nil
}

func isIdent(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func (p *parser) peek(kind string) bool { return p.pos < len(p.toks) && p.toks[p.pos].kind == kind }

func (p *parser) next() token {
	t := p.toks[p.pos]
	p.pos++
	return t
}

// expr := term ("||" term)*
func (p *parser) expr() (dnf, error) {
	left, err := p.term()
	if err != nil {
		return nil, err
	}
	for p.peek("||") {
		p.next()
		right, err := p.term()
		if err != nil {
			return nil, err
		}
		left = append(left, right...)
		if len(left) > maxAlternatives {
			return nil, fmt.Errorf("rule has more than %d alternatives", maxAlternatives)
		}
	}
	return left, nil
}

// term := factor ("&&" factor)*
func (p *parser) term() (dnf, error) {
	left, err := p.factor()
	if err != nil {
		return nil, err
	}
	for p.peek("&&") {
		p.next()
		right, err := p.factor()
		if err != nil {
			return nil, err
		}
		left = and(left, right)
		if len(left) > maxAlternatives {
			return nil, fmt.Errorf("rule has more than %d alternatives", maxAlternatives)
		}
	}
	return left, nil
}

// factor := "!" factor | "(" expr ")" | ident "(" [str ("," str)*] ")"
func (p *parser) factor() (dnf, error) {
	switch {
	case p.peek("!"):
		p.next()
		x, err := p.factor()
		if err != nil {
			return nil, err
		}
		return not(x), nil
	case p.peek("("):
		p.next()
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		if !p.peek(")") {
			return nil, fmt.Errorf("missing )")
		}
		p.next()
		return x, nil
	case p.peek("ident"):
		name := p.next().text
		if !p.peek("(") {
			return nil, fmt.Errorf("%s: missing (", name)
		}
		p.next()
		var args []string
		for !p.peek(")") {
			if !p.peek("str") {
				return nil, fmt.Errorf("%s: expected a string argument", name)
			}
			args = append(args, p.next().text)
			if p.peek(",") {
				p.next()
			}
		}
		p.next() // ")"
		return dnf{{{name: name, args: args}}}, nil
	}
	if p.pos >= len(p.toks) {
		return nil, fmt.Errorf("rule ends early")
	}
	return nil, fmt.Errorf("unexpected %q", p.toks[p.pos].text)
}
