//go:build !unix

package adminsock

// checkPrivateDir has no ownership model to check on this platform.
func checkPrivateDir(string) error { return nil }
