// Command kubemap-agent runs inside a Kubernetes cluster:
//
//	kubemap-agent node      reports host health for one node (DaemonSet)
//	kubemap-agent check     read-only diagnostics of what the server's cluster agent would see
//	kubemap-agent bundle    prints the install manifest (deploy/kubemap.yaml)
//
// The cluster agent (cache, queries, actions) runs inside the kubemap server.
package main

import (
	"fmt"
	"os"

	"kubemap/pkg/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "check":
		os.Exit(runCheck(os.Args[2:]))
	case "node":
		os.Exit(runNode(os.Args[2:]))
	case "bundle":
		os.Exit(runBundle(os.Args[2:]))
	case "version":
		fmt.Println(version.Version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kubemap-agent node|check|bundle|version")
}
