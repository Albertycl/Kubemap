package cache

import "k8s.io/apimachinery/pkg/runtime/schema"

// Resource is one kind of object the cache holds.
type Resource struct {
	GVR  schema.GroupVersionResource
	Kind string
	// Optional resources are CRDs that many clusters lack (Argo CD, Traefik,
	// CNPG). They are watched with dynamic informers if discovery finds them.
	Optional bool
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

// Core, apps, batch, networking, autoscaling, discovery, policy and RBAC.
// Secrets are never listed: the agent's RBAC excludes them (A4), and the
// cache would not need them anyway.
var builtinResources = []Resource{
	{GVR: gvr("", "v1", "namespaces"), Kind: "Namespace"},
	{GVR: gvr("", "v1", "nodes"), Kind: "Node"},
	{GVR: gvr("", "v1", "pods"), Kind: "Pod"},
	{GVR: gvr("", "v1", "services"), Kind: "Service"},
	{GVR: gvr("", "v1", "configmaps"), Kind: "ConfigMap"},
	{GVR: gvr("", "v1", "serviceaccounts"), Kind: "ServiceAccount"},
	{GVR: gvr("", "v1", "persistentvolumeclaims"), Kind: "PersistentVolumeClaim"},
	{GVR: gvr("", "v1", "persistentvolumes"), Kind: "PersistentVolume"},
	{GVR: gvr("", "v1", "events"), Kind: "Event"}, // Warning events only, see Options
	{GVR: gvr("apps", "v1", "deployments"), Kind: "Deployment"},
	{GVR: gvr("apps", "v1", "replicasets"), Kind: "ReplicaSet"},
	{GVR: gvr("apps", "v1", "statefulsets"), Kind: "StatefulSet"},
	{GVR: gvr("apps", "v1", "daemonsets"), Kind: "DaemonSet"},
	{GVR: gvr("batch", "v1", "jobs"), Kind: "Job"},
	{GVR: gvr("batch", "v1", "cronjobs"), Kind: "CronJob"},
	{GVR: gvr("discovery.k8s.io", "v1", "endpointslices"), Kind: "EndpointSlice"},
	{GVR: gvr("networking.k8s.io", "v1", "ingresses"), Kind: "Ingress"},
	{GVR: gvr("networking.k8s.io", "v1", "ingressclasses"), Kind: "IngressClass"},
	{GVR: gvr("networking.k8s.io", "v1", "networkpolicies"), Kind: "NetworkPolicy"},
	{GVR: gvr("autoscaling", "v2", "horizontalpodautoscalers"), Kind: "HorizontalPodAutoscaler"},
	{GVR: gvr("policy", "v1", "poddisruptionbudgets"), Kind: "PodDisruptionBudget"},
	{GVR: gvr("rbac.authorization.k8s.io", "v1", "roles"), Kind: "Role"},
	{GVR: gvr("rbac.authorization.k8s.io", "v1", "rolebindings"), Kind: "RoleBinding"},
	{GVR: gvr("rbac.authorization.k8s.io", "v1", "clusterroles"), Kind: "ClusterRole"},
	{GVR: gvr("rbac.authorization.k8s.io", "v1", "clusterrolebindings"), Kind: "ClusterRoleBinding"},
}

// CRDs used by the application map (KM-34, KM-51, KM-52).
var optionalResources = []Resource{
	{GVR: gvr("argoproj.io", "v1alpha1", "applications"), Kind: "Application", Optional: true},
	// Sync windows for an Argo CD sync (KM-41; read-only, approved 2026-09-28).
	{GVR: gvr("argoproj.io", "v1alpha1", "appprojects"), Kind: "AppProject", Optional: true},
	{GVR: gvr("traefik.io", "v1alpha1", "ingressroutes"), Kind: "IngressRoute", Optional: true},
	{GVR: gvr("traefik.containo.us", "v1alpha1", "ingressroutes"), Kind: "IngressRoute", Optional: true},
	{GVR: gvr("postgresql.cnpg.io", "v1", "clusters"), Kind: "Cluster", Optional: true},
	{GVR: gvr("postgresql.cnpg.io", "v1", "poolers"), Kind: "Pooler", Optional: true},
}

// Resources returns every resource the cache can watch, built-in first.
func Resources() []Resource {
	return append(append([]Resource(nil), builtinResources...), optionalResources...)
}

// Well-known GVRs, for callers of List and Get.
var (
	Namespaces   = builtinResources[0].GVR
	Nodes        = builtinResources[1].GVR
	Pods         = builtinResources[2].GVR
	Services     = builtinResources[3].GVR
	ConfigMaps   = builtinResources[4].GVR
	Events       = builtinResources[8].GVR
	Deployments  = builtinResources[9].GVR
	ReplicaSets  = builtinResources[10].GVR
	StatefulSets = builtinResources[11].GVR
	DaemonSets   = builtinResources[12].GVR
)
