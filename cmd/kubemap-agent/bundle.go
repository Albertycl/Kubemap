package main

import (
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"

	"kubemap/agent/install"
)

// runBundle prints the install manifest. `make manifest` writes it to
// deploy/kubemap.yaml.
func runBundle(args []string) int {
	fs := flag.NewFlagSet("bundle", flag.ContinueOnError)
	image := fs.String("image", "", "kubemap image, for example localhost/kubemap:v0.2.0")
	pull := fs.String("pull-policy", string(corev1.PullIfNotPresent), "imagePullPolicy")
	storage := fs.String("storage-class", "", "storageClassName of the data volume (empty: the cluster default)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	b, err := install.Bundle(install.Options{Image: *image, PullPolicy: corev1.PullPolicy(*pull), StorageClass: *storage})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemap-agent bundle:", err)
		return 2
	}
	os.Stdout.Write(b)
	return 0
}
