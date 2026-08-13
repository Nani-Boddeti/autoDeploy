//go:build !linux

package checkoutquota

// Service is deliberately unavailable outside Linux. Portable callers must
// inject a test adapter instead of silently losing preventive enforcement.
type Service struct{}

func New(Config) (*Service, error) { return nil, ErrUnavailable }
