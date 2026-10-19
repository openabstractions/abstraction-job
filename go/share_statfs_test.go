//go:build darwin || linux

package job

import (
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
)

// shareSpelling marks dir as a network volume for identity.LocalPath, as a
// network file system on Linux or a volume without MNT_LOCAL on macOS reads.
func shareSpelling(t *testing.T, dir string) string {
	t.Helper()
	t.Cleanup(identity.TreatVolumeAsNetwork(dir))
	return dir
}
