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
	// argonMaxMemory caps the memory parameter read from a stored hash (KiB):
	// one odd row must not cost the process gigabytes.
	argonMaxMemory = 256 * 1024
)

// hashSlots bounds the argon2 computations running at once in a process.
// Each takes argonMemory (64 MiB): a burst of logins hashed side by side
// outgrew the 512 MiB of an account service and killed it (2026-10-21, a
// dev test with a few dozen concurrent logins). Two at a time cap the hashing
// at 128 MiB; the others wait their turn (about 40 checks a second per
// replica, plenty for logins and registrations).
//
// hashSlots 限制一个进程里同时进行的 argon2 计算：每次 64 MiB，一波并发登录同时算会撑爆账号服务的 512 MiB
// （2026-10-21 dev 测试时 OOMKilled）。同时最多 2 个，哈希内存封顶 128 MiB，其余排队（每副本约每秒 40 次）。
var hashSlots = make(chan struct{}, 2)

func argonKey(pw, salt []byte, t, m uint32, p uint8, n uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(pw, salt, t, m, p, n)
}

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
	key := argonKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
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
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m > argonMaxMemory || t > 10 || p == 0 {
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
	got := argonKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyHash is a hash of a random password: verify against it when the
// account does not exist, so an unknown username costs the same time as a
// wrong password and the two cannot be told apart.
var DummyHash = func() string {
	h, _ := HashPassword("dummy-password-for-constant-time")
	return h
}()
