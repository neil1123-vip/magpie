//go:build windows && !nogui && sub2api_webview2

package sub2apibrowser

import (
	"fmt"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type browserProcess struct {
	cmd *exec.Cmd
	job windows.Handle
}

func startBrowserProcess(cmd *exec.Cmd) (*browserProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
		LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE}}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return startBrowserInJob(cmd, job)
}

func startBrowserInJob(cmd *exec.Cmd, job windows.Handle) (*browserProcess, error) {
	p := &browserProcess{cmd: cmd, job: job}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { p.Kill(); return nil }
	}
	if err := cmd.Start(); err != nil {
		p.Close()
		return nil, err
	}
	// The host waits on its input pipe until this assignment succeeds.
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		_ = windows.CloseHandle(process)
	}
	if err != nil {
		p.Kill()
		_ = cmd.Wait()
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *browserProcess) Kill() {
	_ = windows.TerminateJobObject(p.job, 1)
	_ = p.cmd.Process.Kill()
}

func (p *browserProcess) Close() { _ = windows.CloseHandle(p.job) }

func (p *browserProcess) Wait() error {
	result := p.cmd.Wait()
	processes, captureErr := p.processHandles()
	defer func() {
		for _, process := range processes {
			_ = windows.CloseHandle(process)
		}
	}()
	if err := windows.TerminateJobObject(p.job, 1); err != nil {
		return fmt.Errorf("Cannot end the browser process tree: %w", err)
	}
	if captureErr != nil {
		return captureErr
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION: four LARGE_INTEGERs, then four DWORDs.
		var info struct {
			Times                   [4]int64
			PageFaults, Total       uint32
			Active, TotalTerminated uint32
		}
		if err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
			return fmt.Errorf("Cannot verify browser process exit: %w", err)
		}
		if info.Active == 0 {
			// Accounting reaches zero before kernel cleanup signals process handles.
			for _, process := range processes {
				remaining := time.Until(deadline).Milliseconds()
				if remaining < 0 {
					remaining = 0
				}
				state, err := windows.WaitForSingleObject(process, uint32(remaining))
				if err != nil || state != windows.WAIT_OBJECT_0 {
					return fmt.Errorf("Cannot verify browser child exit: state=%d err=%v", state, err)
				}
			}
			return result
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Browser process tree did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *browserProcess) processHandles() (processes []windows.Handle, err error) {
	type processList struct {
		Assigned, Count uint32
		PIDs            [1]uintptr
	}
	for capacity := 64; ; capacity *= 2 {
		data := make([]byte, 8+capacity*int(unsafe.Sizeof(uintptr(0))))
		err = windows.QueryInformationJobObject(p.job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&data[0])), uint32(len(data)), nil)
		if err == windows.ERROR_MORE_DATA {
			continue
		}
		if err != nil {
			return processes, fmt.Errorf("Cannot enumerate browser processes: %w", err)
		}
		list := (*processList)(unsafe.Pointer(&data[0]))
		if list.Count > uint32(capacity) {
			return processes, fmt.Errorf("Invalid browser process list")
		}
		for _, pid := range unsafe.Slice(&list.PIDs[0], list.Count) {
			process, openErr := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
			if openErr == windows.ERROR_INVALID_PARAMETER {
				continue // The process exited before its handle could be opened.
			}
			if openErr != nil {
				return processes, fmt.Errorf("Cannot observe browser process exit: %w", openErr)
			}
			processes = append(processes, process)
		}
		return processes, nil
	}
}
