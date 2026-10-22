package authx

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPassword(t *testing.T) {
	if err := CheckPasswordPolicy("short"); err == nil {
		t.Fatal("short")
	}
	if err := CheckPasswordPolicy(" padded12"); err == nil {
		t.Fatal("padded")
	}
	if err := CheckPasswordPolicy("password123"); err != nil {
		t.Fatal(err)
	}
	h, err := HashPassword("password123")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPasswordHash(h, "password123") || VerifyPasswordHash(h, "password124") || VerifyPasswordHash("garbage", "x") {
		t.Fatal("verify")
	}
	// the seed hash of the project's dev data
	seed := "$argon2id$v=19$m=65536,t=3,p=2$n8fDUJXvEkuyIISKz3Gp1A$TvRvIIk4VOGR+dNBaPhDqHPFBVNWvb11V6UGsrGGLeE"
	if !VerifyPasswordHash(seed, "admin123456") {
		t.Fatal("seed")
	}
	if VerifyPasswordHash(DummyHash, "dummy-password-for-constant-time") != true || VerifyPasswordHash(DummyHash, "x") {
		t.Fatal("dummy")
	}
}

// Concurrent hashing waits for a slot: never more than cap(hashSlots) argon2
// computations at once, and every caller still gets its answer.
func TestHashingIsBounded(t *testing.T) {
	h, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var peak, now int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				if n := int32(len(hashSlots)); n > atomic.LoadInt32(&peak) {
					atomic.StoreInt32(&peak, n)
				}
			}
		}
	}()
	ok := int32(0)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			atomic.AddInt32(&now, 1)
			if VerifyPasswordHash(h, "correct-horse") {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	close(stop)
	if ok != 8 || peak > int32(cap(hashSlots)) {
		t.Fatalf("ok %d, peak %d", ok, peak)
	}
	// a stored hash asking for more memory than the cap never matches (and is not computed)
	if VerifyPasswordHash(strings.Replace(h, "m=65536", "m=4194304", 1), "correct-horse") {
		t.Fatal("oversized memory parameter accepted")
	}
}
