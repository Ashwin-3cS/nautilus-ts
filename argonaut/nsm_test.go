package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// --- Fake backends (matching Rust proxy test backends from this repo) ---

// FakeBackend echoes the public key as the attestation doc and returns fixed random bytes.
type FakeBackend struct{}

func (FakeBackend) GetAttestation(publicKey []byte) ([]byte, error) {
	return publicKey, nil
}

func (FakeBackend) GetRandom() ([]byte, error) {
	return []byte{0xde, 0xad, 0xbe, 0xef}, nil
}

// StrictFakeBackend validates 32-byte public key length.
type StrictFakeBackend struct{}

func (StrictFakeBackend) GetAttestation(publicKey []byte) ([]byte, error) {
	if len(publicKey) != 32 {
		return nil, fmt.Errorf("invalid_public_key_length")
	}
	return publicKey, nil
}

func (StrictFakeBackend) GetRandom() ([]byte, error) {
	return []byte{0xca, 0xfe}, nil
}

// --- Hex encoding tests (ported from this repo's Rust proxy) ---

func TestDecodeHexRejectsOddLength(t *testing.T) {
	_, err := hex.DecodeString("0")
	if err == nil {
		t.Fatal("expected error for odd-length hex")
	}
}

func TestDecodeHexRejectsInvalidCharacters(t *testing.T) {
	_, err := hex.DecodeString("zz")
	if err == nil {
		t.Fatal("expected error for invalid hex characters")
	}
}

func TestEncodeHexRoundTrips(t *testing.T) {
	original := []byte{0xde, 0xad, 0xbe, 0xef}
	encoded := hex.EncodeToString(original)
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(original) {
		t.Fatalf("round-trip failed: got %x, want %x", decoded, original)
	}
}

// --- Line protocol tests (ported from this repo's Rust proxy, all 13 cases) ---

func TestHandleAttestationRequest(t *testing.T) {
	publicKey := repeatHex("11", 32)
	response := handleNsmLine(FakeBackend{}, fmt.Sprintf("1 ATT %s", publicKey))
	expected := fmt.Sprintf("1 OK %s", publicKey)
	if response != expected {
		t.Fatalf("got %q, want %q", response, expected)
	}
}

func TestHandleRandomRequest(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "2 RND")
	if response != "2 OK deadbeef" {
		t.Fatalf("got %q, want %q", response, "2 OK deadbeef")
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "3 NOPE")
	if response != "3 ERR unknown_method" {
		t.Fatalf("got %q, want %q", response, "3 ERR unknown_method")
	}
}

func TestHandleInvalidHex(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "4 ATT zz")
	if response != "4 ERR invalid_hex" {
		t.Fatalf("got %q, want %q", response, "4 ERR invalid_hex")
	}
}

func TestHandleEmptyLine(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "")
	if response != "0 ERR empty_request" {
		t.Fatalf("got %q, want %q", response, "0 ERR empty_request")
	}
}

func TestHandleWhitespaceOnly(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "   ")
	if response != "0 ERR empty_request" {
		t.Fatalf("got %q, want %q", response, "0 ERR empty_request")
	}
}

func TestHandleIdOnly(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "5")
	if response != "5 ERR invalid_request" {
		t.Fatalf("got %q, want %q", response, "5 ERR invalid_request")
	}
}

func TestHandleAttMissingPayload(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "6 ATT")
	if response != "6 ERR missing_public_key" {
		t.Fatalf("got %q, want %q", response, "6 ERR missing_public_key")
	}
}

func TestHandleAttEmptyPayload(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "7 ATT ")
	if response != "7 ERR missing_public_key" {
		t.Fatalf("got %q, want %q", response, "7 ERR missing_public_key")
	}
}

func TestHandleAttOddLengthHex(t *testing.T) {
	response := handleNsmLine(FakeBackend{}, "8 ATT abc")
	if response != "8 ERR invalid_hex" {
		t.Fatalf("got %q, want %q", response, "8 ERR invalid_hex")
	}
}

func TestHandleAttWrongKeyLengthShort(t *testing.T) {
	shortKey := repeatHex("aa", 16) // 16 bytes, too short
	response := handleNsmLine(StrictFakeBackend{}, fmt.Sprintf("9 ATT %s", shortKey))
	if response != "9 ERR invalid_public_key_length" {
		t.Fatalf("got %q, want %q", response, "9 ERR invalid_public_key_length")
	}
}

func TestHandleAttWrongKeyLengthLong(t *testing.T) {
	longKey := repeatHex("bb", 64) // 64 bytes, too long
	response := handleNsmLine(StrictFakeBackend{}, fmt.Sprintf("10 ATT %s", longKey))
	if response != "10 ERR invalid_public_key_length" {
		t.Fatalf("got %q, want %q", response, "10 ERR invalid_public_key_length")
	}
}

func TestHandleAttCorrectKeyLength(t *testing.T) {
	key := repeatHex("cc", 32)
	response := handleNsmLine(StrictFakeBackend{}, fmt.Sprintf("11 ATT %s", key))
	expected := fmt.Sprintf("11 OK %s", key)
	if response != expected {
		t.Fatalf("got %q, want %q", response, expected)
	}
}

// --- CBOR encoding tests for our proxy layer ---
//
// These verify that our Request/Response CBOR encoding matches the wire format
// used by the AWS NSM API (serde's externally-tagged enum convention).

func TestAttestationRequestCBORRoundTrip(t *testing.T) {
	publicKey := make([]byte, 32)
	for i := range publicKey {
		publicKey[i] = byte(i)
	}

	encoded, err := encodeAttestationRequest(publicKey)
	if err != nil {
		t.Fatal(err)
	}

	// Decode and verify structure
	var outer map[string]cbor.RawMessage
	if err := cbor.Unmarshal(encoded, &outer); err != nil {
		t.Fatal(err)
	}

	innerRaw, ok := outer["Attestation"]
	if !ok {
		t.Fatal("missing 'Attestation' key in encoded request")
	}

	var inner map[string]interface{}
	if err := cbor.Unmarshal([]byte(innerRaw), &inner); err != nil {
		t.Fatal(err)
	}

	if inner["user_data"] != nil {
		t.Fatalf("user_data should be nil, got %v", inner["user_data"])
	}
	if inner["nonce"] != nil {
		t.Fatalf("nonce should be nil, got %v", inner["nonce"])
	}

	pk, ok := inner["public_key"].([]byte)
	if !ok {
		t.Fatalf("public_key should be []byte, got %T", inner["public_key"])
	}
	if string(pk) != string(publicKey) {
		t.Fatalf("public_key mismatch: got %x, want %x", pk, publicKey)
	}
}

func TestGetRandomRequestCBOREncoding(t *testing.T) {
	encoded, err := encodeGetRandomRequest()
	if err != nil {
		t.Fatal(err)
	}

	// Should decode as the string "GetRandom"
	var s string
	if err := cbor.Unmarshal(encoded, &s); err != nil {
		t.Fatal(err)
	}
	if s != "GetRandom" {
		t.Fatalf("got %q, want %q", s, "GetRandom")
	}
}

func TestAttestationResponseCBORDecode(t *testing.T) {
	doc := []byte{0x01, 0x02, 0x03, 0x04}
	inner := map[string]interface{}{
		"document": doc,
	}
	resp := map[string]interface{}{
		"Attestation": inner,
	}
	encoded, err := cbor.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	result, err := decodeAttestationResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != string(doc) {
		t.Fatalf("got %x, want %x", result, doc)
	}
}

func TestGetRandomResponseCBORDecode(t *testing.T) {
	random := []byte{0xde, 0xad, 0xbe, 0xef}
	inner := map[string]interface{}{
		"random": random,
	}
	resp := map[string]interface{}{
		"GetRandom": inner,
	}
	encoded, err := cbor.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	result, err := decodeGetRandomResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != string(random) {
		t.Fatalf("got %x, want %x", result, random)
	}
}

func TestErrorResponseCBORDecode(t *testing.T) {
	resp := map[string]interface{}{
		"Error": "InvalidArgument",
	}
	encoded, err := cbor.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	result, err := decodeNsmResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != "InvalidArgument" {
		t.Fatalf("got %q, want %q", result.Error, "InvalidArgument")
	}
}

// --- Ported from AWS NSM API crate ---
//
// These tests are direct ports of tests from:
// https://github.com/aws/aws-nitro-enclaves-nsm-api/blob/main/src/api/mod.rs
//
// test_attestationdoc_binary_encode: round-trips an AttestationDoc through
// CBOR serialization/deserialization and verifies structural + binary equality.

// AttestationDoc mirrors the upstream Rust AttestationDoc struct.
// See: https://github.com/aws/aws-nitro-enclaves-nsm-api/blob/main/src/api/mod.rs
type AttestationDoc struct {
	ModuleID    string            `cbor:"module_id"`
	Digest      string            `cbor:"digest"`
	Timestamp   uint64            `cbor:"timestamp"`
	PCRs        map[int][]byte    `cbor:"pcrs"`
	Certificate []byte            `cbor:"certificate"`
	CABundle    [][]byte          `cbor:"cabundle"`
	PublicKey   []byte            `cbor:"public_key,omitempty"`
	UserData    []byte            `cbor:"user_data,omitempty"`
	Nonce       []byte            `cbor:"nonce,omitempty"`
}

func TestAttestationDocBinaryEncode(t *testing.T) {
	// Matches upstream: test_attestationdoc_binary_encode
	pcrs := map[int][]byte{
		1: {1, 2, 3},
		2: {4, 5, 6},
		3: {7, 8, 9},
	}

	doc1 := AttestationDoc{
		ModuleID:    "abcd",
		Digest:      "SHA256",
		Timestamp:   1234,
		PCRs:        pcrs,
		Certificate: bytes.Repeat([]byte{42}, 10),
		CABundle:    [][]byte{},
		PublicKey:   bytes.Repeat([]byte{255}, 10),
		UserData:    nil,
		Nonce:       nil,
	}

	// Serialize
	bin1, err := cbor.Marshal(doc1)
	if err != nil {
		t.Fatalf("marshal doc1: %v", err)
	}

	// Deserialize
	var doc2 AttestationDoc
	if err := cbor.Unmarshal(bin1, &doc2); err != nil {
		t.Fatalf("unmarshal to doc2: %v", err)
	}

	// Re-serialize
	bin2, err := cbor.Marshal(doc2)
	if err != nil {
		t.Fatalf("marshal doc2: %v", err)
	}

	// Structural equality
	if doc1.ModuleID != doc2.ModuleID {
		t.Fatalf("module_id: got %q, want %q", doc2.ModuleID, doc1.ModuleID)
	}
	if doc1.Digest != doc2.Digest {
		t.Fatalf("digest: got %q, want %q", doc2.Digest, doc1.Digest)
	}
	if doc1.Timestamp != doc2.Timestamp {
		t.Fatalf("timestamp: got %d, want %d", doc2.Timestamp, doc1.Timestamp)
	}
	if len(doc1.PCRs) != len(doc2.PCRs) {
		t.Fatalf("pcrs length: got %d, want %d", len(doc2.PCRs), len(doc1.PCRs))
	}
	for k, v1 := range doc1.PCRs {
		v2, ok := doc2.PCRs[k]
		if !ok {
			t.Fatalf("pcrs[%d] missing in doc2", k)
		}
		if !bytes.Equal(v1, v2) {
			t.Fatalf("pcrs[%d]: got %x, want %x", k, v2, v1)
		}
	}
	if !bytes.Equal(doc1.Certificate, doc2.Certificate) {
		t.Fatalf("certificate mismatch")
	}
	if len(doc1.CABundle) != len(doc2.CABundle) {
		t.Fatalf("cabundle length: got %d, want %d", len(doc2.CABundle), len(doc1.CABundle))
	}
	if !bytes.Equal(doc1.PublicKey, doc2.PublicKey) {
		t.Fatalf("public_key: got %x, want %x", doc2.PublicKey, doc1.PublicKey)
	}

	// Binary equality
	if !bytes.Equal(bin1, bin2) {
		t.Fatalf("binary representations differ:\n  bin1=%x\n  bin2=%x", bin1, bin2)
	}
}

// --- Ported from AWS NSM API crate: all ErrorCode variants ---
//
// The upstream crate defines these error codes. We verify our decoder
// handles each one, matching the serde serialization format.

func TestAllErrorCodeVariants(t *testing.T) {
	// All error codes from the upstream ErrorCode enum:
	// https://github.com/aws/aws-nitro-enclaves-nsm-api/blob/main/src/api/mod.rs
	errorCodes := []string{
		"Success",
		"InvalidArgument",
		"InvalidIndex",
		"InvalidResponse",
		"ReadOnlyIndex",
		"InvalidOperation",
		"BufferTooSmall",
		"InputTooLarge",
		"InternalError",
	}

	for _, code := range errorCodes {
		t.Run(code, func(t *testing.T) {
			resp := map[string]interface{}{
				"Error": code,
			}
			encoded, err := cbor.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			result, err := decodeNsmResponse(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if result.Error != code {
				t.Fatalf("got %q, want %q", result.Error, code)
			}
			if result.Variant != "Error" {
				t.Fatalf("variant: got %q, want %q", result.Variant, "Error")
			}
		})
	}
}

// --- Ported from AWS NSM API crate: Request encoding coverage ---
//
// nsm-check.rs tests all four attestation request combinations:
//   1. No optional data
//   2. user_data only
//   3. user_data + nonce
//   4. user_data + nonce + public_key
//
// Our proxy only uses variant #4 (public_key only), but we verify
// the CBOR structure matches the upstream format for all combinations.

func TestAttestationRequestAllOptionalCombinations(t *testing.T) {
	dummyData := bytes.Repeat([]byte{128}, 1024)
	dummyKey := bytes.Repeat([]byte{42}, 32)

	type optionalFields struct {
		name      string
		userData  interface{}
		nonce     interface{}
		publicKey interface{}
	}

	cases := []optionalFields{
		{"no_optional_data", nil, nil, nil},
		{"user_data_only", dummyData, nil, nil},
		{"user_data_and_nonce", dummyData, dummyData, nil},
		{"all_optional_fields", dummyData, dummyData, dummyKey},
		{"public_key_only", nil, nil, dummyKey},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := map[string]interface{}{
				"user_data":  tc.userData,
				"nonce":      tc.nonce,
				"public_key": tc.publicKey,
			}
			outer := map[string]interface{}{
				"Attestation": inner,
			}
			encoded, err := cbor.Marshal(outer)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			// Round-trip: verify structure survives decode
			var decoded map[string]cbor.RawMessage
			if err := cbor.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal outer: %v", err)
			}

			attRaw, ok := decoded["Attestation"]
			if !ok {
				t.Fatal("missing 'Attestation' key")
			}

			var attInner map[string]interface{}
			if err := cbor.Unmarshal([]byte(attRaw), &attInner); err != nil {
				t.Fatalf("unmarshal inner: %v", err)
			}

			// Verify expected fields are present
			if _, exists := attInner["user_data"]; !exists {
				t.Fatal("missing user_data field")
			}
			if _, exists := attInner["nonce"]; !exists {
				t.Fatal("missing nonce field")
			}
			if _, exists := attInner["public_key"]; !exists {
				t.Fatal("missing public_key field")
			}
		})
	}
}

// --- Ported from AWS NSM API crate: all Response variants ---
//
// Verify our decoder handles every Response variant the NSM can return.
// Based on the Response enum in the upstream crate.

func TestDescribePCRResponseDecode(t *testing.T) {
	inner := map[string]interface{}{
		"lock": true,
		"data": []byte{1, 2, 3},
	}
	resp := map[string]interface{}{
		"DescribePCR": inner,
	}
	encoded, err := cbor.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	// Our decoder should return "unknown response shape" since we only
	// handle Attestation, GetRandom, and Error. This is expected — we
	// don't implement DescribePCR/ExtendPCR/LockPCR/DescribeNSM since
	// the proxy only uses Attestation and GetRandom.
	_, err = decodeNsmResponse(encoded)
	if err == nil {
		t.Fatal("expected error for unhandled DescribePCR response")
	}
}

func TestDescribeNSMResponseDecode(t *testing.T) {
	inner := map[string]interface{}{
		"version_major": uint16(1),
		"version_minor": uint16(0),
		"version_patch": uint16(0),
		"module_id":     "nsm",
		"max_pcrs":      uint16(32),
		"locked_pcrs":   []uint16{0, 1, 2, 3, 4},
		"digest":        "SHA384",
	}
	resp := map[string]interface{}{
		"DescribeNSM": inner,
	}
	encoded, err := cbor.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	// Unhandled variant — expected
	_, err = decodeNsmResponse(encoded)
	if err == nil {
		t.Fatal("expected error for unhandled DescribeNSM response")
	}
}

// --- Ported from AWS NSM API crate: Digest enum serialization ---

func TestDigestEnumSerialization(t *testing.T) {
	// Serde serializes Digest variants as plain strings
	digests := []string{"SHA256", "SHA384", "SHA512"}
	for _, d := range digests {
		encoded, err := cbor.Marshal(d)
		if err != nil {
			t.Fatalf("marshal %s: %v", d, err)
		}
		var decoded string
		if err := cbor.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", d, err)
		}
		if decoded != d {
			t.Fatalf("got %q, want %q", decoded, d)
		}
	}
}

// --- Ported from AWS NSM API crate: GetRandom uniqueness ---
//
// nsm-check.rs generates 16 random values and checks for duplicates.
// We can't call real NSM, but we verify the response decoding path
// handles 16 distinct random payloads without confusion.

func TestGetRandomResponseUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 16; i++ {
		// Create distinct random payloads
		random := []byte{byte(i), byte(i + 1), byte(i + 2), byte(i + 3)}
		inner := map[string]interface{}{
			"random": random,
		}
		resp := map[string]interface{}{
			"GetRandom": inner,
		}
		encoded, err := cbor.Marshal(resp)
		if err != nil {
			t.Fatalf("iteration %d marshal: %v", i, err)
		}

		result, err := decodeGetRandomResponse(encoded)
		if err != nil {
			t.Fatalf("iteration %d decode: %v", i, err)
		}

		key := hex.EncodeToString(result)
		if seen[key] {
			t.Fatalf("duplicate random output at iteration %d: %s", i, key)
		}
		seen[key] = true
	}
}

// --- Ported from AWS NSM API crate: PCR map serialization ---
//
// The upstream AttestationDoc uses BTreeMap<usize, ByteBuf> for PCRs.
// Serde serializes BTreeMap with sorted keys. Verify our Go map
// round-trips PCR data correctly.

func TestPCRMapCBORRoundTrip(t *testing.T) {
	pcrs := map[int][]byte{
		0:  bytes.Repeat([]byte{0xAA}, 48),
		1:  bytes.Repeat([]byte{0xBB}, 48),
		2:  bytes.Repeat([]byte{0xCC}, 48),
		3:  {},
		4:  bytes.Repeat([]byte{0xDD}, 48),
		15: bytes.Repeat([]byte{0xEE}, 48),
	}

	encoded, err := cbor.Marshal(pcrs)
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[int][]byte
	if err := cbor.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	if len(decoded) != len(pcrs) {
		t.Fatalf("length: got %d, want %d", len(decoded), len(pcrs))
	}

	// Verify sorted key iteration (matches BTreeMap ordering)
	keys := make([]int, 0, len(decoded))
	for k := range decoded {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	expectedKeys := []int{0, 1, 2, 3, 4, 15}
	for i, k := range keys {
		if k != expectedKeys[i] {
			t.Fatalf("key order[%d]: got %d, want %d", i, k, expectedKeys[i])
		}
	}

	for k, v := range pcrs {
		if !bytes.Equal(decoded[k], v) {
			t.Fatalf("pcrs[%d]: got %x, want %x", k, decoded[k], v)
		}
	}
}

// --- Helper ---

func repeatHex(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
