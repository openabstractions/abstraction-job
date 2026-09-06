//go:build !windows

package job

import (
	"os/exec"
	"runtime"
)

// The inhibitor is a child that lives exactly as long as its stdin: our end of
// the pipe closes when this process exits, however it exits, and cat goes with
// it. That is the lifetime a D-Bus inhibitor fd has, without a D-Bus client.
// The echo arrives once the inhibitor is in place, so the hold is real on return.
func keepAwake(who, why string) (func(), error) {
	var c *exec.Cmd
	if runtime.GOOS == "darwin" {
		c = exec.Command("caffeinate", "-i", "sh", "-c", "echo; exec cat")
	} else {
		c = exec.Command("systemd-inhibit", "--what=idle:sleep", "--mode=block",
			"--who="+who, "--why="+why, "sh", "-c", "echo; exec cat")
	}
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	if _, err := out.Read(make([]byte, 1)); err != nil {
		in.Close()
		c.Wait()
		return nil, err
	}
	return func() {
		in.Close()
		c.Wait()
	}, nil
}
