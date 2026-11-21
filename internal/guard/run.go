package guard

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

type Result struct {
	Stdout, Stderr []byte
	RC             int
	Duration       time.Duration
	LaunchErr      error
	CaptureErr     error
}

type waited struct{ duration time.Duration }

func Run(args []string, input []byte) Result {
	var result Result
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	inR, inW, err := pipe()
	if err != nil {
		return Result{RC: 2, LaunchErr: err}
	}
	outR, outW, err := pipe()
	if err != nil {
		return Result{RC: 2, LaunchErr: err}
	}
	errR, errW, err := pipe()
	if err != nil {
		return Result{RC: 2, LaunchErr: err}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 500 * time.Millisecond
	start := time.Now()
	if err = cmd.Start(); err != nil {
		return Result{RC: 2, LaunchErr: err, Duration: time.Since(start)}
	}
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()
	go func() { _, _ = io.Copy(inW, bytes.NewReader(input)); _ = inW.Close() }()
	var stdout, stderr bytes.Buffer
	copied := make(chan error, 2)
	go func() { _, e := io.Copy(&stdout, outR); copied <- e }()
	go func() { _, e := io.Copy(&stderr, errR); copied <- e }()
	done := make(chan waited, 1)
	go func() { _ = cmd.Wait(); done <- waited{time.Since(start)} }()
	var state waited
	var interrupted os.Signal
	select {
	case state = <-done:
	case interrupted = <-signals:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(2 * time.Second)
		expired := false
		select {
		case state = <-done:
		case <-timer.C:
			expired = true
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			state = <-done
		}
		// The main process may exit while a descendant ignores TERM.
		if !expired && syscall.Kill(-cmd.Process.Pid, 0) == nil {
			<-timer.C
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		timer.Stop()
	}
	_ = inW.Close()
	result.Duration = state.duration
	result.RC = cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		result.RC = 128 + int(status.Signal())
	}
	if interrupted != nil {
		result.RC = 128 + int(interrupted.(syscall.Signal))
	}
	// Own the pipes explicitly so process duration excludes pipe drain. This is
	// the same 500 ms bound as Cmd.WaitDelay, including both streams together.
	timer := time.NewTimer(cmd.WaitDelay)
	pending := 2
	for pending > 0 {
		select {
		case sig := <-signals:
			result.RC = 128 + int(sig.(syscall.Signal))
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if syscall.Kill(-cmd.Process.Pid, 0) == nil {
				deadline := time.NewTimer(2 * time.Second)
				<-deadline.C
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case e := <-copied:
			pending--
			if e != nil && result.CaptureErr == nil {
				result.CaptureErr = e
			}
		case <-timer.C:
			result.CaptureErr = exec.ErrWaitDelay
			_ = outR.Close()
			_ = errR.Close()
			for pending > 0 {
				<-copied
				pending--
			}
		}
	}
	timer.Stop()
	result.Stdout = stdout.Bytes()
	result.Stderr = stderr.Bytes()
	return result
}
