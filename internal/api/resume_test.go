package api

import (
	"context"
	"crypto/x509"
	"os"
	"testing"
	"time"
)

func TestUTLSResumption(t *testing.T) {
	if os.Getenv("UTLS_SMOKE") == "" {
		t.Skip()
	}
	roots, _ := x509.SystemCertPool()
	pem, _ := os.ReadFile("/tmp/tlscap/ca.crt")
	roots.AppendCertsFromPEM(pem)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := claudeTLSDial(ctx, "tcp", "localhost:8899", "localhost", roots)
		cancel()
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Logf("dial %d ok", i)
		c.Close()
	}
}
