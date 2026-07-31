//go:build !linux
// +build !linux

package main

func setupAutoRouteNetlink() error {
	return nil
}

func cleanupAutoRouteNetlink() {
}
