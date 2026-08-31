package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"golang.org/x/sys/unix"
)

const (
	credentialSender                = "colin@nixc.us"
	credentialRecipient             = "colin.knapp@boompay.ca"
	credentialSubject               = "OpenPGP credential delivery"
	credentialFingerprint           = "6183F2DE176E9D46EDB602951B7D7262C3D0207D"
	credentialEncryptionFingerprint = "9E5310E1F125CC2696E2C0385FE016062B506A77"
	credentialSigningFingerprint    = "33EA65A9C078126556C150E1EA43219BE7B419F1"
	credentialEncryptionKeyID       = uint64(0x5FE016062B506A77)
	credentialGPGBinary             = "/opt/homebrew/bin/gpg"
	credentialSSHHost               = "box.p.nixc.us"
	credentialSSHHostKeyAlias       = "89.117.56.210"
	credentialSSHUser               = "root"
	credentialReceiverStateDir      = "/var/lib/one-shot-tally-openpgp-mail"
	credentialPlaintextLimit        = 64 * 1024
	credentialWireLimit             = 256 * 1024
	credentialReceiptLimit          = 32 * 1024
	credentialTransportTimeout      = 30 * time.Second
	credentialWKDLookupTimeout      = 15 * time.Second
	credentialWKDCleanupTimeout     = 3 * time.Second
	credentialWKDCacheTTL           = time.Hour
	credentialWKDFailureCacheTTL    = 5 * time.Minute
	credentialChannel               = "openpgp-gnupg-wkd-pgp-mime-signed-encrypted-ssh"
	credentialPrivateKeyFilename    = "credential-mail_ed25519"
	credentialKeyCacheFilename      = "wkd-openpgpkey-cache.json"
	credentialKeyCacheVersion       = 2
)

var (
	credentialOperationIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	credentialAccountRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:/+~-]{0,127}$`)
)

type credentialReceipt struct {
	OperationID           string    `json:"operation_id"`
	MessageID             string    `json:"message_id"`
	State                 string    `json:"state"`
	Sender                string    `json:"sender"`
	Recipient             string    `json:"recipient"`
	Channel               string    `json:"channel"`
	KeyFingerprint        string    `json:"key_fingerprint"`
	EncryptionFingerprint string    `json:"encryption_fingerprint"`
	SigningFingerprint    string    `json:"signing_fingerprint"`
	CiphertextSHA256      string    `json:"ciphertext_sha256"`
	CiphertextBytes       int       `json:"ciphertext_bytes"`
	AccountRefs           []string  `json:"account_refs,omitempty"`
	Failure               string    `json:"failure,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type credentialExitError struct {
	message string
	code    int
}

func (e *credentialExitError) Error() string { return e.message }
func (e *credentialExitError) ExitCode() int { return e.code }

type credentialTransportError struct {
	state   string
	reason  string
	message string
	code    int
}

func (e *credentialTransportError) Error() string { return e.message }

type credentialSendDependencies struct {
	now        func() time.Time
	seal       func([]byte, time.Time) ([]byte, error)
	submit     func([]byte, string) error
	receiptDir func() (string, error)
}

type credentialReceiveDependencies struct {
	now      func() time.Time
	stateDir func() (string, error)
	deliver  func([]byte) (bool, error)
}

type credentialMessageMetadata struct {
	OperationID string
	MessageID   string
	Ciphertext  []byte
	Hash        string
}

type credentialKeyCache struct {
	Version               int       `json:"version"`
	Recipient             string    `json:"recipient"`
	State                 string    `json:"state"`
	KeyFingerprint        string    `json:"key_fingerprint"`
	EncryptionFingerprint string    `json:"encryption_fingerprint"`
	EncryptionKeyID       string    `json:"encryption_key_id"`
	Certificate           string    `json:"certificate,omitempty"`
	CachedAt              time.Time `json:"cached_at"`
	ExpiresAt             time.Time `json:"expires_at"`
}

type credentialAccounts []string

func (a *credentialAccounts) String() string { return strings.Join(*a, ",") }

func (a *credentialAccounts) Set(value string) error {
	if !credentialAccountRE.MatchString(value) {
		return errors.New("account reference must be a non-secret label using letters, numbers, or @._:/+~-")
	}
	if len(*a) >= 16 {
		return errors.New("at most 16 account references are allowed")
	}
	*a = append(*a, value)
	return nil
}

func credentialCommand(args []string, r io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: one-shot-tally credential <key-check|send|receive>")
	}
	switch args[0] {
	case "key-check":
		return credentialKeyCheck(args[1:], w, time.Now, refreshCredentialRecipientWKD)
	case "send":
		return credentialSend(args[1:], r, w, credentialSendDependencies{
			now:        time.Now,
			seal:       signAndEncryptCredentialGPG,
			submit:     submitCredentialSSH,
			receiptDir: credentialLocalReceiptDir,
		})
	case "receive":
		return credentialReceive(args[1:], r, w, credentialReceiveDependencies{
			now: time.Now,
			stateDir: func() (string, error) {
				return secureCredentialDirectory(credentialReceiverStateDir)
			},
			deliver: deliverCredentialSendmail,
		})
	default:
		return errors.New("usage: one-shot-tally credential <key-check|send|receive>")
	}
}

func credentialKeyCheck(args []string, w io.Writer, now func() time.Time, resolve func(time.Time) ([]byte, error)) error {
	if len(args) != 0 {
		return errors.New("usage: one-shot-tally credential key-check")
	}
	checkTime := now().UTC()
	certificate, err := resolve(checkTime)
	if err != nil {
		return fmt.Errorf("GnuPG WKD key check failed: %w", err)
	}
	if _, err := credentialRecipientEntity(checkTime, certificate, credentialFingerprint, credentialRecipient, credentialEncryptionFingerprint, credentialEncryptionKeyID); err != nil {
		return fmt.Errorf("GnuPG WKD key check failed: %w", err)
	}
	fmt.Fprintln(w, "GnuPG WKD lookup (--auto-key-locate clear,wkd)")
	fmt.Fprintf(w, "recipient %s; primary fingerprint %s; encryption fingerprint %s; encryption key %016X\n", credentialRecipient, credentialFingerprint, credentialEncryptionFingerprint, credentialEncryptionKeyID)
	return nil
}

func credentialSend(args []string, r io.Reader, w io.Writer, deps credentialSendDependencies) error {
	fs := flag.NewFlagSet("credential send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var operationID string
	var accounts credentialAccounts
	fs.StringVar(&operationID, "operation-id", "", "idempotency UUID")
	fs.Var(&accounts, "account", "non-secret account reference (repeatable)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: one-shot-tally credential send --operation-id UUID --account REF")
	}
	operationID = strings.ToLower(strings.TrimSpace(operationID))
	if !credentialOperationIDRE.MatchString(operationID) {
		return errors.New("--operation-id must be a UUID")
	}
	if len(accounts) == 0 {
		return errors.New("at least one non-secret --account reference is required")
	}
	dir, err := deps.receiptDir()
	if err != nil {
		return fmt.Errorf("prepare credential receipt store: %w", err)
	}
	receiptPath := filepath.Join(dir, operationID+".json")
	unlock, err := acquireCredentialOperationLock(receiptPath)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	if existing, found, err := loadCredentialReceipt(receiptPath); err != nil {
		return fmt.Errorf("read credential receipt: %w", err)
	} else if found {
		if existing.OperationID != operationID {
			return errors.New("credential receipt operation ID mismatch; not submitted")
		}
		if existing.State == "submitted" {
			fmt.Fprintf(w, "already submitted %s to %s; no transport was attempted\n", operationID, credentialRecipient)
			return nil
		}
		return &credentialExitError{message: fmt.Sprintf("operation %s is already %s; no automatic retry was attempted", operationID, existing.State), code: 3}
	}
	plaintext, err := readCredentialInput(r, credentialPlaintextLimit, "credential plaintext")
	if err != nil {
		return err
	}
	defer wipeBytes(plaintext)
	now := deps.now().UTC()
	innerEntity, err := buildCredentialInnerEntity(plaintext)
	if err != nil {
		return err
	}
	defer wipeBytes(innerEntity)
	armored, err := deps.seal(innerEntity, now)
	if err != nil {
		return fmt.Errorf("sign and encrypt credential plaintext: %w", err)
	}
	wire, ciphertext, err := buildCredentialMessage(operationID, armored, now)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(ciphertext)
	receipt := credentialReceipt{
		OperationID:           operationID,
		MessageID:             credentialMessageID(operationID),
		State:                 "pending",
		Sender:                credentialSender,
		Recipient:             credentialRecipient,
		Channel:               credentialChannel,
		KeyFingerprint:        credentialFingerprint,
		EncryptionFingerprint: credentialEncryptionFingerprint,
		SigningFingerprint:    credentialSigningFingerprint,
		CiphertextSHA256:      hex.EncodeToString(sum[:]),
		CiphertextBytes:       len(ciphertext),
		AccountRefs:           append([]string(nil), accounts...),
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := saveCredentialReceipt(receiptPath, receipt); err != nil {
		return fmt.Errorf("record pending credential delivery: %w", err)
	}
	if err := deps.submit(wire, operationID); err != nil {
		transport := &credentialTransportError{state: "unknown", reason: "transport_unknown", message: "credential delivery outcome is unknown; no automatic retry was attempted", code: 3}
		var classified *credentialTransportError
		if errors.As(err, &classified) {
			transport = classified
		}
		receipt.State = transport.state
		receipt.Failure = transport.reason
		receipt.UpdatedAt = deps.now().UTC()
		if saveErr := saveCredentialReceipt(receiptPath, receipt); saveErr != nil {
			return &credentialExitError{message: "credential delivery outcome is unknown and its receipt could not be finalized; no automatic retry was attempted", code: 3}
		}
		return &credentialExitError{message: transport.message, code: transport.code}
	}
	receipt.State = "submitted"
	receipt.UpdatedAt = deps.now().UTC()
	if err := saveCredentialReceipt(receiptPath, receipt); err != nil {
		return &credentialExitError{message: "credential was submitted, but its receipt could not be finalized; do not retry this operation ID", code: 3}
	}
	fmt.Fprintf(w, "submitted %s to %s; signed by %s and encrypted to %s\n", operationID, credentialRecipient, credentialSigningFingerprint, credentialEncryptionFingerprint)
	fmt.Fprintf(w, "metadata-only receipt: %s\n", receiptPath)
	fmt.Fprintln(w, "Reminder: rotate consequential credentials after they have served their purpose.")
	return nil
}

func credentialReceive(args []string, r io.Reader, w io.Writer, deps credentialReceiveDependencies) error {
	if len(args) != 0 {
		return errors.New("usage: one-shot-tally credential receive")
	}
	wire, err := readCredentialInput(r, credentialWireLimit, "encrypted credential message")
	if err != nil {
		return err
	}
	metadata, err := validateCredentialMessage(wire)
	if err != nil {
		return fmt.Errorf("reject encrypted credential message: %w", err)
	}
	dir, err := deps.stateDir()
	if err != nil {
		return errors.New("prepare receiver receipt store")
	}
	receiptPath := filepath.Join(dir, metadata.OperationID+".json")
	unlock, err := acquireCredentialOperationLock(receiptPath)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	if existing, found, err := loadCredentialReceipt(receiptPath); err != nil {
		return errors.New("read receiver receipt")
	} else if found {
		if existing.OperationID == metadata.OperationID && existing.CiphertextSHA256 == metadata.Hash && existing.State == "submitted" {
			fmt.Fprintf(w, "submitted: %s\n", metadata.OperationID)
			return nil
		}
		return &credentialExitError{message: "operation already exists in a non-final state; no delivery was attempted", code: 3}
	}
	now := deps.now().UTC()
	receipt := credentialReceipt{
		OperationID:           metadata.OperationID,
		MessageID:             metadata.MessageID,
		State:                 "pending",
		Sender:                credentialSender,
		Recipient:             credentialRecipient,
		Channel:               credentialChannel,
		KeyFingerprint:        credentialFingerprint,
		EncryptionFingerprint: credentialEncryptionFingerprint,
		SigningFingerprint:    credentialSigningFingerprint,
		CiphertextSHA256:      metadata.Hash,
		CiphertextBytes:       len(metadata.Ciphertext),
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := saveCredentialReceipt(receiptPath, receipt); err != nil {
		return errors.New("record pending receiver receipt")
	}
	started, deliveryErr := deps.deliver(wire)
	if deliveryErr != nil {
		if started {
			receipt.State = "unknown"
			receipt.Failure = "sendmail_outcome_unknown"
		} else {
			receipt.State = "failed"
			receipt.Failure = "sendmail_not_started"
		}
		receipt.UpdatedAt = deps.now().UTC()
		if saveErr := saveCredentialReceipt(receiptPath, receipt); saveErr != nil {
			return &credentialExitError{message: "receiver outcome is unknown and its receipt could not be finalized", code: 3}
		}
		if started {
			return &credentialExitError{message: "receiver delivery outcome is unknown; no automatic retry was attempted", code: 3}
		}
		return &credentialExitError{message: "receiver did not start sendmail; no delivery was attempted", code: 1}
	}
	receipt.State = "submitted"
	receipt.UpdatedAt = deps.now().UTC()
	if err := saveCredentialReceipt(receiptPath, receipt); err != nil {
		return &credentialExitError{message: "receiver submitted the message, but its receipt could not be finalized", code: 3}
	}
	fmt.Fprintf(w, "submitted: %s\n", metadata.OperationID)
	return nil
}

func readCredentialInput(r io.Reader, limit int64, label string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s", label)
	}
	if int64(len(b)) > limit {
		wipeBytes(b)
		return nil, fmt.Errorf("%s exceeds %d bytes", label, limit)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%s is empty", label)
	}
	return b, nil
}

func buildCredentialInnerEntity(plaintext []byte) ([]byte, error) {
	if !utf8.Valid(plaintext) {
		return nil, errors.New("credential plaintext must be UTF-8 text")
	}
	if bytes.IndexByte(plaintext, 0) >= 0 {
		return nil, errors.New("credential plaintext must not contain NUL bytes")
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(plaintext)))
	base64.StdEncoding.Encode(encoded, plaintext)
	defer wipeBytes(encoded)
	entity := []byte("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	for len(encoded) > 0 {
		lineLength := minInt(76, len(encoded))
		entity = append(entity, encoded[:lineLength]...)
		entity = append(entity, '\r', '\n')
		encoded = encoded[lineLength:]
	}
	return entity, nil
}

func resolveCredentialRecipientWKD(now time.Time) ([]byte, error) {
	return resolveCredentialRecipientWKDCached(now, false, lookupCredentialRecipientWKD)
}

func refreshCredentialRecipientWKD(now time.Time) ([]byte, error) {
	return resolveCredentialRecipientWKDCached(now, true, lookupCredentialRecipientWKD)
}

func lookupCredentialRecipientWKD(now time.Time) ([]byte, error) {
	resolvedBinary, err := credentialValidatedGPGBinary()
	if err != nil {
		return nil, err
	}
	gpgconfBinary, err := credentialValidatedGPGConfBinary(resolvedBinary)
	if err != nil {
		return nil, err
	}
	// Keep this name short enough for GnuPG's Unix-domain agent socket paths.
	gnupgHome, err := os.MkdirTemp("", "ost-wkd-*")
	if err != nil {
		return nil, errors.New("prepare isolated GnuPG WKD home")
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), credentialWKDCleanupTimeout)
		defer cancel()
		cleanup := exec.CommandContext(cleanupCtx, gpgconfBinary, credentialGPGConfArguments(gnupgHome)...)
		_ = cleanup.Run()
		_ = os.RemoveAll(gnupgHome)
	}()
	if err := os.Chmod(gnupgHome, 0o700); err != nil {
		return nil, errors.New("secure isolated GnuPG WKD home")
	}
	diagnosticFile, err := os.CreateTemp(gnupgHome, "wkd-lookup-*.log")
	if err != nil {
		return nil, errors.New("prepare GnuPG WKD diagnostic")
	}
	defer func() { _ = diagnosticFile.Close() }()
	if err := diagnosticFile.Chmod(0o600); err != nil {
		return nil, errors.New("secure GnuPG WKD diagnostic")
	}

	ctx, cancel := context.WithTimeout(context.Background(), credentialWKDLookupTimeout)
	defer cancel()
	locate := exec.CommandContext(ctx, resolvedBinary, credentialWKDLocateArguments(gnupgHome)...)
	locate.Stderr = diagnosticFile
	if err := locate.Run(); err != nil || ctx.Err() != nil {
		return nil, credentialGPGDiagnosticError("GnuPG WKD lookup failed", err, ctx.Err(), diagnosticFile, gnupgHome)
	}

	export := exec.CommandContext(ctx, resolvedBinary, credentialWKDExportArguments(gnupgHome)...)
	var certificate credentialBoundedBuffer
	certificate.limit = credentialReceiptLimit
	export.Stdout = &certificate
	export.Stderr = diagnosticFile
	if err := export.Run(); err != nil || ctx.Err() != nil || len(certificate.Bytes()) == 0 {
		return nil, credentialGPGDiagnosticError("GnuPG could not export the pinned WKD key", err, ctx.Err(), diagnosticFile, gnupgHome)
	}
	result := append([]byte(nil), certificate.Bytes()...)
	if _, err := credentialRecipientEntity(now, result, credentialFingerprint, credentialRecipient, credentialEncryptionFingerprint, credentialEncryptionKeyID); err != nil {
		return nil, errors.New("GnuPG WKD result lacks the pinned recipient key")
	}
	return result, nil
}

func credentialGPGDiagnosticError(label string, commandErr, contextErr error, file *os.File, home string) error {
	if contextErr != nil {
		return fmt.Errorf("%s: timed out", label)
	}
	if commandErr == nil {
		commandErr = errors.New("no key data")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%s: %v", label, commandErr)
	}
	b, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return fmt.Errorf("%s: %v", label, commandErr)
	}
	diagnostic := strings.ReplaceAll(string(b), home, "<isolated-home>")
	diagnostic = strings.Join(strings.Fields(diagnostic), " ")
	if diagnostic == "" {
		return fmt.Errorf("%s: %v", label, commandErr)
	}
	return fmt.Errorf("%s: %s", label, diagnostic)
}

func resolveCredentialRecipientWKDCached(now time.Time, refresh bool, fetch func(time.Time) ([]byte, error)) ([]byte, error) {
	cachePath, err := credentialKeyCachePath()
	if err != nil {
		return nil, fmt.Errorf("prepare WKD key cache: %w", err)
	}
	unlock, err := lockCredentialKeyCache(cachePath)
	if err != nil {
		return nil, fmt.Errorf("lock WKD key cache: %w", err)
	}
	defer func() { _ = unlock() }()

	if !refresh {
		cache, found, err := loadCredentialKeyCache(cachePath)
		if err != nil {
			return nil, fmt.Errorf("read WKD key cache: %w", err)
		}
		if found && credentialKeyCacheMatches(cache) && !now.Before(cache.CachedAt) && now.Before(cache.ExpiresAt) {
			switch cache.State {
			case "valid":
				certificate, err := base64.StdEncoding.DecodeString(cache.Certificate)
				if err != nil {
					return nil, errors.New("WKD key cache is invalid; run credential key-check")
				}
				if _, err := credentialRecipientEntity(now, certificate, credentialFingerprint, credentialRecipient, credentialEncryptionFingerprint, credentialEncryptionKeyID); err != nil {
					return nil, errors.New("WKD key cache is invalid; run credential key-check")
				}
				return certificate, nil
			case "failure":
				return nil, fmt.Errorf("WKD lookup is suppressed after a recent failure until %s; run credential key-check to refresh now", cache.ExpiresAt.UTC().Format(time.RFC3339))
			}
		}
	}

	certificate, lookupErr := fetch(now)
	if lookupErr != nil {
		if err := saveCredentialKeyFailure(cachePath, now); err != nil {
			return nil, fmt.Errorf("%w; could not persist WKD failure cache: %v", lookupErr, err)
		}
		return nil, lookupErr
	}
	if _, err := credentialRecipientEntity(now, certificate, credentialFingerprint, credentialRecipient, credentialEncryptionFingerprint, credentialEncryptionKeyID); err != nil {
		lookupErr = errors.New("GnuPG WKD result lacks the pinned recipient key")
		if saveErr := saveCredentialKeyFailure(cachePath, now); saveErr != nil {
			return nil, fmt.Errorf("%w; could not persist WKD failure cache: %v", lookupErr, saveErr)
		}
		return nil, lookupErr
	}
	cache := newCredentialKeyCache("valid", certificate, now, now.Add(credentialWKDCacheTTL))
	if err := saveCredentialKeyCache(cachePath, cache); err != nil {
		return nil, fmt.Errorf("persist validated WKD key cache: %w", err)
	}
	return append([]byte(nil), certificate...), nil
}

func credentialKeyCachePath() (string, error) {
	dir, err := credentialLocalReceiptDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, credentialKeyCacheFilename), nil
}

func newCredentialKeyCache(state string, certificate []byte, cachedAt, expiresAt time.Time) credentialKeyCache {
	return credentialKeyCache{
		Version:               credentialKeyCacheVersion,
		Recipient:             credentialRecipient,
		State:                 state,
		KeyFingerprint:        credentialFingerprint,
		EncryptionFingerprint: credentialEncryptionFingerprint,
		EncryptionKeyID:       fmt.Sprintf("%016X", credentialEncryptionKeyID),
		Certificate:           base64.StdEncoding.EncodeToString(certificate),
		CachedAt:              cachedAt.UTC(),
		ExpiresAt:             expiresAt.UTC(),
	}
}

func credentialKeyCacheMatches(cache credentialKeyCache) bool {
	if cache.Version != credentialKeyCacheVersion ||
		cache.Recipient != credentialRecipient ||
		cache.KeyFingerprint != credentialFingerprint ||
		cache.EncryptionFingerprint != credentialEncryptionFingerprint ||
		cache.EncryptionKeyID != fmt.Sprintf("%016X", credentialEncryptionKeyID) {
		return false
	}
	lifetime := cache.ExpiresAt.Sub(cache.CachedAt)
	if lifetime <= 0 {
		return false
	}
	switch cache.State {
	case "valid":
		return cache.Certificate != "" && lifetime <= credentialWKDCacheTTL
	case "failure":
		return cache.Certificate == "" && lifetime <= credentialWKDFailureCacheTTL
	default:
		return false
	}
}

func saveCredentialKeyFailure(path string, now time.Time) error {
	cache := newCredentialKeyCache("failure", nil, now, now.Add(credentialWKDFailureCacheTTL))
	return saveCredentialKeyCache(path, cache)
}

func lockCredentialKeyCache(path string) (func() error, error) {
	lockPath := path + ".lock"
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open cache lock")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, errors.New("cache lock is not a private regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() error {
		unlockErr := unix.Flock(fd, unix.LOCK_UN)
		closeErr := file.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}

func loadCredentialKeyCache(path string) (credentialKeyCache, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return credentialKeyCache{}, false, nil
	}
	if err != nil {
		return credentialKeyCache{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return credentialKeyCache{}, false, errors.New("cache is not a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return credentialKeyCache{}, false, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, credentialReceiptLimit+1))
	if err != nil || len(b) == 0 || len(b) > credentialReceiptLimit {
		return credentialKeyCache{}, false, errors.New("cache is invalid")
	}
	var cache credentialKeyCache
	if err := json.Unmarshal(b, &cache); err != nil {
		return credentialKeyCache{}, false, errors.New("cache is invalid")
	}
	return cache, true, nil
}

func saveCredentialKeyCache(path string, cache credentialKeyCache) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("cache target is not a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := json.Marshal(cache)
	if err != nil || len(b) > credentialReceiptLimit {
		return errors.New("cache is invalid")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".wkd-key-cache-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(b); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func credentialRecipientEntity(now time.Time, certificate []byte, fingerprint, recipient, encryptionFingerprint string, encryptionKeyID uint64) (*openpgp.Entity, error) {
	var entities openpgp.EntityList
	var err error
	if bytes.HasPrefix(certificate, []byte("-----BEGIN PGP ")) {
		entities, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(certificate))
	} else {
		entities, err = openpgp.ReadKeyRing(bytes.NewReader(certificate))
	}
	if err != nil || len(entities) != 1 {
		return nil, errors.New("recipient certificate is invalid")
	}
	entity := entities[0]
	if entity.PrivateKey != nil {
		return nil, errors.New("recipient certificate must not contain private key material")
	}
	for _, subkey := range entity.Subkeys {
		if subkey.PrivateKey != nil {
			return nil, errors.New("recipient certificate must not contain private key material")
		}
	}
	if strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint)) != fingerprint {
		return nil, errors.New("recipient certificate fingerprint mismatch")
	}
	validRecipientUID := false
	for _, identity := range entity.Identities {
		if identity.UserId == nil || identity.UserId.Email != recipient {
			continue
		}
		if identity.SelfSignature != nil && !identity.Revoked(now) && !identity.SelfSignature.SigExpired(now) {
			validRecipientUID = true
		}
	}
	if !validRecipientUID {
		return nil, errors.New("recipient certificate lacks a valid target email identity")
	}
	encryptionKey, ok := entity.EncryptionKey(now)
	if !ok || encryptionKey.PublicKey == nil || encryptionKey.PublicKey.KeyId != encryptionKeyID || strings.ToUpper(hex.EncodeToString(encryptionKey.PublicKey.Fingerprint)) != encryptionFingerprint {
		return nil, errors.New("recipient certificate lacks the pinned encryption key")
	}
	return entity, nil
}

func sealCredentialEntityTo(inner []byte, now time.Time, recipient, signer *openpgp.Entity) ([]byte, error) {
	if len(inner) == 0 || !hasCanonicalCRLF(inner) {
		return nil, errors.New("inner MIME entity is not canonical")
	}
	var armored bytes.Buffer
	armorWriter, err := armor.Encode(&armored, "PGP MESSAGE", nil)
	if err != nil {
		return nil, err
	}
	config := &packet.Config{DefaultHash: crypto.SHA256, DefaultCipher: packet.CipherAES256, Time: func() time.Time { return now }}
	plainWriter, err := openpgp.Encrypt(armorWriter, []*openpgp.Entity{recipient}, signer, &openpgp.FileHints{IsBinary: true, FileName: "credentials.eml", ModTime: now}, config)
	if err != nil {
		_ = armorWriter.Close()
		return nil, err
	}
	if _, err := plainWriter.Write(inner); err != nil {
		_ = plainWriter.Close()
		_ = armorWriter.Close()
		return nil, err
	}
	if err := plainWriter.Close(); err != nil {
		_ = armorWriter.Close()
		return nil, err
	}
	if err := armorWriter.Close(); err != nil {
		return nil, err
	}
	return canonicalCRLF(bytes.TrimSpace(armored.Bytes())), nil
}

func signAndEncryptCredentialGPG(inner []byte, now time.Time) ([]byte, error) {
	certificate, err := resolveCredentialRecipientWKD(now)
	if err != nil {
		return nil, fmt.Errorf("obtain GnuPG WKD recipient key: %w", err)
	}
	return signAndEncryptCredentialGPGWithRecipient(inner, now, certificate)
}

func signAndEncryptCredentialGPGWithRecipient(inner []byte, now time.Time, certificate []byte) ([]byte, error) {
	if _, err := credentialRecipientEntity(now, certificate, credentialFingerprint, credentialRecipient, credentialEncryptionFingerprint, credentialEncryptionKeyID); err != nil {
		return nil, err
	}
	if len(inner) == 0 || !hasCanonicalCRLF(inner) {
		return nil, errors.New("inner MIME entity is not canonical")
	}
	resolvedBinary, err := credentialValidatedGPGBinary()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("GnuPG home is unavailable")
	}
	gnupgHome := filepath.Join(home, ".gnupg")
	homeInfo, err := os.Lstat(gnupgHome)
	if err != nil || homeInfo.Mode()&os.ModeSymlink != 0 || !homeInfo.IsDir() || homeInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("GnuPG home is unavailable or has unsafe permissions")
	}
	recipientFile, err := os.CreateTemp("", "one-shot-tally-openpgpkey-*.pgp")
	if err != nil {
		return nil, errors.New("prepare WKD recipient key for GnuPG")
	}
	recipientPath := recipientFile.Name()
	defer func() { _ = os.Remove(recipientPath) }()
	if err := recipientFile.Chmod(0o600); err != nil {
		_ = recipientFile.Close()
		return nil, errors.New("secure WKD recipient key file")
	}
	if _, err := recipientFile.Write(certificate); err != nil {
		_ = recipientFile.Close()
		return nil, errors.New("write WKD recipient key file")
	}
	if err := recipientFile.Close(); err != nil {
		return nil, errors.New("close WKD recipient key file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialTransportTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, resolvedBinary, credentialGPGArguments(home, recipientPath)...)
	command.Stdin = bytes.NewReader(inner)
	var armored credentialBoundedBuffer
	armored.limit = credentialWireLimit
	command.Stdout = &armored
	if err := command.Run(); err != nil || ctx.Err() != nil {
		return nil, errors.New("GnuPG could not sign and encrypt with the pinned key")
	}
	result := canonicalCRLF(bytes.TrimSpace(armored.Bytes()))
	if err := validateCredentialArmor(result); err != nil {
		return nil, err
	}
	return result, nil
}

func buildCredentialMessage(operationID string, armored []byte, now time.Time) ([]byte, []byte, error) {
	if !credentialOperationIDRE.MatchString(operationID) {
		return nil, nil, errors.New("invalid credential operation ID")
	}
	ciphertext := canonicalCRLF(bytes.TrimSpace(armored))
	if err := validateCredentialArmor(ciphertext); err != nil {
		return nil, nil, err
	}
	boundary := credentialBoundary(operationID)
	var wire bytes.Buffer
	fmt.Fprintf(&wire, "From: %s\r\n", credentialSender)
	fmt.Fprintf(&wire, "To: %s\r\n", credentialRecipient)
	fmt.Fprintf(&wire, "Subject: %s\r\n", credentialSubject)
	fmt.Fprintf(&wire, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&wire, "Message-ID: %s\r\n", credentialMessageID(operationID))
	fmt.Fprint(&wire, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&wire, "X-One-Shot-Tally-Operation-ID: %s\r\n", operationID)
	fmt.Fprintf(&wire, "Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=\"%s\"\r\n\r\n", boundary)
	fmt.Fprintf(&wire, "--%s\r\n", boundary)
	fmt.Fprint(&wire, "Content-Type: application/pgp-encrypted\r\nContent-Transfer-Encoding: 7bit\r\n\r\nVersion: 1\r\n")
	fmt.Fprintf(&wire, "--%s\r\n", boundary)
	fmt.Fprint(&wire, "Content-Type: application/octet-stream; name=\"encrypted.asc\"\r\nContent-Disposition: inline; filename=\"encrypted.asc\"\r\nContent-Transfer-Encoding: 7bit\r\n\r\n")
	wire.Write(ciphertext)
	fmt.Fprintf(&wire, "\r\n--%s--\r\n", boundary)
	if wire.Len() > credentialWireLimit {
		return nil, nil, errors.New("encrypted credential message exceeds transport limit")
	}
	return wire.Bytes(), ciphertext, nil
}

func validateCredentialMessage(wire []byte) (credentialMessageMetadata, error) {
	if len(wire) == 0 || len(wire) > credentialWireLimit {
		return credentialMessageMetadata{}, errors.New("message size is invalid")
	}
	if !hasCanonicalCRLF(wire) {
		return credentialMessageMetadata{}, errors.New("message must use canonical CRLF line endings")
	}
	message, err := mail.ReadMessage(bytes.NewReader(wire))
	if err != nil {
		return credentialMessageMetadata{}, errors.New("malformed message headers")
	}
	allowedHeaders := map[string]bool{
		"from": true, "to": true, "subject": true, "date": true, "message-id": true,
		"mime-version": true, "x-one-shot-tally-operation-id": true, "content-type": true,
	}
	for name := range message.Header {
		if !allowedHeaders[strings.ToLower(name)] {
			return credentialMessageMetadata{}, errors.New("message contains an unapproved outer header")
		}
	}
	from, err := singleCredentialHeader(message.Header, "From")
	if err != nil || from != credentialSender {
		return credentialMessageMetadata{}, errors.New("message sender is invalid")
	}
	to, err := singleCredentialHeader(message.Header, "To")
	if err != nil || to != credentialRecipient {
		return credentialMessageMetadata{}, errors.New("message recipient is invalid")
	}
	subject, err := singleCredentialHeader(message.Header, "Subject")
	if err != nil || subject != credentialSubject {
		return credentialMessageMetadata{}, errors.New("message subject is invalid")
	}
	mimeVersion, err := singleCredentialHeader(message.Header, "MIME-Version")
	if err != nil || mimeVersion != "1.0" {
		return credentialMessageMetadata{}, errors.New("message MIME version is invalid")
	}
	date, err := singleCredentialHeader(message.Header, "Date")
	if err != nil {
		return credentialMessageMetadata{}, errors.New("message date is missing")
	}
	if _, err := mail.ParseDate(date); err != nil {
		return credentialMessageMetadata{}, errors.New("message date is invalid")
	}
	operationID, err := singleCredentialHeader(message.Header, "X-One-Shot-Tally-Operation-ID")
	if err != nil || !credentialOperationIDRE.MatchString(operationID) {
		return credentialMessageMetadata{}, errors.New("message operation ID is invalid")
	}
	messageID, err := singleCredentialHeader(message.Header, "Message-ID")
	if err != nil || messageID != credentialMessageID(operationID) {
		return credentialMessageMetadata{}, errors.New("message ID is invalid")
	}
	contentType, err := singleCredentialHeader(message.Header, "Content-Type")
	if err != nil {
		return credentialMessageMetadata{}, errors.New("message content type is missing")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "multipart/encrypted") || !strings.EqualFold(params["protocol"], "application/pgp-encrypted") {
		return credentialMessageMetadata{}, errors.New("message is not OpenPGP/MIME")
	}
	boundary := credentialBoundary(operationID)
	if params["boundary"] != boundary {
		return credentialMessageMetadata{}, errors.New("message boundary is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(message.Body, credentialWireLimit+1))
	if err != nil || len(body) > credentialWireLimit {
		return credentialMessageMetadata{}, errors.New("message body is invalid")
	}
	if !bytes.HasPrefix(body, []byte("--"+boundary+"\r\n")) || !bytes.HasSuffix(body, []byte("--"+boundary+"--\r\n")) {
		return credentialMessageMetadata{}, errors.New("message contains a MIME preamble or epilogue")
	}
	multipartReader := multipart.NewReader(bytes.NewReader(body), boundary)
	versionPart, err := multipartReader.NextPart()
	if err != nil {
		return credentialMessageMetadata{}, errors.New("OpenPGP version part is missing")
	}
	if err := validateCredentialPartHeaders(versionPart.Header, map[string]string{
		"Content-Type": "application/pgp-encrypted", "Content-Transfer-Encoding": "7bit",
	}); err != nil {
		return credentialMessageMetadata{}, err
	}
	versionBody, err := io.ReadAll(io.LimitReader(versionPart, 64))
	_ = versionPart.Close()
	if err != nil || string(bytes.TrimSpace(versionBody)) != "Version: 1" {
		return credentialMessageMetadata{}, errors.New("OpenPGP version part is invalid")
	}
	encryptedPart, err := multipartReader.NextPart()
	if err != nil {
		return credentialMessageMetadata{}, errors.New("OpenPGP encrypted part is missing")
	}
	if err := validateCredentialEncryptedPartHeaders(encryptedPart.Header); err != nil {
		return credentialMessageMetadata{}, err
	}
	ciphertext, err := io.ReadAll(io.LimitReader(encryptedPart, credentialWireLimit+1))
	_ = encryptedPart.Close()
	if err != nil || len(ciphertext) > credentialWireLimit {
		return credentialMessageMetadata{}, errors.New("OpenPGP ciphertext is invalid")
	}
	ciphertext = bytes.TrimSpace(ciphertext)
	if err := validateCredentialArmor(ciphertext); err != nil {
		return credentialMessageMetadata{}, err
	}
	if extra, err := multipartReader.NextPart(); err != io.EOF {
		if extra != nil {
			_ = extra.Close()
		}
		return credentialMessageMetadata{}, errors.New("message has unexpected MIME parts")
	}
	sum := sha256.Sum256(ciphertext)
	return credentialMessageMetadata{
		OperationID: operationID,
		MessageID:   messageID,
		Ciphertext:  append([]byte(nil), ciphertext...),
		Hash:        hex.EncodeToString(sum[:]),
	}, nil
}

func validateCredentialArmor(ciphertext []byte) error {
	if !bytes.HasPrefix(ciphertext, []byte("-----BEGIN PGP MESSAGE-----\r\n")) || !bytes.HasSuffix(ciphertext, []byte("-----END PGP MESSAGE-----")) {
		return errors.New("OpenPGP armor is invalid")
	}
	block, err := armor.Decode(bytes.NewReader(ciphertext))
	if err != nil || block.Type != "PGP MESSAGE" {
		return errors.New("OpenPGP armor is invalid")
	}
	decoded, err := io.ReadAll(io.LimitReader(block.Body, credentialWireLimit+1))
	if err != nil || len(decoded) == 0 || len(decoded) > credentialWireLimit {
		return errors.New("OpenPGP packet stream is invalid")
	}
	packetReader := packet.NewReader(bytes.NewReader(decoded))
	first, err := packetReader.Next()
	if err != nil {
		return errors.New("OpenPGP recipient packet is invalid")
	}
	encryptedKey, ok := first.(*packet.EncryptedKey)
	if !ok || encryptedKey.KeyId != credentialEncryptionKeyID {
		return errors.New("OpenPGP message is not encrypted solely to the pinned recipient key")
	}
	second, err := packetReader.Next()
	if err != nil {
		return errors.New("OpenPGP encrypted data packet is invalid")
	}
	switch encrypted := second.(type) {
	case *packet.SymmetricallyEncrypted:
		if !encrypted.IntegrityProtected {
			return errors.New("OpenPGP message is not integrity protected")
		}
		return nil
	case *packet.AEADEncrypted:
		return nil
	default:
		return errors.New("OpenPGP message has unexpected recipient or plaintext packets")
	}
}

func validateCredentialPartHeaders(header map[string][]string, expected map[string]string) error {
	if len(header) != len(expected) {
		return errors.New("OpenPGP MIME part has unapproved headers")
	}
	for name, value := range expected {
		values := credentialHeaderValues(header, name)
		if len(values) != 1 || !strings.EqualFold(strings.TrimSpace(values[0]), value) {
			return errors.New("OpenPGP MIME part header is invalid")
		}
	}
	return nil
}

func validateCredentialEncryptedPartHeaders(header map[string][]string) error {
	if len(header) != 3 {
		return errors.New("OpenPGP encrypted part has unapproved headers")
	}
	transfer := credentialHeaderValues(header, "Content-Transfer-Encoding")
	if len(transfer) != 1 || !strings.EqualFold(strings.TrimSpace(transfer[0]), "7bit") {
		return errors.New("OpenPGP encrypted transfer encoding is invalid")
	}
	contentTypes := credentialHeaderValues(header, "Content-Type")
	if len(contentTypes) != 1 {
		return errors.New("OpenPGP encrypted content type is invalid")
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || !strings.EqualFold(mediaType, "application/octet-stream") || params["name"] != "encrypted.asc" {
		return errors.New("OpenPGP encrypted content type is invalid")
	}
	dispositions := credentialHeaderValues(header, "Content-Disposition")
	if len(dispositions) != 1 {
		return errors.New("OpenPGP encrypted disposition is invalid")
	}
	disposition, params, err := mime.ParseMediaType(dispositions[0])
	if err != nil || !strings.EqualFold(disposition, "inline") || params["filename"] != "encrypted.asc" {
		return errors.New("OpenPGP encrypted disposition is invalid")
	}
	return nil
}

func singleCredentialHeader(header mail.Header, name string) (string, error) {
	values := credentialHeaderValues(map[string][]string(header), name)
	if len(values) != 1 {
		return "", errors.New("header must occur exactly once")
	}
	return strings.TrimSpace(values[0]), nil
}

func credentialHeaderValues(header map[string][]string, name string) []string {
	var values []string
	for candidate, candidateValues := range header {
		if strings.EqualFold(candidate, name) {
			values = append(values, candidateValues...)
		}
	}
	return values
}

func submitCredentialSSH(wire []byte, operationID string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return &credentialTransportError{state: "failed", reason: "local_transport_unavailable", message: "SSH transport is unavailable; no delivery was attempted", code: 1}
	}
	privateKey, knownHosts, args := credentialSSHArguments(home)
	if err := validateCredentialSSHFiles(privateKey, knownHosts); err != nil {
		return &credentialTransportError{state: "failed", reason: "local_transport_unavailable", message: "dedicated credential SSH files are unavailable or unsafe; no delivery was attempted", code: 1}
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialTransportTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/ssh", args...)
	command.Stdin = bytes.NewReader(wire)
	response := credentialBoundedBuffer{limit: 1024}
	command.Stdout = &response
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return &credentialTransportError{state: "failed", reason: "ssh_not_started", message: "SSH transport did not start; no delivery was attempted", code: 1}
	}
	if err := command.Wait(); err != nil {
		return &credentialTransportError{state: "unknown", reason: "ssh_outcome_unknown", message: "SSH credential delivery outcome is unknown; no automatic retry was attempted", code: 3}
	}
	if ctx.Err() != nil || strings.TrimSpace(response.String()) != "submitted: "+operationID {
		return &credentialTransportError{state: "unknown", reason: "ssh_response_unknown", message: "SSH credential delivery confirmation is invalid; no automatic retry was attempted", code: 3}
	}
	return nil
}

func deliverCredentialSendmail(wire []byte) (bool, error) {
	command := exec.Command("/usr/sbin/sendmail", credentialSendmailArguments()...)
	command.Stdin = bytes.NewReader(wire)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return false, err
	}
	return true, command.Wait()
}

func credentialSSHArguments(home string) (string, string, []string) {
	privateKey := filepath.Join(home, ".config", "one-shot-tally", credentialPrivateKeyFilename)
	knownHosts := filepath.Join(home, ".ssh", "known_hosts")
	return privateKey, knownHosts, []string{
		"-F", "/dev/null", "-T", "-oBatchMode=yes", "-oIdentitiesOnly=yes", "-oIdentityAgent=none",
		"-oPreferredAuthentications=publickey", "-oPubkeyAuthentication=yes",
		"-oPasswordAuthentication=no", "-oKbdInteractiveAuthentication=no", "-oCertificateFile=none",
		"-oStrictHostKeyChecking=yes", "-oUserKnownHostsFile=" + knownHosts,
		"-oGlobalKnownHostsFile=/dev/null", "-oHostKeyAlias=" + credentialSSHHostKeyAlias,
		"-oConnectTimeout=10", "-oConnectionAttempts=1", "-oClearAllForwardings=yes",
		"-oPermitLocalCommand=no", "-oControlMaster=no", "-oControlPath=none", "-oControlPersist=no",
		"-i", privateKey,
		credentialSSHUser + "@" + credentialSSHHost,
		"/usr/local/bin/one-shot-tally credential receive",
	}
}

func validateCredentialSSHFiles(privateKey, knownHosts string) error {
	if err := requirePrivateRegularFile(privateKey); err != nil {
		return err
	}
	if _, err := os.Lstat(privateKey + "-cert.pub"); err == nil {
		return errors.New("companion SSH certificate is not allowed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return requireRegularFile(knownHosts)
}

func credentialSendmailArguments() []string {
	return []string{"-i", "-f", credentialSender, credentialRecipient}
}

func credentialValidatedGPGBinary() (string, error) {
	resolvedBinary, err := filepath.EvalSymlinks(credentialGPGBinary)
	if err != nil {
		return "", errors.New("pinned GnuPG binary is unavailable")
	}
	info, err := os.Lstat(resolvedBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("pinned GnuPG binary is unavailable or writable")
	}
	return resolvedBinary, nil
}

func credentialValidatedGPGConfBinary(gpgBinary string) (string, error) {
	gpgconfBinary := filepath.Join(filepath.Dir(gpgBinary), "gpgconf")
	info, err := os.Lstat(gpgconfBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("pinned GnuPG cleanup binary is unavailable or writable")
	}
	return gpgconfBinary, nil
}

func credentialGPGConfArguments(home string) []string {
	return []string{"--homedir", home, "--kill", "all"}
}

func credentialWKDLocateArguments(home string) []string {
	return []string{
		"--no-options", "--homedir", home,
		"--batch", "--no-tty", "--no-auto-key-retrieve",
		"--auto-key-locate", "clear,wkd", "--locate-external-key", credentialRecipient,
	}
}

func credentialWKDExportArguments(home string) []string {
	return []string{
		"--no-options", "--homedir", home,
		"--batch", "--no-tty", "--export-options", "export-minimal",
		"--export", credentialFingerprint,
	}
}

func credentialGPGArguments(home, recipientFile string) []string {
	return []string{
		"--no-options", "--homedir", filepath.Join(home, ".gnupg"),
		"--batch", "--yes", "--no-tty", "--pinentry-mode", "error",
		"--no-auto-key-retrieve", "--auto-key-locate", "clear", "--trust-model", "always",
		"--local-user", credentialSigningFingerprint + "!",
		"--recipient-file", recipientFile,
		"--digest-algo", "SHA256", "--cipher-algo", "AES256", "--compress-algo", "none",
		"--armor", "--output", "-", "--sign", "--encrypt",
	}
}

type credentialBoundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *credentialBoundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 || b.buffer.Len()+len(p) > b.limit {
		return 0, errors.New("command output exceeds limit")
	}
	return b.buffer.Write(p)
}

func (b *credentialBoundedBuffer) String() string { return b.buffer.String() }
func (b *credentialBoundedBuffer) Bytes() []byte  { return b.buffer.Bytes() }

func credentialLocalReceiptDir() (string, error) {
	base := os.Getenv("ONE_SHOT_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".codex", "state", "one-shot-delivery")
	}
	if _, err := secureCredentialDirectory(base); err != nil {
		return "", err
	}
	return secureCredentialDirectory(filepath.Join(base, "credential-mail"))
}

func secureCredentialDirectory(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("credential state path is not a real directory")
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil {
			return "", err
		}
	}
	return path, nil
}

func acquireCredentialOperationLock(receiptPath string) (func() error, error) {
	lockPath := receiptPath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, &credentialExitError{message: "credential operation is already in progress; no automatic retry was attempted", code: 3}
	}
	if err != nil {
		return nil, err
	}
	token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	if _, err := file.WriteString(token); err != nil {
		_ = file.Close()
		_ = os.Remove(lockPath)
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(lockPath)
		return nil, err
	}
	return func() error { return removeOwnedLock(lockPath, token) }, nil
}

func loadCredentialReceipt(path string) (credentialReceipt, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return credentialReceipt{}, false, nil
	}
	if err != nil {
		return credentialReceipt{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return credentialReceipt{}, false, errors.New("receipt is not a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return credentialReceipt{}, false, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, credentialReceiptLimit+1))
	if err != nil || len(b) > credentialReceiptLimit {
		return credentialReceipt{}, false, errors.New("receipt is invalid")
	}
	var receipt credentialReceipt
	if err := json.Unmarshal(b, &receipt); err != nil {
		return credentialReceipt{}, false, errors.New("receipt is invalid")
	}
	return receipt, true, nil
}

func saveCredentialReceipt(path string, receipt credentialReceipt) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("receipt target is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".credential-receipt-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(b); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

func requirePrivateRegularFile(path string) error {
	if err := requireRegularFile(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("private file is accessible by group or others")
	}
	return nil
}

func credentialBoundary(operationID string) string { return "one-shot-" + operationID }

func credentialMessageID(operationID string) string {
	return "<one-shot-" + operationID + "@boompay.ca>"
}

func canonicalCRLF(input []byte) []byte {
	out := make([]byte, 0, len(input)+16)
	for i := 0; i < len(input); i++ {
		switch input[i] {
		case '\r':
			if i+1 < len(input) && input[i+1] == '\n' {
				i++
			}
			out = append(out, '\r', '\n')
		case '\n':
			out = append(out, '\r', '\n')
		default:
			out = append(out, input[i])
		}
	}
	return out
}

func hasCanonicalCRLF(input []byte) bool {
	for i, value := range input {
		if value == '\n' && (i == 0 || input[i-1] != '\r') {
			return false
		}
		if value == '\r' && (i+1 >= len(input) || input[i+1] != '\n') {
			return false
		}
	}
	return true
}

func wipeBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
