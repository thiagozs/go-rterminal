package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// signalSession signals every process of the shell's session. An interactive
// shell puts background jobs in their own process groups, so signalling only
// the shell's group would leave them running after the session ends.
func signalSession(sid int, sig syscall.Signal) {
	_ = syscall.Kill(-sid, sig)
	pids, ok := sessionMembers(sid)
	if !ok {
		// No /proc (e.g. macOS): fall back to pkill, which matches by session.
		_ = exec.Command("pkill", "-"+strconv.Itoa(int(sig)), "-s", strconv.Itoa(sid)).Run()
		return
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, sig)
	}
}

// sessionMembers lists the processes whose session id is sid using /proc. The
// kernel does not reuse a pid while it is still a session id, so a match is safe
// to signal even after the session leader exited.
func sessionMembers(sid int) (pids []int, ok bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// Format: pid (comm) state ppid pgrp session ... ; comm may hold spaces.
		text := string(data)
		end := strings.LastIndexByte(text, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(text[end+1:])
		if len(fields) < 4 {
			continue
		}
		if session, err := strconv.Atoi(fields[3]); err == nil && session == sid {
			pids = append(pids, pid)
		}
	}
	return pids, true
}
