//go:build !windows

package job

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
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
	// The refusal a caller has to act on is the inhibitor's own words. Without
	// them a machine that has no seat to inhibit, one whose policy says no, and
	// one where the tool is simply absent all arrive as a bare EOF on the pipe.
	var refusal bytes.Buffer
	c.Stderr = &refusal
	if err := c.Start(); err != nil {
		return nil, err
	}
	if _, err := out.Read(make([]byte, 1)); err != nil {
		in.Close()
		waited := c.Wait()
		if said := strings.TrimSpace(refusal.String()); said != "" {
			return nil, fmt.Errorf("%s: %s", c.Args[0], said)
		}
		if waited != nil {
			return nil, fmt.Errorf("%s: %w", c.Args[0], waited)
		}
		return nil, fmt.Errorf("%s: %w", c.Args[0], err)
	}
	return func() {
		in.Close()
		c.Wait()
	}, nil
}
