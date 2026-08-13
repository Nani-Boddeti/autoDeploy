//go:build !linux

package checkoutcontain

import "context"

func (SpawnClient) Run(context.Context, SpawnRequest) (SpawnResult, error) {
	return SpawnResult{}, ErrUnavailable
}
