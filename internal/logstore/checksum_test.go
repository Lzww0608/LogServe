package logstore

import (
	"bytes"
	"testing"
)

// TestChecksumTypesAgreeWithChunkedPath verifies that every checksum type used by
// the store produces identical values through one-shot and chunked helpers.
func TestChecksumTypesAgreeWithChunkedPath(t *testing.T) {
	payload := bytes.Repeat([]byte("chunked-checksum"), 8<<10)
	for _, typ := range []ChecksumType{ChecksumTypeIEEE, ChecksumTypeCRC32C, ChecksumTypeXXH3, ChecksumTypeNone} {
		t.Run(typ.String(), func(t *testing.T) {
			once, err := checksumOnce(payload, typ)
			if err != nil {
				t.Fatal(err)
			}
			chunked, err := checksumChunked(payload, typ)
			if err != nil {
				t.Fatal(err)
			}
			if once != chunked {
				t.Fatalf("checksum mismatch: once=%d chunked=%d", once, chunked)
			}
			got, err := checksum(payload, typ)
			if err != nil {
				t.Fatal(err)
			}
			if got != once {
				t.Fatalf("checksum() = %d, want %d", got, once)
			}
		})
	}
}

// TestChecksumChunkedMatchesSingleShotForCRC32C covers the large-payload CRC32C
// path used by streaming record verification.
func TestChecksumChunkedMatchesSingleShotForCRC32C(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), checksumChunkSize*2+17)
	single, err := checksumOnce(payload, ChecksumTypeCRC32C)
	if err != nil {
		t.Fatal(err)
	}
	chunked, err := checksumChunked(payload, ChecksumTypeCRC32C)
	if err != nil {
		t.Fatal(err)
	}
	if single != chunked {
		t.Fatalf("CRC32C single=%d chunked=%d", single, chunked)
	}
}

// TestVerifyChecksumDetectsCorruption ensures payload mutation is rejected by
// checksum verification.
func TestVerifyChecksumDetectsCorruption(t *testing.T) {
	payload := []byte("payload-body")
	sum, err := checksum(payload, ChecksumTypeCRC32C)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyChecksum(payload, ChecksumTypeCRC32C, sum) {
		t.Fatal("expected valid checksum")
	}
	corrupt := append([]byte(nil), payload...)
	corrupt[0] ^= 0xff
	if verifyChecksum(corrupt, ChecksumTypeCRC32C, sum) {
		t.Fatal("expected corrupt payload to fail verification")
	}
}

// TestValidateChecksumTypeRejectsUnknown locks down validation for unsupported
// on-disk checksum identifiers.
func TestValidateChecksumTypeRejectsUnknown(t *testing.T) {
	if err := validateChecksumType(ChecksumType(99)); err == nil {
		t.Fatal("expected unsupported checksum type error")
	}
}
