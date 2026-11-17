package realizeapp

import (
	"os"
	"syscall"
)

func appliedMetadata(info os.FileInfo) (appliedFile, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return appliedFile{}, false
	}
	return appliedFile{
		Device: uint64(stat.Dev), Inode: stat.Ino, Size: stat.Size,
		ModSeconds: int64(stat.Mtim.Sec), ModNanos: int64(stat.Mtim.Nsec),
		ChangeSeconds: int64(stat.Ctim.Sec), ChangeNanos: int64(stat.Ctim.Nsec),
	}, true
}
