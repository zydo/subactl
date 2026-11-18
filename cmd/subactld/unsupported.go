//go:build !linux && !darwin

package main

// subactl supports only Linux and macOS. On any other system the build
// stops here, with an error that names the reason.
var _ = subactl_supports_only_linux_and_macos
