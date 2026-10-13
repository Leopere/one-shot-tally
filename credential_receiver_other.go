//go:build !linux

package main

import (
	"context"
	"errors"
	"io"
)

func credentialReceive(_ []string, _ io.Reader, _ io.Writer, _ credentialReceiveDependencies) error {
	return errors.New("credential receive requires Linux")
}

func deliverCredentialSendmail(_ context.Context, _ []byte) (bool, error) {
	return false, errors.New("credential receive requires Linux")
}
