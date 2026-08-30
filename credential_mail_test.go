package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"golang.org/x/net/dns/dnsmessage"
)

const credentialTestOperationID = "123e4567-e89b-12d3-a456-426614174000"

//go:embed openpgp-recipient.asc
var credentialTestRecipientCertificate string

func TestCredentialEncryptionRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	entity, _, _, _, _ := credentialFixtureEntity(t, now)
	secret := []byte("fixture-secret-that-must-not-leak\nsecond line")
	inner, err := buildCredentialInnerEntity(secret)
	if err != nil {
		t.Fatal(err)
	}
	armored, err := sealCredentialEntityTo(inner, now, entity, entity)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(armored, secret) || bytes.Contains(armored, []byte("fixture-secret")) {
		t.Fatal("ciphertext contains credential plaintext")
	}
	block, err := armor.Decode(bytes.NewReader(armored))
	if err != nil {
		t.Fatal(err)
	}
	message, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{entity}, nil, &packet.Config{Time: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := io.ReadAll(message.UnverifiedBody)
	if err != nil {
		t.Fatal(err)
	}
	if !message.IsSigned || message.SignatureError != nil || message.Signature == nil {
		t.Fatalf("combined signature was not valid: signed=%v signature=%#v err=%v", message.IsSigned, message.Signature, message.SignatureError)
	}
	if !bytes.Equal(decrypted, inner) {
		t.Fatalf("decrypted entity differs\ngot:  %q\nwant: %q", decrypted, inner)
	}
	encodedBody := decrypted[bytes.Index(decrypted, []byte("\r\n\r\n"))+4:]
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(encodedBody)))
	if err != nil || !bytes.Equal(decoded, secret) {
		t.Fatalf("decoded credential differs: got=%q err=%v", decoded, err)
	}
}

func TestCredentialEncryptionRejectsTrustDriftAndPrivateMaterial(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	entity, certificate, fingerprint, _, keyID := credentialFixtureEntity(t, now)
	for _, test := range []struct {
		name        string
		certificate []byte
		fingerprint string
		recipient   string
		keyID       uint64
	}{
		{name: "fingerprint", certificate: certificate, fingerprint: strings.Repeat("0", 40), recipient: "fixture@example.com", keyID: keyID},
		{name: "recipient UID", certificate: certificate, fingerprint: fingerprint, recipient: "other@example.com", keyID: keyID},
		{name: "encryption key", certificate: certificate, fingerprint: fingerprint, recipient: "fixture@example.com", keyID: keyID + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := credentialRecipientEntity(now, test.certificate, test.fingerprint, test.recipient, test.keyID); err == nil {
				t.Fatal("trust drift was accepted")
			}
		})
	}
	var private bytes.Buffer
	writer, err := armor.Encode(&private, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := entity.SerializePrivate(writer, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialRecipientEntity(now, private.Bytes(), fingerprint, "fixture@example.com", keyID); err == nil || !strings.Contains(err.Error(), "private key material") {
		t.Fatalf("private certificate error = %v", err)
	}
}

func TestCredentialOPENPGPKEYNameUsesRFC7929LocalPartHash(t *testing.T) {
	name, err := credentialOPENPGPKEYName("colin.knapp@boompay.ca")
	if err != nil {
		t.Fatal(err)
	}
	const want = "b80ba3001a716db4b66bb39f1913ba1c3716838a6f505a0f3ceb3391._openpgpkey.boompay.ca."
	if name != want {
		t.Fatalf("OPENPGPKEY name = %q, want %q", name, want)
	}
	caseVariant, err := credentialOPENPGPKEYName("Colin.Knapp@boompay.ca")
	if err != nil {
		t.Fatal(err)
	}
	if caseVariant == name {
		t.Fatal("RFC 7929 local-part was incorrectly lowercased")
	}
}

func TestFetchCredentialRecipientRFC7929RequiresDNSSECSecurePinnedKey(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	key := credentialRFC7929TestKey(t)
	for _, test := range []struct {
		name    string
		secure  bool
		records [][]byte
		wantErr string
	}{
		{name: "secure pinned key", secure: true, records: [][]byte{[]byte("unrelated-record"), key}},
		{name: "insecure answer", secure: false, records: [][]byte{key}, wantErr: "not DNSSEC Secure"},
		{name: "wrong key", secure: true, records: [][]byte{[]byte("unrelated-record")}, wantErr: "lacks the pinned recipient key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			doer := credentialHTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.URL.String() != "https://resolver.test/dns-query" {
					t.Fatalf("unexpected DNS-over-HTTPS request: %s %s", request.Method, request.URL)
				}
				if request.Header.Get("Accept") != "application/dns-message" || request.Header.Get("Content-Type") != "application/dns-message" {
					t.Fatalf("unexpected DNS-over-HTTPS headers: %#v", request.Header)
				}
				requestWire, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				var query dnsmessage.Message
				if err := query.Unpack(requestWire); err != nil {
					t.Fatal(err)
				}
				if !query.Header.RecursionDesired || len(query.Questions) != 1 || query.Questions[0].Type != credentialOPENPGPKEYType {
					t.Fatalf("unexpected DNS question: %#v", query)
				}
				if len(query.Additionals) != 1 || query.Additionals[0].Header.Type != dnsmessage.TypeOPT || query.Additionals[0].Header.TTL&(1<<15) == 0 {
					t.Fatalf("DNSSEC OK bit is missing: %#v", query.Additionals)
				}
				answers := make([]dnsmessage.Resource, 0, len(test.records))
				for _, record := range test.records {
					answers = append(answers, dnsmessage.Resource{
						Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Type: credentialOPENPGPKEYType, Class: dnsmessage.ClassINET, TTL: 300},
						Body:   &dnsmessage.UnknownResource{Type: credentialOPENPGPKEYType, Data: record},
					})
				}
				responseMessage := dnsmessage.Message{
					Header:    dnsmessage.Header{ID: query.Header.ID, Response: true, RecursionAvailable: true, AuthenticData: test.secure, RCode: dnsmessage.RCodeSuccess},
					Questions: query.Questions,
					Answers:   answers,
				}
				responseWire, err := responseMessage.Pack()
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
					Body:       io.NopCloser(bytes.NewReader(responseWire)),
				}, nil
			})
			got, err := fetchCredentialRecipientRFC7929(context.Background(), doer, "https://resolver.test/dns-query", now)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Certificate, key) || got.TTL != 300 {
				t.Fatal("resolver did not return the DNSSEC Secure pinned key")
			}
		})
	}
}

func TestCredentialRFC7929CacheHonorsTTLAndRemembersFailures(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	key := credentialRFC7929TestKey(t)
	t.Run("positive TTL", func(t *testing.T) {
		t.Setenv("ONE_SHOT_STATE_DIR", t.TempDir())
		calls := 0
		fetch := func(time.Time) (credentialDNSKey, error) {
			calls++
			return credentialDNSKey{Certificate: key, TTL: 300}, nil
		}
		for _, checkTime := range []time.Time{now, now.Add(299 * time.Second), now.Add(300 * time.Second)} {
			certificate, err := resolveCredentialRecipientRFC7929Cached(checkTime, false, fetch)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(certificate, key) {
				t.Fatal("cached RFC 7929 key differs")
			}
		}
		if calls != 2 {
			t.Fatalf("DNS fetches = %d, want 2 across a hit and TTL expiry", calls)
		}
		cachePath, err := credentialDNSCachePath()
		if err != nil {
			t.Fatal(err)
		}
		cache, found, err := loadCredentialDNSCache(cachePath)
		if err != nil || !found {
			t.Fatalf("load cache: found=%v err=%v", found, err)
		}
		cache.ExpiresAt = cache.CachedAt.Add(credentialDNSCacheMaxTTL + time.Second)
		if err := saveCredentialDNSCache(cachePath, cache); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveCredentialRecipientRFC7929Cached(now.Add(301*time.Second), false, fetch); err != nil {
			t.Fatal(err)
		}
		if calls != 3 {
			t.Fatalf("tampered expiry was trusted; DNS fetches = %d, want 3", calls)
		}
	})
	t.Run("maximum TTL", func(t *testing.T) {
		t.Setenv("ONE_SHOT_STATE_DIR", t.TempDir())
		calls := 0
		fetch := func(time.Time) (credentialDNSKey, error) {
			calls++
			return credentialDNSKey{Certificate: key, TTL: uint32((48 * time.Hour) / time.Second)}, nil
		}
		for _, checkTime := range []time.Time{now, now.Add(credentialDNSCacheMaxTTL - time.Second), now.Add(credentialDNSCacheMaxTTL)} {
			if _, err := resolveCredentialRecipientRFC7929Cached(checkTime, false, fetch); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 2 {
			t.Fatalf("DNS fetches = %d, want 2 at the cache TTL cap", calls)
		}
	})
	t.Run("failure and forced refresh", func(t *testing.T) {
		t.Setenv("ONE_SHOT_STATE_DIR", t.TempDir())
		calls := 0
		secure := false
		fetch := func(time.Time) (credentialDNSKey, error) {
			calls++
			if !secure {
				return credentialDNSKey{}, errors.New("resolver unavailable")
			}
			return credentialDNSKey{Certificate: key, TTL: 300}, nil
		}
		if _, err := resolveCredentialRecipientRFC7929Cached(now, false, fetch); err == nil || !strings.Contains(err.Error(), "resolver unavailable") {
			t.Fatalf("initial failure = %v", err)
		}
		if _, err := resolveCredentialRecipientRFC7929Cached(now.Add(time.Second), false, fetch); err == nil || !strings.Contains(err.Error(), "suppressed after a recent failure") {
			t.Fatalf("cached failure = %v", err)
		}
		if calls != 1 {
			t.Fatalf("failure cache made %d DNS fetches, want 1", calls)
		}
		secure = true
		certificate, err := resolveCredentialRecipientRFC7929Cached(now.Add(2*time.Second), true, fetch)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(certificate, key) || calls != 2 {
			t.Fatalf("forced refresh calls=%d key_matches=%v", calls, bytes.Equal(certificate, key))
		}
	})
	t.Run("zero TTL failure", func(t *testing.T) {
		t.Setenv("ONE_SHOT_STATE_DIR", t.TempDir())
		calls := 0
		fetch := func(time.Time) (credentialDNSKey, error) {
			calls++
			return credentialDNSKey{Certificate: key, TTL: 0}, nil
		}
		if _, err := resolveCredentialRecipientRFC7929Cached(now, false, fetch); err == nil || !strings.Contains(err.Error(), "zero TTL") {
			t.Fatalf("zero TTL error = %v", err)
		}
		if _, err := resolveCredentialRecipientRFC7929Cached(now.Add(time.Second), false, fetch); err == nil || !strings.Contains(err.Error(), "suppressed after a recent failure") {
			t.Fatalf("cached zero TTL error = %v", err)
		}
		if calls != 1 {
			t.Fatalf("zero TTL failure made %d DNS fetches, want 1", calls)
		}
	})
}

func TestCredentialRFC7929CacheSerializesConcurrentMisses(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", stateDir)
	counterPath := filepath.Join(t.TempDir(), "fetch-count")
	const processes = 4
	commands := make([]*exec.Cmd, 0, processes)
	outputs := make([]bytes.Buffer, processes)
	for i := range processes {
		command := exec.Command(os.Args[0], "-test.run=^TestCredentialRFC7929CacheProcessHelper$")
		command.Env = append(os.Environ(),
			"ONE_SHOT_RFC7929_CACHE_HELPER=1",
			"ONE_SHOT_RFC7929_CACHE_COUNTER="+counterPath,
		)
		command.Stdout = &outputs[i]
		command.Stderr = &outputs[i]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("cache helper failed: %v\n%s", err, outputs[i].String())
		}
	}
	count, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(count) != 1 {
		t.Fatalf("concurrent cache misses made %d DNS fetches, want 1", len(count))
	}
}

func TestCredentialRFC7929CacheProcessHelper(t *testing.T) {
	if os.Getenv("ONE_SHOT_RFC7929_CACHE_HELPER") != "1" {
		return
	}
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	fetch := func(time.Time) (credentialDNSKey, error) {
		counter, err := os.OpenFile(os.Getenv("ONE_SHOT_RFC7929_CACHE_COUNTER"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return credentialDNSKey{}, err
		}
		if _, err := counter.Write([]byte{1}); err != nil {
			_ = counter.Close()
			return credentialDNSKey{}, err
		}
		if err := counter.Close(); err != nil {
			return credentialDNSKey{}, err
		}
		time.Sleep(200 * time.Millisecond)
		return credentialDNSKey{Certificate: credentialRFC7929TestKey(t), TTL: 300}, nil
	}
	if _, err := resolveCredentialRecipientRFC7929Cached(now, false, fetch); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialKeyCheckReportsOnlyPublicMetadata(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	err := credentialKeyCheck(nil, &output, func() time.Time { return now }, func(got time.Time) ([]byte, error) {
		if !got.Equal(now) {
			t.Fatalf("key check time = %s, want %s", got, now)
		}
		return credentialRFC7929TestKey(t), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DNSSEC Secure RFC 7929 OPENPGPKEY", credentialRecipient, credentialFingerprint, fmt.Sprintf("%016X", credentialEncryptionKeyID)} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("key check output misses %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "BEGIN PGP") {
		t.Fatalf("key check leaked certificate data: %s", output.String())
	}
}

func TestProductionCredentialMessageIsPinnedPGPMIME(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	secret := []byte("production-pinning-sentinel")
	inner, err := buildCredentialInnerEntity(secret)
	if err != nil {
		t.Fatal(err)
	}
	armored, err := credentialProductionTestSeal(t, inner, now)
	if err != nil {
		t.Fatal(err)
	}
	wire, ciphertext, err := buildCredentialMessage(credentialTestOperationID, armored, now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, secret) || bytes.Contains(wire, []byte("production-pinning")) {
		t.Fatal("wire message contains credential plaintext")
	}
	metadata, err := validateCredentialMessage(wire)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.OperationID != credentialTestOperationID || metadata.MessageID != credentialMessageID(credentialTestOperationID) {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	sum := sha256.Sum256(ciphertext)
	if metadata.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("ciphertext hash = %q, want %x", metadata.Hash, sum)
	}
	for _, want := range []string{
		"From: " + credentialSender,
		"To: " + credentialRecipient,
		"Subject: " + credentialSubject,
		"multipart/encrypted",
		"application/pgp-encrypted",
	} {
		if !bytes.Contains(wire, []byte(want)) {
			t.Fatalf("wire message misses %q", want)
		}
	}
}

func TestCredentialSendRecordsMetadataAndNeverResubmits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	secret := []byte("send-sentinel-credential")
	transportCalls := 0
	deps := credentialSendDependencies{
		now: func() time.Time { return now },
		seal: func(inner []byte, sealTime time.Time) ([]byte, error) {
			return credentialProductionTestSeal(t, inner, sealTime)
		},
		submit: func(wire []byte, operationID string) error {
			transportCalls++
			if operationID != credentialTestOperationID {
				t.Fatalf("operation ID = %q", operationID)
			}
			if bytes.Contains(wire, secret) {
				t.Fatal("transport received plaintext")
			}
			_, err := validateCredentialMessage(wire)
			return err
		},
		receiptDir: credentialLocalReceiptDir,
	}
	args := []string{"--operation-id", credentialTestOperationID, "--account", "boompay-admin", "--account", "boompay.ca/colin"}
	var output bytes.Buffer
	if err := credentialSend(args, bytes.NewReader(secret), &output, deps); err != nil {
		t.Fatal(err)
	}
	if transportCalls != 1 {
		t.Fatalf("transport calls = %d, want 1", transportCalls)
	}
	if strings.Count(output.String(), "Reminder:") != 1 || strings.Contains(output.String(), string(secret)) {
		t.Fatalf("unexpected output: %q", output.String())
	}
	receiptPath := filepath.Join(dir, "credential-mail", credentialTestOperationID+".json")
	receiptBytes, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	plainHash := sha256.Sum256(secret)
	if bytes.Contains(receiptBytes, secret) || bytes.Contains(receiptBytes, []byte(hex.EncodeToString(plainHash[:]))) {
		t.Fatal("receipt contains plaintext or its hash")
	}
	var receipt credentialReceipt
	if err := json.Unmarshal(receiptBytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "submitted" || receipt.Recipient != credentialRecipient || receipt.KeyFingerprint != credentialFingerprint || receipt.SigningFingerprint != credentialSigningFingerprint || len(receipt.AccountRefs) != 2 {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}
	receiptInfo, err := os.Stat(receiptPath)
	if err != nil || receiptInfo.Mode().Perm() != 0o600 {
		t.Fatalf("receipt stat = %#v err=%v", receiptInfo, err)
	}
	directoryInfo, err := os.Stat(filepath.Dir(receiptPath))
	if err != nil || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("receipt directory stat = %#v err=%v", directoryInfo, err)
	}
	output.Reset()
	if err := credentialSend(args, errorCredentialReader{}, &output, deps); err != nil {
		t.Fatal(err)
	}
	if transportCalls != 1 || !strings.Contains(output.String(), "no transport was attempted") || strings.Contains(output.String(), "Reminder:") {
		t.Fatalf("repeat operation was not idempotent: calls=%d output=%q", transportCalls, output.String())
	}
}

func TestCredentialSendPreservesUnknownOutcomeWithoutRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	secret := []byte("unknown-outcome-sentinel")
	transportCalls := 0
	deps := credentialSendDependencies{
		now: func() time.Time { return now },
		seal: func(inner []byte, sealTime time.Time) ([]byte, error) {
			return credentialProductionTestSeal(t, inner, sealTime)
		},
		submit: func([]byte, string) error {
			transportCalls++
			return &credentialTransportError{state: "unknown", reason: "fixture_timeout", message: "fixture outcome unknown", code: 3}
		},
		receiptDir: credentialLocalReceiptDir,
	}
	args := []string{"--operation-id", credentialTestOperationID, "--account", "boompay-admin"}
	err := credentialSend(args, bytes.NewReader(secret), io.Discard, deps)
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("error = %v, want exit status 3", err)
	}
	receiptPath := filepath.Join(dir, "credential-mail", credentialTestOperationID+".json")
	receipt, found, err := loadCredentialReceipt(receiptPath)
	if err != nil || !found || receipt.State != "unknown" || receipt.Failure != "fixture_timeout" {
		t.Fatalf("unexpected receipt: found=%v receipt=%#v err=%v", found, receipt, err)
	}
	err = credentialSend(args, bytes.NewReader(secret), io.Discard, deps)
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || transportCalls != 1 {
		t.Fatalf("unknown operation retried: calls=%d err=%v", transportCalls, err)
	}
}

func TestCredentialSendRejectsOversizeBeforeEncryptionOrTransport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	sealCalls, transportCalls := 0, 0
	deps := credentialSendDependencies{
		now: func() time.Time { return time.Now().UTC() },
		seal: func([]byte, time.Time) ([]byte, error) {
			sealCalls++
			return nil, errors.New("must not be called")
		},
		submit: func([]byte, string) error {
			transportCalls++
			return nil
		},
		receiptDir: credentialLocalReceiptDir,
	}
	args := []string{"--operation-id", credentialTestOperationID, "--account", "boompay-admin"}
	err := credentialSend(args, bytes.NewReader(bytes.Repeat([]byte("x"), credentialPlaintextLimit+1)), io.Discard, deps)
	if err == nil || sealCalls != 0 || transportCalls != 0 {
		t.Fatalf("oversize input was processed: seal=%d transport=%d err=%v", sealCalls, transportCalls, err)
	}
	receipts, err := filepath.Glob(filepath.Join(dir, "credential-mail", "*.json"))
	if err != nil || len(receipts) != 0 {
		t.Fatalf("oversize input created receipts: %v err=%v", receipts, err)
	}
}

func TestCredentialReceiverSubmitsOnceAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	secret := []byte("receiver-dedupe-sentinel")
	inner, err := buildCredentialInnerEntity(secret)
	if err != nil {
		t.Fatal(err)
	}
	armored, err := credentialProductionTestSeal(t, inner, now)
	if err != nil {
		t.Fatal(err)
	}
	wire, _, err := buildCredentialMessage(credentialTestOperationID, armored, now)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "receiver")
	deliveryCalls := 0
	deps := credentialReceiveDependencies{
		now: func() time.Time { return now },
		stateDir: func() (string, error) {
			return secureCredentialDirectory(stateDir)
		},
		deliver: func(message []byte) (bool, error) {
			deliveryCalls++
			if !bytes.Equal(message, wire) || bytes.Contains(message, secret) {
				t.Fatal("receiver did not pass the exact ciphertext-only message")
			}
			return true, nil
		},
	}
	for i := 0; i < 2; i++ {
		var output bytes.Buffer
		if err := credentialReceive(nil, bytes.NewReader(wire), &output, deps); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(output.String()) != "submitted: "+credentialTestOperationID {
			t.Fatalf("receiver output = %q", output.String())
		}
	}
	if deliveryCalls != 1 {
		t.Fatalf("receiver delivery calls = %d, want 1", deliveryCalls)
	}
	differentInner, err := buildCredentialInnerEntity([]byte("different-ciphertext-same-operation"))
	if err != nil {
		t.Fatal(err)
	}
	differentArmor, err := credentialProductionTestSeal(t, differentInner, now)
	if err != nil {
		t.Fatal(err)
	}
	differentWire, _, err := buildCredentialMessage(credentialTestOperationID, differentArmor, now)
	if err != nil {
		t.Fatal(err)
	}
	err = credentialReceive(nil, bytes.NewReader(differentWire), io.Discard, deps)
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || deliveryCalls != 1 {
		t.Fatalf("receiver accepted an operation ID with different ciphertext: calls=%d err=%v", deliveryCalls, err)
	}
	receiptBytes, err := os.ReadFile(filepath.Join(stateDir, credentialTestOperationID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(receiptBytes, secret) || bytes.Contains(receiptBytes, []byte("account_refs")) {
		t.Fatal("receiver receipt contains plaintext or local account metadata")
	}
}

func TestCredentialReceiverRejectsOuterLeaksAndOtherRecipientKeys(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	inner, err := buildCredentialInnerEntity([]byte("header-leak-sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	armored, err := credentialProductionTestSeal(t, inner, now)
	if err != nil {
		t.Fatal(err)
	}
	wire, _, err := buildCredentialMessage(credentialTestOperationID, armored, now)
	if err != nil {
		t.Fatal(err)
	}
	leaky := bytes.Replace(wire, []byte("To: "+credentialRecipient+"\r\n"), []byte("To: "+credentialRecipient+"\r\nX-Secret: header-leak-sentinel\r\n"), 1)
	if _, err := validateCredentialMessage(leaky); err == nil {
		t.Fatal("receiver accepted an unapproved plaintext outer header")
	}
	fixtureEntity, _, _, _, _ := credentialFixtureEntity(t, now)
	fixtureInner, err := buildCredentialInnerEntity([]byte("wrong-key"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureArmor, err := sealCredentialEntityTo(fixtureInner, now, fixtureEntity, fixtureEntity)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialArmor(fixtureArmor); err == nil || !strings.Contains(err.Error(), "pinned recipient") {
		t.Fatalf("wrong recipient armor error = %v", err)
	}
}

func TestCredentialReceiverRejectsLegacyUnauthenticatedEncryption(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(credentialRFC7929TestKey(t)))
	if err != nil || len(entities) != 1 {
		t.Fatalf("recipient certificate: entities=%d err=%v", len(entities), err)
	}
	key, ok := entities[0].EncryptionKey(now)
	if !ok {
		t.Fatal("recipient certificate has no encryption key")
	}
	var packets bytes.Buffer
	if err := packet.SerializeEncryptedKey(&packets, key.PublicKey, packet.CipherAES256, bytes.Repeat([]byte{0x42}, 32), &packet.Config{Time: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	packets.Write([]byte{0xc9, 0x01, 0x00}) // New-format tag 9: encrypted data without integrity protection.
	var encoded bytes.Buffer
	writer, err := armor.Encode(&encoded, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(packets.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	err = validateCredentialArmor(canonicalCRLF(bytes.TrimSpace(encoded.Bytes())))
	if err == nil || !strings.Contains(err.Error(), "integrity protected") {
		t.Fatalf("legacy encryption error = %v", err)
	}
}

func TestCredentialTransportArgumentsAreFixed(t *testing.T) {
	privateKey, knownHosts, sshArgs := credentialSSHArguments("/fixture/home")
	if privateKey != "/fixture/home/.config/one-shot-tally/credential-mail_ed25519" || knownHosts != "/fixture/home/.ssh/known_hosts" {
		t.Fatalf("unexpected SSH paths: private=%q known_hosts=%q", privateKey, knownHosts)
	}
	wantSSH := []string{
		"-F", "/dev/null", "-T", "-oBatchMode=yes", "-oIdentitiesOnly=yes", "-oIdentityAgent=none",
		"-oPreferredAuthentications=publickey", "-oPubkeyAuthentication=yes",
		"-oPasswordAuthentication=no", "-oKbdInteractiveAuthentication=no", "-oCertificateFile=none",
		"-oStrictHostKeyChecking=yes", "-oUserKnownHostsFile=/fixture/home/.ssh/known_hosts",
		"-oGlobalKnownHostsFile=/dev/null", "-oHostKeyAlias=89.117.56.210",
		"-oConnectTimeout=10", "-oConnectionAttempts=1", "-oClearAllForwardings=yes",
		"-oPermitLocalCommand=no", "-oControlMaster=no", "-oControlPath=none", "-oControlPersist=no",
		"-i", privateKey,
		"root@box.p.nixc.us", "/usr/local/bin/one-shot-tally credential receive",
	}
	if !reflect.DeepEqual(sshArgs, wantSSH) {
		t.Fatalf("SSH args = %#v, want %#v", sshArgs, wantSSH)
	}
	wantSendmail := []string{"-i", "-f", "colin@nixc.us", "colin.knapp@boompay.ca"}
	if got := credentialSendmailArguments(); !reflect.DeepEqual(got, wantSendmail) {
		t.Fatalf("sendmail args = %#v, want %#v", got, wantSendmail)
	}
	wantGPG := []string{
		"--no-options", "--homedir", "/fixture/home/.gnupg",
		"--batch", "--yes", "--no-tty", "--pinentry-mode", "error",
		"--no-auto-key-retrieve", "--auto-key-locate", "clear", "--trust-model", "always",
		"--local-user", credentialSigningFingerprint + "!",
		"--recipient-file", "/fixture/recipient.pgp",
		"--digest-algo", "SHA256", "--cipher-algo", "AES256", "--compress-algo", "none",
		"--armor", "--output", "-", "--sign", "--encrypt",
	}
	if got := credentialGPGArguments("/fixture/home", "/fixture/recipient.pgp"); !reflect.DeepEqual(got, wantGPG) {
		t.Fatalf("GnuPG args = %#v, want %#v", got, wantGPG)
	}
}

func TestCredentialTransportRejectsCompanionCertificate(t *testing.T) {
	dir := t.TempDir()
	privateKey := filepath.Join(dir, "credential-mail_ed25519")
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(privateKey, []byte("fixture-private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHosts, []byte("fixture-known-host"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialSSHFiles(privateKey, knownHosts); err != nil {
		t.Fatalf("safe SSH files rejected: %v", err)
	}
	if err := os.WriteFile(privateKey+"-cert.pub", []byte("fixture-certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialSSHFiles(privateKey, knownHosts); err == nil || !strings.Contains(err.Error(), "companion SSH certificate") {
		t.Fatalf("companion certificate error = %v", err)
	}
}

func TestCredentialGPGCombinedSigningWhenKeyIsAvailable(t *testing.T) {
	if _, err := os.Stat(credentialGPGBinary); err != nil {
		t.Skip("pinned GnuPG binary is not installed")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("home directory is unavailable")
	}
	probe := exec.Command(credentialGPGBinary, "--no-options", "--homedir", filepath.Join(home, ".gnupg"), "--batch", "--list-secret-keys", credentialSigningFingerprint)
	probe.Stdout = io.Discard
	probe.Stderr = io.Discard
	if err := probe.Run(); err != nil {
		t.Skip("pinned signing key is not available")
	}
	secret := []byte("NON_SECRET_SIGNED_ENCRYPTED_ACCEPTANCE")
	inner, err := buildCredentialInnerEntity(secret)
	if err != nil {
		t.Fatal(err)
	}
	armored, err := signAndEncryptCredentialGPGWithRecipient(inner, time.Now().UTC(), credentialRFC7929TestKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(armored, secret) {
		t.Fatal("signed ciphertext contains plaintext")
	}
	decrypt := exec.Command(credentialGPGBinary, "--no-options", "--homedir", filepath.Join(home, ".gnupg"), "--batch", "--no-tty", "--status-fd", "2", "--decrypt")
	decrypt.Stdin = bytes.NewReader(armored)
	var decrypted, status bytes.Buffer
	decrypt.Stdout = &decrypted
	decrypt.Stderr = &status
	if err := decrypt.Run(); err != nil {
		t.Fatalf("GnuPG decrypt and verify failed: %v\n%s", err, status.String())
	}
	if !bytes.Equal(decrypted.Bytes(), inner) {
		t.Fatalf("decrypted inner MIME entity differs: got=%q want=%q", decrypted.Bytes(), inner)
	}
	for _, marker := range []string{"[GNUPG:] DECRYPTION_OKAY", "[GNUPG:] GOODSIG", "[GNUPG:] VALIDSIG " + credentialSigningFingerprint} {
		if !strings.Contains(status.String(), marker) {
			t.Fatalf("GnuPG status misses %q:\n%s", marker, status.String())
		}
	}
}

func credentialFixtureEntity(t *testing.T, now time.Time) (*openpgp.Entity, []byte, string, string, uint64) {
	t.Helper()
	config := &packet.Config{RSABits: 2048, Time: func() time.Time { return now }}
	entity, err := openpgp.NewEntity("Fixture", "", "fixture@example.com", config)
	if err != nil {
		t.Fatal(err)
	}
	var public bytes.Buffer
	writer, err := armor.Encode(&public, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := entity.Serialize(writer); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	key, ok := entity.EncryptionKey(now)
	if !ok {
		t.Fatal("fixture has no encryption key")
	}
	fingerprint := strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint))
	signingKey, ok := entity.SigningKey(now)
	if !ok {
		t.Fatal("fixture has no signing key")
	}
	signingFingerprint := strings.ToUpper(hex.EncodeToString(signingKey.PublicKey.Fingerprint))
	return entity, public.Bytes(), fingerprint, signingFingerprint, key.PublicKey.KeyId
}

func credentialRFC7929TestKey(t *testing.T) []byte {
	t.Helper()
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(credentialTestRecipientCertificate))
	if err != nil || len(entities) != 1 {
		t.Fatalf("test recipient certificate: entities=%d err=%v", len(entities), err)
	}
	entity := entities[0]
	entity.Subkeys = nil
	for name, identity := range entity.Identities {
		if identity.UserId == nil || identity.UserId.Email != credentialRecipient {
			delete(entity.Identities, name)
		}
	}
	var binary bytes.Buffer
	if err := entity.Serialize(&binary); err != nil {
		t.Fatal(err)
	}
	return binary.Bytes()
}

func credentialProductionTestSeal(t *testing.T, inner []byte, now time.Time) ([]byte, error) {
	t.Helper()
	recipient, err := credentialRecipientEntity(now, credentialRFC7929TestKey(t), credentialFingerprint, credentialRecipient, credentialEncryptionKeyID)
	if err != nil {
		return nil, err
	}
	return sealCredentialEntityTo(inner, now, recipient, nil)
}

type credentialHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f credentialHTTPDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

type errorCredentialReader struct{}

func (errorCredentialReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("credential input should not be read for an existing operation")
}
