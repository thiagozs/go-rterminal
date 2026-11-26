//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize calls onResize whenever the terminal is resized (SIGWINCH).
// The returned function stops the watcher.
func watchResize(onResize func()) (stop func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-signals:
				onResize()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}
