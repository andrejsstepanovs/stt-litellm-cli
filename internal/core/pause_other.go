//go:build !windows

package core

// POSIX platforms pause via kill -STOP in Recorder.Pause; nothing to do here.
func suspendProcess(pid int) {}
func resumeProcess(pid int)  {}
