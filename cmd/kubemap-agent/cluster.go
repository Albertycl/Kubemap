package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/cache"
	"kubemap/agent/cluster/datastores"
	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/query"
	"kubemap/agent/cluster/routes"
	"kubemap/agent/cluster/stream"
	"kubemap/agent/cluster/summary"
	"kubemap/pkg/protocol"
	"kubemap/pkg/version"
	"kubemap/portal/localcluster"
)

// runCheck is a read-only diagnostic. The cluster agent itself runs inside
// the kubemap server; this fills the same cache once (list and watch only),
// prints what it holds and exits.
func runCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig file (default: in-cluster, then $KUBECONFIG or ~/.kube/config)")
	timeout := fs.Duration("timeout", 60*time.Second, "how long to wait for the first sync")
	showGraph := fs.Bool("graph", false, "also build the relationship graph and print its findings")
	showApps := fs.Bool("apps", false, "also group workloads into applications and print them")
	showDeps := fs.Bool("deps", false, "also find dependencies from env vars and ConfigMaps")
	showRoutes := fs.Bool("routes", false, "also print the Ingress and IngressRoute route table")
	showStores := fs.Bool("datastores", false, "also recognise databases, caches and queues")
	showSummary := fs.Bool("summary", false, "also build the summary message pushed to the portal")
	follow := fs.String("stream", "", "follow one pod log as the portal would, as namespace/pod[/container] (read-only)")
	followFor := fs.Duration("for", 5*time.Second, "with --stream: how long to follow")
	requests := fs.String("request", "", "answer these protocol requests (JSON object or array) and print the responses")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *follow != "" {
		return followLog(*kubeconfig, *follow, *followFor)
	}
	cfg, err := restConfig(*kubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
		return 1
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
		return 1
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
		return 1
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	c := cache.New(kube, dyn, disc, cache.Options{})
	if err := c.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
		return 1
	}
	wctx, wcancel := context.WithTimeout(ctx, *timeout)
	synced := c.WaitForSync(wctx)
	wcancel()
	took := time.Since(start)

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tSTATE\tCOUNT\tERROR")
	total := 0
	for _, s := range c.Status() {
		total += s.Count
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.GVR.GroupResource(), s.State, s.Count, s.LastError)
	}
	tw.Flush()
	fmt.Printf("\n%d objects, all synced: %v, sync took %v, heap in use %.1f MiB (sys %.1f MiB)\n",
		total, synced, took.Round(time.Millisecond), float64(ms.HeapInuse)/(1<<20), float64(ms.Sys)/(1<<20))
	if *showGraph {
		printGraph(c)
	}
	if *showApps {
		printApps(c)
	}
	if *showDeps {
		printDeps(c)
	}
	if *showRoutes {
		printRoutes(c)
	}
	if *showStores {
		printDatastores(c)
	}
	if *showSummary {
		version := ""
		if v, err := disc.ServerVersion(); err == nil {
			version = v.GitVersion
		}
		printSummary(c, version)
	}
	if *requests != "" {
		h := query.New(c, kube, dyn, graph.NewBuilder(c))
		if h.Thresholds, err = localcluster.HealthThresholds(); err != nil {
			fmt.Fprintln(os.Stderr, "kubemap-agent check: health thresholds:", err)
			return 1
		}
		if err := answer(ctx, h, *requests); err != nil {
			fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
			return 1
		}
	}
	if !synced {
		return 1
	}
	return 0
}

// printSender prints what a log stream would send over the tunnel.
type printSender struct{ bytes, chunks int }

func (p *printSender) Send(id string, m protocol.Payload) error {
	switch x := m.(type) {
	case *protocol.StreamData:
		p.chunks++
		p.bytes += len(x.Data)
		fmt.Printf("stream.data id=%s seq=%d %d bytes\n%s", id, x.Seq, len(x.Data), x.Data)
	case *protocol.StreamClose:
		fmt.Printf("stream.close id=%s reason=%q error=%v\n", id, x.Reason, x.Error)
	}
	return nil
}

// followLog follows one pod log through the stream manager for a while.
func followLog(kubeconfig, target string, d time.Duration) int {
	parts := strings.Split(target, "/")
	if len(parts) < 2 || len(parts) > 3 {
		fmt.Fprintln(os.Stderr, "kubemap-agent check: --stream wants namespace/pod[/container]")
		return 2
	}
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
		return 1
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent check:", err)
		return 1
	}
	out := &printSender{}
	m := &stream.Manager{Logs: stream.KubeLogs(kube), Sender: out}
	opts := &protocol.LogOptions{TailLines: 5, Timestamps: true}
	if len(parts) == 3 {
		opts.Container = parts[2]
	}
	m.Open(context.Background(), "s-1", &protocol.StreamOpen{Kind: protocol.StreamLogs, Namespace: parts[0], Name: parts[1], Logs: opts})
	time.Sleep(d)
	m.Close("s-1")
	m.CloseAll()
	fmt.Printf("\nfollowed %s for %v: %d chunks, %d bytes\n", target, d, out.chunks, out.bytes)
	return 0
}

// answer runs protocol requests through the query handler, as the portal
// would, and prints each response's size and a short digest.
func answer(ctx context.Context, h *query.Handler, raw string) error {
	var reqs []protocol.Request
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
			return err
		}
	} else {
		var r protocol.Request
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return err
		}
		reqs = []protocol.Request{r}
	}
	for i := range reqs {
		req := &reqs[i]
		if req.Op == protocol.OpTokenReview {
			return fmt.Errorf("tokenreview creates an object; not run from the command line")
		}
		start := time.Now()
		resp := h.Handle(ctx, req)
		msg, err := protocol.Encode(fmt.Sprintf("q-%d", i+1), resp)
		if err != nil {
			return err
		}
		fmt.Printf("\nrequest %d: op=%s %s/%s ns=%q name=%q -> %d bytes in %v\n", i+1, req.Op, req.Group, req.Resource,
			req.Namespace, req.Name, len(msg), time.Since(start).Round(time.Millisecond))
		switch {
		case resp.Error != nil:
			fmt.Printf("  error %s: %s\n", resp.Error.Code, resp.Error.Message)
		case req.Op == protocol.OpList:
			fmt.Printf("  %d items, continue=%q\n", len(resp.Items), resp.Continue)
		case req.Op == protocol.OpLogs:
			lines := strings.Split(strings.TrimRight(string(resp.Logs), "\n"), "\n")
			fmt.Printf("  %d bytes, %d lines, truncated=%v; last line: %.120s\n", len(resp.Logs), len(lines), resp.Truncated, lines[len(lines)-1])
		case req.Op == protocol.OpGraph:
			var m query.AppMap
			if err := json.Unmarshal(resp.Object, &m); err != nil {
				return err
			}
			fmt.Printf("  %d apps, %d links, %d routes, %d datastores, graph %d nodes / %d edges\n",
				len(m.Apps), len(m.Links), len(m.Routes), len(m.Datastores), len(m.Graph.Nodes), len(m.Graph.Edges))
		case req.Op == protocol.OpNodePressure:
			var r protocol.NodePressureReport
			if err := json.Unmarshal(resp.Object, &r); err != nil {
				return err
			}
			for _, n := range r.Nodes {
				fmt.Printf("  node %s (ready %v, PSI available %v): %s — %s\n", n.Node, n.Ready, n.PSIAvailable, n.Verdict, n.Summary)
				for _, x := range n.Resources {
					cond := x.Condition
					if cond != "" {
						cond += "=" + x.ConditionStatus
					}
					fmt.Printf("    %-6s %-8s %-22s %s\n", x.Resource, x.Verdict, cond, x.Detail)
				}
			}
			for _, n := range r.Notes {
				fmt.Printf("  note: %s\n", n)
			}
		case req.Op == protocol.OpPodDebug:
			var d protocol.PodDebug
			if err := json.Unmarshal(resp.Object, &d); err != nil {
				return err
			}
			fmt.Printf("  %s/%s (%s), owner %s\n", d.Namespace, d.Name, d.Status, d.Owner)
			if d.Cause != "" {
				fmt.Printf("  cause [%s]: %s\n", d.CauseStep, d.Cause)
			} else {
				fmt.Printf("  no problem found\n")
			}
			for i, s := range d.Steps {
				fmt.Printf("  %d. %-30s %-8s %.200s\n", i+1, s.Title, s.Verdict, s.Finding)
				for _, e := range s.Evidence[:min(len(s.Evidence), 3)] {
					fmt.Printf("       · %.180s\n", e)
				}
				fmt.Printf("       $ %.180s\n", s.Commands[0])
			}
		case req.Op == protocol.OpServiceCheck:
			var r protocol.ServiceCheckResult
			if err := json.Unmarshal(resp.Object, &r); err != nil {
				return err
			}
			fmt.Printf("  %d Services, %d with problems\n", len(r.Services), r.Problems)
			for _, s := range r.Services {
				if s.Healthy {
					continue
				}
				fmt.Printf("  %s: %d pods, %d ready, %d ready endpoints\n", s.ID, s.PodsMatched, s.PodsReady, s.ReadyEndpoints)
				for _, f := range s.Findings {
					fmt.Printf("    [%s] %s %s\n      %.260s\n      $ %s\n", f.Severity, f.Problem, f.Port, f.Detail, f.Command)
				}
			}
		case req.Op == protocol.OpRBACWhoCan:
			var r protocol.RBACResult
			if err := json.Unmarshal(resp.Object, &r); err != nil {
				return err
			}
			switch r.Mode {
			case protocol.RBACWhoCan:
				fmt.Printf("  %d subjects can do it:\n", len(r.Subjects))
				for _, w := range r.Subjects {
					names := ""
					if len(w.ResourceNames) > 0 {
						names = " (only " + strings.Join(w.ResourceNames, ",") + ")"
					}
					fmt.Printf("    %-14s %-55s via %s → %s [%s]%s\n", w.Kind, strings.TrimPrefix(w.Namespace+"/"+w.Name, "/"), w.Binding, w.Role, w.Scope, names)
				}
			default:
				if r.Allowed != nil {
					fmt.Printf("  allowed: %v\n", *r.Allowed)
				}
				fmt.Printf("  groups: %v; risky: %v\n", r.Groups, r.Risky)
				for _, g := range r.Grants {
					fmt.Printf("    %s → %s [%s] via %s risky=%v (%d rules)\n", g.Binding, g.Role, g.Scope, g.Via, g.Risky, len(g.Rules))
				}
			}
			for _, n := range r.Notes[1:] {
				fmt.Printf("  note: %s\n", n)
			}
		case req.Op == protocol.OpPodUsage:
			var r protocol.PodUsageReport
			if err := json.Unmarshal(resp.Object, &r); err != nil {
				return err
			}
			fmt.Printf("  %d pods, largest memory first:\n", len(r.Pods))
			for _, p := range r.Pods[:min(len(r.Pods), 15)] {
				fmt.Printf("  %-55s %6dm CPU %8.1f MiB mem  on %s, sampled %s over %.0fs\n", p.Namespace+"/"+p.Name, p.CPUMillis,
					float64(p.MemoryBytes)/(1<<20), p.Node, p.SampledAt.Format("15:04:05"), p.WindowSeconds)
				for _, c := range p.Containers {
					lim := "no limit"
					if c.MemoryLimitBytes > 0 {
						lim = fmt.Sprintf("%.0f%% of %d MiB limit", c.MemoryOfLimit, c.MemoryLimitBytes>>20)
					}
					fmt.Printf("      %-20s %6dm CPU %8.1f MiB (%s)\n", c.Name, c.CPUMillis, float64(c.MemoryBytes)/(1<<20), lim)
				}
			}
			for _, n := range r.Notes {
				fmt.Printf("  note: %s\n", n)
			}
		case req.Op == protocol.OpHealthCheck:
			var r protocol.HealthReport
			if err := json.Unmarshal(resp.Object, &r); err != nil {
				return err
			}
			fmt.Printf("  %d findings from %d rules; thresholds (%s): evictionHard %v, minimum reclaim %v, image GC at %d%%\n",
				len(r.Findings), len(r.Rules), r.Thresholds.Source, r.Thresholds.EvictionHard, r.Thresholds.EvictionMinimumReclaim, r.Thresholds.ImageGCHighPercent)
			for _, f := range r.Findings {
				fmt.Printf("\n  [%s] %s  %s\n    %s\n", f.Severity, f.Object, f.Title, f.Evidence)
				fmt.Printf("    meaning: %s\n    why:     %s\n    fix:     %s\n    verify:  %s\n", f.Meaning, f.Why, f.Fix,
					strings.ReplaceAll(f.Verify, "\n", "\n             "))
				if f.Count > len(f.Items) {
					fmt.Printf("    items:   %d of %d shown\n", len(f.Items), f.Count)
				}
			}
			for _, n := range r.Notes {
				fmt.Printf("  note: %s\n", n)
			}
		case req.Op == protocol.OpPodDiagnose:
			var d protocol.PodDiagnosis
			if err := json.Unmarshal(resp.Object, &d); err != nil {
				return err
			}
			fmt.Printf("  %s/%s on %s, owner %s: %s (phase %s)\n", d.Namespace, d.Name, d.Node, d.Owner, d.Status, d.Phase)
			for _, f := range d.Findings {
				fmt.Printf("  [%s] %s %s\n      %.300s\n", f.Severity, f.Container, f.Title, f.Detail)
			}
			for _, c := range d.Containers {
				fmt.Printf("  container %s: %s, restarts %d, requests %v, limits %v\n", c.Name, c.State, c.Restarts, c.Requests, c.Limits)
				if c.Last != nil {
					fmt.Printf("      last run: %s\n", c.Last.Meaning)
				}
				if c.Usage != nil {
					fmt.Printf("      usage: %dm CPU (%.1f%% of limit), %d MiB (%.1f%% of limit)\n", c.Usage.CPUMillis, c.Usage.CPUOfLimit, c.Usage.MemoryBytes>>20, c.Usage.MemOfLimit)
				}
				if c.Logs != "" {
					lines := strings.Split(strings.TrimRight(c.Logs, "\n"), "\n")
					fmt.Printf("      logs (%s, %d lines), last: %.160s\n", c.LogsFrom, len(lines), lines[len(lines)-1])
				}
			}
			for _, e := range d.Events[:min(len(d.Events), 5)] {
				fmt.Printf("  event %s %s x%d: %.140s\n", e.Type, e.Reason, e.Count, e.Message)
			}
			for _, n := range d.Notes {
				fmt.Printf("  note: %s\n", n)
			}
		case req.Op == protocol.OpNetpolCheck:
			var r protocol.NetpolResult
			if err := json.Unmarshal(resp.Object, &r); err != nil {
				return err
			}
			fmt.Printf("  %s\n", r.Summary)
			for _, p := range r.Paths {
				fmt.Printf("    -> %s:%d allowed=%v\n       egress:  %s\n       ingress: %s\n", p.To, p.Port, p.Allowed, p.Egress.Decision, p.Ingress.Decision)
			}
			for _, n := range r.Notes {
				fmt.Printf("    note: %s\n", n)
			}
		case req.Op == protocol.OpLogSearch:
			var r protocol.LogSearchResult
			if err := json.Unmarshal(resp.Object, &r); err != nil {
				return err
			}
			fmt.Printf("  %s, %s to %s: %d matched, %d shown, levels %v\n", r.App, r.Since.Format(time.RFC3339), r.Until.Format(time.RFC3339),
				r.Matched, len(r.Entries), r.Levels)
			for _, s := range r.Sources {
				fmt.Printf("  source %s/%s/%s previous=%v: %d lines, %d matched, truncated=%v %s\n", s.Namespace, s.Pod, s.Container, s.Previous, s.Lines, s.Matched, s.Truncated, s.Error)
			}
			for _, e := range r.Entries {
				fmt.Printf("  %s %-7s %s/%s: %.160s\n", e.Time.Format("15:04:05"), e.Level, e.Pod, e.Container, e.Text)
				for _, l := range e.Stack {
					fmt.Printf("      | %.160s\n", l)
				}
			}
			for _, n := range r.Notes {
				fmt.Printf("  note: %s\n", n)
			}
		case req.Op == protocol.OpChain:
			var c protocol.ObjectChain
			if err := json.Unmarshal(resp.Object, &c); err != nil {
				return err
			}
			for _, m := range c.Matches {
				fmt.Printf("  match %s %s\n", m.ID, m.Status)
			}
			if c.Object != nil {
				fmt.Printf("  %s (%s) apps %v\n", c.Object.ID, c.Object.Status, c.Apps)
			}
			for _, s := range c.Sections {
				fmt.Printf("  %s:\n", s.Key)
				for _, it := range s.Items {
					miss := ""
					if it.Missing {
						miss = " MISSING"
					}
					fmt.Printf("      %s %s%s  %.100s\n", it.ID, it.Status, miss, it.Detail)
				}
				if s.More > 0 {
					fmt.Printf("      … and %d more\n", s.More)
				}
			}
			for _, n := range c.Notes {
				fmt.Printf("  note: %s\n", n)
			}
		case req.Op == protocol.OpAppMap:
			var m protocol.AppMap
			if err := json.Unmarshal(resp.Object, &m); err != nil {
				return err
			}
			printAppMap(&m)
		default:
			fmt.Printf("  object: %.160s\n", resp.Object)
		}
	}
	return nil
}

// printAppMap prints the user apps of an app map by tier, then the links
// with their evidence. System apps are only counted.
func printAppMap(m *protocol.AppMap) {
	system := 0
	for _, a := range m.Apps {
		if a.System {
			system++
			continue
		}
		fmt.Printf("  app %s (%s) %s\n", a.ID, a.Source, a.Health)
		for _, c := range a.Components {
			mark := "ok"
			if !c.Healthy {
				mark = "!!"
			}
			fmt.Printf("    %s %-9s %-62s %-26.26s %s\n", mark, c.Tier, c.ID, c.Status, c.TierReason)
			for _, r := range c.Routes {
				fmt.Printf("         route %s%s -> %s:%s ready=%d %v\n", r.Host, r.Path, r.Service, r.Port, r.ReadyEndpoints, r.Problems)
			}
			for _, p := range c.Pods {
				fmt.Printf("         pod %s on %s: %s ready=%v restarts=%d\n", p.Name, p.Node, p.Status, p.Ready, p.Restarts)
			}
		}
	}
	fmt.Printf("  + %d system apps, %d peers, %d unassigned\n", system, len(m.Peers), len(m.Unassigned))
	for _, c := range m.Unassigned {
		fmt.Printf("  unassigned %s: %s\n", c.ID, c.Status)
	}
	for _, l := range m.Links {
		broken := ""
		if l.Missing {
			broken = " BROKEN"
		}
		fmt.Printf("  link %s -> %s%s\n", l.From, l.To, broken)
		for _, e := range l.Evidence {
			fmt.Printf("         %-9s %s\n", e.Type, e.Detail)
		}
	}
}

// printSummary builds the summary exactly as it would be pushed, encodes it
// and prints its size, counts and problems.
func printSummary(c *cache.Cache, k8sVersion string) {
	s := summary.Build(c, graph.Build(c), k8sVersion, time.Now())
	msg, err := protocol.Encode("", s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "summary does not encode:", err)
		return
	}
	fmt.Printf("\nsummary: %d bytes on the wire, Kubernetes %s\n", len(msg), s.KubernetesVersion)
	fmt.Printf("  nodes %d/%d ready, %d namespaces, pods %+v\n  workloads %+v\n", s.Nodes.Ready, s.Nodes.Total, s.Namespaces, s.Pods, s.Workloads)
	fmt.Printf("  %d problems (worst first):\n", len(s.Problems))
	for _, p := range s.Problems {
		ago := ""
		if !p.Since.IsZero() {
			ago = " (" + time.Since(p.Since).Round(time.Second).String() + " ago)"
		}
		fmt.Printf("  %-11s %s/%s/%s: %s%s\n", p.Reason, p.Kind, p.Namespace, p.Name, p.Message, ago)
	}
}

// printDatastores prints the recognised data stores with their clients.
func printDatastores(c *cache.Cache) {
	list := datastores.Detect(c, graph.Build(c), deps.Find(c))
	fmt.Printf("\n%d data stores:\n", len(list))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STORE\tENGINE\tCATEGORY\tFOUND BY\tVERSION\tREADY\tSTATUS\tPERSISTENT")
	for _, d := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\t%v\n", d.ID, d.Engine, d.Category, d.Confidence, d.Version, d.Ready, d.Desired, d.Status, d.Persistent)
	}
	tw.Flush()
	for _, d := range list {
		if d.Target != "" {
			fmt.Printf("  %s pools %s\n", d.ID, d.Target)
		}
		if d.Primary != "" {
			fmt.Printf("  %s primary: %s\n", d.ID, d.Primary)
		}
		for _, cl := range d.Clients {
			fmt.Printf("  %s <- used by %s\n", d.ID, cl)
		}
	}
}

// printRoutes prints the route table in evaluation order and its findings.
func printRoutes(c *cache.Cache) {
	table := routes.Table(c)
	fmt.Printf("\n%d routes:\n", len(table))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST+PATH\tPRIO\tENTRY\tTLS\tBACKEND\tREADY\tMIDDLEWARES\tSOURCE")
	for _, r := range table {
		ready := "?"
		if r.ReadyEndpoints >= 0 {
			ready = fmt.Sprint(r.ReadyEndpoints)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%v\t%s:%s\t%s\t%s\t%s\n", r.Detail(), r.Priority, strings.Join(r.EntryPoints, ","), r.TLS,
			r.Service, r.Port, ready, strings.Join(r.Middlewares, ","), r.Source)
	}
	tw.Flush()
	findings := routes.Check(table)
	fmt.Printf("\n%d route findings\n", len(findings))
	for _, f := range findings {
		fmt.Printf("  %s -> %s: %s\n", f.Route.Detail(), f.Route.Service, f.Problem)
	}
}

// printDeps prints the dependencies found in configuration, then the
// application-level links they add up to.
func printDeps(c *cache.Cache) {
	ds := deps.Find(c)
	fmt.Printf("\n%d dependencies from configuration:\n", len(ds))
	for _, d := range ds {
		mark := ""
		if d.Missing {
			mark = "  MISSING"
		}
		port := ""
		if d.Port != "" {
			port = ":" + d.Port
		}
		fmt.Printf("  %s -> %s%s  (%s)%s\n", d.From, d.To, port, d.Via, mark)
	}
	links := deps.Links(ds, apps.Group(c, graph.Build(c)))
	fmt.Printf("\n%d application links:\n", len(links))
	for _, l := range links {
		fmt.Printf("  %s -> %s\n", l.From, l.To)
	}
}

// printApps groups the cluster's workloads into applications and prints them,
// user applications first.
func printApps(c *cache.Cache) {
	list := apps.Group(c, graph.Build(c))
	sort.SliceStable(list, func(i, j int) bool { return !list[i].System && list[j].System })
	fmt.Printf("\n%d applications:\n", len(list))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tSOURCE\tWORKLOADS\tMEMBERS\tHEALTH\tSYSTEM")
	for _, a := range list {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%v\n", a.ID, a.Source, len(a.Workloads), len(a.Members), a.Health, a.System)
	}
	tw.Flush()
	for _, a := range list {
		for _, p := range a.Problems {
			fmt.Printf("  %s: %s\n", a.ID, p)
		}
	}
}

// printGraph builds the relationship graph once and prints edge counts and
// the broken links an SRE would look at first.
func printGraph(c *cache.Cache) {
	start := time.Now()
	g := graph.Build(c)
	took := time.Since(start)
	byType := map[graph.EdgeType]int{}
	for _, e := range g.Edges {
		byType[e.Type]++
	}
	fmt.Printf("\ngraph: %d nodes, %d edges, built in %v\n", len(g.Nodes), len(g.Edges), took.Round(time.Millisecond))
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, string(t))
	}
	sort.Strings(types)
	for _, t := range types {
		fmt.Printf("  %-11s %d\n", t, byType[graph.EdgeType(t)])
	}
	if len(g.Incomplete) > 0 {
		fmt.Printf("incomplete (not cached): %s\n", strings.Join(g.Incomplete, ", "))
	}
	fmt.Println("\nfindings:")
	for _, n := range g.Nodes {
		switch {
		case n.Missing:
			var refs []string
			for _, e := range g.In(n.ID) {
				refs = append(refs, e.From+" ("+string(e.Type)+")")
			}
			fmt.Printf("  missing  %s  <- %s\n", n.ID, strings.Join(refs, ", "))
		case n.Status == "no pods match selector":
			fmt.Printf("  no pods  %s\n", n.ID)
		case n.Kind == "Pod" && n.Status != "Running" && n.Status != "Succeeded":
			fmt.Printf("  pod      %s  %s  (owner %s)\n", n.ID, n.Status, g.Owner(n.ID))
		}
	}
}

// restConfig uses the in-cluster config inside a pod, else a kubeconfig.
func restConfig(kubeconfig string) (*rest.Config, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	}
	if kubeconfig != "" || err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = kubeconfig
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	}
	if err != nil {
		return nil, err
	}
	// The first sync lists about 30 resources; the client default (5 QPS)
	// would make it take several seconds.
	cfg.QPS, cfg.Burst = 20, 40
	cfg.UserAgent = "kubemap-agent/" + version.Version
	return cfg, nil
}
