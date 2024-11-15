package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
)

var (
	remotePort = 33334
)

func main() {
	// create socket
	for i := 0; i < 100000; i++ {
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
		if err != nil {
			log.Fatal(os.NewSyscallError("socket", err))
		}

		// construct remote socket address
		raddr := &syscall.SockaddrInet4{Port: remotePort}
		copy(raddr.Addr[:], net.IPv4(127, 0, 0, 1).To4())

		// connect returns an os.File
		f, err := connect(context.Background(), fd, raddr)
		if err != nil {
			syscall.Close(fd)
			log.Fatal(os.NewSyscallError("connect", err))
		}

		// write
		n, err := f.Write([]byte("hello,"))
		if err != nil {
			f.Close()
			log.Fatal(err)
		}

		// close
		log.Printf("i: %d, bytes written: %d", i, n)
		err = f.Close()
		if err != nil {
			log.Fatal(err)
		}
	}
}

// code taken from go net package and modified according to our needs
func connect(ctx context.Context, fd int, ra syscall.Sockaddr) (f *os.File, ret error) {
	err := syscall.Connect(fd, ra)
	switch err {
	case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
	case nil, syscall.EISCONN:
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		f = os.NewFile(uintptr(fd), "")
		if f == nil {
			return nil, errors.New("os.NewFile returned nil")
		}

	default:
		return nil, os.NewSyscallError("connect", err)
	}

	// Change: By creating our connection here we register the connected fd with the go runtime network poller.
	// We have to do this here in order for the deadlines below to work.
	f = os.NewFile(uintptr(fd), "")
	if f == nil {
		return nil, errors.New("os.NewFile returned nil")
	}

	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		f.SetWriteDeadline(deadline)
		defer f.SetWriteDeadline(time.Time{})
	}
	// Start the "interrupter" goroutine, if this context might be canceled.
	//
	// The interrupter goroutine waits for the context to be done and interrupts the
	// dial (by altering the conn's write deadline, which wakes up waitWrite).
	ctxDone := ctx.Done()
	if ctxDone != nil {
		// Wait for the interrupter goroutine to exit before returning from connect.
		done := make(chan struct{})
		interruptRes := make(chan error)
		defer func() {
			close(done)
			if ctxErr := <-interruptRes; ctxErr != nil && ret == nil {
				// The interrupter goroutine called SetWriteDeadline,
				// but the connect code below had returned from
				// waitWrite already and did a successful connect (ret
				// == nil). Because we've now poisoned the connection
				// by making it unwritable, don't return a successful
				// dial. This was issue 16523.
				_ = f.Close()
				f = nil
				ret = ctxErr
			}
		}()
		go func() {
			select {
			case <-ctxDone:
				// Force the runtime's poller to immediately give up
				// waiting for writability, unblocking waitWrite below.
				f.SetWriteDeadline(time.Unix(1, 0))
				interruptRes <- ctx.Err()
			case <-done:
				interruptRes <- nil
			}
		}()
	}

	// get rawConn to perform Write
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}

	for {
		// Change: The netFD.connect func from go runtime is calling waitWrite here directly
		// from the poll descriptor (fd.pfd.WaitWrite()). This we can not do directly as we don't have
		// access to this poll descriptor.
		// Instead, the rawConn.Write function calls internally WaitWrite, and we
		// can trick it to do that with a dummy function passed to it. This
		// function should return false the first time and true afterward.
		// See the os.rawConn.Write function for details.
		dummyFuncCalled := false
		doErr := rc.Write(func(fd uintptr) bool {
			if !dummyFuncCalled {
				dummyFuncCalled = true
				return false // first time only causing the call to WaitWrite
			}
			return true // causing exit from pfd.RawWrite
		})
		if doErr != nil {
			_ = f.Close()
			select {
			case <-ctxDone:
				return nil, ctx.Err()
			default:
			}
			return nil, doErr
		}

		// Performing multiple connect system calls on a
		// non-blocking socket under Unix variants does not
		// necessarily result in earlier errors being
		// returned. Instead, once runtime-integrated network
		// poller tells us that the socket is ready, get the
		// SO_ERROR socket option to see if the connection
		// succeeded or failed. See issue 7474 for further
		// details.
		nerr, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			_ = f.Close()
			return nil, os.NewSyscallError("getsockopt", err)
		}

		switch err = syscall.Errno(nerr); err {
		case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
		case syscall.EISCONN:
			return f, nil
		case syscall.Errno(0):
			// The runtime poller can wake us up spuriously;
			// see issues 14548 and 19289. Check that we are
			// really connected; if not, wait again.

			if _, err = syscall.Getpeername(fd); err == nil {
				return f, nil
			}

		default:
			_ = f.Close()
			return nil, os.NewSyscallError("connect", err)
		}
		runtime.KeepAlive(f)
	}
}
