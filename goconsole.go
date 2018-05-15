package main

import (

	"fmt"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"k8s.io/client-go/tools/remotecommand"
	"io"
	"github.com/pkg/errors"
	"github.com/moby/moby/pkg/term"
	"os"
	"net"
	"strings"
	"golang.org/x/crypto/ssh/terminal"
	"github.com/moby/moby/pkg/signal"
	gosignal "os/signal"
)

const (
	AttachPipeStdin  = 1
	AttachPipeStdout = 2
	AttachPipeStderr = 3
)

// AttachConsole contains streams that will be attached to the container
type AttachConsole struct {
	// OutputStream will be attached to container's STDOUT
	OutputStream io.WriteCloser
	// ErrorStream will be attached to container's STDERR
	ErrorStream io.WriteCloser
	// InputStream will be attached to container's STDIN
	InputStream io.Reader
	// AttachOutput is whether to attach to STDOUT
	// If false, stdout will not be attached
	AttachOutput bool
	// AttachError is whether to attach to STDERR
	// If false, stdout will not be attached
	AttachError bool
	// AttachInput is whether to attach to STDIN
	// If false, stdout will not be attached
	AttachInput bool
	SocketPath string
	ControlPath string

}
// DetachError is special error which returned in case of container detach.
type DetachError struct{}

func (DetachError) Error() string {
	return "detached from container"
}

func Attach(streams *AttachConsole, keys string, resize <-chan remotecommand.TerminalSize) error {
	// Check the validity of the provided keys first
	var err error
	detachKeys := []byte{}
	if len(keys) > 0 {
		detachKeys, err = term.ToBytes(keys)
		if err != nil {
			return errors.Wrapf(err, "invalid detach keys")
		}
	}

	return attachContainerSocket(resize, detachKeys, streams)
}

func attachContainerSocket(resize <-chan remotecommand.TerminalSize, detachKeys []byte, streams *AttachConsole) error {
	HandleResizing(resize, func(size remotecommand.TerminalSize) {
		controlFile, err := os.OpenFile(streams.ControlPath, unix.O_WRONLY, 0)
		if err != nil {
			logrus.Debugf("Could not open ctl file: %v", err)
			return
		}
		defer controlFile.Close()

		logrus.Debugf("Received a resize event: %+v", size)
		if _, err = fmt.Fprintf(controlFile, "%d %d %d\n", 1, size.Height, size.Width); err != nil {
			logrus.Warnf("Failed to write to control file to resize terminal: %v", err)
		}
	})
	logrus.Debug("connecting to socket ", streams.SocketPath)

	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: streams.SocketPath, Net: "unixpacket"})
	if err != nil {
		return errors.Wrapf(err, "failed to connect to container's attach socket: %v")
	}
	defer conn.Close()

	receiveStdoutError := make(chan error)
	if streams.AttachOutput || streams.AttachError {
		go func() {
			receiveStdoutError <- redirectResponseToOutputStreams(streams.OutputStream, streams.ErrorStream, streams.AttachOutput, streams.AttachError, conn)
		}()
	}

	stdinDone := make(chan error)
	go func() {
		var err error
		if streams.AttachInput {
			_, err = CopyDetachable(conn, streams.InputStream, detachKeys)
			conn.CloseWrite()
		}
		stdinDone <- err
	}()

	select {
	case err := <-receiveStdoutError:
		return err
	case err := <-stdinDone:
		if _, ok := err.(DetachError); ok {
			return nil
		}
		if streams.AttachOutput || streams.AttachError {
			return <-receiveStdoutError
		}
	}
	return nil
}

func redirectResponseToOutputStreams(outputStream, errorStream io.Writer, writeOutput, writeError bool, conn io.Reader) error {
	var err error
	buf := make([]byte, 8192+1) /* Sync with conmon STDIO_BUF_SIZE */
	for {
		nr, er := conn.Read(buf)
		if nr > 0 {
			var dst io.Writer
			var doWrite bool
			switch buf[0] {
			case AttachPipeStdout:
				dst = outputStream
				doWrite = writeOutput
			case AttachPipeStderr:
				dst = errorStream
				doWrite = writeError
			default:
				logrus.Infof("Received unexpected attach type %+d", buf[0])
			}

			if doWrite {
				nw, ew := dst.Write(buf[1:nr])
				if ew != nil {
					err = ew
					break
				}
				if nr != nw+1 {
					err = io.ErrShortWrite
					break
				}
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			err = er
			break
		}
	}
	return err
}

// CopyDetachable is similar to io.Copy but support a detach key sequence to break out.
func CopyDetachable(dst io.Writer, src io.Reader, keys []byte) (written int64, err error) {
	if len(keys) == 0 {
		// Default keys : ctrl-p ctrl-q
		keys = []byte{16, 17}
	}

	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			preservBuf := []byte{}
			for i, key := range keys {
				preservBuf = append(preservBuf, buf[0:nr]...)
				if nr != 1 || buf[0] != key {
					break
				}
				if i == len(keys)-1 {
					// src.Close()
					return 0, DetachError{}
				}
				nr, er = src.Read(buf)
			}
			var nw int
			var ew error
			if len(preservBuf) > 0 {
				nw, ew = dst.Write(preservBuf)
				nr = len(preservBuf)
			} else {
				nw, ew = dst.Write(buf[0:nr])
			}
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err
}

// Helper for prepareAttach - set up a goroutine to generate terminal resize events
func resizeTty(resize chan remotecommand.TerminalSize) {
	sigchan := make(chan os.Signal, 1)
	gosignal.Notify(sigchan, signal.SIGWINCH)
	sendUpdate := func() {
		winsize, err := term.GetWinsize(os.Stdin.Fd())
		if err != nil {
			logrus.Warnf("Could not get terminal size %v", err)
			return
		}
		resize <- remotecommand.TerminalSize{
			Width:  winsize.Width,
			Height: winsize.Height,
		}
	}
	go func() {
		defer close(resize)
		// Update the terminal size immediately without waiting
		// for a SIGWINCH to get the correct initial size.
		sendUpdate()
		for range sigchan {
			sendUpdate()
		}
	}()
}

func exit(err string) {
	fmt.Println(err)
	os.Exit(1)
}

func main() {
	var (
		controlPath, socketPath string
	)
	args := os.Args
	if len(args) < 2 {
		exit("must provide control path and socketpath")
	}

	if strings.HasSuffix(args[1], "ctl") {
		controlPath = args[1]
		socketPath = args[2]
	} else {
		controlPath = args[2]
		socketPath = args[1]

	}
	fmt.Printf("Control Path: %s\n", controlPath)
	fmt.Printf("Socket Path: %s\n", socketPath)
	streams := new(AttachConsole)
	streams.OutputStream = os.Stdout
	streams.ErrorStream = os.Stderr
	streams.InputStream = os.Stdin
	streams.AttachOutput = true
	streams.AttachError = true
	streams.AttachInput = true
	streams.SocketPath = socketPath
	streams.ControlPath = controlPath

	resize := make(chan remotecommand.TerminalSize)

	haveTerminal := terminal.IsTerminal(int(os.Stdin.Fd()))

	// Check if we are attached to a terminal. If we are, generate resize
	// events, and set the terminal to raw mode
	if haveTerminal {
		logrus.Debugf("Handling terminal attach")

		resizeTty(resize)

		oldTermState, err := term.SaveState(os.Stdin.Fd())
		if err != nil {
			exit("unable to save terminal state")
		}

		term.SetRawTerminal(os.Stdin.Fd())

		defer term.RestoreTerminal(os.Stdin.Fd(), oldTermState)
	}
	if err := Attach(streams, "", resize ); err != nil {
		exit(err.Error())
	}
}
