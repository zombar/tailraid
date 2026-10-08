package sysfstest

import "os"

func truncateFile(path string, size int64) error { return os.Truncate(path, size) }
