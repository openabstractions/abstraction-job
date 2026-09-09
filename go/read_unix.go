//go:build !windows

package job

import "os"

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
