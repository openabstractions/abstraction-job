package acceptanceprovider

import (
	"runtime"
	"testing"
)

func TestLocalSubjectScopePreservesGoldenBytes(t *testing.T) {
	program, kind, account, want := "/opt/oa/runtime", "posix", "1000", "owner-program@1:43f482fddc296a0bcf9e6fd6f847e1fbd83494efd90994aa3b824c10a70bf6d0"
	if runtime.GOOS == "windows" {
		program, kind, account, want = `C:\OA\runtime.exe`, "windows", "S-1-test", "owner-program@1:88fa3b2da6a888be9699dab4ae92e8c090d41e60de066c24aa3601c0aa90488b"
	}
	if got, err := LocalSubjectScope(kind, account, program); err != nil || got != want {
		t.Fatal(got, err, want)
	}
}
