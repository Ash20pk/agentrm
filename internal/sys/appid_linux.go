package sys

// AppCaller reports whether pid is the AgentRM app. There is no AgentRM app on Linux, and a
// binary's name proves nothing there, so no caller is.
func AppCaller(procs map[int]*Proc, pid int) bool { return false }
