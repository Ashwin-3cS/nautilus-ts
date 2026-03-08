// NSM (Nitro Secure Module) interface for /dev/nsm.
//
// Implements the ioctl interface, CBOR request/response encoding,
// and a line-based stdin/stdout protocol for the TypeScript client.
//
// Line protocol:
//
//	<id> ATT <hex-public-key>  →  <id> OK <hex-attestation-doc>
//	<id> RND                   →  <id> OK <hex-random-bytes>
//	                              <id> ERR <reason>
package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/sys/unix"
)

const (
	nsmDevicePath      = "/dev/nsm"
	nsmRequestMaxSize  = 0x1000 // 4096 bytes
	nsmResponseMaxSize = 0x3000 // 12288 bytes

	// _IOWR(0x0A, 0, 32) on x86_64
	// Direction=ReadWrite(3) << 30 | Size(32) << 16 | Type(0x0A) << 8 | Nr(0)
	nsmIoctlRequest = 0xC0200A00
)

// iovec matches the Linux struct iovec layout on x86_64.
type iovec struct {
	Base uintptr
	Len  uint64
}

// nsmMessage matches the kernel struct nsm_message (two iovecs, 32 bytes total).
type nsmMessage struct {
	Request  iovec
	Response iovec
}

// --- NsmBackend interface for testability ---

// NsmBackend abstracts NSM operations for testing.
type NsmBackend interface {
	GetAttestation(publicKey []byte) ([]byte, error)
	GetRandom() ([]byte, error)
}

// --- NitroBackend: real /dev/nsm ioctl ---

// NitroBackend talks to the real NSM device via ioctl.
type NitroBackend struct {
	fd int
}

// OpenNsm opens /dev/nsm and returns a backend.
func OpenNsm() (*NitroBackend, error) {
	fd, err := unix.Open(nsmDevicePath, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", nsmDevicePath, err)
	}
	return &NitroBackend{fd: fd}, nil
}

// Close releases the file descriptor.
func (n *NitroBackend) Close() {
	unix.Close(n.fd)
}

func (n *NitroBackend) GetAttestation(publicKey []byte) ([]byte, error) {
	if len(publicKey) != 32 {
		return nil, fmt.Errorf("invalid_public_key_length")
	}

	req, err := encodeAttestationRequest(publicKey)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	resp, err := nsmIoctl(n.fd, req)
	if err != nil {
		return nil, err
	}

	return decodeAttestationResponse(resp)
}

func (n *NitroBackend) GetRandom() ([]byte, error) {
	req, err := encodeGetRandomRequest()
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	resp, err := nsmIoctl(n.fd, req)
	if err != nil {
		return nil, err
	}

	return decodeGetRandomResponse(resp)
}

// --- ioctl ---

func nsmIoctl(fd int, request []byte) ([]byte, error) {
	if len(request) > nsmRequestMaxSize {
		return nil, fmt.Errorf("request too large (%d > %d)", len(request), nsmRequestMaxSize)
	}

	response := make([]byte, nsmResponseMaxSize)

	msg := nsmMessage{
		Request: iovec{
			Base: uintptr(unsafe.Pointer(&request[0])),
			Len:  uint64(len(request)),
		},
		Response: iovec{
			Base: uintptr(unsafe.Pointer(&response[0])),
			Len:  uint64(len(response)),
		},
	}

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(nsmIoctlRequest),
		uintptr(unsafe.Pointer(&msg)),
	)

	// Keep buffers alive across the syscall
	runtime.KeepAlive(request)
	runtime.KeepAlive(response)

	if errno != 0 {
		if errno == unix.EMSGSIZE {
			return nil, fmt.Errorf("input too large")
		}
		return nil, fmt.Errorf("ioctl failed: %v", errno)
	}

	return response[:msg.Response.Len], nil
}

// --- CBOR encoding/decoding ---
//
// The AWS NSM API uses serde's externally-tagged enum format:
//   - Unit variants: CBOR text string, e.g. "GetRandom"
//   - Struct variants: CBOR map, e.g. {"Attestation": {"public_key": <bytes>, ...}}

func encodeAttestationRequest(publicKey []byte) ([]byte, error) {
	// {"Attestation": {"user_data": null, "nonce": null, "public_key": <bytes>}}
	inner := map[string]interface{}{
		"user_data":  nil,
		"nonce":      nil,
		"public_key": publicKey,
	}
	outer := map[string]interface{}{
		"Attestation": inner,
	}
	return cbor.Marshal(outer)
}

func encodeGetRandomRequest() ([]byte, error) {
	// Unit variant: just the string "GetRandom"
	return cbor.Marshal("GetRandom")
}

func decodeAttestationResponse(data []byte) ([]byte, error) {
	resp, err := decodeNsmResponse(data)
	if err != nil {
		return nil, err
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("nsm error: %s", resp.Error)
	}
	if resp.Variant != "Attestation" {
		return nil, fmt.Errorf("unexpected response variant: %s", resp.Variant)
	}
	return resp.Document, nil
}

func decodeGetRandomResponse(data []byte) ([]byte, error) {
	resp, err := decodeNsmResponse(data)
	if err != nil {
		return nil, err
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("nsm error: %s", resp.Error)
	}
	if resp.Variant != "GetRandom" {
		return nil, fmt.Errorf("unexpected response variant: %s", resp.Variant)
	}
	return resp.Random, nil
}

type nsmResponse struct {
	Variant  string
	Document []byte // Attestation response
	Random   []byte // GetRandom response
	Error    string // Error response
}

func decodeNsmResponse(data []byte) (*nsmResponse, error) {
	// Try as map first (struct variants and Error)
	var m map[string]cbor.RawMessage
	if err := cbor.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	for key, raw := range m {
		switch key {
		case "Attestation":
			var inner struct {
				Document []byte `cbor:"document"`
			}
			if err := cbor.Unmarshal([]byte(raw), &inner); err != nil {
				return nil, fmt.Errorf("decode attestation: %w", err)
			}
			return &nsmResponse{Variant: "Attestation", Document: inner.Document}, nil

		case "GetRandom":
			var inner struct {
				Random []byte `cbor:"random"`
			}
			if err := cbor.Unmarshal([]byte(raw), &inner); err != nil {
				return nil, fmt.Errorf("decode random: %w", err)
			}
			return &nsmResponse{Variant: "GetRandom", Random: inner.Random}, nil

		case "Error":
			var errCode string
			if err := cbor.Unmarshal([]byte(raw), &errCode); err != nil {
				return nil, fmt.Errorf("decode error: %w", err)
			}
			return &nsmResponse{Variant: "Error", Error: errCode}, nil
		}
	}

	return nil, fmt.Errorf("unknown response shape")
}

// --- Line protocol handler ---

func handleNsmLine(backend NsmBackend, line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "0 ERR empty_request"
	}

	parts := strings.SplitN(trimmed, " ", 3)
	id := parts[0]

	if len(parts) < 2 {
		return fmt.Sprintf("%s ERR invalid_request", id)
	}

	method := parts[1]

	switch method {
	case "ATT":
		hexKey := ""
		if len(parts) == 3 {
			hexKey = strings.TrimSpace(parts[2])
		}
		if hexKey == "" {
			return fmt.Sprintf("%s ERR missing_public_key", id)
		}
		publicKey, err := hex.DecodeString(hexKey)
		if err != nil {
			return fmt.Sprintf("%s ERR invalid_hex", id)
		}
		doc, err := backend.GetAttestation(publicKey)
		if err != nil {
			return fmt.Sprintf("%s ERR %s", id, err.Error())
		}
		return fmt.Sprintf("%s OK %s", id, hex.EncodeToString(doc))

	case "RND":
		random, err := backend.GetRandom()
		if err != nil {
			return fmt.Sprintf("%s ERR %s", id, err.Error())
		}
		return fmt.Sprintf("%s OK %s", id, hex.EncodeToString(random))

	default:
		return fmt.Sprintf("%s ERR unknown_method", id)
	}
}

// --- NSM subcommand entry point ---

func nsmMode() {
	backend, err := OpenNsm()
	if err != nil {
		log.Fatalf("[nsm] %v", err)
	}
	defer backend.Close()

	log.Println("[nsm] ready")

	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	for scanner.Scan() {
		response := handleNsmLine(backend, scanner.Text())
		fmt.Fprintln(writer, response)
		writer.Flush()
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Printf("[nsm] stdin error: %v", err)
	}
}
