package httpmw

import "runtime"

// maxStackBytes caps a captured panic stack.
//
// Generous for a real stack, small enough that a pathological recursion cannot
// push a megabyte of text into a log sink one entry at a time.
const maxStackBytes = 8 << 10

// stack returns the current goroutine's stack, truncated to maxStackBytes.
func stack() string {
	buf := make([]byte, maxStackBytes)

	return string(buf[:runtime.Stack(buf, false)])
}
