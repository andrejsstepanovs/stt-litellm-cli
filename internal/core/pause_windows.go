//go:build windows

package core

import "syscall"

// Console signals (SIGSTOP/SIGCONT) do not exist on Windows, so pause/resume
// suspends the recorder's threads via ntdll instead. This is what process
// managers use and needs no external tooling.

var (
	ntdll     = syscall.NewLazyDLL("ntdll.dll")
	ntSuspend = ntdll.NewProc("NtSuspendProcess")
	ntResume  = ntdll.NewProc("NtResumeProcess")
)

const processSuspendResume = 0x0800

func suspendProcess(pid int) {
	h, err := syscall.OpenProcess(processSuspendResume, false, uint32(pid))
	if err != nil {
		return
	}
	ntSuspend.Call(uintptr(h))
	syscall.CloseHandle(h)
}

func resumeProcess(pid int) {
	h, err := syscall.OpenProcess(processSuspendResume, false, uint32(pid))
	if err != nil {
		return
	}
	ntResume.Call(uintptr(h))
	syscall.CloseHandle(h)
}
