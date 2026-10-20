package cache

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

const (
	lastApplied = "kubectl.kubernetes.io/last-applied-configuration"
	// TruncatedAnnotation marks a ConfigMap whose large values the cache cut.
	// KM-25 reads such a ConfigMap live from the API when it shows its YAML.
	TruncatedAnnotation = "kubemap.io/cache-truncated"
)

// transform runs on every object before it enters the cache. It drops fields
// the agent never uses but that often make up most of an object's size:
// managedFields, the kubectl last-applied copy, and ConfigMap values larger
// than maxValue (Grafana dashboards, for example, are hundreds of KiB).
func transform(maxValue int) func(any) (any, error) {
	return func(obj any) (any, error) {
		acc, err := meta.Accessor(obj)
		if err != nil {
			return obj, nil // tombstones and the like pass through
		}
		acc.SetManagedFields(nil)
		if ann := acc.GetAnnotations(); ann[lastApplied] != "" {
			delete(ann, lastApplied)
			acc.SetAnnotations(ann)
		}
		if cm, ok := obj.(*corev1.ConfigMap); ok {
			truncateConfigMap(cm, maxValue)
		}
		return obj, nil
	}
}

func truncateConfigMap(cm *corev1.ConfigMap, maxValue int) {
	cut := 0
	for k, v := range cm.Data {
		if len(v) > maxValue {
			cm.Data[k] = v[:maxValue]
			cut++
		}
	}
	if len(cm.BinaryData) > 0 {
		cut += len(cm.BinaryData)
		cm.BinaryData = nil
	}
	if cut > 0 {
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations[TruncatedAnnotation] = fmt.Sprintf("%d values cut to %d bytes or dropped (binary)", cut, maxValue)
	}
}

// StripMetadata removes managedFields and the kubectl last-applied copy from
// an object read live from the API (KM-25), like the cache does for its own
// objects. Unlike the cache it keeps every ConfigMap value: a live read is
// how the full object is shown.
func StripMetadata(obj any) {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return
	}
	acc.SetManagedFields(nil)
	if ann := acc.GetAnnotations(); ann[lastApplied] != "" {
		delete(ann, lastApplied)
		acc.SetAnnotations(ann)
	}
}
