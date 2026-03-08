// Unified VSOCK↔TCP forwarder for Nautilus.
//
// Two modes:
//
//	forwarder host <tcp-port> <cid> <vsock-port>
//	    Listen on TCP, forward to VSOCK. Used on the parent EC2 instance
//	    to bridge inbound HTTP into the enclave.
//
//	forwarder enclave
//	    Read JSON config from stdin, then:
//	    1. Write /etc/hosts for endpoint hostname resolution
//	    2. Inbound:  VSOCK listen → TCP connect to localhost (HTTP to Bun)
//	    3. Outbound: TCP listen on loopback → VSOCK connect to parent
//	    Runs inside the Nitro Enclave where there is no network.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/mdlayher/vsock"
)

const parentCID = 3

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "host":
		hostMode()
	case "enclave":
		enclaveMode()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  %s host <tcp-port> <cid> <vsock-port>\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s enclave  (reads JSON config from stdin)\n", os.Args[0])
	os.Exit(1)
}

// --- Host mode: TCP listen → VSOCK connect ---

func hostMode() {
	if len(os.Args) != 5 {
		usage()
	}

	listenPort, err := strconv.ParseUint(os.Args[2], 10, 16)
	if err != nil {
		log.Fatalf("invalid listen port: %s", os.Args[2])
	}
	cid, err := strconv.ParseUint(os.Args[3], 10, 32)
	if err != nil {
		log.Fatalf("invalid enclave CID: %s", os.Args[3])
	}
	vsockPort, err := strconv.ParseUint(os.Args[4], 10, 32)
	if err != nil {
		log.Fatalf("invalid VSOCK port: %s", os.Args[4])
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
		go func() {
			if err := bridgeToVSOCK(tcp, uint32(cid), uint32(vsockPort)); err != nil {
				log.Printf("bridge error: %v", err)
			}
		}()
	}
}

func bridgeToVSOCK(src net.Conn, cid, port uint32) error {
	defer src.Close()

	vc, err := vsock.Dial(cid, port, nil)
	if err != nil {
		return fmt.Errorf("VSOCK dial %d:%d: %w", cid, port, err)
	}
	defer vc.Close()

	return copyBidirectional(src, vc)
}

// --- Enclave mode: JSON config from stdin, multiple bridges ---

type Config struct {
	HTTPVsockPort uint32     `json:"http_vsock_port"`
	HTTPTCPPort   uint16     `json:"http_tcp_port"`
	Endpoints     []Endpoint `json:"endpoints"`
}

type Endpoint struct {
	Host      string `json:"host"`
	VsockPort uint32 `json:"vsock_port"`
}

func enclaveMode() {
	var config Config
	if err := json.NewDecoder(os.Stdin).Decode(&config); err != nil {
		log.Fatalf("invalid config JSON: %v", err)
	}

	if len(config.Endpoints) > 191 {
		log.Fatalf("too many endpoints (max 191, got %d)", len(config.Endpoints))
	}

	writeHosts(config.Endpoints)

	// All bridges run forever — if any returns, something failed.
	var wg sync.WaitGroup

	// Inbound: VSOCK listen → TCP connect (HTTP traffic to Bun)
	wg.Add(1)
	go func() {
		defer wg.Done()
		inboundBridge(config.HTTPVsockPort, config.HTTPTCPPort)
	}()

	// Outbound: TCP listen → VSOCK connect (external services)
	for i, ep := range config.Endpoints {
		ip := fmt.Sprintf("127.0.0.%d", 64+i)
		log.Printf("[traffic] %s → %s:443 → VSOCK:%d:%d", ep.Host, ip, parentCID, ep.VsockPort)

		wg.Add(1)
		go func(localIP string, vsockPort uint32) {
			defer wg.Done()
			outboundForwarder(localIP, vsockPort)
		}(ip, ep.VsockPort)
	}

	log.Println("[traffic] ready")
	wg.Wait()
	log.Println("[traffic] bridge exited unexpectedly")
	os.Exit(1)
}

func writeHosts(endpoints []Endpoint) {
	lines := []string{"127.0.0.1   localhost"}
	for i, ep := range endpoints {
		lines = append(lines, fmt.Sprintf("127.0.0.%d   %s", 64+i, ep.Host))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile("/etc/hosts", []byte(content), 0644); err != nil {
		log.Printf("[traffic] warning: could not write /etc/hosts: %v", err)
	}
}

func inboundBridge(vsockPort uint32, tcpPort uint16) {
	ln, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		log.Fatalf("[traffic] failed to bind VSOCK:%d: %v", vsockPort, err)
	}
	log.Printf("[traffic] inbound VSOCK:%d → TCP:127.0.0.1:%d", vsockPort, tcpPort)

	for {
		vc, err := ln.Accept()
		if err != nil {
			log.Printf("[traffic] inbound accept error: %v", err)
			continue
		}
		go func() {
			if err := bridgeToTCP(vc, tcpPort); err != nil {
				log.Printf("[traffic] inbound bridge error: %v", err)
			}
		}()
	}
}

func bridgeToTCP(src net.Conn, tcpPort uint16) error {
	defer src.Close()

	tcp, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tcpPort))
	if err != nil {
		return fmt.Errorf("TCP dial 127.0.0.1:%d: %w", tcpPort, err)
	}
	defer tcp.Close()

	return copyBidirectional(src, tcp)
}

func outboundForwarder(localIP string, vsockPort uint32) {
	addr := fmt.Sprintf("%s:443", localIP)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[traffic] failed to bind %s: %v", addr, err)
	}

	for {
		tcp, err := ln.Accept()
		if err != nil {
			log.Printf("[traffic] outbound accept error: %v", err)
			continue
		}
		go func() {
			if err := bridgeToVSOCK(tcp, parentCID, vsockPort); err != nil {
				log.Printf("[traffic] outbound bridge error: %v", err)
			}
		}()
	}
}

// --- Shared bidirectional copy with proper half-close ---

func copyBidirectional(a, b net.Conn) error {
	done := make(chan error, 2)

	go func() {
		_, err := io.Copy(b, a)
		closeWrite(b)
		done <- err
	}()

	go func() {
		_, err := io.Copy(a, b)
		closeWrite(a)
		done <- err
	}()

	// Wait for both directions
	err1 := <-done
	err2 := <-done

	if err1 != nil {
		return err1
	}
	return err2
}

// closeWrite calls CloseWrite on connections that support it (TCP and VSOCK).
func closeWrite(c net.Conn) {
	type halfCloser interface {
		CloseWrite() error
	}
	if hc, ok := c.(halfCloser); ok {
		hc.CloseWrite()
	}
}
