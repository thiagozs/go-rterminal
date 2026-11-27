//go:build !linux

package main

import "errors"

// File transfer as another account needs per-thread credentials, which only
// Linux offers; elsewhere it is not advertised when --shell-user is in use.
const fileIdentitySupported = false

func runAsIdentity(id *identity, fn func() error) error {
	if id != nil {
		return errors.New("transferência de arquivos como outro usuário só existe no Linux")
	}
	return fn()
}
