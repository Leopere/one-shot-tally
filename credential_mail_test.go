package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

const credentialTestOperationID = "123e4567-e89b-12d3-a456-426614174000"

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
	entity, certificate, fingerprint, signingFingerprint, keyID := credentialFixtureEntity(t, now)
	for _, test := range []struct {
		name        string
		certificate string
		fingerprint string
		signing     string
		recipient   string
		keyID       uint64
	}{
		{name: "fingerprint", certificate: certificate, fingerprint: strings.Repeat("0", 40), signing: signingFingerprint, recipient: "fixture@example.com", keyID: keyID},
		{name: "signing key", certificate: certificate, fingerprint: fingerprint, signing: strings.Repeat("0", 40), recipient: "fixture@example.com", keyID: keyID},
		{name: "recipient UID", certificate: certificate, fingerprint: fingerprint, signing: signingFingerprint, recipient: "other@example.com", keyID: keyID},
		{name: "encryption key", certificate: certificate, fingerprint: fingerprint, signing: signingFingerprint, recipient: "fixture@example.com", keyID: keyID + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := credentialRecipientEntity(now, test.certificate, test.fingerprint, test.signing, test.recipient, test.keyID); err == nil {
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
	if _, err := credentialRecipientEntity(now, private.String(), fingerprint, signingFingerprint, "fixture@example.com", keyID); err == nil || !strings.Contains(err.Error(), "private key material") {
		t.Fatalf("private certificate error = %v", err)
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
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(credentialRecipientCertificate))
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
		"-T", "-oBatchMode=yes", "-oIdentitiesOnly=yes", "-oStrictHostKeyChecking=yes",
		"-oUserKnownHostsFile=/fixture/home/.ssh/known_hosts", "-oConnectTimeout=10", "-oConnectionAttempts=1",
		"-oClearAllForwardings=yes", "-oPermitLocalCommand=no", "-i", privateKey,
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
		"--recipient", credentialSigningFingerprint + "!",
		"--digest-algo", "SHA256", "--cipher-algo", "AES256", "--compress-algo", "none",
		"--armor", "--output", "-", "--sign", "--encrypt",
	}
	if got := credentialGPGArguments("/fixture/home"); !reflect.DeepEqual(got, wantGPG) {
		t.Fatalf("GnuPG args = %#v, want %#v", got, wantGPG)
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
	armored, err := signAndEncryptCredentialGPG(inner, time.Now().UTC())
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

func credentialFixtureEntity(t *testing.T, now time.Time) (*openpgp.Entity, string, string, string, uint64) {
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
	return entity, public.String(), fingerprint, signingFingerprint, key.PublicKey.KeyId
}

func credentialProductionTestSeal(t *testing.T, inner []byte, now time.Time) ([]byte, error) {
	t.Helper()
	recipient, err := credentialRecipientEntity(now, credentialRecipientCertificate, credentialFingerprint, credentialSigningFingerprint, credentialRecipient, credentialEncryptionKeyID)
	if err != nil {
		return nil, err
	}
	return sealCredentialEntityTo(inner, now, recipient, nil)
}

type errorCredentialReader struct{}

func (errorCredentialReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("credential input should not be read for an existing operation")
}
