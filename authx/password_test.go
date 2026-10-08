package authx

import "testing"

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
