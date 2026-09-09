//go:build !windows

package job

import (
	"os/exec"
	"runtime"
	"strings"
)

func inhibited() bool {
	if runtime.GOOS == "darwin" {
		out, _ := exec.Command("pmset", "-g", "assertions").Output()
		return strings.Contains(string(out), "caffeinate")
	}
	out, _ := exec.Command("systemd-inhibit", "--list", "--no-legend").Output()
	return strings.Contains(string(out), "holder")
}
