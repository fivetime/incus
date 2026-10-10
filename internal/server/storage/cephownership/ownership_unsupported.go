//go:build !linux || !cgo

package cephownership

import (
	"errors"
)

// Transfer fails closed when the native librados adapter is unavailable.
func Transfer(binding Binding, previous string, next string) error {
	err := binding.validate(previous, next)
	if err != nil {
		return err
	}

	return errors.New("Immutable RBD ownership transfer requires Linux and cgo")
}
