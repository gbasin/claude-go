package api

import (
	"context"
	"crypto/x509"
	"io"
	"os"
	"testing"
	"time"
)

func TestUTLSResumeVsTicket(t *testing.T) {
	if os.Getenv("UTLS_SMOKE") == "" {
		t.Skip()
	}
	roots, _ := x509.SystemCertPool()
	pem, _ := os.ReadFile("/tmp/tlscap/ca.crt")
	roots.AppendCertsFromPEM(pem)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := claudeTLSDial(ctx, "tcp", "localhost:8897", "localhost", roots)
		cancel()
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
		io.ReadAll(c)
		c.Close()
		time.Sleep(100 * time.Millisecond)
	}
}
