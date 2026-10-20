package health

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"kubemap/pkg/protocol"
)

// Eviction signals the node rules use (kubelet names).
const (
	SignalMemory       = "memory.available"
	SignalNodeFS       = "nodefs.available"
	SignalNodeFSInodes = "nodefs.inodesFree"
	SignalImageFS      = "imagefs.available"
	SignalImageInodes  = "imagefs.inodesFree"
)

// Value is a threshold: a percentage of capacity, or an absolute quantity.
type Value struct {
	Percent float64 // > 0 for "5%"
	Bytes   int64   // > 0 for "100Mi" (or an inode count)
	Raw     string
}

// Of returns the value for a given capacity, in the capacity's unit.
func (v Value) Of(capacity uint64) float64 {
	if v.Percent > 0 {
		return float64(capacity) * v.Percent / 100
	}
	return float64(v.Bytes)
}

// PercentOf returns the value as a percentage of capacity.
func (v Value) PercentOf(capacity uint64) float64 {
	if v.Percent > 0 || capacity == 0 {
		return v.Percent
	}
	return float64(v.Bytes) * 100 / float64(capacity)
}

// Thresholds are the kubelet settings the rules compare with.
type Thresholds struct {
	EvictionHard map[string]Value
	MinReclaim   map[string]Value
	ImageGCHigh  int
	Source       string
}

// Defaults are the kubelet's defaults (Kubernetes 1.34): evictionHard
// memory.available<100Mi, nodefs.available<10%, nodefs.inodesFree<5%,
// imagefs.available<15%, imagefs.inodesFree<5%; no minimum reclaim;
// imageGCHighThresholdPercent 85.
func Defaults() Thresholds {
	t, err := Parse("memory.available<100Mi,nodefs.available<10%,nodefs.inodesFree<5%,imagefs.available<15%,imagefs.inodesFree<5%", "", 85)
	if err != nil {
		panic(err)
	}
	t.Source = "kubelet defaults"
	return t
}

// Configure applies agent settings in the kubelet's flag syntax on top of
// the defaults: evictionHard "nodefs.available<5%,imagefs.available<5%",
// minReclaim "nodefs.available=10%,imagefs.available=10%", imageGCHigh 0
// keeps the default. Signals not named keep their defaults.
func Configure(evictionHard, minReclaim string, imageGCHigh int) (Thresholds, error) {
	t := Defaults()
	if evictionHard == "" && minReclaim == "" && imageGCHigh == 0 {
		return t, nil
	}
	o, err := Parse(evictionHard, minReclaim, imageGCHigh)
	if err != nil {
		return t, err
	}
	maps.Copy(t.EvictionHard, o.EvictionHard)
	t.MinReclaim = o.MinReclaim
	if imageGCHigh != 0 {
		t.ImageGCHigh = imageGCHigh
	}
	t.Source = "agent settings"
	return t, nil
}

// Parse reads the kubelet flag syntax.
func Parse(evictionHard, minReclaim string, imageGCHigh int) (Thresholds, error) {
	t := Thresholds{EvictionHard: map[string]Value{}, MinReclaim: map[string]Value{}, ImageGCHigh: imageGCHigh}
	for _, part := range split(evictionHard) {
		k, v, ok := strings.Cut(part, "<")
		if !ok {
			return t, fmt.Errorf("eviction threshold %q: want signal<value", part)
		}
		val, err := parseValue(v)
		if err != nil {
			return t, fmt.Errorf("eviction threshold %q: %w", part, err)
		}
		t.EvictionHard[strings.TrimSpace(k)] = val
	}
	for _, part := range split(minReclaim) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return t, fmt.Errorf("minimum reclaim %q: want signal=value", part)
		}
		val, err := parseValue(v)
		if err != nil {
			return t, fmt.Errorf("minimum reclaim %q: %w", part, err)
		}
		t.MinReclaim[strings.TrimSpace(k)] = val
	}
	if imageGCHigh < 0 || imageGCHigh > 100 {
		return t, fmt.Errorf("image GC high threshold %d: want 0-100", imageGCHigh)
	}
	return t, nil
}

func split(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseValue(s string) (Value, error) {
	s = strings.TrimSpace(s)
	if p, ok := strings.CutSuffix(s, "%"); ok {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f <= 0 || f >= 100 {
			return Value{}, fmt.Errorf("bad percentage %q", s)
		}
		return Value{Percent: f, Raw: s}, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() <= 0 {
		return Value{}, fmt.Errorf("bad quantity %q", s)
	}
	return Value{Bytes: q.Value(), Raw: s}, nil
}

func (t Thresholds) protocol() protocol.HealthThresholds {
	out := protocol.HealthThresholds{EvictionHard: map[string]string{}, ImageGCHighPercent: t.ImageGCHigh, Source: t.Source}
	for k, v := range t.EvictionHard {
		out.EvictionHard[k] = v.Raw
	}
	if len(t.MinReclaim) > 0 {
		out.EvictionMinimumReclaim = map[string]string{}
		for k, v := range t.MinReclaim {
			out.EvictionMinimumReclaim[k] = v.Raw
		}
	}
	return out
}
