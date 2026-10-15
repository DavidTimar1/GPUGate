package console

import (
	"fmt"
	"os"
	"runtime/debug"
)

// reportPanic records a recovered panic: the value and the stack go to the log ring (what
// GET /api/log shows) and to stderr (.state/gpugate.out), so a bad provider answer leaves its
// cause behind instead of ending the process. It is only ever called from a deferred recover().
//
// GOTCHA: recover() catches a panic and nothing else. "fatal error: concurrent map writes" and
// a deadlock are not panics and still end the process, so every shared map stays under its mutex.
func (s *Server) reportPanic(where string, value any) {
	stack := debug.Stack()
	text := fmt.Sprintf("PANIC in %s: %v", where, value)
	s.log.Add("console", text+"\n"+string(stack))
	fmt.Fprintf(os.Stderr, "%s\n%s\n", text, stack)
}
