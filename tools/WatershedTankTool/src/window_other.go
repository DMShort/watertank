//go:build !windows

package main

// Development builds on other systems use a browser window.
func runAppWindow(url string, a *App) bool { return false }

func iconFile() string { return "" }
