package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
)

var (
	port         = 33334
	aLongTimeAgo = time.Unix(1, 0)
	noDeadline   = time.Time{}
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ln, err := net.Listen("tcp4", "127.0.0.1:"+fmt.Sprint(port))
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	// Echo back the first line of each incoming connection.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				break
			}
			log.Printf("connection ACCEPTED: %v <----------> %v", c.LocalAddr(), c.RemoteAddr())
			rb := bufio.NewReader(c)
			log.Printf("reading from accepted connection...")
			line, err := rb.ReadString('\n')
			if err != nil {
				log.Println(err)
				c.Close()
				continue
			}
			log.Printf("reading from accepted connection done, now writing back...")
			if _, err := c.Write([]byte(line)); err != nil {
				log.Println(err)
			}
			c.Close()
		}
	}()

	try := func() {
		// construct remote socket address
		raddr := &syscall.SockaddrInet4{Port: port}
		//copy(raddr.Addr[:], net.IPv4(192, 168, 12, 12).To4())
		copy(raddr.Addr[:], net.IPv4(127, 0, 0, 1).To4())

		f, err := RawDial(raddr) // comment this line and uncomment next to use net.Dial and net.Conn api
		//f, err := net.Dial("tcp4", "127.0.0.1:"+fmt.Sprint(port))

		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		log.Printf("dial successful")
		//time.Sleep(1 * time.Hour)

		// Send some data
		const message = "echo!\n"
		if _, err := f.Write([]byte(message)); err != nil {
			log.Fatal(err)
		}

		// The server should echo the line, and close the connection.
		rb := bufio.NewReader(f)
		line, err := rb.ReadString('\n')
		if err != nil {
			log.Fatal(err)
		}
		if line != message {
			log.Printf("got %q; want %q", line, message)
		}
		if _, err := rb.ReadByte(); err != io.EOF {
			log.Printf("got %v; want %v", err, io.EOF)
		}
	}

	for i := 0; i < 10000; i++ {
		try()
	}

	log.Printf("done")
}

func RawDial(raddr syscall.Sockaddr) (*os.File, error) {
	// create socket
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	if err != nil {
		log.Fatal(os.NewSyscallError("socket", err))
	}

	// connect returns an os.File
	ctx, cancel := context.WithTimeout(context.Background(), 23*time.Second)
	defer cancel()
	f, err := connect(ctx, fd, raddr)
	if err != nil {
		syscall.Close(fd)
		log.Fatal(os.NewSyscallError("connect syscall", err))
	}
	return f, nil
}

// code taken from go net package and modified according to our needs
func connect(ctx context.Context, fd int, ra syscall.Sockaddr) (f *os.File, ret error) {
	log.Printf("Connecting...")
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
		return f, nil

	default:
		log.Printf("1 connect returned error: %v", err)
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
	}

	// Start the "interrupter" goroutine, if this context might be canceled.
	//
	// The interrupter goroutine waits for the context to be done and interrupts the
	// dial (by altering the conn's write deadline, which wakes up waitWrite).
	ctxDone := ctx.Done()

	// until issue #70373 is fixed (if it gets fixed :), use a timer to periodically wake up the waitWrite and check the connection
	// if ctxDone != nil {
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
		} else {
			f.SetWriteDeadline(noDeadline) // restore the writeDeadline
		}
	}()
	go func() {
		for {
			select {
			case <-ctxDone:
				// Force the runtime's poller to immediately give up
				// waiting for writability, unblocking waitWrite below.
				f.SetWriteDeadline(aLongTimeAgo)
				interruptRes <- ctx.Err()
				return
			case <-done:
				interruptRes <- nil
				return
			case <-time.After(2 * time.Millisecond): // TODO: increase wakeup timeout after every wakeup
				// Force the runtime's poller to immediately give up
				// waiting for writability, unblocking waitWrite below.
				log.Printf("TIMER INFLICTED WAKEUP...")
				f.SetWriteDeadline(aLongTimeAgo)
			}
		}
	}()
	//}

	// get rawConn to perform Write
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		log.Printf("2 connect returned error: %v", err)
		return nil, err
	}

	for {
		// Change: The netFD.connect func from go runtime is calling waitWrite here directly
		// from the poll descriptor (fd.pfd.WaitWrite()). This we can not do directly as we don't have
		// access to this poll descriptor.
		// Instead, the rawConn.Write function calls internally WaitWrite, and we
		// can make it to do that with a dummy function passed to it. This
		// function should return false the first time and true afterward.
		// See the os.rawConn.Write function for details.
		log.Printf("Entering pollWait...")
		dummyFuncCalled := false
		doErr := rc.Write(func(fd uintptr) bool {
			if !dummyFuncCalled {
				dummyFuncCalled = true
				return false // first time only causing the call to WaitWrite
			}
			return true // causing exit from pfd.RawWrite
		})
		if doErr != nil {
			select {
			case <-ctxDone:
				_ = f.Close()
				return nil, ctx.Err()
			default:
			}
			// here if the error is timeout then this is caused by our wakeup timer (workaround for issue #70373)
			// in that case we just skip to SO_ERROR checking
			log.Printf("rc.Write returned error: %T, %v, isTimeout: %v", doErr, doErr, errors.Is(doErr, os.ErrDeadlineExceeded))
			if !errors.Is(doErr, os.ErrDeadlineExceeded) {
				_ = f.Close()
				return nil, doErr
			}
		}

		err = f.SetWriteDeadline(noDeadline) // restore the writeDeadline
		if err != nil {
			_ = f.Close()
			log.Printf("4 f.SetWriteDeadline returned error: %v", err)
			return nil, err
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
			log.Printf("5 syscall.GetsockoptInt returned error: %v", err)
			return nil, os.NewSyscallError("getsockopt", err)
		}
		log.Printf("SO_ERROR syscall.Errno is %d", nerr)

		switch err = syscall.Errno(nerr); err {
		case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
		case syscall.EISCONN:
			return f, nil
		case syscall.Errno(0):
			// The runtime poller can wake us up spuriously;
			// see issues 14548 and 19289. Check that we are
			// really connected; if not, wait again.
			log.Printf("syscall.Errno is 0, calling rawGetpeername")
			if _, err = syscall.Getpeername(fd); err == nil {
				return f, nil
			} else {
				log.Printf("Getpeername returned error: %v", err)
			}

		default:
			_ = f.Close()
			log.Printf("SO_ERROR: %d, %v", err.(syscall.Errno), err)
			return nil, os.NewSyscallError("connect", err)
		}
		runtime.KeepAlive(f)
	}
}
