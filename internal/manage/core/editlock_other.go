//go:build !linux

package core

import "fmt"

func lockConfigFile(string) (func(), error) {
	return nil, fmt.Errorf("safe tunnel writes require Linux file locking")
}

func lockConfigTree(string, bool) (func(), error) {
	return nil, fmt.Errorf("safe configuration restores require Linux file locking")
}
