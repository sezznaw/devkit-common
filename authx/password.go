package authx

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password hashing for every account service (members, admins, agents):
// argon2id in the PHC string format, so the parameters travel with the
// hash and can be raised later without touching old rows.
//
// 密码散列，所有账号服务（会员、管理员、代理）共用：argon2id，PHC 字符串格式，
// 参数随散列一起存，以后提高参数不用动旧数据。

// MinPasswordLen is the policy: at least this many characters.
const MinPasswordLen = 8

// ErrWeakPassword: the password does not meet the policy.
var ErrWeakPassword = errors.New("password does not meet the policy")

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// CheckPasswordPolicy: at least MinPasswordLen characters, no leading or
// trailing spaces.
func CheckPasswordPolicy(pw string) error {
	if len(pw) < MinPasswordLen || strings.TrimSpace(pw) != pw {
		return ErrWeakPassword
	}
	return nil
}

// HashPassword returns the argon2id PHC string of pw with a fresh salt.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPasswordHash reports whether pw matches the PHC string, using the
// parameters stored in it. A malformed string never matches.
func VerifyPasswordHash(phc, pw string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyHash is a hash of a random password: verify against it when the
// account does not exist, so an unknown username costs the same time as a
// wrong password and the two cannot be told apart.
var DummyHash = func() string {
	h, _ := HashPassword("dummy-password-for-constant-time")
	return h
}()
