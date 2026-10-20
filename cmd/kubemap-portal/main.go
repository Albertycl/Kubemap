// Command kubemap-portal is the Kubemap server. It runs as one Deployment in
// the cluster it watches: it serves the web UI over plain HTTP (TLS ends at
// the Traefik ingress), keeps its SQLite database on a volume and runs the
// cluster agent in the same process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"kubemap/agent/install"
	"kubemap/pkg/protocol"
	"kubemap/pkg/version"
	"kubemap/portal/access"
	"kubemap/portal/alerts"
	"kubemap/portal/appmap"
	"kubemap/portal/auth"
	"kubemap/portal/clusters"
	"kubemap/portal/health"
	"kubemap/portal/hints"
	"kubemap/portal/httpjson"
	"kubemap/portal/incidents"
	"kubemap/portal/journal"
	"kubemap/portal/localcluster"
	"kubemap/portal/logfile"
	"kubemap/portal/metrics"
	"kubemap/portal/netpol"
	"kubemap/portal/observe"
	"kubemap/portal/resources"
	scenarioapi "kubemap/portal/scenarios"
	"kubemap/portal/server"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
	"kubemap/portal/yamlview"
	"kubemap/web"
)

// defaultDataDir is /data/kubemap in the container image (a folder the server creates and owns: the volume root belongs to root on local-path storage) and ./data elsewhere.
var defaultDataDir = func() string {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return "/data/kubemap"
	}
	return "data"
}()

const usage = `kubemap-portal: the Kubemap server.

Commands:
  kubemap-portal [run] [flags]            run the server (Ctrl+C stops it)
  kubemap-portal reset-admin-password [-data-dir DIR]
  kubemap-portal disable-totp [-data-dir DIR] [-user NAME]
                                          turn two-factor login off (lost phone
                                          and recovery codes)
  kubemap-portal journal-import [-data-dir DIR] [-replace] FILE.md...
                                          import incident or change records
                                          (docs/incidents, docs/changes) into
                                          the incident journal
  kubemap-portal version

Flags for run:
`

type config struct {
	Listen    string
	DataDir   string
	LogFile   string
	Namespace string
	// AuditDays is how long audit entries are kept; 0 keeps them forever.
	AuditDays int
	// IncidentIgnore are namespaces the incident drafter leaves out
	// (KM-90), comma-separated; default the practice labs.
	IncidentIgnore string
	// TrustedProxies are the peers whose X-Forwarded-For is believed.
	TrustedProxies string
	// Observability sources (read-only); empty turns one off.
	PrometheusURL, TempoURL, GrafanaURL string
}

func parseFlags(name string, args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", ":"+fmt.Sprint(install.ServerPort), "HTTP listen address (TLS ends at the ingress)")
	fs.StringVar(&c.DataDir, "data-dir", defaultDataDir, "data directory (database, first admin password)")
	fs.StringVar(&c.LogFile, "log-file", "", "write the log to this file (rotated at 10 MiB, 5 kept) instead of the terminal")
	fs.StringVar(&c.Namespace, "namespace", envOr("KUBEMAP_NAMESPACE", install.SystemNamespace), "the namespace the server runs in")
	fs.IntVar(&c.AuditDays, "audit-retention-days", 365, "delete audit log entries older than this many days (0: keep all)")
	fs.StringVar(&c.IncidentIgnore, "incident-ignore-namespaces", strings.Join(incidents.DefaultIgnoreNamespaces, ","),
		"namespaces whose problems never open a draft incident, comma-separated (empty: none)")
	fs.StringVar(&c.TrustedProxies, "trusted-proxies", envOr("KUBEMAP_TRUSTED_PROXIES", httpjson.DefaultTrustedProxies),
		"peers (CIDRs, comma-separated) whose X-Forwarded-For header gives the client address, e.g. Traefik; empty: trust none")
	fs.StringVar(&c.PrometheusURL, "prometheus-url", envOr("KUBEMAP_PROMETHEUS_URL", ""), "Prometheus base URL for the app metrics panels (empty: off)")
	fs.StringVar(&c.TempoURL, "tempo-url", envOr("KUBEMAP_TEMPO_URL", ""), "Tempo base URL for the traces panel (empty: off)")
	fs.StringVar(&c.GrafanaURL, "grafana-url", envOr("KUBEMAP_GRAFANA_URL", ""), "Grafana URL for the 'Open in Grafana' links (empty: no links)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if c.AuditDays < 0 {
		return c, fmt.Errorf("-audit-retention-days must be 0 or more")
	}
	if err := httpjson.SetTrustedProxies(c.TrustedProxies); err != nil {
		return c, fmt.Errorf("-trusted-proxies: %w", err)
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return c, err
	}
	c.DataDir = abs
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cmd, args := "run", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "version":
		fmt.Println(version.Version)
	case "reset-admin-password":
		os.Exit(resetAdminPassword(args))
	case "disable-totp":
		os.Exit(disableTOTP(args))
	case "journal-import":
		os.Exit(journalImport(args))
	case "run":
		c, err := parseFlags(cmd, args)
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintln(os.Stderr, "kubemap-portal:", err)
			os.Exit(2)
		}
		os.Exit(runPortal(c))
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "kubemap-portal: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

// runPortal runs the server until Ctrl+C or SIGTERM and returns the exit code.
func runPortal(c config) int {
	var out = os.Stderr
	log := slog.New(slog.NewTextHandler(out, nil))
	if c.LogFile != "" {
		w, err := logfile.Open(c.LogFile, 0, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kubemap-portal:", err)
			return 1
		}
		defer w.Close()
		log = slog.New(slog.NewTextHandler(w, nil))
	}

	// The data folder is made private before anything in it is read or
	// written (review KM-44 M6). A folder that fails is not used.
	warnings, err := secureDataDir(c.DataDir)
	if err != nil {
		log.Error("refusing to start: the data folder is not safe", "dir", c.DataDir, "err", err)
		return 1
	}
	for _, w := range warnings {
		log.Warn("the data folder was not private", "detail", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, log, c); err != nil {
		log.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

// kubeConfig is the in-cluster configuration, or (development) the
// KUBECONFIG file.
func kubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
}

// serve runs the server until ctx ends.
func serve(ctx context.Context, log *slog.Logger, c config) error {
	log.Info("kubemap-portal starting", "version", version.Version, "listen", c.Listen, "data_dir", c.DataDir, "namespace", c.Namespace)

	st, err := store.Open(ctx, filepath.Join(c.DataDir, dbFile))
	if err != nil {
		return err
	}
	defer st.Close()
	login, err := auth.New(st, log, c.DataDir)
	if err != nil {
		return err
	}
	if created, err := login.Bootstrap(ctx); err != nil {
		return err
	} else if created {
		log.Info("created the admin account; get the first password with: kubemap-portal reset-admin-password (the image has no shell), change it at first login",
			"username", auth.AdminUser, "password_file", login.InitialPasswordPath())
	}
	go pruneLoop(ctx, log, st, time.Duration(c.AuditDays)*24*time.Hour)

	// No agent is connected yet after a start. The one cluster is this one.
	if err := st.ResetConnections(ctx); err != nil {
		return err
	}
	if _, err := st.EnsureCluster(ctx, install.LocalClusterID, localcluster.ClusterName); err != nil {
		return err
	}
	collector := metrics.New(st, log, nil)
	go collector.Run(ctx)
	hub := tunnel.New(tunnel.Config{Store: st, Log: log, Metrics: collector})

	// The cluster agent runs in this process: it reads the cluster with the
	// server's ServiceAccount and answers the hub directly.
	kcfg, err := kubeConfig()
	if err != nil {
		return fmt.Errorf("no Kubernetes configuration (run inside the cluster, or set KUBECONFIG): %w", err)
	}
	agent, err := localcluster.Start(ctx, kcfg, c.Namespace, log)
	if err != nil {
		return fmt.Errorf("start the cluster agent: %w", err)
	}
	sender, detach, err := hub.AttachLocal(ctx, install.LocalClusterID, version.Version, agent)
	if err != nil {
		return err
	}
	defer detach()
	go agent.RunSummary(ctx, sender)

	nodeHealth := metrics.NewAPI(collector, st, hub.NodeConnected)
	nodeHealth.Agent = hub
	nodeHealth.Kube = func(clusterID string) (*protocol.Summary, bool) {
		sum, _, ok := hub.Summary(clusterID)
		if !ok {
			return nil, hub.Connected(clusterID)
		}
		return sum, hub.Connected(clusterID)
	}
	appMap := appmap.NewAPI(st, hub)
	objects := resources.NewAPI(st, resources.HubAgent{Hub: hub})
	problems := health.NewAPI(st, hub)
	problems.Reports = collector.Reports
	// Drafts live in the journal's tables, so they survive a restart.
	drafts := journal.DraftSink{Store: st}
	drafter := &incidents.Drafter{Sink: drafts, Check: problems.Check, Clusters: st.ListClusters, Log: log,
		IgnoreNamespaces: splitList(c.IncidentIgnore)}
	go drafter.Run(ctx)
	draftAPI := &incidents.API{Sink: drafts}
	alertAPI := &alerts.API{Sink: drafts}
	policies := netpol.NewAPI(st, hub)
	yamlAPI := yamlview.NewAPI(st, hub) // KM-39: view, diff and kubectl commands; never applies
	accessViews := access.NewAPI(st, hub)
	learn, err := scenarioapi.NewAPI(st, hub)
	if err != nil {
		return err
	}
	learn.Reports = collector.Reports
	journalAPI := journal.NewAPI(st)
	journalAPI.Findings = problems.Report
	journalAPI.Agent = hub
	journalAPI.Reports = collector.Reports
	observeAPI := observe.NewAPI(observe.Config{PromURL: c.PrometheusURL, TempoURL: c.TempoURL, GrafanaURL: c.GrafanaURL})
	registry := clusters.New(clusters.Config{Store: st, Log: log})
	srv := server.New(c.Listen, server.Handler(login.Routes, registry.Routes(login.Require), nodeHealth.Routes(login.Require), appMap.Routes(login.Require), objects.Routes(login.Require), problems.Routes(login.Require), hints.API{}.Routes(login.Require), draftAPI.Routes(login.Require), alertAPI.Routes(login.Require), journalAPI.Routes(login.Require), policies.Routes(login.Require), yamlAPI.Routes(login.Require), accessViews.Routes(login.Require), learn.Routes(login.Require), observeAPI.Routes(login.Require), hub.Routes, web.Routes), nil)
	srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// Stop accepting connections first, so a node agent that reconnects at
	// once cannot start a new one. Shutdown does not wait for the node
	// agents' WebSockets (hijacked); hub.Close ends them and lets their
	// handlers record the disconnect before the deferred st.Close (KM-79 F4).
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serr := srv.Shutdown(sctx)
	if !hub.Close(5 * time.Second) {
		log.Warn("agent connections still closing after 5 s; their last status may not be saved")
	}
	if serr != nil {
		return serr
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const dbFile = "portal.db"

// pruneLoop deletes expired sessions every hour.
func pruneLoop(ctx context.Context, log *slog.Logger, st store.Store, auditKeep time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, err := st.PruneSessions(ctx); err != nil && ctx.Err() == nil {
			log.Error("prune sessions", "err", err)
		}
		// Retention (review KM-44 L9).
		for what, prune := range map[string]func(context.Context) (int64, error){
			"audit log": func(ctx context.Context) (int64, error) { return st.PruneAudit(ctx, auditKeep) },
			"drafts":    st.PruneDrafts,
		} {
			if n, err := prune(ctx); err != nil && ctx.Err() == nil {
				log.Error("prune "+what, "err", err)
			} else if n > 0 {
				log.Info("pruned old records", "what", what, "count", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// secureForCommand makes the data folder private before a command opens it,
// and prints what it found. It reports whether the command may go on.
func secureForCommand(dir string) bool {
	warnings, err := secureDataDir(dir)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-portal: the data folder is not safe:", err)
		return false
	}
	return true
}

// resetAdminPassword is the "reset-admin-password" command: it sets a new
// random admin password, ends all admin sessions and prints the password.
// In the cluster: kubectl -n kubemap-system exec deploy/kubemap -- kubemap-portal reset-admin-password.
func resetAdminPassword(args []string) int {
	fs := flag.NewFlagSet("reset-admin-password", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "data directory")
	fs.Parse(args)
	if !secureForCommand(*dataDir) {
		return 1
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(*dataDir, dbFile))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	login, err := auth.New(st, slog.New(slog.NewTextHandler(os.Stderr, nil)), *dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pw, err := login.ResetAdminPassword(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("New password for %q (change it at next login): %s\n", auth.AdminUser, pw)
	return 0
}

// disableTOTP is the "disable-totp" command: it removes the two-factor secret
// and the recovery codes of a user (default admin), for a lockout. The audit
// log names the user as "cli".
// In the cluster: kubectl -n kubemap-system exec deploy/kubemap -- kubemap-portal disable-totp.
func disableTOTP(args []string) int {
	fs := flag.NewFlagSet("disable-totp", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "data directory")
	user := fs.String("user", auth.AdminUser, "user name")
	fs.Parse(args)
	if !secureForCommand(*dataDir) {
		return 1
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(*dataDir, dbFile))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	login, err := auth.New(st, slog.New(slog.NewTextHandler(os.Stderr, nil)), *dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	had, err := login.DisableTOTP(ctx, *user)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !had {
		fmt.Printf("Two-factor login was not set up for %q.\n", *user)
		return 0
	}
	fmt.Printf("Two-factor login is off for %q. Log in with the password and enable it again.\n", *user)
	return 0
}

// journalImport imports Markdown files into the incident journal. The
// portal may be running: SQLite waits for its lock. The audit log names the
// user as "cli".
func journalImport(args []string) int {
	fs := flag.NewFlagSet("journal-import", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "data directory")
	replace := fs.Bool("replace", false, "replace an entry imported earlier from the same file (its evidence stays)")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "kubemap-portal journal-import: name one or more .md files")
		return 2
	}
	if !secureForCommand(*dataDir) {
		return 1
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(*dataDir, dbFile))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	code := 0
	for _, path := range fs.Args() {
		md, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
			continue
		}
		e, exact, err := journal.Import(ctx, st, journal.ImportRequest{Markdown: string(md), Source: filepath.Base(path), Replace: *replace}, "cli")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			code = 1
			continue
		}
		note := "exports unchanged"
		if !exact {
			note = "exports with small layout changes"
		}
		fmt.Printf("%s: %s %q (%s, %s)\n", path, e.ID, e.Title, e.Kind, note)
	}
	return code
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
