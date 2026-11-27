//go:build !darwin && !linux

package realize

import "os"

func fileChangeTime(os.FileInfo) int64 { return 0 }
