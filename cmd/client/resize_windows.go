//go:build windows

package main

// watchResize is a no-op on Windows, which has no SIGWINCH.
func watchResize(func()) (stop func()) { return func() {} }
