//go:build !linux

package checkoutquota

import "context"

func (Client) Allocate(context.Context, Limits) (Allocation, error) {
	return Allocation{}, ErrUnavailable
}
func (Client) ResolveAllocation(context.Context, Allocation) (Allocation, error) {
	return Allocation{}, ErrUnavailable
}
func (Client) ReleaseAndCleanup(context.Context, Allocation) error { return ErrUnavailable }
