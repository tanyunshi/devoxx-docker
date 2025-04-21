package ipc

import (
	"os"
	"syscall"
)

// CreateSockerPair creates a socketpair and returns the file descriptors
func CreateSockerPair() (parent *os.File, child *os.File, err error) {
	// Create a pair of connected sockets
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	parent = os.NewFile(uintptr(fds[0]), "parent-socket")
	child = os.NewFile(uintptr(fds[1]), "child-socket")
	return parent, child, nil
}

func SendReday(f *os.File) error {
	// Send a message to the socket
	_, err := f.Write([]byte("ready"))
	if err != nil {
		return err
	}
	return nil
}

func WaitForReady(f *os.File) error {
	// Read the message from the socket
	buf := make([]byte, 5)
	_, err := f.Read(buf)
	if err != nil {
		return err
	}
	if string(buf) != "ready" {
		return syscall.EINVAL
	}
	return nil
}
