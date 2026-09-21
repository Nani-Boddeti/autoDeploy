//go:build !linux

package docker

// NewClient is unavailable outside Linux: the agent's Docker Engine API
// surface targets Linux hosts only, and callers must fail closed rather
// than receive a client that could silently no-op on an unsupported host.
func NewClient(string) (*Client, error) {
	return nil, ErrUnsupportedPlatform
}
