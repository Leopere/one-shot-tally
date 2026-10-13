//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	credentialReceiverAdmissions   = 128
	credentialReceiverReceipts     = 4096
	credentialReceiverStoreBytes   = 64 * 1024 * 1024
	credentialReceiverBudgetBytes  = 1024
	credentialReceiverFinalizeTime = 3 * time.Second
	credentialReceiverLock         = ".receiver.lock"
	credentialReceiverBudgetFile   = ".receiver-budget.json"
	credentialReceiverTemp         = ".receiver-write.tmp"
)

// Filesystem I/O cannot be interrupted safely in Go. The production launcher must
// also impose a 30-second wall limit on the entire receiver cgroup.
func credentialReceive(args []string, r io.Reader, w io.Writer, deps credentialReceiveDependencies) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, credentialTransportTimeout)
	defer cancel()
	return credentialReceiveContext(ctx, args, r, w, deps)
}

func credentialReceiveContext(ctx context.Context, args []string, r io.Reader, w io.Writer, deps credentialReceiveDependencies) error {
	if len(args) != 0 {
		return errors.New("usage: one-shot-tally credential receive")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("receiver requires an absolute deadline")
	}
	work, cancel := context.WithDeadline(ctx, deadline.Add(-credentialReceiverFinalizeTime))
	defer cancel()
	if err := work.Err(); err != nil {
		return err
	}
	dir, err := deps.stateDir()
	if err != nil {
		return errors.New("prepare receiver receipt store")
	}
	store, err := openCredentialReceiverStore(dir)
	if err != nil {
		return fmt.Errorf("prepare receiver receipt store: %w", err)
	}
	defer store.close()
	// Admission and its durable daily charge precede every read, including malformed input.
	if err := store.admit(work, deps.now().UTC()); err != nil {
		return err
	}
	wire, err := readCredentialReceiverInput(work, r)
	if err != nil {
		return err
	}
	metadata, err := validateCredentialMessage(wire)
	if err != nil {
		return fmt.Errorf("reject encrypted credential message: %w", err)
	}
	if err := work.Err(); err != nil {
		return err
	}
	if err := store.verify(); err != nil {
		return err
	}
	if existing, found := store.receipts[metadata.OperationID]; found {
		if existing.CiphertextSHA256 == metadata.Hash && existing.State == "submitted" {
			return writeCredentialReceiverAck(ctx, w, metadata.OperationID)
		}
		return &credentialExitError{message: "operation already exists in a non-final state; no delivery was attempted", code: 3}
	}
	// Two full receipt slots cover the pending receipt and the final atomic temporary.
	if len(store.receipts) >= credentialReceiverReceipts || store.receiptBytes+2*credentialReceiptLimit > credentialReceiverStoreBytes-2*credentialReceiverBudgetBytes {
		return errors.New("receiver receipt capacity exhausted; no delivery was attempted")
	}
	now := deps.now().UTC()
	if now.Before(store.budget.LastAdmission) {
		return errors.New("receiver clock moved backwards")
	}
	receipt := credentialReceipt{OperationID: metadata.OperationID, MessageID: metadata.MessageID, State: "pending", Sender: credentialSender, Recipient: credentialRecipient, Channel: credentialChannel, SigningFingerprint: credentialSigningFingerprint, CiphertextSHA256: metadata.Hash, CiphertextBytes: len(metadata.Ciphertext), CreatedAt: now, UpdatedAt: now}
	name := metadata.OperationID + ".json"
	pending, _ := json.Marshal(receipt)
	if err := store.write(name, pending, nil); err != nil {
		return errors.New("record pending receiver receipt")
	}
	if err := store.verify(); err != nil {
		return err
	}
	started := false
	deliveryErr := work.Err()
	if deliveryErr == nil {
		started, deliveryErr = deps.deliver(work, wire)
	}
	if deliveryErr == nil && work.Err() != nil {
		deliveryErr = work.Err()
	}
	if deliveryErr == nil && !started {
		deliveryErr = errors.New("sendmail did not start")
	}
	receipt.State = "submitted"
	if deliveryErr != nil {
		receipt.State, receipt.Failure = "failed", "sendmail_not_started"
		if started {
			receipt.State, receipt.Failure = "unknown", "sendmail_outcome_unknown"
		}
	}
	receipt.UpdatedAt = deps.now().UTC()
	// A rollback after submission cannot erase the pending evidence or acknowledge success.
	if receipt.UpdatedAt.Before(receipt.CreatedAt) {
		return &credentialExitError{message: "receiver clock moved backwards; pending outcome retained", code: 3}
	}
	final, _ := json.Marshal(receipt)
	// Finalization uses the reserved wall-clock interval even when work was cancelled.
	if time.Now().After(deadline) {
		return &credentialExitError{message: "receiver deadline expired; pending outcome retained", code: 3}
	}
	if err := store.write(name, final, pending); err != nil {
		return &credentialExitError{message: "receiver outcome could not be finalized; no automatic retry was attempted", code: 3}
	}
	if deliveryErr != nil {
		if started {
			return &credentialExitError{message: "receiver delivery outcome is unknown; no automatic retry was attempted", code: 3}
		}
		return &credentialExitError{message: "receiver did not start sendmail; no delivery was attempted", code: 1}
	}
	if err := store.verify(); err != nil {
		return err
	}
	return writeCredentialReceiverAck(ctx, w, metadata.OperationID)
}

func readCredentialReceiverInput(ctx context.Context, r io.Reader) ([]byte, error) {
	// Arbitrary Readers can block forever. Production uses the polled SSH stdin fd;
	// the finite memory readers keep business tests independent of OS plumbing.
	switch input := r.(type) {
	case *bytes.Reader, *bytes.Buffer, *strings.Reader:
		return readCredentialInput(r, credentialWireLimit, "encrypted credential message")
	case *os.File:
		fd := int(input.Fd())
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return nil, err
		}
		if err := unix.SetNonblock(fd, true); err != nil {
			return nil, err
		}
		defer unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
		result := make([]byte, 0, 4096)
		buffer := make([]byte, 4096)
		for {
			if err := credentialReceiverPoll(ctx, fd, unix.POLLIN); err != nil {
				return nil, err
			}
			n, err := unix.Read(fd, buffer)
			if n > 0 {
				result = append(result, buffer[:n]...)
			}
			if len(result) > credentialWireLimit {
				return nil, errors.New("encrypted credential message exceeds limit")
			}
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			if err != nil {
				return nil, errors.New("read encrypted credential message")
			}
			if n == 0 {
				if len(result) == 0 {
					return nil, errors.New("encrypted credential message is empty")
				}
				return result, nil
			}
		}
	default:
		return nil, errors.New("receiver input requires a cancellable file descriptor")
	}
}

func credentialReceiverPoll(ctx context.Context, fd int, events int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := 50
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			if remaining < 50*time.Millisecond {
				timeout = int(remaining/time.Millisecond) + 1
			}
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if fds[0].Revents&unix.POLLNVAL != 0 {
				return errors.New("receiver descriptor is invalid")
			}
			return ctx.Err()
		}
	}
}

func writeCredentialReceiverAck(ctx context.Context, w io.Writer, id string) error {
	data := []byte("submitted: " + id + "\n")
	if err := ctx.Err(); err != nil {
		return err
	}
	if file, ok := w.(*os.File); ok {
		fd := int(file.Fd())
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return err
		}
		if err := unix.SetNonblock(fd, true); err != nil {
			return err
		}
		defer unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
		for len(data) > 0 {
			if err := credentialReceiverPoll(ctx, fd, unix.POLLOUT); err != nil {
				return err
			}
			n, err := unix.Write(fd, data)
			if n > 0 {
				data = data[n:]
			}
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if _, ok := w.(*bytes.Buffer); ok {
		_, err := w.Write(data)
		return err
	}
	if w == io.Discard {
		return nil
	}
	return errors.New("receiver output requires a cancellable file descriptor")
}

func deliverCredentialSendmail(ctx context.Context, wire []byte) (bool, error) {
	return runCredentialReceiverSendmail(ctx, "/usr/sbin/sendmail", credentialSendmailArguments(), wire)
}

func runCredentialReceiverSendmail(ctx context.Context, executable string, args []string, wire []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	command := exec.Command(executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Stdin = bytes.NewReader(wire)
	// A real /dev/null fd avoids output-copy goroutines retained by descendants.
	command.Stdout = nil
	command.Stderr = nil
	command.WaitDelay = 250 * time.Millisecond
	if err := command.Start(); err != nil {
		return false, err
	}
	// Keep the leader unreaped until its entire group is killed. This prevents a
	// recycled PID from turning descendant cleanup into a kill of another process.
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, command.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if err != nil && err != unix.EINTR {
			break
		}
		if info.Signo != 0 || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, err
	case <-time.After(500 * time.Millisecond):
		return true, errors.New("sendmail cleanup deadline exceeded")
	}

}

type credentialReceiverBudget struct {
	Version       int       `json:"version"`
	Day           string    `json:"day"`
	Admissions    int       `json:"admissions"`
	LastAdmission time.Time `json:"last_admission"`
}

type credentialReceiverDir struct {
	fd   int
	name string
	stat unix.Stat_t
}
type credentialReceiverStore struct {
	dirs         []credentialReceiverDir
	lock         int
	receipts     map[string]credentialReceipt
	receiptBytes int64
	budget       credentialReceiverBudget
}

func (s *credentialReceiverStore) fd() int { return s.dirs[len(s.dirs)-1].fd }
func (s *credentialReceiverStore) close() {
	if s.lock >= 0 {
		_ = unix.Close(s.lock)
	}
	for i := len(s.dirs) - 1; i >= 0; i-- {
		_ = unix.Close(s.dirs[i].fd)
	}
}

func openCredentialReceiverStore(path string) (_ *credentialReceiverStore, result error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("receiver store must use an absolute clean path")
	}
	s := &credentialReceiverStore{lock: -1, receipts: make(map[string]credentialReceipt)}
	defer func() {
		if result != nil {
			s.close()
		}
	}()
	components := append([]string{"/"}, strings.Split(strings.TrimPrefix(path, "/"), "/")...)
	for i, name := range components {
		parent := unix.AT_FDCWD
		if i > 0 {
			parent = s.fd()
		}
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.ENOENT && i == len(components)-1 {
			if err = unix.Mkdirat(parent, name, 0700); err != nil && err != unix.EEXIST {
				return nil, err
			}
			if err = unix.Fsync(parent); err != nil {
				return nil, err
			}
			fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, errors.New("unsafe or unavailable receiver directory")
		}
		entry := credentialReceiverDir{fd: fd, name: name}
		if err = unix.Fstat(fd, &entry.stat); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		s.dirs = append(s.dirs, entry)
		if err = credentialReceiverCheckDir(fd, &entry.stat, i == len(components)-1); err != nil {
			return nil, err
		}
	}
	if err := s.verify(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(s.fd(), credentialReceiverLock, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err == unix.EEXIST {
		fd, err = unix.Openat(s.fd(), credentialReceiverLock, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, errors.New("open receiver admission lock")
	}
	s.lock = fd
	if _, err := s.checkFile(fd, credentialReceiverLock, 0); err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.New("receiver is already active; no input was read")
	}
	if err := unix.Fsync(fd); err != nil {
		return nil, err
	}
	if err := unix.Fsync(s.fd()); err != nil {
		return nil, err
	}
	return s, nil
}

func credentialReceiverNoACL(fd int) error {
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		n, err := unix.Fgetxattr(fd, name, nil)
		if err == unix.ENODATA || err == unix.ENOTSUP {
			continue
		}
		if err != nil || n > 0 {
			return errors.New("receiver path has an ACL or cannot be checked")
		}
	}
	return nil
}

func credentialReceiverCheckDir(fd int, st *unix.Stat_t, leaf bool) error {
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || (st.Uid != 0 && st.Uid != uid) || (st.Gid != 0 && st.Gid != gid) || st.Mode&0022 != 0 {
		return errors.New("receiver ancestor is not trusted")
	}
	if leaf && (st.Uid != uid || st.Gid != gid || st.Mode&07777 != 0700) {
		return errors.New("receiver directory must be owned by its identity with mode 0700")
	}
	return credentialReceiverNoACL(fd)
}

func (s *credentialReceiverStore) verify() error {
	for i, dir := range s.dirs {
		var st unix.Stat_t
		if err := unix.Fstat(dir.fd, &st); err != nil {
			return err
		}
		if st.Dev != dir.stat.Dev || st.Ino != dir.stat.Ino {
			return errors.New("receiver directory identity changed")
		}
		if err := credentialReceiverCheckDir(dir.fd, &st, i == len(s.dirs)-1); err != nil {
			return err
		}
		parent := unix.AT_FDCWD
		if i > 0 {
			parent = s.dirs[i-1].fd
		}
		var link unix.Stat_t
		if err := unix.Fstatat(parent, dir.name, &link, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if link.Dev != st.Dev || link.Ino != st.Ino || link.Mode != st.Mode || link.Uid != st.Uid || link.Gid != st.Gid {
			return errors.New("receiver path was substituted")
		}
	}
	if s.lock >= 0 {
		_, err := s.checkFile(s.lock, credentialReceiverLock, 0)
		return err
	}
	return nil
}

func (s *credentialReceiverStore) checkFile(fd int, name string, limit int64) (unix.Stat_t, error) {
	var st, link unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Gid != uint32(os.Getegid()) || st.Nlink != 1 || st.Size < 0 || st.Size > limit {
		return st, errors.New("receiver file has unsafe identity, metadata, or size")
	}
	if err := credentialReceiverNoACL(fd); err != nil {
		return st, err
	}
	if err := unix.Fstatat(s.fd(), name, &link, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, err
	}
	if link.Dev != st.Dev || link.Ino != st.Ino || link.Mode != st.Mode || link.Uid != st.Uid || link.Gid != st.Gid || link.Nlink != st.Nlink || link.Size != st.Size {
		return st, errors.New("receiver file was substituted")
	}
	return st, nil
}

func (s *credentialReceiverStore) read(name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(s.fd(), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOATIME, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	before, err := s.checkFile(fd, name, limit)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := s.checkFile(fd, name, limit)
	if err != nil {
		return nil, err
	}
	if len(data) > int(limit) || int64(len(data)) != before.Size || before != after {
		return nil, errors.New("receiver file changed during read")
	}
	return data, nil
}

func decodeCredentialReceiverJSON(data []byte, dest any) error {
	// DisallowUnknownFields alone accepts duplicate keys; the outer schema is flat.
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("receiver metadata is not an object")
	}
	allowed := map[string]bool{}
	required := map[string]bool{}
	switch dest.(type) {
	case *credentialReceipt:
		for _, name := range []string{"operation_id", "message_id", "state", "sender", "recipient", "channel", "key_fingerprint", "signing_fingerprint", "ciphertext_sha256", "ciphertext_bytes", "created_at", "updated_at"} {
			allowed[name] = true
			required[name] = true
		}
		allowed["encryption_fingerprint"] = true // absent from the oldest retained schema
		allowed["failure"] = true
	case *credentialReceiverBudget:
		for _, name := range []string{"version", "day", "admissions", "last_admission"} {
			allowed[name] = true
			required[name] = true
		}
	default:
		return errors.New("unsupported receiver metadata schema")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok || seen[name] || !allowed[name] {
			return errors.New("duplicate or unknown receiver metadata field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("null receiver metadata field")
		}
	}
	for name := range required {
		if !seen[name] {
			return errors.New("missing receiver metadata field")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing receiver metadata")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dest)
}

// Legacy receipts recorded public key metadata. It is not cryptographic attestation.
func credentialReceiverFingerprint(value string) bool {
	return value == "" || ((len(value) == 40 || len(value) == 64) && strings.Trim(value, "0123456789ABCDEF") == "")
}

func validateCredentialReceiverReceipt(receipt credentialReceipt, id string) error {
	_, createdOffset := receipt.CreatedAt.Zone()
	_, updatedOffset := receipt.UpdatedAt.Zone()
	if receipt.OperationID != id || !credentialOperationIDRE.MatchString(id) || receipt.MessageID != credentialMessageID(id) || receipt.Sender != credentialSender || receipt.Recipient != credentialRecipient || (receipt.Channel != credentialChannel && receipt.Channel != "openpgp-pgp-mime-signed-encrypted-ssh") || receipt.SigningFingerprint != credentialSigningFingerprint || !credentialReceiverFingerprint(receipt.KeyFingerprint) || !credentialReceiverFingerprint(receipt.EncryptionFingerprint) || len(receipt.AccountRefs) != 0 || receipt.CiphertextBytes < 1 || receipt.CiphertextBytes > credentialWireLimit || len(receipt.CiphertextSHA256) != 64 || strings.Trim(receipt.CiphertextSHA256, "0123456789abcdef") != "" || receipt.CreatedAt.IsZero() || receipt.UpdatedAt.IsZero() || receipt.UpdatedAt.Before(receipt.CreatedAt) || createdOffset != 0 || updatedOffset != 0 {
		return errors.New("invalid receiver receipt schema")
	}
	switch receipt.State {
	case "pending", "submitted":
		if receipt.Failure != "" {
			return errors.New("unexpected receiver failure metadata")
		}
	case "unknown":
		if receipt.Failure != "sendmail_outcome_unknown" {
			return errors.New("invalid unknown receiver receipt")
		}
	case "failed":
		if receipt.Failure != "sendmail_not_started" {
			return errors.New("invalid failed receiver receipt")
		}
	default:
		return errors.New("invalid receiver receipt state")
	}
	return nil
}

func (s *credentialReceiverStore) admit(ctx context.Context, now time.Time) error {
	fd, err := unix.Openat(s.fd(), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), "receiver")
	defer directory.Close()
	var budgetBytes []byte
	total := int64(0)
	entries := 0
	latest := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, err := directory.Readdirnames(64)
		if err != nil && err != io.EOF {
			return err
		}
		for _, name := range names {
			entries++
			if entries > credentialReceiverReceipts+2 {
				return errors.New("receiver entry capacity exceeded")
			}
			if name == credentialReceiverLock {
				continue
			}
			limit := int64(credentialReceiptLimit)
			if name == credentialReceiverBudgetFile {
				limit = credentialReceiverBudgetBytes
			}
			data, err := s.read(name, limit)
			if err != nil {
				return err
			}
			total += int64(len(data))
			if total > credentialReceiverStoreBytes {
				return errors.New("receiver storage budget exceeded")
			}
			if name == credentialReceiverBudgetFile {
				budgetBytes = data
				if err := decodeCredentialReceiverJSON(data, &s.budget); err != nil {
					return err
				}
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if name != id+".json" || !credentialOperationIDRE.MatchString(id) {
				return errors.New("unexpected retained receiver file; review required")
			}
			var receipt credentialReceipt
			if err := decodeCredentialReceiverJSON(data, &receipt); err != nil {
				return err
			}
			if err := validateCredentialReceiverReceipt(receipt, id); err != nil {
				return err
			}
			s.receipts[id] = receipt
			s.receiptBytes += int64(len(data))
			if receipt.UpdatedAt.After(latest) {
				latest = receipt.UpdatedAt
			}
		}
		if err == io.EOF {
			break
		}
	}
	// Keep independent reserved space for the rate journal and its atomic temporary,
	// so an existing submitted operation remains confirmable at receipt capacity.
	if s.receiptBytes > credentialReceiverStoreBytes-2*credentialReceiverBudgetBytes {
		return errors.New("receiver storage reserve exhausted")
	}
	if now.IsZero() || now.Before(latest) {
		return errors.New("receiver clock precedes receipt history")
	}
	if budgetBytes != nil {
		budget := s.budget
		_, offset := budget.LastAdmission.Zone()
		if budget.Version != 1 || budget.LastAdmission.IsZero() || offset != 0 || budget.Day != budget.LastAdmission.Format("2006-01-02") || budget.Admissions < 1 || budget.Admissions > credentialReceiverAdmissions {
			return errors.New("invalid receiver admission history")
		}
		if now.Before(budget.LastAdmission) {
			return errors.New("receiver clock moved backwards")
		}
	}
	day := now.Format("2006-01-02")
	count := 1
	if s.budget.Day == day {
		count = s.budget.Admissions + 1
	}
	if count > credentialReceiverAdmissions {
		return errors.New("receiver daily admission budget exhausted")
	}
	s.budget = credentialReceiverBudget{Version: 1, Day: day, Admissions: count, LastAdmission: now}
	data, _ := json.Marshal(s.budget)
	return s.write(credentialReceiverBudgetFile, data, budgetBytes)
}

func (s *credentialReceiverStore) write(name string, data, previous []byte) error {
	if err := s.verify(); err != nil {
		return err
	}
	if len(data) > credentialReceiptLimit {
		return errors.New("receiver metadata exceeds write limit")
	}
	if previous != nil {
		current, err := s.read(name, credentialReceiptLimit)
		if err != nil || !bytes.Equal(current, previous) {
			return errors.New("receiver target changed before replacement")
		}
	} else {
		var st unix.Stat_t
		if err := unix.Fstatat(s.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != unix.ENOENT {
			return errors.New("receiver target already exists")
		}
	}
	fd, err := unix.Openat(s.fd(), credentialReceiverTemp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("receiver atomic temporary exists or cannot be created")
	}
	file := os.NewFile(uintptr(fd), credentialReceiverTemp)
	defer file.Close()
	// Never remove retained temporaries on error: they are evidence of an interrupted write.
	if _, err := s.checkFile(fd, credentialReceiverTemp, credentialReceiptLimit); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if _, err := s.checkFile(fd, credentialReceiverTemp, credentialReceiptLimit); err != nil {
		return err
	}
	if err := s.verify(); err != nil {
		return err
	}
	if previous != nil {
		current, err := s.read(name, credentialReceiptLimit)
		if err != nil || !bytes.Equal(current, previous) {
			return errors.New("receiver target changed during replacement")
		}
		err = unix.Renameat(s.fd(), credentialReceiverTemp, s.fd(), name)
	} else {
		err = unix.Renameat2(s.fd(), credentialReceiverTemp, s.fd(), name, unix.RENAME_NOREPLACE)
	}
	if err != nil {
		return err
	}
	if err := unix.Fsync(s.fd()); err != nil {
		return err
	}
	_, err = s.checkFile(fd, name, credentialReceiptLimit)
	return err
}
