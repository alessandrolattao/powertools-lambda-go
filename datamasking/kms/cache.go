package kms

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	mpltypes "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygeneratedtypes"
)

// cachingCMM reuses the encryption materials, and so the data key, of a
// backing materials manager: the Go form of the CachingCryptoMaterialsManager
// over a LocalCryptoMaterialsCache that Powertools Python builds, which the
// AWS Encryption SDK for Go does not ship.
//
// It follows the Python SDK: an entry is keyed by the algorithm suite asked
// for and the encryption context, so each context gets a data key of its
// own; an entry serves until it is older than maxAge, or the messages or
// bytes encrypted under it exceed maxMessages or maxBytes (the message that
// would exceed them gets a new key); a plaintext of maxBytes or more is
// never cached; suites without a key derivation function are never cached;
// the least recently used entry goes when the cache is full.
//
// Concurrent encryptions that miss the same entry wait for one request to
// the backing manager instead of each calling KMS.
type cachingCMM struct {
	backing     mpltypes.ICryptographicMaterialsManager
	capacity    int
	maxAge      time.Duration
	maxMessages uint64
	maxBytes    uint64
	now         func() time.Time

	mu       sync.Mutex
	entries  map[string]*cacheEntry
	lru      cacheEntry // sentinel of the ring: most recently used next
	inflight map[string]chan struct{}
}

type cacheEntry struct {
	key        string
	output     mpltypes.GetEncryptionMaterialsOutput
	created    time.Time
	messages   uint64
	bytes      uint64
	prev, next *cacheEntry
}

func newCachingCMM(backing mpltypes.ICryptographicMaterialsManager, c config) *cachingCMM {
	cache := &cachingCMM{
		backing:     backing,
		capacity:    c.cacheCapacity,
		maxAge:      c.maxCacheAge,
		maxMessages: c.maxMessagesEncrypted,
		maxBytes:    c.maxBytesEncrypted,
		now:         c.now,
		entries:     map[string]*cacheEntry{},
		inflight:    map[string]chan struct{}{},
	}
	cache.lru.prev, cache.lru.next = &cache.lru, &cache.lru
	return cache
}

func (c *cachingCMM) GetEncryptionMaterials(in mpltypes.GetEncryptionMaterialsInput) (*mpltypes.GetEncryptionMaterialsOutput, error) {
	// The materials outlive this message: the backing manager must not see
	// its length.
	inner := in
	inner.MaxPlaintextLength = nil
	if !cacheable(in) {
		return c.backing.GetEncryptionMaterials(inner) //nolint:wrapcheck // the SDK classifies the errors of its own materials manager
	}
	length := uint64(*in.MaxPlaintextLength) //nolint:gosec // the SDK passes the length of a slice, never negative
	key := cacheKey(in)
	for {
		c.mu.Lock()
		if out, ok := c.use(key, length); ok {
			c.mu.Unlock()
			return out, nil
		}
		if wait, busy := c.inflight[key]; busy {
			c.mu.Unlock()
			<-wait
			continue
		}
		done := make(chan struct{})
		c.inflight[key] = done
		c.mu.Unlock()
		return c.fill(key, inner, length, done)
	}
}

// fill asks the backing manager for new materials, caches them when they
// may be, and wakes the encryptions waiting for them, even if it panics.
func (c *cachingCMM) fill(key string, in mpltypes.GetEncryptionMaterialsInput, length uint64, done chan struct{}) (
	out *mpltypes.GetEncryptionMaterialsOutput, err error,
) {
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.inflight, key)
		close(done)
		if err == nil && out != nil && safeToCache(out.EncryptionMaterials.AlgorithmSuite) && length < c.maxBytes {
			c.put(key, *out, length)
		}
	}()
	return c.backing.GetEncryptionMaterials(in) //nolint:wrapcheck // the SDK classifies the errors of its own materials manager
}

// DecryptMaterials is not cached: Powertools decrypts with its keys
// directly, calling KMS every time.
func (c *cachingCMM) DecryptMaterials(in mpltypes.DecryptMaterialsInput) (*mpltypes.DecryptMaterialsOutput, error) {
	return c.backing.DecryptMaterials(in) //nolint:wrapcheck // the SDK classifies the errors of its own materials manager
}

// use counts one more message of length bytes against the entry for key
// and returns its materials, or reports that there is no entry fit to use.
// The caller holds mu.
func (c *cachingCMM) use(key string, length uint64) (*mpltypes.GetEncryptionMaterialsOutput, bool) {
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if c.now().Sub(e.created) > c.maxAge {
		c.remove(e)
		return nil, false
	}
	e.messages++
	e.bytes = saturatingAdd(e.bytes, length)
	if e.messages > c.maxMessages || e.bytes > c.maxBytes {
		c.remove(e)
		return nil, false
	}
	c.unlink(e)
	c.pushFront(e)
	out := e.output
	return &out, true
}

// put stores new materials, already used for one message of length bytes.
// The caller holds mu.
func (c *cachingCMM) put(key string, out mpltypes.GetEncryptionMaterialsOutput, length uint64) {
	if e, ok := c.entries[key]; ok {
		c.remove(e)
	}
	for len(c.entries) >= c.capacity {
		c.remove(c.lru.prev) // the least recently used
	}
	e := &cacheEntry{key: key, output: out, created: c.now(), messages: 1, bytes: length}
	c.entries[key] = e
	c.pushFront(e)
}

func (c *cachingCMM) remove(e *cacheEntry) {
	delete(c.entries, e.key)
	c.unlink(e)
}

func (c *cachingCMM) pushFront(e *cacheEntry) {
	e.prev, e.next = &c.lru, c.lru.next
	c.lru.next.prev = e
	c.lru.next = e
}

func (*cachingCMM) unlink(e *cacheEntry) {
	e.prev.next, e.next.prev = e.next, e.prev
	e.prev, e.next = nil, nil
}

// len reports how many entries the cache holds, for the tests.
func (c *cachingCMM) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func saturatingAdd(a, b uint64) uint64 {
	if a > ^uint64(0)-b {
		return ^uint64(0)
	}
	return a + b
}

// cacheable reports whether a request may be served from the cache: the
// plaintext length is known, no required context keys are asked for, and
// the requested suite, if any, derives its data key.
func cacheable(in mpltypes.GetEncryptionMaterialsInput) bool {
	if in.MaxPlaintextLength == nil || *in.MaxPlaintextLength < 0 || len(in.RequiredEncryptionContextKeys) > 0 {
		return false
	}
	switch suite := in.AlgorithmSuiteId.(type) {
	case nil:
		return true
	case *mpltypes.AlgorithmSuiteIdMemberESDK:
		return !noKDF(suite.Value)
	default:
		return false
	}
}

// noKDF reports the suites that use the data key as it is: reusing it
// across messages would reuse the encryption key itself.
func noKDF(id mpltypes.ESDKAlgorithmSuiteId) bool {
	switch id {
	case mpltypes.ESDKAlgorithmSuiteIdAlgAes128GcmIv12Tag16NoKdf,
		mpltypes.ESDKAlgorithmSuiteIdAlgAes192GcmIv12Tag16NoKdf,
		mpltypes.ESDKAlgorithmSuiteIdAlgAes256GcmIv12Tag16NoKdf:
		return true
	default:
		return false
	}
}

// safeToCache is Python's Algorithm.safe_to_cache: the suite derives the
// message key from the data key.
func safeToCache(info mpltypes.AlgorithmSuiteInfo) bool {
	_, identity := info.Kdf.(*mpltypes.DerivationAlgorithmMemberIDENTITY)
	return info.Kdf != nil && !identity
}

// cacheKey identifies the materials a request may share: the suite asked
// for, if any, and the encryption context, each part length-prefixed so
// that no two requests collide.
func cacheKey(in mpltypes.GetEncryptionMaterialsInput) string {
	var b strings.Builder
	suite := ""
	if s, ok := in.AlgorithmSuiteId.(*mpltypes.AlgorithmSuiteIdMemberESDK); ok {
		suite = string(s.Value)
	}
	writePart(&b, suite)
	for _, k := range slices.Sorted(maps.Keys(in.EncryptionContext)) {
		writePart(&b, k)
		writePart(&b, in.EncryptionContext[k])
	}
	return b.String()
}

func writePart(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
}
