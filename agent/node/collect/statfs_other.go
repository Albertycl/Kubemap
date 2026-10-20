//go:build !linux

package collect

import "errors"

// The node agent runs only on Linux; this keeps the package building elsewhere.
func statfs(string) (fsUsage, error) { return fsUsage{}, errors.New("statfs: linux only") }

func devOf(string) (string, error) { return "", errors.New("stat: linux only") }
