//go:build !windows

package provider

func symlinkUnavailable(error) bool { return false }
