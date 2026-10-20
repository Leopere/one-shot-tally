package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStateLockDoesNotStealAnOldLiveHolder(t *testing.T) {
	path := filepath.Join(retainedTestDir(t), "state.json")
	unlock, err := acquireStateLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		second, err := acquireStateLock(path)
		if err == nil {
			err = second()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("second holder entered before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second holder did not acquire released lock")
	}
}

func TestStateLockReleasedOnProcessExit(t *testing.T) {
	if path := os.Getenv("ONE_SHOT_LOCK_TEST_CHILD"); path != "" {
		if _, err := acquireStateLock(path); err != nil {
			t.Fatal(err)
		}
		fmt.Println("locked")
		// Exit without running an unlock callback.
		os.Exit(0)
	}
	path := filepath.Join(retainedTestDir(t), "state.json")
	child := exec.Command(os.Args[0], "-test.run=^TestStateLockReleasedOnProcessExit$")
	child.Env = append(os.Environ(), "ONE_SHOT_LOCK_TEST_CHILD="+path)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	if err := child.Wait(); err != nil || readErr != nil || line != "locked\n" {
		t.Fatalf("lock child failed: wait=%v read=%v output=%q", err, readErr, line)
	}
	unlock, err := acquireStateLock(path)
	if err != nil {
		t.Fatalf("process exit left a stuck lock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}
