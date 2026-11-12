//go:build !unix

package csi

// aliased reports no hard links where the platform does not expose link counts.
func aliased(string) (string, error) { return "", nil }
