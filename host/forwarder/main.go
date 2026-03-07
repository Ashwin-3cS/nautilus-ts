// Host-side forwarder for Nautilus enclaves.
//
// Bridges inbound TCP connections to the enclave's VSOCK listener.
// Each accepted TCP connection dials a new VSOCK connection and copies
// data bidirectionally using goroutines with proper half-close via
// CloseWrite, ensuring clean connection teardown.
//
// Usage:
//
//	host-forwarder <listen-port> <enclave-cid> <vsock-port>
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"

	"github.com/mdlayher/vsock"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <listen-port> <enclave-cid> <vsock-port>\n", os.Args[0])
		os.Exit(1)
	}

	listenPort, err := strconv.ParseUint(os.Args[1], 10, 16)
	if err != nil {
		log.Fatalf("invalid listen port: %s", os.Args[1])
	}
	cid, err := strconv.ParseUint(os.Args[2], 10, 32)
	if err != nil {
		log.Fatalf("invalid enclave CID: %s", os.Args[2])
	}
	vsockPort, err := strconv.ParseUint(os.Args[3], 10, 32)
	if err != nil {
		log.Fatalf("invalid VSOCK port: %s", os.Args[3])
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", listenPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	log.Printf("TCP:%d → VSOCK:%d:%d", listenPort, cid, vsockPort)

	for {
		tcp, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go bridge(tcp, uint32(cid), uint32(vsockPort))
	}
}

func bridge(tcp net.Conn, cid, port uint32) {
	defer tcp.Close()

	vc, err := vsock.Dial(cid, port, nil)
	if err != nil {
		log.Printf("VSOCK dial error: %v", err)
		return
	}
	defer vc.Close()

	done := make(chan struct{}, 2)

	// TCP → VSOCK (request)
	go func() {
		io.Copy(vc, tcp)
		vc.CloseWrite()
		done <- struct{}{}
	}()

	// VSOCK → TCP (response)
	go func() {
		io.Copy(tcp, vc)
		if tc, ok := tcp.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	<-done
	<-done
}
