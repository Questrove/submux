package auth

import (
	"testing"
	"time"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !CheckPassword(h, "s3cret") {
		t.Fatalf("correct password rejected")
	}
	if CheckPassword(h, "wrong") {
		t.Fatalf("wrong password accepted")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef")
	tok := IssueSession(secret, time.Hour)
	if !ValidateSession(secret, tok) {
		t.Fatalf("valid session rejected")
	}
	if ValidateSession([]byte("different-secret!"), tok) {
		t.Fatalf("session accepted under wrong secret")
	}
}

func TestSessionExpired(t *testing.T) {
	secret := []byte("0123456789abcdef")
	tok := IssueSession(secret, -time.Minute) // 已过期
	if ValidateSession(secret, tok) {
		t.Fatalf("expired session accepted")
	}
}

func TestSessionTampered(t *testing.T) {
	secret := []byte("0123456789abcdef")
	if ValidateSession(secret, "garbage.deadbeef") {
		t.Fatalf("garbage accepted")
	}
}
