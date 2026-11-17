package play

import (
	"os"
	"syscall"
)

// sysFileID is the inode and the change time (unix nanoseconds) of st.
func sysFileID(st os.FileInfo) (int64, int64) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int64(s.Ino), s.Ctimespec.Nano()
	}
	return 0, 0
}
