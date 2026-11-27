package main

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// fileIdentitySupported tells whether file transfer can run as another account.
const fileIdentitySupported = true

// runAsIdentity runs fn with the file-system credentials of id, on a thread
// that is never reused afterwards. Linux keeps credentials per thread, so
// after LockOSThread the changes affect nothing but this goroutine; because the
// goroutine never unlocks, Go terminates the thread when it returns instead of
// handing a downgraded thread to other work. With a nil id fn just runs.
//
// Changing the file-system uid from root also drops the capabilities that
// bypass permission checks (CAP_DAC_OVERRIDE and friends) for this thread, so
// everything fn does to files is checked like the account's own processes.
func runAsIdentity(id *identity, fn func() error) error {
	if id == nil {
		return fn()
	}
	runtime.LockOSThread() // deliberately not unlocked: see above
	if err := dropFileCredentials(id); err != nil {
		return fmt.Errorf("assumir o usuário %s nos arquivos: %w", id.name, err)
	}
	return fn()
}

func dropFileCredentials(id *identity) error {
	groups := make([]uint32, len(id.groups))
	copy(groups, id.groups)
	var ptr unsafe.Pointer
	if len(groups) > 0 {
		ptr = unsafe.Pointer(&groups[0])
	}
	// SYS_SETGROUPS through RawSyscall changes this thread only; the
	// syscall.Setgroups wrapper would change every thread of the agent.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_SETGROUPS, uintptr(len(groups)), uintptr(ptr), 0); errno != 0 {
		return errno
	}
	// The uid comes last: after it the thread can no longer change the others.
	for _, step := range []struct {
		nr   uintptr
		want uint32
		name string
	}{{syscall.SYS_SETFSGID, id.gid, "gid"}, {syscall.SYS_SETFSUID, id.uid, "uid"}} {
		syscall.RawSyscall(step.nr, uintptr(step.want), 0, 0)
		// setfs*id never reports failure; asking for an impossible id returns the current one.
		current, _, _ := syscall.RawSyscall(step.nr, ^uintptr(0), 0, 0)
		if uint32(current) != step.want {
			return errors.New("o sistema recusou a troca de " + step.name)
		}
	}
	return nil
}
