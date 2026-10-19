package job

import "testing"

// shareSpelling reaches the local folder dir through the administrative share
// \\localhost\X$, the spelling a folder on a network share has.
func shareSpelling(t *testing.T, dir string) string {
	t.Helper()
	return `\\localhost\` + dir[:1] + `$` + dir[2:]
}
