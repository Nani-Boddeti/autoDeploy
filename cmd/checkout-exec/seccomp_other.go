//go:build linux && !(amd64 || arm64)

package main

import "errors"

// Fail closed: the namespace filter has only been written for amd64/arm64 syscall ABIs.
func installNamespaceFilter() error { return errors.New("unsupported architecture") }
