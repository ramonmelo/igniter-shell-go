package shell

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Gameye/igniter-shell-go/statemachine"
	"github.com/kr/pty"
)

const outputBuffer = 200
const inputBuffer = 20
const signalBuffer = 20
const stateChangeBuffer = 20

// TODO: figure out which signals we should actually pass
var signalsToPass = []os.Signal{
	syscall.SIGABRT,
	syscall.SIGALRM,
	syscall.SIGBUS,
	syscall.SIGCHLD,
	syscall.SIGCLD,
	syscall.SIGCONT,
	syscall.SIGFPE,
	syscall.SIGHUP,
	syscall.SIGILL,
	syscall.SIGINT,
	syscall.SIGIO,
	syscall.SIGIOT,
	syscall.SIGKILL,
	syscall.SIGPIPE,
	syscall.SIGPOLL,
	syscall.SIGPROF,
	syscall.SIGPWR,
	syscall.SIGQUIT,
	syscall.SIGSEGV,
	syscall.SIGSTKFLT,
	syscall.SIGSTOP,
	syscall.SIGSYS,
	syscall.SIGTERM,
	syscall.SIGTRAP,
	syscall.SIGTSTP,
	syscall.SIGTTIN,
	syscall.SIGTTOU,
	syscall.SIGUNUSED,
	syscall.SIGURG,
	syscall.SIGUSR1,
	syscall.SIGUSR2,
	syscall.SIGVTALRM,
	syscall.SIGWINCH,
	syscall.SIGXCPU,
	syscall.SIGXFSZ,
}

// RunWithStateMachine runs a command
func RunWithStateMachine(
	cmd *exec.Cmd,
	config *statemachine.Config,
	withPty bool,
) (
	exit int,
	err error,
) {
	/*
		This order matters! Mostly because of the deferred closing of
		the channels. Changing this order might cause a panic for writing
		to a closed channel.
	*/
	outputLines := make(chan string, outputBuffer)
	// TODO: close this!
	// defer close(outputLines)

	inputLines := make(chan string, inputBuffer)
	defer close(inputLines)

	stateChanges := make(chan statemachine.StateChange, stateChangeBuffer)
	defer close(stateChanges)

	signals := make(chan os.Signal, signalBuffer)
	defer close(signals)

	signal.Notify(signals, signalsToPass...)
	defer signal.Stop(signals)
	/*
	 */

	go passStateChanges(inputLines, stateChanges)
	go statemachine.Run(config, stateChanges, outputLines)

	if withPty {
		exit, err = runCommandPTY(cmd, outputLines, inputLines, signals)
		if err != nil {
			return
		}
	} else {
		exit, err = runCommand(cmd, outputLines, inputLines, signals)
		if err != nil {
			return
		}
	}

	return
}

// runCommand runs a command
func runCommand(
	cmd *exec.Cmd,
	outputLines chan<- string,
	inputLines <-chan string,
	signals <-chan os.Signal,
) (
	exit int,
	err error,
) {
	// setup pipes

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	defer stdout.Close()

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return
	}
	defer stderr.Close()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	defer stdin.Close()

	stdoutPipeReader, stdoutPipeWriter := io.Pipe()
	stderrPipeReader, stderrPipeWriter := io.Pipe()
	defer stdoutPipeReader.Close()
	defer stdoutPipeWriter.Close()
	defer stderrPipeReader.Close()
	defer stderrPipeWriter.Close()

	stdoutTee := io.TeeReader(stdout, stdoutPipeWriter)
	stderrTee := io.TeeReader(stderr, stderrPipeWriter)

	stdoutReader := bufio.NewReader(stdoutTee)
	stderrReader := bufio.NewReader(stderrTee)
	defer stdoutReader.Reset(bytes.NewReader(make([]byte, 0)))
	defer stderrReader.Reset(bytes.NewReader(make([]byte, 0)))

	// connect readers, writers, channels

	go readLines(stdoutReader, outputLines)
	go readLines(stderrReader, outputLines)
	go writeLines(stdin, inputLines)

	go io.Copy(os.Stdout, stdoutPipeReader)
	go io.Copy(os.Stderr, stderrPipeReader)
	go io.Copy(stdin, os.Stdin)

	// start the command

	err = cmd.Start()
	if err != nil {
		return
	}

	go passSignals(cmd.Process, signals)

	// wait for exit

	exit, err = waitCommand(cmd)
	if err != nil {
		return
	}

	return
}

// runCommand runs a command as a pseudo terminal!
func runCommandPTY(
	cmd *exec.Cmd,
	outputLines chan<- string,
	inputLines <-chan string,
	signals <-chan os.Signal,
) (
	exit int,
	err error,
) {
	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()
	defer pipeWriter.Close()

	// start process and get pty

	ptyStream, err := pty.Start(cmd)
	if err != nil {
		return
	}
	defer ptyStream.Close()

	ptyTee := io.TeeReader(ptyStream, pipeWriter)
	ptyReader := bufio.NewReader(ptyTee)
	// stop this reader from emitting possible buffered lines
	defer ptyReader.Reset(bytes.NewReader(make([]byte, 0)))

	// connect

	go readLines(ptyReader, outputLines)
	go writeLines(ptyStream, inputLines)

	go io.Copy(os.Stdout, pipeReader)
	go io.Copy(ptyStream, os.Stdin)

	go passSignals(cmd.Process, signals)

	// wait for exit

	exit, err = waitCommand(cmd)
	if err != nil {
		return
	}

	return
}

// waitCommand waits for a command to exit and returns the exit code
func waitCommand(
	cmd *exec.Cmd,
) (
	exit int,
	err error,
) {
	err = cmd.Wait()
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			err = nil
			exit = status.ExitStatus()
			return
		}
	}

	if err != nil {
		return
	}

	return
}

// readLines reads lines from a reader in a channel
func readLines(
	bufferedReader *bufio.Reader,
	lines chan<- string,
) {
	var err error
	var line string
	for {
		line, err = bufferedReader.ReadString('\n')
		if err == io.EOF {
			return
		}
		if err != nil {
			panic(err)
		}

		line = strings.TrimSpace(line)
		if line != "" {
			lines <- line
		}
	}
}

// writeLines writes lines from a channel in a writer
func writeLines(
	writer io.Writer,
	lines <-chan string,
) {
	var err error
	var line string
	for line = range lines {
		_, err = io.WriteString(writer, line+"\n")
		if err == io.EOF {
			return
		}
		if err != nil {
			panic(err)
		}
	}
	return
}

// passSignals passes singnals from a channel to a process
func passSignals(
	process *os.Process,
	signals <-chan os.Signal,
) {
	var err error
	var signal os.Signal
	for signal = range signals {
		err = process.Signal(signal)
		if err != nil {
			return
		}
	}
	return
}

// passStateChanges passes commands of state change structs into a
// string channel
func passStateChanges(
	inputLines chan<- string,
	stateChanges <-chan statemachine.StateChange,
) {
	for stateChange := range stateChanges {
		inputLines <- stateChange.Command
	}
}
