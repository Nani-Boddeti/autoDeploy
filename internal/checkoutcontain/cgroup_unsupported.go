//go:build !linux

package checkoutcontain

import "errors"

var ErrUnavailable = errors.New("checkout containment unavailable")

type Limits struct{}
type Manager struct{}
type Leaf struct{}
type Directory struct{}

func New(string, Limits) (*Manager, error)                    { return nil, ErrUnavailable }
func OpenTrustedDirectory(string, uint32) (*Directory, error) { return nil, ErrUnavailable }
