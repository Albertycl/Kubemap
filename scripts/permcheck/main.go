// Command permcheck is the static half of the KM-81 permission proof kit.
// It reads the rendered agent YAML (the bundle, or a whole import file) and
// needs no cluster:
//
//	go run ./scripts/permcheck [-checks scripts/agent-permission-checks.txt] FILE...
//
// It does two things:
//
//  1. Audit: every RBAC rule in the files may only read (get, list, watch),
//     names its resources (no "*"), and grants nothing on Secrets, except the
//     one approved write exception (A8): create on tokenreviews. Bindings
//     may only name the two ServiceAccounts (the server and the node agent).
//  2. Checks: it evaluates the same list of dangerous actions that
//     verify-agent-permissions.sh asks the API server about, with the
//     Kubernetes RBAC rules, for both ServiceAccounts.
//
// Only the files given are seen. Grants the cluster already gives to every
// ServiceAccount (groups system:serviceaccounts, system:authenticated) are
// checked by the live mode of verify-agent-permissions.sh.
//
// Exit status: 0 when everything passes, 1 when a check or the audit fails,
// 2 on a usage or read error.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	checksPath := flag.String("checks", "scripts/agent-permission-checks.txt", "the list of checks")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: permcheck [-checks FILE] YAML...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	os.Exit(run(os.Stdout, *checksPath, flag.Args()))
}

func run(out io.Writer, checksPath string, files []string) int {
	checks, err := readChecks(checksPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "permcheck:", err)
		return 2
	}
	set := &Objects{}
	for _, f := range files {
		if err := set.ReadFile(f); err != nil {
			fmt.Fprintln(os.Stderr, "permcheck:", err)
			return 2
		}
	}
	failed := false

	fmt.Fprintf(out, "== RBAC objects in %d file(s): %d ClusterRole, %d Role, %d ClusterRoleBinding, %d RoleBinding, %d ServiceAccount\n",
		len(files), len(set.ClusterRoles), len(set.Roles), len(set.ClusterRoleBindings), len(set.RoleBindings), len(set.ServiceAccounts))
	fmt.Fprintln(out, "\n== Audit: every rule")
	for _, f := range Audit(set) {
		fmt.Fprintln(out, f.String())
		if !f.OK {
			failed = true
		}
	}

	fmt.Fprintln(out, "\n== Checks: dangerous actions")
	fails := 0
	for _, a := range Agents {
		for _, c := range checks {
			if !c.AppliesTo(a) {
				continue
			}
			for _, ns := range c.Scopes() {
				allowed, by := set.Allowed(a, c, ns)
				ok := allowed == (c.Expect == "allow")
				verdict := "PASS"
				if !ok {
					verdict = "FAIL"
					fails++
				}
				ans := "no"
				if allowed {
					ans = "yes (" + by + ")"
				}
				fmt.Fprintf(out, "%s  %-7s %-16s %-12s %-40s -> %s (want %s)\n",
					verdict, a.Short, scopeName(ns), c.Verb, c.Target(), ans, wantWord(c.Expect))
			}
		}
	}
	if fails > 0 {
		failed = true
	}
	fmt.Fprintf(out, "\n%d check(s) failed.\n", fails)
	if failed {
		fmt.Fprintln(out, "RESULT: FAIL")
		return 1
	}
	fmt.Fprintln(out, "RESULT: PASS (static: only the RBAC in the given files; run the live mode on the cluster too)")
	return 0
}

func scopeName(ns string) string {
	if ns == "" {
		return "all-namespaces"
	}
	return ns
}

func wantWord(expect string) string {
	if expect == "allow" {
		return "yes"
	}
	return "no"
}
