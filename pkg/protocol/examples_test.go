package protocol

import (
	"encoding/json"
	"strings"
	"time"
)

// example is one message used by the round-trip and golden tests. Its golden
// file is testdata/<name>.json. Tokens and credentials are placeholders.
type example struct {
	name string
	id   string
	body Payload
}

var exampleTime = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func examples() []example {
	return []example{
		{"register-node", "reg-3", &Register{
			Role: RoleNode, AgentVersion: "v0.1.0", ClusterID: "c-example",
			NodeName: "k8server", ServiceAccountToken: "satok_example",
		}},
		{"register.ack-active", "reg-1", &RegisterAck{ClusterID: "c-example", Status: StatusActive}},
		{"heartbeat", "", &Heartbeat{Time: exampleTime}},
		{"summary", "", &Summary{
			Time: exampleTime, KubernetesVersion: "v1.33.4+k3s1",
			Nodes: NodeCounts{Total: 1, Ready: 1}, Namespaces: 14,
			NodeList:  []NodeStatus{{Name: "k8server", Ready: true}},
			Pods:      PodCounts{Total: 62, Running: 58, Pending: 1, Succeeded: 1, Failed: 2},
			Workloads: WorkloadCounts{Deployments: 30, DeploymentsDegraded: 2, StatefulSets: 4, DaemonSets: 3},
			Problems: []Problem{
				{Kind: "Pod", Namespace: "kubemap-demo", Name: "crashloop-7d9f8", Reason: "CrashLoopBackOff",
					Message: "back-off 5m0s restarting failed container", Since: exampleTime.Add(-10 * time.Minute)},
				{Kind: "Pod", Namespace: "kubemap-demo", Name: "pending-0", Reason: "Unschedulable"},
			},
		}},
		{"metrics", "", &Metrics{
			Time: exampleTime, NodeName: "k8server",
			CPU:    CPUStats{Cores: 4, UsagePercent: 37.5},
			Memory: MemoryStats{TotalBytes: 12884901888, AvailableBytes: 1288490188},
			Swap:   SwapStats{TotalBytes: 4294967296, UsedBytes: 536870912},
			Load:   LoadStats{Load1: 1.2, Load5: 1.4, Load15: 1.1},
			PSI: &PSIStats{
				CPU:    Pressure{Some: PressureAvg{Avg10: 14.2, Avg60: 12.8, Avg300: 11}},
				Memory: Pressure{Some: PressureAvg{Avg10: 0.5}, Full: &PressureAvg{Avg10: 0.1}},
				IO:     Pressure{Some: PressureAvg{Avg10: 2.3, Avg60: 1.9, Avg300: 1.5}, Full: &PressureAvg{Avg10: 1.1, Avg60: 0.9, Avg300: 0.7}},
			},
			Filesystems: []FilesystemStats{{
				MountPoint: "/", Device: "/dev/sda2", FSType: "ext4",
				TotalBytes: 107374182400, UsedBytes: 59055800320, AvailableBytes: 42949672960,
				InodesTotal: 6553600, InodesFree: 5898240,
			}},
			Network: []NetworkStats{{Interface: "eth0", RxBytes: 123456789, TxBytes: 98765432}},
			TopProcesses: []ProcessStats{
				{PID: 1234, Command: "java", CPUPercent: 22.5, RSSBytes: 734003200, Origin: OriginPod,
					ContainerID: "0000000000000000000000000000000000000000000000000000000000000abc",
					PodUID:      "00000000-0000-0000-0000-00000000abcd", PodNamespace: "default", PodName: "backend-5c6b7", Container: "app"},
				{PID: 812, Command: "k3s-server", CPUPercent: 9.1, RSSBytes: 681574400, Origin: OriginService, Unit: "k3s.service"},
				{PID: 950, Command: "mariadbd", CPUPercent: 1.2, RSSBytes: 104857600, Origin: OriginContainer,
					ContainerID: "0000000000000000000000000000000000000000000000000000000000000def"},
			},
		}},
		{"request-list", "q-1", &Request{Op: OpList, Version: "v1", Resource: "pods", Namespace: "kubemap-demo", LabelSelector: "app=web", Limit: 100}},
		{"request-get", "q-2", &Request{Op: OpGet, Group: "apps", Version: "v1", Resource: "deployments", Namespace: "kubemap-demo", Name: "web"}},
		{"request-logs", "q-3", &Request{Op: OpLogs, Namespace: "kubemap-demo", Name: "crashloop-7d9f8", Logs: &LogOptions{Container: "app", TailLines: 200, Previous: true}}},
		{"request-graph", "q-5", &Request{Op: OpGraph, Namespace: "sre-lab-app", Graph: &GraphOptions{Around: "Deployment/sre-lab-app/url-shortener", Depth: 2}}},
		{"request-appmap", "q-6", &Request{Op: OpAppMap, Namespace: "sre-lab-app", AppMap: &AppMapOptions{App: "argocd/url-shortener"}}},
		{"request-netpol.check", "q-7", &Request{Op: OpNetpolCheck, NetpolCheck: &NetpolCheckRequest{
			From: NetpolEndpoint{Namespace: "sre-lab-app", Pod: "url-shortener-6f89bdc657-rgwsv"}, To: NetpolEndpoint{IP: "1.1.1.1"}, Port: 443}}},
		{"request-pod.diagnose", "q-8", &Request{Op: OpPodDiagnose, Namespace: "sre-lab-app", Name: "url-shortener-6f89bdc657-rgwsv"}},
		{"request-node.pressure", "q-9", &Request{Op: OpNodePressure, Name: "k8server", NodeMetrics: []Metrics{exampleNodeReport()}}},
		{"request-service.check", "q-11", &Request{Op: OpServiceCheck, Namespace: "kubemap-demo"}},
		{"request-pod.debug", "q-10", &Request{Op: OpPodDebug, Namespace: "kubemap-demo", Name: "web-59f856967c-lj7fl"}},
		{"request-health.check", "q-12", &Request{Op: OpHealthCheck, NodeMetrics: []Metrics{exampleNodeReport()}}},
		{"request-rbac.whocan", "q-14", &Request{Op: OpRBACWhoCan, RBAC: &RBACQuery{Verb: "get", Resource: "secrets", Namespace: "sre-lab-app"}}},
		{"request-pod.usage", "q-13", &Request{Op: OpPodUsage, Namespace: "sre-lab-app"}},
		{"request-object.chain", "q-17", &Request{Op: OpChain, Chain: &ChainRequest{ID: "Deployment/sre-lab-app/url-shortener"}}},
		{"request-logs.search", "q-16", &Request{Op: OpLogSearch, LogSearch: &LogSearchRequest{App: "argocd/url-shortener", Text: "pool",
			Levels: []string{LevelError, LevelWarn}, Since: exampleTime.Add(-time.Hour), Previous: true}}},
		{"request-tokenreview", "q-4", &Request{Op: OpTokenReview, TokenReview: &TokenReviewRequest{Token: "satok_example", Audience: NodeTokenAudience}}},
		{"response-list", "q-1", &Response{
			Items:    []json.RawMessage{json.RawMessage(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web-1","namespace":"kubemap-demo"}}`)},
			Continue: "cont_example",
		}},
		{"response-get", "q-2", &Response{Object: json.RawMessage(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"kubemap-demo"}}`)}},
		{"response-logs", "q-3", &Response{Logs: []byte("\x1b[32mstarting\x1b[0m\npanic: config missing <nil>\n"), Truncated: true}},
		{"response-tokenreview", "q-4", &Response{TokenReview: &TokenReviewResult{
			Authenticated: true, Audiences: []string{NodeTokenAudience}, Username: NodeAgentUsername,
			PodName: "kubemap-node-agent-x7k2p", PodUID: "00000000-0000-0000-0000-0000000000ef", NodeName: "k8server",
			ExpiresAt: ptr(exampleTime.Add(time.Hour)),
		}}},
		{"response-appmap", "q-6", &Response{Object: mustJSON(exampleAppMap())}},
		{"response-netpol.check", "q-7", &Response{Object: mustJSON(exampleNetpolResult())}},
		{"response-pod.diagnose", "q-8", &Response{Object: mustJSON(examplePodDiagnosis())}},
		{"response-node.pressure", "q-9", &Response{Object: mustJSON(exampleNodePressure())}},
		{"response-service.check", "q-11", &Response{Object: mustJSON(exampleServiceCheck())}},
		{"response-pod.debug", "q-10", &Response{Object: mustJSON(examplePodDebug())}},
		{"response-health.check", "q-12", &Response{Object: mustJSON(exampleHealthReport())}},
		{"response-rbac.whocan", "q-14", &Response{Object: mustJSON(exampleRBACWhoCan())}},
		{"response-pod.usage", "q-13", &Response{Object: mustJSON(examplePodUsage())}},
		{"response-logs.search", "q-16", &Response{Object: mustJSON(exampleLogSearch())}},
		{"response-object.chain", "q-17", &Response{Object: mustJSON(exampleChain())}},
		{"response-error", "q-2", &Response{Error: &Error{Code: CodeNotFound, Message: `deployments.apps "web" not found`}}},
		{"stream.open", "s-1", &StreamOpen{Kind: StreamLogs, Namespace: "kubemap-demo", Name: "web-1", Logs: &LogOptions{Container: "app", TailLines: 100, Timestamps: true}}},
		{"stream.data", "s-1", &StreamData{Seq: 0, Data: []byte("2026-09-27T12:00:00Z GET /api/health 200\n")}},
		{"stream.close", "s-1", &StreamClose{Reason: "pod deleted"}},
		{"error", "x-9", &Error{Code: CodeUnsupported, Message: `unknown message type "exec"`}},
	}
}

func ptr[T any](v T) *T { return &v }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// exampleAppMap is url-shortener as the agent sees it on the lab cluster:
// an IngressRoute, the app, PgBouncer in front of CNPG Postgres, and Redis.
func exampleAppMap() *AppMap {
	const ns = "sre-lab-app"
	const route = "IngressRoute.traefik.io/sre-lab-app/url-shortener"
	const app = "Deployment/sre-lab-app/url-shortener"
	const pooler = "Pooler.postgresql.cnpg.io/sre-lab-app/url-shortener-pooler"
	const db = "Cluster.postgresql.cnpg.io/sre-lab-app/url-shortener-db"
	const redis = "StatefulSet/sre-lab-app/redis-master"
	return &AppMap{
		Apps: []MapApp{{
			ID: "argocd/url-shortener", Name: "url-shortener", Namespace: ns, Source: "argocd", Health: "Healthy, OutOfSync", State: StateWarn,
			Components: []MapComponent{
				{ID: route, Kind: "IngressRoute.traefik.io", Namespace: ns, Name: "url-shortener", App: "argocd/url-shortener",
					Tier: TierEntry, TierReason: "IngressRoute.traefik.io rules", Healthy: true,
					Routes: []MapRoute{{Host: "sre-lab.albertycliu.xyz", Path: "/", TLS: true, EntryPoints: []string{"websecure"},
						Service: "Service/sre-lab-app/url-shortener", Port: "80", ReadyEndpoints: 2}}},
				{ID: app, Kind: "Deployment", Namespace: ns, Name: "url-shortener", App: "argocd/url-shortener",
					Tier: TierBackend, TierReason: "receives traffic from an entry and calls a data store", Status: "2/2 ready", Healthy: true,
					Services: []string{"Service/sre-lab-app/url-shortener"},
					Pods:     []MapPod{{Name: "url-shortener-6d5f7-abcde", Node: "k8server", Status: "Running", Ready: true, Restarts: 1}}},
			},
		}, {
			ID: "sre-lab-app/url-shortener-db", Name: "url-shortener-db", Namespace: ns, Source: "label", Health: "Degraded",
			State:    StateFail,
			Problems: []MapProblem{{ID: "Pod/sre-lab-app/url-shortener-db-1", Status: "CrashLoopBackOff (OOMKilled)"}},
			Components: []MapComponent{
				{ID: db, Kind: "Cluster.postgresql.cnpg.io", Namespace: ns, Name: "url-shortener-db", App: "sre-lab-app/url-shortener-db",
					Tier: TierDatabase, TierReason: "postgres: CloudNativePG Cluster", Status: "Cluster in healthy state",
					Engine: "postgres", Version: "16", Services: []string{"Service/sre-lab-app/url-shortener-db-rw"},
					Pods: []MapPod{
						{Name: "url-shortener-db-1", Node: "k8server", Status: "CrashLoopBackOff (OOMKilled)", Restarts: 7},
						{Name: "url-shortener-db-2", Node: "k8server", Status: "Running", Ready: true, Primary: true},
					}},
				{ID: pooler, Kind: "Pooler.postgresql.cnpg.io", Namespace: ns, Name: "url-shortener-pooler", App: "sre-lab-app/url-shortener-db",
					Tier: TierDatabase, TierReason: "pgbouncer: CloudNativePG Pooler", Status: "active", Healthy: true, Engine: "pgbouncer",
					Services: []string{"Service/sre-lab-app/url-shortener-pooler"},
					Pods:     []MapPod{{Name: "url-shortener-pooler-7c9d8-fghij", Node: "k8server", Status: "Running", Ready: true}}},
			},
		}},
		Links: []MapLink{
			{From: app, To: redis, FromApp: "argocd/url-shortener", ToApp: "sre-lab-app/redis", Via: []string{"Service/sre-lab-app/redis-master"},
				Evidence: []MapEvidence{{Type: EvidenceConfigMap, Object: "ConfigMap/sre-lab-app/url-shortener-db-config", Name: "REDIS_HOST", Port: "6379",
					Detail: "env REDIS_HOST (ConfigMap url-shortener-db-config) → Service/sre-lab-app/redis-master:6379"}}},
			{From: app, To: pooler, FromApp: "argocd/url-shortener", ToApp: "sre-lab-app/url-shortener-db", Via: []string{"Service/sre-lab-app/url-shortener-pooler"},
				Evidence: []MapEvidence{{Type: EvidenceConfigMap, Object: "ConfigMap/sre-lab-app/url-shortener-db-config", Name: "DB_HOST", Key: "host", Port: "5432",
					Detail: "env DB_HOST (ConfigMap url-shortener-db-config) → Service/sre-lab-app/url-shortener-pooler:5432"}}},
			{From: pooler, To: db, FromApp: "sre-lab-app/url-shortener-db", ToApp: "sre-lab-app/url-shortener-db",
				Evidence: []MapEvidence{{Type: EvidenceOperator, Object: pooler, Name: "url-shortener-db",
					Detail: "CloudNativePG Pooler pgbouncer pools url-shortener-db"}}},
			{From: route, To: app, FromApp: "argocd/url-shortener", ToApp: "argocd/url-shortener", Via: []string{"Service/sre-lab-app/url-shortener"},
				Evidence: []MapEvidence{{Type: EvidenceRoute, Object: route, Name: "sre-lab.albertycliu.xyz/", Port: "80",
					Detail: "sre-lab.albertycliu.xyz/ → Service/sre-lab-app/url-shortener:80"}}},
		},
		Peers: []MapComponent{{ID: redis, Kind: "StatefulSet", Namespace: ns, Name: "redis-master", App: "sre-lab-app/redis",
			Tier: TierCache, TierReason: "redis: image bitnamilegacy/redis:latest", Status: "1/1 ready", Healthy: true, Engine: "redis",
			Services: []string{"Service/sre-lab-app/redis-master"}}},
	}
}

// exampleNetpolResult is the lab cluster's deny-egress-to-argocd finding:
// the policy's only egress rule selects namespaces, so it also blocks every
// non-pod IP, such as the internet.
func exampleNetpolResult() *NetpolResult {
	return &NetpolResult{
		Allowed:  false,
		Summary:  "blocked: IP 1.1.1.1: egress blocked: isolated by sre-lab-app/deny-egress-to-argocd and no egress rule matches",
		From:     "Pod/sre-lab-app/url-shortener-6f89bdc657-rgwsv",
		Port:     443,
		Protocol: "TCP",
		Paths: []NetpolPath{{
			To: "IP 1.1.1.1", Port: 443,
			Egress: NetpolSide{Direction: "egress", Isolated: true,
				Decision:  "blocked: isolated by sre-lab-app/deny-egress-to-argocd and no egress rule matches",
				Selecting: []string{"sre-lab-app/deny-egress-to-argocd"}},
			Ingress: NetpolSide{Direction: "ingress", Allowed: true, Decision: "not a pod: no policy applies at this end"},
		}},
	}
}

// examplePodDiagnosis is the lab cluster's first real case (2026-09-28): the
// url-shortener image was imported locally with pull policy Never and was
// removed by image garbage collection under disk pressure.
func examplePodDiagnosis() *PodDiagnosis {
	return &PodDiagnosis{
		Namespace: "sre-lab-app", Name: "url-shortener-6f89bdc657-rgwsv", Node: "k8server",
		Owner: "Deployment/sre-lab-app/url-shortener", Phase: "Pending", Status: "ErrImageNeverPull", QOS: "BestEffort",
		Findings: []DiagFinding{
			{Severity: "critical", Container: "url-shortener", Title: "Image not on the node",
				Detail: `Container image "docker.io/library/url-shortener:v0.2" is not present with pull policy of Never The image docker.io/library/url-shortener:v0.2 must already be on node k8server because imagePullPolicy is Never. A locally built or imported image is lost when the kubelet's image garbage collection runs (for example under disk pressure) or when the pod moves to another node. Import the image on the node again, or push it to a registry and use IfNotPresent.`},
			{Severity: "info", Container: "url-shortener", Title: "No memory limit",
				Detail: "Without a limit the container can use the node's memory until the node is under pressure, and then it is among the first to be evicted or OOM-killed."},
		},
		Containers: []DiagContainer{{
			Name: "url-shortener", Image: "docker.io/library/url-shortener:v0.2", State: "waiting: ErrImageNeverPull",
			Probes: []string{"liveness: GET /healthz :8080 every 10s, timeout 1s, 3 failures, first after 10s"},
		}},
		Events: []DiagEvent{
			{Type: "Warning", Reason: "ErrImageNeverPull", Message: `Container image "docker.io/library/url-shortener:v0.2" is not present with pull policy of Never`, Count: 88, Last: exampleTime},
			{Type: "Normal", Reason: "Scheduled", Message: "Successfully assigned sre-lab-app/url-shortener-6f89bdc657-rgwsv to k8server", Count: 1, Last: exampleTime.Add(-18 * time.Minute)},
		},
		Notes: []string{"usage not available: the pod is Pending"},
	}
}

// exampleNodePressure is k8server on 2026-09-28: Kubernetes says all clear,
// but tasks wait for CPU a quarter of the time.
func exampleNodePressure() *NodePressureReport {
	since := time.Date(2026, 9, 27, 15, 29, 9, 0, time.UTC)
	return &NodePressureReport{Time: exampleTime, Nodes: []NodePressure{{
		Node: "k8server", Ready: true, PSIAvailable: true, ReportTime: exampleTime.Add(-5 * time.Second), Verdict: VerdictPressure, Summary: "pressure: [cpu]",
		Resources: []ResourcePressure{
			{Resource: "cpu", PSI: &Pressure{Some: PressureAvg{Avg10: 24.2, Avg60: 27.26, Avg300: 27.25}, Full: &PressureAvg{}}, Verdict: VerdictPressure,
				Detail: "tasks waited for CPU 27.3% of the last minute (10 s: 24.2%, 5 min: 27.2%); Kubernetes has no cpu pressure condition, so it reports all clear"},
			{Resource: "memory", Condition: "MemoryPressure", ConditionStatus: "False", ConditionSince: since,
				PSI: &Pressure{Some: PressureAvg{Avg300: 0.04}, Full: &PressureAvg{}}, Verdict: VerdictOK,
				Detail: "tasks stalled on memory (reclaim, swap) 0.0% of the last minute (10 s: 0.0%, 5 min: 0.0%)"},
			{Resource: "io", PSI: &Pressure{Some: PressureAvg{Avg10: 0.3, Avg60: 0.2, Avg300: 0.4}, Full: &PressureAvg{Avg60: 0.1}}, Verdict: VerdictOK,
				Detail: "tasks waited for disk or network I/O 0.2% of the last minute (10 s: 0.3%, 5 min: 0.4%), all tasks stalled 0.1%"},
			{Resource: "disk", Condition: "DiskPressure", ConditionStatus: "False", ConditionSince: since, Verdict: VerdictOK, Detail: "DiskPressure is False"},
			{Resource: "pids", Condition: "PIDPressure", ConditionStatus: "False", ConditionSince: since, Verdict: VerdictOK, Detail: "PIDPressure is False"},
		},
	}}}
}

// exampleServiceCheck is the demo's badsvc scenario and a healthy Service.
func exampleServiceCheck() *ServiceCheckResult {
	return &ServiceCheckResult{Problems: 1, Services: []ServiceStatus{
		{ID: "Service/kubemap-demo/catalog", Namespace: "kubemap-demo", Name: "catalog", Type: "ClusterIP",
			Selector: map[string]string{"app.kubernetes.io/name": "catalog-api"}, Findings: []ServiceFinding{{
				Severity: "critical", Problem: "selector matches no pods",
				Detail:  `No running pod in kubemap-demo has all of app.kubernetes.io/name=catalog-api. Pods here have app.kubernetes.io/name=api,batch-worker,catalog,checkout,oom, not catalog-api (a typo? "catalog-api" vs "catalog").`,
				Fix:     "Fix the Service's selector, or the labels in the workload's pod template, so they agree.",
				Command: "kubectl -n kubemap-demo get pods -l app.kubernetes.io/name=catalog-api; kubectl -n kubemap-demo get pods --show-labels",
			}}},
		{ID: "Service/kubemap-demo/api", Namespace: "kubemap-demo", Name: "api", Type: "ClusterIP",
			Selector: map[string]string{"app.kubernetes.io/name": "api"}, PodsMatched: 1, PodsReady: 1, ReadyEndpoints: 1, Healthy: true},
	}}
}

// examplePodDebug is the demo's netpol scenario seen from the client pod
// (2026-09-28): the pod is healthy, its logs show timeouts, and the
// NetworkPolicy on the backend is the cause.
func examplePodDebug() *PodDebug {
	const k = "kubectl -n kubemap-demo "
	const pod = "web-59f856967c-lj7fl"
	return &PodDebug{
		Namespace: "kubemap-demo", Name: pod, Owner: "Deployment/kubemap-demo/web", Status: "Running",
		Cause:     "NetworkPolicy kubemap-demo/api-allow-monitoring-only blocks this pod → Service kubemap-demo/api:80 (named in env API_URL).",
		CauseStep: StepNetpol,
		Steps: []DebugStep{
			{Key: StepStatus, Title: "Status and reason", Verdict: VerdictOK, Finding: "Running, and every container is ready.",
				Evidence: []string{"phase Running, status Running", "container web: running since 2026-09-28T00:24:33Z, ready true, restarts 0"},
				Commands: []string{k + "get pod " + pod + " -o wide"}},
			{Key: StepEvents, Title: "Events", Verdict: VerdictOK, Finding: "No warning events (5 normal).",
				Commands: []string{k + "describe pod " + pod}},
			{Key: StepLogs, Title: "Logs", Verdict: VerdictProblem,
				Finding:  "The logs show errors: 2026-09-28T00:40:16Z ERROR GET http://api.kubemap-demo.svc/api failed: wget: download timed out",
				Evidence: []string{"web (current run): 2026-09-28T00:40:16Z ERROR GET http://api.kubemap-demo.svc/api failed: wget: download timed out"},
				Commands: []string{k + "logs " + pod + " -c web --tail=100"}},
			{Key: StepExitCode, Title: "Exit code", Verdict: VerdictOK, Finding: "No container has exited.",
				Commands: []string{k + "get pod " + pod + " -o jsonpath='{.status.containerStatuses[*].lastState}'"}},
			{Key: StepResources, Title: "Requests and limits vs usage", Verdict: VerdictUnknown,
				Finding:  "Usage is not available (metrics API), and nothing was killed for memory.",
				Evidence: []string{"web: requests cpu 5m, memory 6Mi, limits cpu 50m, memory 6Mi"},
				Commands: []string{k + "top pod " + pod + " --containers"}},
			{Key: StepNode, Title: "Node", Verdict: VerdictOK, Finding: "Node k8server is Ready with no memory, disk or PID pressure.",
				Commands: []string{"kubectl describe node k8server"}},
			{Key: StepService, Title: "Service endpoints", Verdict: VerdictOK, Finding: "The Services this pod serves or uses have ready endpoints.",
				Evidence: []string{"Service kubemap-demo/api (this pod's dependency, named in env API_URL): 1 pods, 1 ready, 1 ready endpoints"},
				Commands: []string{"kubectl -n kubemap-demo get endpointslices -l kubernetes.io/service-name=api"}},
			{Key: StepNetpol, Title: "NetworkPolicy", Verdict: VerdictProblem,
				Finding: "NetworkPolicy kubemap-demo/api-allow-monitoring-only blocks this pod → Service kubemap-demo/api:80 (named in env API_URL).",
				Evidence: []string{"this pod → Service kubemap-demo/api:80 (named in env API_URL): BLOCKED — ingress blocked: isolated by kubemap-demo/api-allow-monitoring-only and no ingress rule matches",
					"ingress isolated by: none; egress isolated by: none"},
				Commands: []string{k + "get networkpolicy", k + "describe networkpolicy api-allow-monitoring-only"}},
		},
		Notes: []string{"usage not available: forbidden by the agent's RBAC"},
	}
}

// exampleChain is url-shortener's Deployment on k8server (28 Sep 2026), cut short.
func exampleChain() *ObjectChain {
	it := func(id, status, detail string) ChainItem {
		p := strings.SplitN(id, "/", 3)
		return ChainItem{ID: id, Kind: p[0], Namespace: p[1], Name: p[2], Status: status, Detail: detail}
	}
	self := it("Deployment/sre-lab-app/url-shortener", "2/2 ready", "")
	return &ObjectChain{
		Object: &self,
		Sections: []ChainSection{
			{Key: ChainOwned, Items: []ChainItem{it("ReplicaSet/sre-lab-app/url-shortener-6f89bdc657", "2/2 ready", "")}, More: 8},
			{Key: ChainPods, Items: []ChainItem{it("Pod/sre-lab-app/url-shortener-6f89bdc657-jxxl8", "Running", ""), it("Pod/sre-lab-app/url-shortener-6f89bdc657-vktnd", "Running", "")}},
			{Key: ChainServices, Items: []ChainItem{it("Service/sre-lab-app/url-shortener", "", "")}},
			{Key: ChainRoutes, Items: []ChainItem{it("IngressRoute.traefik.io/sre-lab-app/url-shortener-https", "", "sre-lab.albertycliu.xyz/ → sre-lab-app/url-shortener:80")}},
			{Key: ChainConfig, Items: []ChainItem{it("ConfigMap/sre-lab-app/url-shortener-db-config", "", "env DB_HOST")}},
			{Key: ChainSecrets, Items: []ChainItem{it("Secret/sre-lab-app/postgres-cnpg-secret", "", "env DB_PASSWORD")}},
			{Key: ChainServiceAccounts, Items: []ChainItem{it("ServiceAccount/sre-lab-app/sre-lab-app-sa", "", "")}},
			{Key: ChainPolicies, Items: []ChainItem{it("NetworkPolicy/sre-lab-app/default-deny-ingress", "", "isolates ingress")}},
			{Key: ChainNodes, Items: []ChainItem{it("Node//k8server", "Ready", "")}},
			{Key: ChainCalls, Items: []ChainItem{it("Service/sre-lab-app/url-shortener-pooler", "", "env DB_HOST (ConfigMap url-shortener-db-config)")}},
		},
		Apps: []string{"argocd/url-shortener"},
	}
}

// exampleLogSearch is url-shortener running out of database connections:
// an error with its Python traceback, and a warning from the pooler.
func exampleLogSearch() *LogSearchResult {
	return &LogSearchResult{App: "argocd/url-shortener", Since: exampleTime.Add(-time.Hour), Until: exampleTime,
		Entries: []LogEntry{{
			Time: exampleTime.Add(-12 * time.Minute), Namespace: "sre-lab-app", Pod: "url-shortener-6f89bdc657-jxxl8", Container: "url-shortener",
			Previous: true, Level: LevelError, Text: "Traceback (most recent call last):",
			Stack: []string{`  File "/app/app.py", line 41, in shorten`, "    conn = pool.getconn()", "psycopg2.pool.PoolError: connection pool exhausted"},
		}, {
			Time: exampleTime.Add(-11 * time.Minute), Namespace: "sre-lab-app", Pod: "url-shortener-pooler-7c9d8-abcde", Container: "pgbouncer",
			Level: LevelWarn, Text: "2026-09-27 11:49:00.000 UTC [1] WARNING C-0x55d: sre/app@10.42.0.31:40112 pooler error: no more connections allowed (max_client_conn)",
		}},
		Levels:  map[string]int{LevelError: 1, LevelWarn: 1, LevelInfo: 214, LevelDebug: 0, LevelUnknown: 3},
		Matched: 2,
		Sources: []LogSource{
			{Namespace: "sre-lab-app", Pod: "url-shortener-6f89bdc657-jxxl8", Container: "url-shortener", Previous: true, Lines: 40, Matched: 1},
			{Namespace: "sre-lab-app", Pod: "url-shortener-6f89bdc657-jxxl8", Container: "url-shortener", Lines: 180, Matched: 0},
			{Namespace: "sre-lab-app", Pod: "url-shortener-pooler-7c9d8-abcde", Container: "pgbouncer", Lines: 60, Matched: 1},
			{Namespace: "sre-lab-app", Pod: "url-shortener-db-1", Container: "postgres", Error: "forbidden: pods \"url-shortener-db-1\" is forbidden"},
		},
	}
}

// exampleHealthReport is k8server during the 2026-09-28 disk incident, with
// the kubelet settings described to the agent.
func exampleHealthReport() *HealthReport {
	return &HealthReport{Time: exampleTime,
		Findings: []HealthFinding{{
			Rule: "node.disk.eviction", Severity: SeverityCritical, Object: "Node/k8server",
			Title:    "DiskPressure: the kubelet is evicting pods (13.7% free)",
			Evidence: "root filesystem (nodefs): 12.8 GiB free of 93.2 GiB (13.7%). The kubelet evicts pods below 5% (nodefs.available<5%) and then keeps evicting until 15% free (14.0 GiB) (minimum reclaim 10%). Container images are on the same filesystem. DiskPressure is True since 2026-09-27 23:32 UTC.",
			Meaning:  "The disk that holds container logs, emptyDir volumes and writable layers (and on k3s also the images) is close to the kubelet's hard eviction threshold.",
			Why:      "At the threshold the kubelet sets DiskPressure, evicts pods (BestEffort first) and deletes unused images, and it does not stop at the threshold: it frees until 15% free (14.0 GiB).",
			Fix: "Eviction continues until 15% free (14.0 GiB): free at least 1.2 GiB now to get above 15% free. " +
				"Do not prune images yet: 2 pods use images that exist only on this node (imagePullPolicy: Never): sre-lab-app/url-shortener-6f89bdc657-jxxl8, sre-lab-app/url-shortener-6f89bdc657-vktnd. Push them to a registry or save them first (k3s ctr images export), and remove only named images. " +
				"In this order: (1) what is outside Kubernetes: build caches (go clean -cache, docker builder prune) and old logs (journalctl --vacuum-size=500M); (2) named images you know a registry still has (crictl images, then crictl rmi <image>); (3) only then crictl rmi --prune, which deletes every image that no running container uses, including the images of evicted, crash-looping or scaled-down workloads.",
			Verify: "df -h /   # above 15% free\nkubectl describe node k8server | grep DiskPressure   # False",
		}, {
			Rule: "pod.evicted", Severity: SeverityWarning, Object: "Cluster",
			Title:    "2840 pods were evicted",
			Evidence: "Stopped by the kubelet (DisruptionTarget TerminationByKubelet, or reason Evicted): argocd/argocd-server-7d9f-abcde and 2839 more.",
			Meaning:  "The kubelet stopped these pods to protect the node (disk, memory or PID pressure). Their records stay until deleted.",
			Why:      "Evictions mean the node ran out of something; the records also hide the real pods in lists.",
			Fix:      "Find what ran out, fix that, then delete only these records by name: kubectl delete pod -n argocd argocd-server-7d9f-abcde … (2839 more). Do not delete by phase across all namespaces: that also removes failed Job pods kept for debugging.",
			Verify:   "kubectl get pod -n argocd argocd-server-7d9f-abcde   # NotFound",
			Items:    []string{"argocd/argocd-server-7d9f-abcde"}, Count: 2840,
		}},
		Rules: []string{"node.not-ready", "node.disk.eviction", "pod.evicted"},
		Thresholds: HealthThresholds{
			EvictionHard:           map[string]string{"nodefs.available": "5%", "imagefs.available": "5%", "memory.available": "100Mi"},
			EvictionMinimumReclaim: map[string]string{"nodefs.available": "10%"},
			ImageGCHighPercent:     85, Source: "agent settings",
		},
	}
}

// examplePodUsage is flask-vue-backend on k8server, from metrics-server.
func examplePodUsage() *PodUsageReport {
	return &PodUsageReport{Time: exampleTime, Pods: []PodUsage{{
		Namespace: "sre-lab-app", Name: "flask-vue-backend-6688dd7cbf-6tspt", Node: "k8server",
		SampledAt: exampleTime.Add(-12 * time.Second), WindowSeconds: 15.1,
		CPUMillis: 2, MemoryBytes: 50_212_864,
		Containers: []ContainerUsage{{Name: "backend", CPUMillis: 2, MemoryBytes: 50_212_864,
			CPURequestMillis: 50, MemoryRequestBytes: 64 << 20, MemoryLimitBytes: 256 << 20,
			CPUOfRequest: 4, MemoryOfRequest: 74.8, MemoryOfLimit: 18.7}},
	}}}
}

// exampleNodeReport is k8server's node agent report as the portal passes it
// in node.pressure and health.check: without topProcesses and network.
func exampleNodeReport() Metrics {
	return Metrics{
		Time: exampleTime.Add(-5 * time.Second), NodeName: "k8server",
		CPU: CPUStats{Cores: 4, UsagePercent: 71.5}, Memory: MemoryStats{TotalBytes: 12 << 30, AvailableBytes: 3 << 30},
		Swap: SwapStats{TotalBytes: 4 << 30, UsedBytes: 4 << 30}, Load: LoadStats{Load1: 6.2, Load5: 5.8, Load15: 5.1},
		PSI: &PSIStats{CPU: Pressure{Some: PressureAvg{Avg10: 24.2, Avg60: 27.26, Avg300: 27.25}, Full: &PressureAvg{}},
			Memory: Pressure{Some: PressureAvg{Avg300: 0.04}, Full: &PressureAvg{}},
			IO:     Pressure{Some: PressureAvg{Avg10: 0.3, Avg60: 0.2, Avg300: 0.4}, Full: &PressureAvg{Avg60: 0.1}}},
		Filesystems: []FilesystemStats{{MountPoint: "/", Device: "/dev/mapper/ubuntu--vg-ubuntu--lv", FSType: "ext4",
			TotalBytes: 100073570304, UsedBytes: 86300000000, AvailableBytes: 13700000000, InodesTotal: 6225920, InodesFree: 5429637}},
	}
}

// exampleRBACWhoCan is "who can get Secrets in sre-lab-app" on the lab
// cluster (2026-09-28), shortened to three of its answers.
func exampleRBACWhoCan() *RBACResult {
	return &RBACResult{Mode: RBACWhoCan,
		Subjects: []RBACWho{
			{Kind: "Group", Name: "system:masters", Binding: "ClusterRoleBinding//cluster-admin", Role: "ClusterRole//cluster-admin", Scope: "cluster"},
			{Kind: "ServiceAccount", Namespace: "argocd", Name: "argocd-application-controller", Binding: "ClusterRoleBinding//argocd-application-controller",
				Role: "ClusterRole//argocd-application-controller", Scope: "cluster"},
			{Kind: "ServiceAccount", Namespace: "kube-system", Name: "traefik", Binding: "ClusterRoleBinding//traefik-kube-system", Role: "ClusterRole//traefik-kube-system", Scope: "cluster"},
		},
		Notes: []string{"Evaluated from the RBAC objects in the agent's cache with the RBAC authorizer's rules; other authorizers (Node, webhooks) and admission (PodSecurity, quota, webhooks) are not included."},
	}
}
