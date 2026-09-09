package job

import (
	"syscall"
	"unsafe"
)

var callNtPowerInformation = syscall.NewLazyDLL("powrprof.dll").NewProc("CallNtPowerInformation")

// inhibited reads the system-wide execution state without elevation, which
// powercfg /requests needs. Anything holding SYSTEM shows here.
func inhibited() bool {
	var state uint32
	callNtPowerInformation.Call(16, 0, 0, uintptr(unsafe.Pointer(&state)), 4)
	return state&1 != 0
}
