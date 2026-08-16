//go:build !demo

package main

// startDemo is a no-op in normal builds; the demo build spawns the scripted
// model server and points AJENT_HOME at a fresh temp dir instead.
func startDemo() func() { return func() {} }
