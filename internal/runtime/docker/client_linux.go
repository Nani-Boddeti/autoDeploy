//go:build linux

package docker

// NewClient constructs an Engine API client bound to the Docker daemon's
// Unix domain socket at socketPath (typically /var/run/docker.sock).
func NewClient(socketPath string) (*Client, error) {
	return newClient(socketPath)
}
