package idempotency

import (
	"crypto/md5"  //nolint:gosec // G501: MD5 derives idempotency keys compatible with Powertools Python; it protects nothing
	"crypto/sha1" //nolint:gosec // G505: offered for parity with Python's hashlib names; it protects nothing
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/hex"
	"hash"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/blake2s"
)

// DefaultHashFunction is the hash of idempotency keys and payload validation
// unless WithHashFunction or WithHash says otherwise, as in Powertools
// Python.
const DefaultHashFunction = "md5"

// hashFunctions maps the names Python's hashlib gives its guaranteed
// algorithms to their Go implementations. The shake ones are missing, as in
// Python: their digest needs a length the configuration cannot give.
var hashFunctions = map[string]func() hash.Hash{
	"md5":      md5.New,
	"sha1":     sha1.New,
	"sha224":   sha256.New224,
	"sha256":   sha256.New,
	"sha384":   sha512.New384,
	"sha512":   sha512.New,
	"sha3_224": func() hash.Hash { return sha3.New224() },
	"sha3_256": func() hash.Hash { return sha3.New256() },
	"sha3_384": func() hash.Hash { return sha3.New384() },
	"sha3_512": func() hash.Hash { return sha3.New512() },
	// Python's default digest sizes: 64 bytes for blake2b, 32 for blake2s.
	"blake2b": func() hash.Hash { return mustHash(blake2b.New512(nil)) },
	"blake2s": func() hash.Hash { return mustHash(blake2s.New256(nil)) },
}

// mustHash unwraps the constructors that fail only on an invalid key; a nil
// key is valid.
func mustHash(h hash.Hash, err error) hash.Hash {
	if err != nil {
		panic(err)
	}
	return h
}

// digest returns the lowercase hex digest of data, as hexdigest() does.
func digest(newHash func() hash.Hash, data []byte) string {
	h := newHash()
	_, _ = h.Write(data) // a hash.Hash never fails to write
	return hex.EncodeToString(h.Sum(nil))
}
