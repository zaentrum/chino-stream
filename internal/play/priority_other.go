//go:build !unix

package play

// lowestPriority does nothing where processes have no nice value.
func lowestPriority(int) {}
