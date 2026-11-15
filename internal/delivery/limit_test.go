package delivery

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/seal"
)

// A target (or anyone on the path before TLS starts) that sends an endless
// multi-line reply must not make Postwerk buffer it: the SMTP client limits
// the length of one line, not the number of continuation lines, so the
// connection stops after maxTargetBytes.
func TestEndlessReplyIsCut(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	sent := make(chan int, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		chunk := bytes.Repeat([]byte("220-"+string(bytes.Repeat([]byte("x"), 990))+"\r\n"), 64)
		n := 0
		for n < 64<<20 {
			if _, err := c.Write(chunk); err != nil {
				break
			}
			n += len(chunk)
		}
		sent <- n
	}()

	m := New(Options{Sealer: seal.Off(), Targets: []config.Target{{
		Name: "endless", Scheme: config.SchemePlain, Host: "127.0.0.1", Addr: l.Addr().String(),
	}}})
	start := time.Now()
	_, err = m.workers[0].connect(context.Background())
	if !errors.Is(err, errTooMuchFromTarget) {
		t.Fatalf("connect = %v, want errTooMuchFromTarget", err)
	}
	if n := <-sent; n >= 64<<20 {
		t.Fatalf("the whole 64 MiB were read (%d bytes)", n)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}
