//go:build !darwin && !linux

package credential

import "errors"

func openCredentialFile(string, int) (int, error) { return -1, errors.New("unsupported platform") }
