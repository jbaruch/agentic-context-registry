package realize

import (
	"os"
	"syscall"
)

func fileChangeTime(info os.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ctimespec.Nano()
	}
	return 0
}
