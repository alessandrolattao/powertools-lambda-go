package kms

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mpltypes "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygeneratedtypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingCMM hands out materials numbered by call, with a key derivation
// function unless identity is set.
type countingCMM struct {
	calls    atomic.Int64
	identity bool
	gate     chan struct{} // when set, each call waits for it
}

func (c *countingCMM) GetEncryptionMaterials(in mpltypes.GetEncryptionMaterialsInput) (*mpltypes.GetEncryptionMaterialsOutput, error) {
	if c.gate != nil {
		<-c.gate
	}
	n := c.calls.Add(1)
	var kdf mpltypes.DerivationAlgorithm = &mpltypes.DerivationAlgorithmMemberHKDF{}
	if c.identity {
		kdf = &mpltypes.DerivationAlgorithmMemberIDENTITY{}
	}
	if in.MaxPlaintextLength != nil {
		panic("the backing manager must not see the plaintext length")
	}
	return &mpltypes.GetEncryptionMaterialsOutput{EncryptionMaterials: mpltypes.EncryptionMaterials{
		AlgorithmSuite:    mpltypes.AlgorithmSuiteInfo{Kdf: kdf},
		PlaintextDataKey:  []byte(strconv.FormatInt(n, 10)),
		EncryptionContext: in.EncryptionContext,
	}}, nil
}

func (*countingCMM) DecryptMaterials(mpltypes.DecryptMaterialsInput) (*mpltypes.DecryptMaterialsOutput, error) {
	return &mpltypes.DecryptMaterialsOutput{}, nil
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestCache(t *testing.T, backing mpltypes.ICryptographicMaterialsManager, opts ...Option) (*cachingCMM, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	cfg, err := newConfig(append([]Option{withClock(clock.Now)}, opts...))
	require.NoError(t, err)
	return newCachingCMM(backing, cfg), clock
}

func request(length int64, ec map[string]string) mpltypes.GetEncryptionMaterialsInput {
	return mpltypes.GetEncryptionMaterialsInput{
		EncryptionContext:  ec,
		CommitmentPolicy:   &mpltypes.CommitmentPolicyMemberESDK{Value: RequireEncryptRequireDecrypt},
		MaxPlaintextLength: &length,
	}
}

// dataKey encrypts one message and returns the number of its data key.
func dataKey(t *testing.T, c *cachingCMM, in mpltypes.GetEncryptionMaterialsInput) string {
	t.Helper()
	out, err := c.GetEncryptionMaterials(in)
	require.NoError(t, err)
	return string(out.EncryptionMaterials.PlaintextDataKey)
}

func TestCacheReusesDataKeys(t *testing.T) {
	t.Parallel()
	backing := &countingCMM{}
	cache, _ := newTestCache(t, backing)
	tenantA := map[string]string{"tenant": "a"}

	first := dataKey(t, cache, request(10, tenantA))
	for range 5 {
		assert.Equal(t, first, dataKey(t, cache, request(10, map[string]string{"tenant": "a"})))
	}
	assert.NotEqual(t, first, dataKey(t, cache, request(10, map[string]string{"tenant": "b"})), "each context has its own key")
	assert.NotEqual(t, first, dataKey(t, cache, request(10, nil)))

	withSuite := request(10, tenantA)
	withSuite.AlgorithmSuiteId = &mpltypes.AlgorithmSuiteIdMemberESDK{Value: AlgAES256GCMHKDFSHA512CommitKey}
	suiteKey := dataKey(t, cache, withSuite)
	assert.NotEqual(t, first, suiteKey, "the suite asked for is part of the key")
	assert.Equal(t, suiteKey, dataKey(t, cache, withSuite))
	assert.Equal(t, int64(4), backing.calls.Load())
}

func TestCacheMaxAge(t *testing.T) {
	t.Parallel()
	backing := &countingCMM{}
	cache, clock := newTestCache(t, backing, WithMaxCacheAge(time.Minute))
	first := dataKey(t, cache, request(1, nil))
	clock.Advance(time.Minute)
	assert.Equal(t, first, dataKey(t, cache, request(1, nil)), "an entry as old as the limit still serves")
	clock.Advance(time.Nanosecond)
	assert.NotEqual(t, first, dataKey(t, cache, request(1, nil)))
}

func TestCacheMaxMessages(t *testing.T) {
	t.Parallel()
	backing := &countingCMM{}
	cache, _ := newTestCache(t, backing, WithMaxMessagesEncrypted(2))
	keys := make([]string, 0, 5)
	for range 5 {
		keys = append(keys, dataKey(t, cache, request(1, nil)))
	}
	assert.Equal(t, []string{"1", "1", "2", "2", "3"}, keys)
}

func TestCacheMaxBytes(t *testing.T) {
	t.Parallel()
	backing := &countingCMM{}
	cache, _ := newTestCache(t, backing, WithMaxBytesEncrypted(2000))
	keys := make([]string, 0, 4)
	for range 4 {
		keys = append(keys, dataKey(t, cache, request(1000, nil)))
	}
	assert.Equal(t, []string{"1", "1", "2", "2"}, keys, "a key serves until its bytes exceed the limit")

	// A message of the limit or more never goes in the cache.
	assert.Equal(t, "3", dataKey(t, cache, request(2000, map[string]string{"big": "1"})))
	assert.Equal(t, "4", dataKey(t, cache, request(2000, map[string]string{"big": "1"})))
}

func TestCacheCapacity(t *testing.T) {
	t.Parallel()
	backing := &countingCMM{}
	cache, _ := newTestCache(t, backing, WithLocalCacheCapacity(2))
	a := dataKey(t, cache, request(1, map[string]string{"k": "a"}))
	b := dataKey(t, cache, request(1, map[string]string{"k": "b"}))
	assert.Equal(t, a, dataKey(t, cache, request(1, map[string]string{"k": "a"})), "a is now the most recently used")
	dataKey(t, cache, request(1, map[string]string{"k": "c"})) // evicts b
	assert.Equal(t, 2, cache.len())
	assert.Equal(t, a, dataKey(t, cache, request(1, map[string]string{"k": "a"})))
	assert.NotEqual(t, b, dataKey(t, cache, request(1, map[string]string{"k": "b"})))
}

func TestCacheSkipsUnsafeRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		identity bool
		in       mpltypes.GetEncryptionMaterialsInput
	}{
		{name: "no key derivation asked", in: func() mpltypes.GetEncryptionMaterialsInput {
			in := request(1, nil)
			in.AlgorithmSuiteId = &mpltypes.AlgorithmSuiteIdMemberESDK{Value: AlgAES256GCMIV12Tag16}
			return in
		}()},
		{name: "no key derivation returned", identity: true, in: request(1, nil)},
		{name: "unknown length", in: mpltypes.GetEncryptionMaterialsInput{}},
		{name: "required context keys", in: func() mpltypes.GetEncryptionMaterialsInput {
			in := request(1, map[string]string{"a": "b"})
			in.RequiredEncryptionContextKeys = []string{"a"}
			return in
		}()},
	}
	for _, tc := range cases {
		backing := &countingCMM{identity: tc.identity}
		cache, _ := newTestCache(t, backing)
		dataKey(t, cache, tc.in)
		dataKey(t, cache, tc.in)
		assert.Equal(t, int64(2), backing.calls.Load(), tc.name)
		assert.Equal(t, 0, cache.len(), tc.name)
	}
}

func TestCacheKeyCannotCollide(t *testing.T) {
	t.Parallel()
	a := cacheKey(request(1, map[string]string{"a:b": "c"}))
	b := cacheKey(request(1, map[string]string{"a": "b:c"}))
	assert.NotEqual(t, a, b)
}

func TestCacheConcurrentMissesShareOneRequest(t *testing.T) {
	t.Parallel()
	backing := &countingCMM{gate: make(chan struct{})}
	cache, _ := newTestCache(t, backing)
	var wg sync.WaitGroup
	keys := make([]string, 8)
	for i := range keys {
		wg.Go(func() {
			out, err := cache.GetEncryptionMaterials(request(1, nil))
			if err == nil {
				keys[i] = string(out.EncryptionMaterials.PlaintextDataKey)
			}
		})
	}
	// Let the goroutines pile up on the first miss, then answer it.
	time.Sleep(50 * time.Millisecond)
	close(backing.gate)
	wg.Wait()
	assert.Equal(t, int64(1), backing.calls.Load())
	for _, k := range keys {
		assert.Equal(t, "1", k)
	}
}

func TestCachePassesDecryptThrough(t *testing.T) {
	t.Parallel()
	cache, _ := newTestCache(t, &countingCMM{})
	_, err := cache.DecryptMaterials(mpltypes.DecryptMaterialsInput{})
	require.NoError(t, err)
}

func TestSaturatingAdd(t *testing.T) {
	t.Parallel()
	assert.Equal(t, uint64(3), saturatingAdd(1, 2))
	assert.Equal(t, ^uint64(0), saturatingAdd(^uint64(0)-1, 5))
}
