package job

import resourceplatform "github.com/openabstractions/abstraction-resource/go/platform"

// keepAwake preserves the local compatibility API through resource's platform owner.
func keepAwake(who, why string, display bool) (func(), error) {
	return resourceplatform.KeepAwake(who, why, display)
}
