package hostinger

import "time"

// SetWriteRetryDelay changes the pause between write attempts and returns a
// function that restores it.
func SetWriteRetryDelay(d time.Duration) (restore func()) {
	old := writeRetryDelay
	writeRetryDelay = d
	return func() { writeRetryDelay = old }
}
