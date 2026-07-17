package agentproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderInstance  = "X-Submux-Instance"
	HeaderTimestamp = "X-Submux-Timestamp"
	HeaderNonce     = "X-Submux-Nonce"
	HeaderSignature = "X-Submux-Signature"
)

func GenerateDeviceKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

func DecodePublicKey(value string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

func SignRequest(request *http.Request, instanceID int64, key ed25519.PrivateKey, body []byte, now time.Time) error {
	if len(key) != ed25519.PrivateKeySize || instanceID <= 0 {
		return errors.New("valid instance ID and Ed25519 private key are required")
	}
	nonceRaw := make([]byte, 16)
	if _, err := rand.Read(nonceRaw); err != nil {
		return err
	}
	timestamp := strconv.FormatInt(now.UTC().Unix(), 10)
	nonce := hex.EncodeToString(nonceRaw)
	instance := strconv.FormatInt(instanceID, 10)
	signature := ed25519.Sign(key, canonicalRequest(request.Method, request.URL.RequestURI(), instance, timestamp, nonce, body))
	request.Header.Set(HeaderInstance, instance)
	request.Header.Set(HeaderTimestamp, timestamp)
	request.Header.Set(HeaderNonce, nonce)
	request.Header.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(signature))
	return nil
}

type VerifyOptions struct {
	Now         time.Time
	MaxSkew     time.Duration
	MaxBodySize int64
	PublicKey   ed25519.PublicKey
	UseNonce    func(instanceID int64, nonce string, expires time.Time) bool
}

func VerifyRequest(request *http.Request, options VerifyOptions) (int64, []byte, error) {
	instanceID, err := strconv.ParseInt(request.Header.Get(HeaderInstance), 10, 64)
	if err != nil || instanceID <= 0 {
		return 0, nil, errors.New("invalid device instance")
	}
	timestampRaw := request.Header.Get(HeaderTimestamp)
	timestampUnix, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil {
		return 0, nil, errors.New("invalid device timestamp")
	}
	if options.MaxSkew <= 0 {
		options.MaxSkew = 2 * time.Minute
	}
	requestTime := time.Unix(timestampUnix, 0)
	if delta := options.Now.Sub(requestTime); delta < -options.MaxSkew || delta > options.MaxSkew {
		return 0, nil, errors.New("device timestamp outside allowed window")
	}
	nonce := request.Header.Get(HeaderNonce)
	if len(nonce) != 32 {
		return 0, nil, errors.New("invalid device nonce")
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return 0, nil, errors.New("invalid device nonce")
	}
	limit := options.MaxBodySize
	if limit <= 0 {
		limit = 1 << 20
	}
	bodyReader := request.Body
	if bodyReader == nil {
		bodyReader = http.NoBody
	}
	body, err := io.ReadAll(io.LimitReader(bodyReader, limit+1))
	if err != nil || int64(len(body)) > limit {
		return 0, nil, errors.New("device request body is too large")
	}
	signature, err := base64.RawURLEncoding.DecodeString(request.Header.Get(HeaderSignature))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return 0, nil, errors.New("invalid device signature")
	}
	if !ed25519.Verify(options.PublicKey, canonicalRequest(request.Method, request.URL.RequestURI(), strconv.FormatInt(instanceID, 10), timestampRaw, nonce, body), signature) {
		return 0, nil, errors.New("device signature verification failed")
	}
	if options.UseNonce != nil && !options.UseNonce(instanceID, nonce, requestTime.Add(options.MaxSkew)) {
		return 0, nil, errors.New("device nonce has already been used")
	}
	return instanceID, body, nil
}

func canonicalRequest(method, requestURI, instance, timestamp, nonce string, body []byte) []byte {
	hash := sha256.Sum256(body)
	return []byte(strings.Join([]string{strings.ToUpper(method), requestURI, instance, timestamp, nonce, fmt.Sprintf("%x", hash[:])}, "\n"))
}
