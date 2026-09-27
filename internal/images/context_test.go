package images

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestContextReaderStopsBeforeReadingSourceAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := bytes.NewBufferString("must remain unread")
	buf := make([]byte, 32)
	if n, err := (contextReader{ctx: ctx, r: source}).Read(buf); !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("canceled reader n=%d err=%v", n, err)
	}
	if source.String() != "must remain unread" {
		t.Fatalf("context reader consumed source after cancellation: %q", source.String())
	}
}
