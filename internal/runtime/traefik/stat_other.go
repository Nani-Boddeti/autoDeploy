//go:build !unix

package traefik

import (
	"errors"
	"io/fs"
)

// openNoFollow has no equivalent off unix; ownerUID fails closed so no
// route directory is ever accepted on these platforms.
const openNoFollow = 0

func ownerUID(fs.FileInfo) (uint32, error) { return 0, errors.ErrUnsupported }
