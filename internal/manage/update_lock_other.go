//go:build !linux

package manage

import "errors"

func lockUpdate() (func(), error) {
	return nil, errors.New("updating the installed service requires Linux file locking")
}
