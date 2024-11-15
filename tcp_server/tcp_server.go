package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	ln, err := net.Listen("tcp4", "127.0.0.1:33334")
	if err != nil {
		panic(err)
	}
	log.Printf("Accepting TCP4 connections on %v ...", ln.Addr())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Println(err)
				break
			}
			b := make([]byte, 256)
			n, err := c.Read(b)
			if err != nil {
				log.Println(err)
			}
			log.Printf("Received %d bytes: %s", n, string(b[:n]))
			_ = c.Close()
		}
	}()
	// shutdown hook
	shutdownChan := make(chan os.Signal, 1) // channel is buffered to ensure signal sending before receiving it will not cause a deadlock
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)
	<-shutdownChan // block until signal is received
	ln.Close()
}
