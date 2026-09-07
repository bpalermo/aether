package identity

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTrustDomainSeedAndSet pins the two things callers rely on: the seed is
// readable before SPIRE has said anything, and Set reports whether the value
// actually changed — the boolean that decides between one INFO line and a WARN
// plus a listener rebuild.
func TestTrustDomainSeedAndSet(t *testing.T) {
	td := NewTrustDomain("aether.internal")
	assert.Equal(t, "aether.internal", td.Get(), "the seed must be readable immediately")

	assert.False(t, td.Set("aether.internal"), "SPIRE confirming the seed is not a change")
	assert.Equal(t, "aether.internal", td.Get())

	assert.True(t, td.Set("example.org"), "a different trust domain must be reported as a change")
	assert.Equal(t, "example.org", td.Get())
}

// TestTrustDomainZeroValue keeps the holder usable before it is seeded.
func TestTrustDomainZeroValue(t *testing.T) {
	var td TrustDomain
	assert.Equal(t, "", td.Get())
	assert.True(t, td.Set("example.org"), "the first value is always a change")
	assert.Equal(t, "example.org", td.Get())
}

// TestTrustDomainConcurrentAccess is the -race test. The whole reason this
// holder exists is that the value is now written by a goroutine waiting on SPIRE
// while the wiring path reads it; a plain string field would be a data race.
func TestTrustDomainConcurrentAccess(t *testing.T) {
	td := NewTrustDomain("aether.internal")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				assert.NotEmpty(t, td.Get())
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				td.Set("example.org")
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, "example.org", td.Get())
}
