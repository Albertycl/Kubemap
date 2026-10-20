package traces

import (
	"context"
	"sync"
	"time"
)

// Source picks the Tempo to ask: a fixed URL (KUBEMAP_TEMPO_URL), or the
// one found in the cache, or none ("off"). It keeps one Client per URL, so
// each keeps its own short cache.
type Source struct {
	Fixed    string // a URL, or "" to discover
	Disabled bool

	mu      sync.Mutex
	clients map[string]*Client
}

// NewSource reads the setting: "" discovers Tempo, "off" disables traces,
// anything else is Tempo's URL.
func NewSource(setting string) *Source {
	return &Source{Fixed: setting, Disabled: setting == "off"}
}

// Spans returns the spans of the last window and where they came from. The
// URL is "" when there is no Tempo to ask; then spans and err are empty.
func (s *Source) Spans(ctx context.Context, c Lister) (spans []Span, url string, window time.Duration, err error) {
	if s == nil || s.Disabled {
		return nil, "", 0, nil
	}
	url = s.Fixed
	if url == "" {
		url = Discover(c)
	}
	if url == "" {
		return nil, "", 0, nil
	}
	s.mu.Lock()
	if s.clients == nil {
		s.clients = map[string]*Client{}
	}
	cl := s.clients[url]
	if cl == nil {
		cl = NewClient(url)
		s.clients[url] = cl
	}
	s.mu.Unlock()
	spans, err = cl.Spans(ctx)
	return spans, url, cl.Window, err
}
