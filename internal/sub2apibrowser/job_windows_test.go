//go:build windows && !nogui && sub2api_webview2

package sub2apibrowser

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"golang.org/x/sys/windows"
)

func runJobFixture() bool {
	if len(os.Args) != 3 || os.Args[1] != "--sub2api-job-fixture" {
		return false
	}
	if os.Args[2] == "child" {
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return true
	}
	input := bufio.NewScanner(os.Stdin)
	if !input.Scan() {
		return true
	}
	child := proc.Command(os.Args[0], "--sub2api-job-fixture", "child")
	output, err := child.StdoutPipe()
	if err != nil || child.Start() != nil {
		return true
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if _, err := bufio.NewReader(output).ReadString('\n'); err != nil {
		return true
	}
	fmt.Println(child.Process.Pid)
	input.Scan()
	// Exit without gracefully stopping the child; the parent's job must end it.
	os.Exit(0)
	return true
}

func TestNativeJobFailureRejectsHost(t *testing.T) {
	cmd := proc.CommandContext(context.Background(), os.Args[0], "--sub2api-job-fixture", "child")
	process, err := startBrowserInJob(cmd, windows.InvalidHandle)
	if err == nil || process != nil {
		t.Fatal("browser continued without a job")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("unisolated host was not waited for")
	}
}

func TestNativeJobWaitEndsDescendants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, os.Args[0], "--sub2api-job-fixture", "parent")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process, err := startBrowserProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	waited := false
	defer func() {
		if !waited {
			process.Kill()
			_ = process.Wait()
		}
	}()
	if _, err := fmt.Fprintln(input, "spawn"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	if _, err := fmt.Fprintln(input, "exit"); err != nil {
		t.Fatal(err)
	}
	err = process.Wait()
	waited = true
	if err != nil {
		t.Fatal(err)
	}
	state, err := windows.WaitForSingleObject(child, 0)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("browser child still running after Wait: state=%d err=%v", state, err)
	}
}
