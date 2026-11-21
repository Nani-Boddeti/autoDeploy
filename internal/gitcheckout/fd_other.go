//go:build !unix

package gitcheckout

import "errors"

func closeFD(int) error { return errors.ErrUnsupported }

func dupFD(int) (int, error) { return -1, errors.ErrUnsupported }

func cloexecPipe() (int, int, error) { return -1, -1, errors.ErrUnsupported }
