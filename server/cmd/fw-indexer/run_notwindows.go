//go:build !windows

package main

// runAsServiceIfManaged is a no-op off Windows: systemd and launchd start the binary
// as an ordinary process (it just runs in the foreground under the service manager),
// so there's no service-control protocol to speak. Only the Windows SCM requires the
// process to implement a control handler — see run_windows.go.
func runAsServiceIfManaged(runConfig) bool { return false }
