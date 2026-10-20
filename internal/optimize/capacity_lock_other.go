//go:build !linux

package optimize

// Public host tuning entry points already reject non-Linux platforms. Keep
// pure configuration tests buildable there without any host mutation.
func lockKernelPolicy() (func(), error) { return func() {}, nil }
