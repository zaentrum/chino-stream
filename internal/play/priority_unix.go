//go:build unix

package play

import "syscall"

// lowestPriority gives the process pid the lowest CPU priority (nice 19).
func lowestPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}
