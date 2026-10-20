package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"kubemap/agent/node/collect"
	"kubemap/agent/node/identity"
	"kubemap/pkg/protocol"
)

// cpuWindow is how long --once waits between the two CPU readings.
const cpuWindow = time.Second

// runNode runs the node agent. Until the tunnel exists (KM-29) only --once
// works: it prints one metrics message as JSON on stdout and a one-line
// summary on stderr.
func runNode(args []string) int {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	once := fs.Bool("once", false, "print one metrics message and exit")
	proc := fs.String("proc", "/host/proc", "host /proc mount")
	sys := fs.String("sys", "/host/sys", "host /sys mount")
	root := fs.String("root", "/host", "host / mount")
	node := fs.String("node-name", os.Getenv("NODE_NAME"), "node name (default: host's hostname)")
	identityCheck := fs.Bool("identity-check", false, "load and check the node identity token, print it and exit")
	tokenPath := fs.String("token", identity.DefaultTokenPath, "projected ServiceAccount token (audience kubemap-portal)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *identityCheck {
		id, err := identity.Load(identity.Config{TokenPath: *tokenPath, ClusterID: os.Getenv("CLUSTER_ID"), NodeName: *node}, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "kubemap-agent node: identity:", err)
			return 1
		}
		fmt.Printf("identity ok: %s; re-register at %s\n", id, id.RenewAt(time.Now()).Format(time.RFC3339))
		return 0
	}
	if !*once {
		return runNodeAgent(*proc, *sys, *root, *tokenPath)
	}
	c, err := collect.New(collect.Config{ProcRoot: *proc, SysRoot: *sys, HostRoot: *root, NodeName: *node})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent node:", err)
		return 1
	}
	time.Sleep(cpuWindow)
	start := time.Now()
	m, err := c.Collect()
	took := time.Since(start)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent node:", err)
		return 1
	}
	data, err := protocol.Encode("", m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent node:", err)
		return 1
	}
	fmt.Println(string(data))
	fmt.Fprintf(os.Stderr, "%s (sample took %v)\n", collect.Brief(m), took.Round(time.Microsecond))
	return 0
}
