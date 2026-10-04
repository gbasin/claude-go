package api

import (
	"context"
	"crypto/x509"
	"os"
	"testing"
	"time"
)

func TestUTLSHandshake(t *testing.T) {
	if os.Getenv("UTLS_SMOKE") == "" {
		t.Skip("set UTLS_SMOKE=1")
	}
	roots, _ := x509.SystemCertPool()
	pem, _ := os.ReadFile("/tmp/tlscap/ca.crt")
	roots.AppendCertsFromPEM(pem)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := claudeTLSDial(ctx, "tcp", "localhost:8899", "localhost", roots)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	t.Log(string(buf[:n]))
}
