//go:build !darwin

package main

// Other platforms don't need a special main-thread event loop.
func runMain(fn func()) {
	fn()
}
