//go:build !linux && !darwin

package play

import "os"

// sysFileID is no inode and no change time where the system gives none:
// the size and the modification time alone tell a file's bytes apart.
func sysFileID(os.FileInfo) (int64, int64) { return 0, 0 }
