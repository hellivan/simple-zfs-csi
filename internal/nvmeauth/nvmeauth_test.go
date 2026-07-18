package nvmeauth

import (
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
)

func TestHostIdentity_DeterministicAndUnique(t *testing.T) {
	nqn1, id1 := HostIdentity("node-a", "pvc-1")
	nqn1b, id1b := HostIdentity("node-a", "pvc-1")
	if nqn1 != nqn1b || id1 != id1b {
		t.Errorf("HostIdentity not deterministic: %q/%q vs %q/%q", nqn1, id1, nqn1b, id1b)
	}
	if !strings.HasPrefix(nqn1, "nqn.2014-08.org.nvmexpress:uuid:") {
		t.Errorf("unexpected NQN format: %q", nqn1)
	}
	if !strings.HasSuffix(nqn1, id1) {
		t.Errorf("NQN %q should end with host id %q", nqn1, id1)
	}

	// Different volume or node -> different identity.
	nqn2, _ := HostIdentity("node-a", "pvc-2")
	nqn3, _ := HostIdentity("node-b", "pvc-1")
	if nqn1 == nqn2 || nqn1 == nqn3 {
		t.Errorf("host identity should differ per (node, volume): %q %q %q", nqn1, nqn2, nqn3)
	}
}

func TestGenerateDHChapKey_Format(t *testing.T) {
	key, err := GenerateDHChapKey()
	if err != nil {
		t.Fatalf("GenerateDHChapKey: %v", err)
	}
	if !strings.HasPrefix(key, "DHHC-1:00:") || !strings.HasSuffix(key, ":") {
		t.Fatalf("unexpected DHHC-1 format: %q", key)
	}
	b64 := strings.TrimSuffix(strings.TrimPrefix(key, "DHHC-1:00:"), ":")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("key payload is not base64: %v", err)
	}
	if len(raw) != 36 {
		t.Fatalf("payload len = %d, want 36 (32 secret + 4 crc)", len(raw))
	}
	secret, gotCRC := raw[:32], binary.LittleEndian.Uint32(raw[32:])
	if want := crc32.ChecksumIEEE(secret); gotCRC != want {
		t.Errorf("crc = %x, want %x", gotCRC, want)
	}

	// Two calls should not collide.
	key2, _ := GenerateDHChapKey()
	if key == key2 {
		t.Errorf("expected distinct keys across calls")
	}
}

func TestResolveSecretKey(t *testing.T) {
	if got := ResolveSecretKey(""); got != SecretKeyDHChap {
		t.Errorf("ResolveSecretKey(empty) = %q, want %q", got, SecretKeyDHChap)
	}
	if got := ResolveSecretKey("custom"); got != "custom" {
		t.Errorf("ResolveSecretKey(custom) = %q, want custom", got)
	}
}
