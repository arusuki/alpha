package chatcli

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func terminalPassword(ctx context.Context) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("无法打开终端，请在终端输入密码或使用 --password-stdin")
	}
	defer tty.Close()
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	mode := *old
	mode.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &mode); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
	fmt.Fprint(tty, "使用者密码: ")
	defer fmt.Fprintln(tty)
	type answer struct {
		text string
		err  error
	}
	done := make(chan answer, 1)
	go func() { s, err := readLine(bufio.NewReader(tty)); done <- answer{s, err} }()
	select {
	case a := <-done:
		return a.text, a.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
