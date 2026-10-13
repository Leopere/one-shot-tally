//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func receiverFixture(t *testing.T) ([]byte, credentialReceipt) {
	t.Helper()
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	armor, err := credentialProductionTestSeal(t, []byte("encrypted test fixture"), now)
	if err != nil {
		t.Fatal(err)
	}
	wire, _, err := buildCredentialMessage(credentialTestOperationID, armor, now)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := validateCredentialMessage(wire)
	if err != nil {
		t.Fatal(err)
	}
	receipt := credentialReceipt{OperationID: metadata.OperationID, MessageID: metadata.MessageID, State: "submitted", Sender: credentialSender, Recipient: credentialRecipient, Channel: credentialChannel, SigningFingerprint: credentialSigningFingerprint, CiphertextSHA256: metadata.Hash, CiphertextBytes: len(metadata.Ciphertext), CreatedAt: now, UpdatedAt: now}
	return wire, receipt
}

func receiverTestDeps(path string, now time.Time) credentialReceiveDependencies {
	return credentialReceiveDependencies{now: func() time.Time { return now }, stateDir: func() (string, error) { return path, nil }, deliver: func(context.Context, []byte) (bool, error) { return true, nil }}
}

func receiverWriteFixture(t *testing.T, path string, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func receiverReadFixture(t *testing.T, path string) credentialReceipt {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt credentialReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func receiverCommand(t *testing.T, path, mode string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestCredentialReceiverSubprocess$", "-test.timeout=40s")
	command.Env = append(os.Environ(), "CREDENTIAL_RECEIVER_TEST="+mode, "CREDENTIAL_RECEIVER_TEST_DIR="+path)
	return command
}

func waitReceiverFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture did not become ready: %s", path)
}

func TestCredentialReceiverSubprocess(t *testing.T) {
	mode := os.Getenv("CREDENTIAL_RECEIVER_TEST")
	if mode == "" {
		return
	}
	path := os.Getenv("CREDENTIAL_RECEIVER_TEST_DIR")
	now := time.Date(2026, 8, 31, 21, 0, 0, 0, time.UTC)
	deps := receiverTestDeps(path, now)
	if mode == "admit" {
		store, err := openCredentialReceiverStore(path)
		if err == nil {
			defer store.close()
			err = store.admit(context.Background(), now)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	if mode == "stall" || mode == "death" || mode == "signal" {
		deps.deliver = func(ctx context.Context, wire []byte) (bool, error) {
			args := []string{"-c", `echo $$ > "$1/leader"; sleep 120 & echo $! > "$1/child"; wait`, "fixture", filepath.Dir(path)}
			return runCredentialReceiverSendmail(ctx, "/bin/sh", args, wire)
		}
	}
	if mode == "idle-real-deadline" || mode == "signal" {
		if err := credentialReceive(nil, os.Stdin, os.Stdout, deps); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	// Production policy remains 30 seconds; shortened tests retain its full
	// three-second finalization reserve and exercise the same deadline path.
	timeout := credentialReceiverFinalizeTime + 400*time.Millisecond
	if mode == "death" {
		timeout = credentialTransportTimeout
	}
	ctx, stop := signalReceiverTestContext(timeout)
	defer stop()
	err := credentialReceiveContext(ctx, nil, os.Stdin, os.Stdout, deps)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func signalReceiverTestContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

func TestCredentialReceiverIdleAndPartialInputBounded(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			path := credentialReceiverTestDirectory(t)
			command := receiverCommand(t, path, "idle")
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			var diagnostic bytes.Buffer
			command.Stderr = &diagnostic
			started := time.Now()
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			if partial {
				_, _ = input.Write([]byte("From: half a header"))
			}
			err = command.Wait()
			if err == nil || time.Since(started) > 2*time.Second || !strings.Contains(diagnostic.String(), "deadline") {
				t.Fatalf("idle stdin bound: elapsed=%s error=%v diagnostic=%s", time.Since(started), err, diagnostic.String())
			}
			var budget credentialReceiverBudget
			data, err := os.ReadFile(filepath.Join(path, credentialReceiverBudgetFile))
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(data, &budget) != nil || budget.Admissions != 1 {
				t.Fatal("idle attempt not charged before input")
			}
		})
	}
}

func TestCredentialReceiverRealThirtySecondPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("real 30-second policy subprocess")
	}
	path := credentialReceiverTestDirectory(t)
	command := receiverCommand(t, path, "idle-real-deadline")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	start := time.Now()
	if err := command.Run(); err == nil {
		t.Fatal("idle receiver succeeded")
	}
	elapsed := time.Since(start)
	if elapsed < 26*time.Second || elapsed > 30*time.Second || !strings.Contains(diagnostic.String(), "deadline") {
		t.Fatalf("production deadline elapsed=%s diagnostic=%s", elapsed, diagnostic.String())
	}
}

func TestCredentialReceiverGlobalAdmissionPrecedesInput(t *testing.T) {
	path := credentialReceiverTestDirectory(t)
	first := receiverCommand(t, path, "death")
	input, err := first.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = first.Process.Kill(); _ = first.Wait() }()
	waitReceiverFile(t, filepath.Join(path, credentialReceiverBudgetFile))
	second := receiverCommand(t, path, "idle")
	secondInput, err := second.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer secondInput.Close()
	start := time.Now()
	out, err := second.CombinedOutput()
	if err == nil || time.Since(start) > time.Second || !bytes.Contains(out, []byte("already active")) {
		t.Fatalf("global lock failed before stdin: %v %s", err, out)
	}
	// Killing the process releases flock without deleting its stable lock inode.
	info, err := os.Stat(filepath.Join(path, credentialReceiverLock))
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Process.Kill()
	_ = first.Wait()
	store, err := openCredentialReceiverStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	after, err := os.Stat(filepath.Join(path, credentialReceiverLock))
	if err != nil || !os.SameFile(info, after) {
		t.Fatal("persistent lock replaced")
	}
}

func TestCredentialReceiverStartedFailureAndReplay(t *testing.T) {
	wire, receipt := receiverFixture(t)
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			path := credentialReceiverTestDirectory(t)
			deps := receiverTestDeps(path, receipt.CreatedAt)
			calls := 0
			deps.deliver = func(context.Context, []byte) (bool, error) {
				calls++
				pending := receiverReadFixture(t, filepath.Join(path, receipt.OperationID+".json"))
				if pending.State != "pending" {
					t.Fatal("submit preceded durable pending")
				}
				// Directory and receipt are readable only after both Sync calls have returned.
				return started, errors.New("fake submit failure")
			}
			var out bytes.Buffer
			if err := credentialReceive(nil, bytes.NewReader(wire), &out, deps); err == nil {
				t.Fatal("failed submit acknowledged")
			}
			saved := receiverReadFixture(t, filepath.Join(path, receipt.OperationID+".json"))
			expected := "failed"
			if started {
				expected = "unknown"
			}
			if saved.State != expected || out.Len() != 0 {
				t.Fatalf("state=%s output=%s", saved.State, out.String())
			}
			if err := credentialReceive(nil, bytes.NewReader(wire), &out, deps); err == nil || calls != 1 {
				t.Fatal("nonfinal ID retried")
			}
		})
	}
}

func TestCredentialReceiverStalledSendmailKillsGroupAndRetainsUnknown(t *testing.T) {
	path := credentialReceiverTestDirectory(t)
	wire, receipt := receiverFixture(t)
	command := receiverCommand(t, path, "stall")
	command.Stdin = bytes.NewReader(wire)
	start := time.Now()
	output, err := command.CombinedOutput()
	if err == nil || time.Since(start) > 2*time.Second || !bytes.Contains(output, []byte("outcome is unknown")) {
		t.Fatalf("stalled sendmail: %v %s", err, output)
	}
	saved := receiverReadFixture(t, filepath.Join(path, receipt.OperationID+".json"))
	if saved.State != "unknown" {
		t.Fatalf("state=%s", saved.State)
	}
	for _, name := range []string{"leader", "child"} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		assertReceiverProcessStopped(t, pid)
	}
}

func assertReceiverProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		// Containers without an init reaper can retain dead descendant zombies.
		fields := strings.Fields(string(data))
		if len(fields) >= 3 && fields[2] == "Z" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process remains alive: pid=%d state=%s", pid, data)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCredentialReceiverDeathPreservesPending(t *testing.T) {
	path := credentialReceiverTestDirectory(t)
	wire, receipt := receiverFixture(t)
	command := receiverCommand(t, path, "death")
	command.Stdin = bytes.NewReader(wire)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitReceiverFile(t, filepath.Join(filepath.Dir(path), "leader"))
	leaderBytes, err := os.ReadFile(filepath.Join(filepath.Dir(path), "leader"))
	if err != nil {
		t.Fatal(err)
	}
	leader, _ := strconv.Atoi(strings.TrimSpace(string(leaderBytes)))
	// The outer production cgroup kill is necessary on receiver SIGKILL. Clean the
	// isolated fixture's full group here as that launcher would, never production.
	defer syscall.Kill(-leader, syscall.SIGKILL)
	_ = command.Process.Kill()
	_ = command.Wait()
	saved := receiverReadFixture(t, filepath.Join(path, receipt.OperationID+".json"))
	if saved.State != "pending" {
		t.Fatal("death lost pending receipt")
	}
	deps := receiverTestDeps(path, receipt.CreatedAt.Add(48*time.Hour))
	deps.deliver = func(context.Context, []byte) (bool, error) { t.Fatal("pending retried after death"); return false, nil }
	if err := credentialReceive(nil, bytes.NewReader(wire), io.Discard, deps); err == nil {
		t.Fatal("pending death acknowledged")
	}
}

func TestCredentialReceiverPersistentRateAndClockRollback(t *testing.T) {
	path := credentialReceiverTestDirectory(t)
	for i := 0; i < 2; i++ {
		if out, err := receiverCommand(t, path, "admit").CombinedOutput(); err != nil {
			t.Fatalf("restart %d: %v %s", i, err, out)
		}
	}
	data, err := os.ReadFile(filepath.Join(path, credentialReceiverBudgetFile))
	if err != nil {
		t.Fatal(err)
	}
	var budget credentialReceiverBudget
	if err := json.Unmarshal(data, &budget); err != nil {
		t.Fatal(err)
	}
	if budget.Admissions != 2 {
		t.Fatalf("restart lost rate count: %d", budget.Admissions)
	}
	store, err := openCredentialReceiverStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.admit(context.Background(), budget.LastAdmission.Add(-time.Nanosecond)); err == nil {
		t.Fatal("clock rollback admitted")
	}
	store.close()
	budget.Admissions = credentialReceiverAdmissions
	receiverWriteFixture(t, filepath.Join(path, credentialReceiverBudgetFile), budget)
	if out, err := receiverCommand(t, path, "admit").CombinedOutput(); err == nil || !bytes.Contains(out, []byte("daily admission")) {
		t.Fatalf("rate cap: %v %s", err, out)
	}
	store, err = openCredentialReceiverStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if err := store.admit(context.Background(), budget.LastAdmission.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if store.budget.Admissions != 1 {
		t.Fatal("next UTC day did not reset rate")
	}
}

func TestCredentialReceiverLegacyReceiptsRemainByteIdentical(t *testing.T) {
	wire, receipt := receiverFixture(t)
	receipt.KeyFingerprint = strings.Repeat("A", 40)
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprint(omit), func(t *testing.T) {
			path := credentialReceiverTestDirectory(t)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			receipt.EncryptionFingerprint = strings.Repeat("B", 40)
			data, _ := json.Marshal(receipt)
			if omit {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(data, &fields)
				delete(fields, "encryption_fingerprint")
				fields["channel"] = json.RawMessage(`"openpgp-pgp-mime-signed-encrypted-ssh"`)
				data, _ = json.Marshal(fields)
			}
			name := filepath.Join(path, receipt.OperationID+".json")
			if err := os.WriteFile(name, data, 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(name)
			deps := receiverTestDeps(path, receipt.CreatedAt)
			deps.deliver = func(context.Context, []byte) (bool, error) {
				t.Fatal("legacy submitted receipt redelivered")
				return false, nil
			}
			var out bytes.Buffer
			if err := credentialReceive(nil, bytes.NewReader(wire), &out, deps); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(name)
			result, _ := os.ReadFile(name)
			if !bytes.Equal(result, data) || !os.SameFile(before, after) || out.String() != "submitted: "+receipt.OperationID+"\n" {
				t.Fatal("legacy receipt was changed")
			}
		})
	}
}

func TestCredentialReceiverReceiptCapacityStillConfirmsReplay(t *testing.T) {
	wire, receipt := receiverFixture(t)
	path := credentialReceiverTestDirectory(t)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(receipt)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(legacy, &fields)
	delete(fields, "encryption_fingerprint")
	fields["channel"] = json.RawMessage(`"openpgp-pgp-mime-signed-encrypted-ssh"`)
	legacy, _ = json.Marshal(fields)
	if err := os.WriteFile(filepath.Join(path, receipt.OperationID+".json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < credentialReceiverReceipts; i++ {
		next := receipt
		next.OperationID = fmt.Sprintf("%08x-e89b-12d3-a456-426614174000", i)
		next.MessageID = credentialMessageID(next.OperationID)
		receiverWriteFixture(t, filepath.Join(path, next.OperationID+".json"), next)
	}
	deps := receiverTestDeps(path, receipt.CreatedAt)
	deps.deliver = func(context.Context, []byte) (bool, error) {
		t.Fatal("capacity allowed new submission")
		return false, nil
	}
	if err := credentialReceive(nil, bytes.NewReader(wire), io.Discard, deps); err != nil {
		t.Fatalf("matching replay rejected at capacity: %v", err)
	}
	newID := "ffffffff-e89b-12d3-a456-426614174000"
	newWire := bytes.ReplaceAll(wire, []byte(receipt.OperationID), []byte(newID))
	if err := credentialReceive(nil, bytes.NewReader(newWire), io.Discard, deps); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("new ID at capacity: %v", err)
	}
}

func TestCredentialReceiverUnsafeFilesystemFailsClosed(t *testing.T) {
	_, receipt := receiverFixture(t)
	tests := map[string]func(*testing.T, string){
		"leaf mode":         func(t *testing.T, path string) { _ = os.Chmod(path, 0755) },
		"ancestor writable": func(t *testing.T, path string) { _ = os.Chmod(filepath.Dir(path), 0777) },
		"receipt symlink": func(t *testing.T, path string) {
			_ = os.Symlink("/etc/passwd", filepath.Join(path, receipt.OperationID+".json"))
		},
		"receipt hardlink": func(t *testing.T, path string) {
			original := filepath.Join(filepath.Dir(path), "other")
			receiverWriteFixture(t, original, receipt)
			_ = os.Link(original, filepath.Join(path, receipt.OperationID+".json"))
		},
		"receipt group readable": func(t *testing.T, path string) {
			name := filepath.Join(path, receipt.OperationID+".json")
			receiverWriteFixture(t, name, receipt)
			_ = os.Chmod(name, 0640)
		},
		"foreign owner": func(t *testing.T, path string) {
			if os.Geteuid() != 0 {
				t.Skip("ownership mutation requires isolated root")
			}
			name := filepath.Join(path, receipt.OperationID+".json")
			receiverWriteFixture(t, name, receipt)
			if err := os.Chown(name, 65534, 65534); err != nil {
				t.Fatal(err)
			}
		},
		"foreign group": func(t *testing.T, path string) {
			if os.Geteuid() != 0 {
				t.Skip("group mutation requires isolated root")
			}
			if err := os.Chown(path, 0, 65534); err != nil {
				t.Fatal(err)
			}
		},
		"retained lock": func(t *testing.T, path string) {
			_ = os.WriteFile(filepath.Join(path, receipt.OperationID+".json.lock"), []byte("123-456"), 0600)
		},
		"retained temporary": func(t *testing.T, path string) {
			receiverWriteFixture(t, filepath.Join(path, credentialReceiverTemp), receipt)
		},
		"unknown file": func(t *testing.T, path string) { _ = os.WriteFile(filepath.Join(path, "extra"), []byte("x"), 0600) },
		"global lock hardlink": func(t *testing.T, path string) {
			original := filepath.Join(filepath.Dir(path), "lock")
			_ = os.WriteFile(original, nil, 0600)
			_ = os.Link(original, filepath.Join(path, credentialReceiverLock))
		},
		"oversize": func(t *testing.T, path string) {
			_ = os.WriteFile(filepath.Join(path, receipt.OperationID+".json"), bytes.Repeat([]byte(" "), credentialReceiptLimit+1), 0600)
		},
		"receipt FIFO": func(t *testing.T, path string) {
			if err := unix.Mkfifo(filepath.Join(path, receipt.OperationID+".json"), 0600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			path := credentialReceiverTestDirectory(t)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			mutate(t, path)
			deps := receiverTestDeps(path, receipt.CreatedAt)
			input := &receiverPanicReader{}
			if err := credentialReceive(nil, input, io.Discard, deps); err == nil {
				t.Fatal("unsafe filesystem admitted")
			}
			if input.called {
				t.Fatal("unsafe filesystem read stdin")
			}
			if _, err := os.Stat(filepath.Join(path, credentialReceiverBudgetFile)); !os.IsNotExist(err) {
				t.Fatal("unsafe filesystem authorized admission")
			}

		})
	}
}

type receiverPanicReader struct{ called bool }

func (r *receiverPanicReader) Read([]byte) (int, error) {
	r.called = true
	panic("stdin read before admission")
}

func TestCredentialReceiverACLAndPathSubstitution(t *testing.T) {
	t.Run("ACL", func(t *testing.T) {
		path := credentialReceiverTestDirectory(t)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		// POSIX ACL v2: owner rwx, named user nobody r, group none, mask r, other none.
		acl := make([]byte, 4+5*8)
		binary.LittleEndian.PutUint32(acl, 2)
		entries := [][3]uint32{{1, 7, 0xffffffff}, {2, 4, 65534}, {4, 0, 0xffffffff}, {16, 4, 0xffffffff}, {32, 0, 0xffffffff}}
		for i, e := range entries {
			at := 4 + 8*i
			binary.LittleEndian.PutUint16(acl[at:], uint16(e[0]))
			binary.LittleEndian.PutUint16(acl[at+2:], uint16(e[1]))
			binary.LittleEndian.PutUint32(acl[at+4:], e[2])
		}
		if err := unix.Setxattr(path, "system.posix_acl_access", acl, 0); err != nil {
			t.Fatal(err)
		}
		if store, err := openCredentialReceiverStore(path); err == nil {
			store.close()
			t.Fatal("ACL admitted")
		}
	})
	t.Run("ancestor replacement", func(t *testing.T) {
		path := credentialReceiverTestDirectory(t)
		store, err := openCredentialReceiverStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.close()
		moved := path + "-moved"
		if err := os.Rename(path, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := store.admit(context.Background(), time.Now().UTC()); err == nil {
			t.Fatal("replaced path admitted")
		}
		if _, err := os.Stat(filepath.Join(path, credentialReceiverBudgetFile)); !os.IsNotExist(err) {
			t.Fatal("replacement directory written")
		}
	})
}

func TestCredentialReceiverStrictMetadata(t *testing.T) {
	_, receipt := receiverFixture(t)
	mutations := map[string]func([]byte) []byte{
		"case alias":        func(b []byte) []byte { return append([]byte(`{"State":"submitted",`), b[1:]...) },
		"missing mandatory": func(b []byte) []byte { return bytes.ReplaceAll(b, []byte(`"key_fingerprint":"",`), nil) },
		"null metadata": func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte(`"key_fingerprint":""`), []byte(`"key_fingerprint":null`))
		},
		"duplicate": func(b []byte) []byte { return append([]byte(`{"state":"submitted",`), b[1:]...) },
		"unknown":   func(b []byte) []byte { return append([]byte(`{"extra":1,`), b[1:]...) },
		"trailing":  func(b []byte) []byte { return append(b, []byte(` {}`)...) },
		"unknown channel": func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte(credentialChannel), []byte("unapproved-protocol"))
		},
		"wrong recipient": func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte(credentialRecipient), []byte("other@example.net"))
		},
		"wrong ID": func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte(receipt.OperationID), []byte("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
		},
		"future timestamp": func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("2026-08-30"), []byte("2027-08-30")) },
		"invalid state":    func(b []byte) []byte { return bytes.ReplaceAll(b, []byte(`"submitted"`), []byte(`"safe-to-retry"`)) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			path := credentialReceiverTestDirectory(t)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(receipt)
			if err := os.WriteFile(filepath.Join(path, receipt.OperationID+".json"), mutate(data), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := openCredentialReceiverStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			if err := store.admit(context.Background(), receipt.CreatedAt); err == nil {
				t.Fatal("invalid persisted metadata admitted")
			}
		})
	}
}

func TestCredentialReceiverCancellationAfterStartIsUnknown(t *testing.T) {
	wire, receipt := receiverFixture(t)
	path := credentialReceiverTestDirectory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deps := receiverTestDeps(path, receipt.CreatedAt)
	deps.deliver = func(context.Context, []byte) (bool, error) { cancel(); return true, nil }
	if err := credentialReceiveContext(ctx, nil, bytes.NewReader(wire), io.Discard, deps); err == nil {
		t.Fatal("cancelled delivery acknowledged")
	}
	saved := receiverReadFixture(t, filepath.Join(path, receipt.OperationID+".json"))
	if saved.State != "unknown" {
		t.Fatalf("cancelled outcome=%s", saved.State)
	}
}

func TestCredentialReceiverSignalFinalizesUnknown(t *testing.T) {
	wire, receipt := receiverFixture(t)
	path := credentialReceiverTestDirectory(t)
	command := receiverCommand(t, path, "signal")
	command.Stdin = bytes.NewReader(wire)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitReceiverFile(t, filepath.Join(filepath.Dir(path), "child"))
	start := time.Now()
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil || time.Since(start) > time.Second {
		t.Fatalf("signal termination was not bounded: err=%v elapsed=%s", err, time.Since(start))
	}
	saved := receiverReadFixture(t, filepath.Join(path, receipt.OperationID+".json"))
	if saved.State != "unknown" || !strings.Contains(output.String(), "outcome is unknown") {
		t.Fatalf("signal receipt=%s output=%s", saved.State, output.String())
	}
}

func TestCredentialReceiverBudgetSchemaAndBootstrapClock(t *testing.T) {
	_, receipt := receiverFixture(t)
	for _, value := range []string{
		`{"version":1,"day":"2026-08-30","admissions":0,"last_admission":"2026-08-30T21:00:00Z"}`,
		`{"version":1,"day":"2026-08-31","admissions":1,"last_admission":"2026-08-30T21:00:00Z"}`,
		`{"version":1,"day":"2026-08-30","admissions":129,"last_admission":"2026-08-30T21:00:00Z"}`,
		`{"version":1,"day":"2026-08-30","admissions":1,"admissions":2,"last_admission":"2026-08-30T21:00:00Z"}`,
	} {
		path := credentialReceiverTestDirectory(t)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, credentialReceiverBudgetFile), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		store, err := openCredentialReceiverStore(path)
		if err != nil {
			t.Fatal(err)
		}
		err = store.admit(context.Background(), receipt.CreatedAt)
		store.close()
		if err == nil {
			t.Fatalf("budget accepted: %s", value)
		}
	}
	path := credentialReceiverTestDirectory(t)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	receiverWriteFixture(t, filepath.Join(path, receipt.OperationID+".json"), receipt)
	store, err := openCredentialReceiverStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if err := store.admit(context.Background(), receipt.CreatedAt.Add(-time.Second)); err == nil {
		t.Fatal("first rate journal ignored retained receipt clock")
	}
}

func TestCredentialReceiverAggregateCapacityReservesFinalizationAndReplay(t *testing.T) {
	wire, receipt := receiverFixture(t)
	path := credentialReceiverTestDirectory(t)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	// Accepted JSON whitespace exercises the independent byte budget without making
	// thousands of oversized or schema-invalid files that fail for another reason.
	remaining := int64(credentialReceiverStoreBytes - 2*credentialReceiverBudgetBytes - credentialReceiptLimit)
	for i := 0; remaining > 0; i++ {
		current := receipt
		if i > 0 {
			current.OperationID = fmt.Sprintf("%08x-e89b-12d3-a456-426614174000", i)
			current.MessageID = credentialMessageID(current.OperationID)
		}
		data, _ := json.Marshal(current)
		size := int64(credentialReceiptLimit)
		if remaining < size {
			size = remaining
		}
		if size < int64(len(data)) {
			break
		}
		data = append(data, bytes.Repeat([]byte(" "), int(size)-len(data))...)
		if err := os.WriteFile(filepath.Join(path, current.OperationID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		remaining -= int64(len(data))
	}
	deps := receiverTestDeps(path, receipt.CreatedAt)
	deps.deliver = func(context.Context, []byte) (bool, error) { t.Fatal("byte budget allowed submit"); return false, nil }
	if err := credentialReceive(nil, bytes.NewReader(wire), io.Discard, deps); err != nil {
		t.Fatalf("replay lacks reserved rate journal room: %v", err)
	}
	newWire := bytes.ReplaceAll(wire, []byte(receipt.OperationID), []byte("ffffffff-e89b-12d3-a456-426614174000"))
	if err := credentialReceive(nil, bytes.NewReader(newWire), io.Discard, deps); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("aggregate capacity result: %v", err)
	}
}

func TestCredentialReceiverReceiptReplacementAndFinalizationFailure(t *testing.T) {
	wire, receipt := receiverFixture(t)
	t.Run("path replacement before submit", func(t *testing.T) {
		path := credentialReceiverTestDirectory(t)
		store, err := openCredentialReceiverStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.close()
		old := filepath.Join(path, "existing.json")
		if err := os.WriteFile(old, []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(old, old+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(old+"-moved", old); err != nil {
			t.Fatal(err)
		}
		if err := store.write("existing.json", []byte("after"), []byte("before")); err == nil {
			t.Fatal("symlink target replacement accepted")
		}
	})
	t.Run("final receipt cannot commit", func(t *testing.T) {
		path := credentialReceiverTestDirectory(t)
		deps := receiverTestDeps(path, receipt.CreatedAt)
		deps.deliver = func(context.Context, []byte) (bool, error) {
			if err := os.WriteFile(filepath.Join(path, credentialReceiverTemp), []byte("retained evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			return true, nil
		}
		var output bytes.Buffer
		if err := credentialReceive(nil, bytes.NewReader(wire), &output, deps); err == nil || output.Len() != 0 {
			t.Fatal("finalization failure acknowledged")
		}
		saved := receiverReadFixture(t, filepath.Join(path, receipt.OperationID+".json"))
		if saved.State != "pending" {
			t.Fatal("pending evidence lost")
		}
		contents, _ := os.ReadFile(filepath.Join(path, credentialReceiverTemp))
		if string(contents) != "retained evidence" {
			t.Fatal("retained temporary pruned")
		}
	})
}

func TestCredentialReceiverSuccessfulLeaderStillStopsProcessGroup(t *testing.T) {
	path := credentialReceiverTestDirectory(t)
	childPath := filepath.Join(filepath.Dir(path), "successful-child")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started, err := runCredentialReceiverSendmail(ctx, "/bin/sh", []string{"-c", `sleep 120 & echo $! > "$1"; exit 0`, "fixture", childPath}, nil)
	if !started || err != nil {
		t.Fatalf("successful fake sendmail: started=%v error=%v", started, err)
	}
	data, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	assertReceiverProcessStopped(t, pid)
}
