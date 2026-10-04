package api

import (
	"bufio"
	"context"
	"crypto/x509"
	"net"

	tlsu "github.com/refraction-networking/utls"
)

// claudeHelloSpec mirrors Bun's ClientHello as captured live from claude
// 2.1.289 (BoringSSL):
//
//	JA3: 771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,
//	     23-65281-10-11-35-16-5-13-18-51-45-43,4588-29-23-24,0
//
// TLS 1.3+1.2, http/1.1-only ALPN, X25519MLKEM768+x25519 keyshares.
// NOTE: ApplyPreset mutates the spec in place (SNI, keyshare payloads,
// session ticket) — build a fresh one per connection.
func claudeHelloSpec() tlsu.ClientHelloSpec {
	return tlsu.ClientHelloSpec{
		TLSVersMin: tlsu.VersionTLS12,
		TLSVersMax: tlsu.VersionTLS13,
		CipherSuites: []uint16{
			tlsu.TLS_AES_128_GCM_SHA256,                  // 1301
			tlsu.TLS_AES_256_GCM_SHA384,                  // 1302
			tlsu.TLS_CHACHA20_POLY1305_SHA256,            // 1303
			tlsu.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // c02b
			tlsu.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,   // c02f
			tlsu.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, // c02c
			tlsu.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,   // c030
			tlsu.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,  // cca9
			tlsu.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,    // cca8
			tlsu.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,    // c009
			tlsu.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,      // c013
			tlsu.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,    // c00a
			tlsu.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,      // c014
			tlsu.TLS_RSA_WITH_AES_128_GCM_SHA256,         // 009c
			tlsu.TLS_RSA_WITH_AES_256_GCM_SHA384,         // 009d
			tlsu.TLS_RSA_WITH_AES_128_CBC_SHA,            // 002f
			tlsu.TLS_RSA_WITH_AES_256_CBC_SHA,            // 0035
		},
		Extensions: []tlsu.TLSExtension{
			&tlsu.SNIExtension{},                  // 0
			&tlsu.ExtendedMasterSecretExtension{}, // 23
			&tlsu.RenegotiationInfoExtension{Renegotiation: tlsu.RenegotiateOnceAsClient}, // 65281
			&tlsu.SupportedCurvesExtension{Curves: []tlsu.CurveID{ // 10
				tlsu.X25519MLKEM768, tlsu.CurveID(0x001d), tlsu.CurveP256, tlsu.CurveP384,
			}},
			&tlsu.SupportedPointsExtension{SupportedPoints: []byte{0}}, // 11
			&tlsu.SessionTicketExtension{},                             // 35
			&tlsu.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},   // 16
			&tlsu.StatusRequestExtension{},                             // 5
			&tlsu.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tlsu.SignatureScheme{ // 13
				tlsu.ECDSAWithP256AndSHA256, tlsu.PSSWithSHA256,
				tlsu.PKCS1WithSHA256, tlsu.ECDSAWithP384AndSHA384,
				tlsu.PSSWithSHA384, tlsu.PKCS1WithSHA384,
				tlsu.PSSWithSHA512, tlsu.PKCS1WithSHA512,
				tlsu.PKCS1WithSHA1,
			}},
			&tlsu.SCTExtension{}, // 18
			&tlsu.KeyShareExtension{KeyShares: []tlsu.KeyShare{ // 51
				{Group: tlsu.X25519MLKEM768},
				{Group: tlsu.CurveID(0x001d)},
			}},
			&tlsu.PSKKeyExchangeModesExtension{Modes: []uint8{tlsu.PskModeDHE}}, // 45
			&tlsu.SupportedVersionsExtension{Versions: []uint16{ // 43
				tlsu.VersionTLS13, tlsu.VersionTLS12,
			}},
			claudePSKExtension(), // 41 — emits only when resuming, like real
		},
	}

}

// claudeTLSDial dials TLS with real's ClientHello bytes via utls.
// Env CLAUDE_GO_STD_TLS=1 drops back to crypto/tls (debugging).
func claudeTLSDial(ctx context.Context, network, addr, serverName string, roots *x509.CertPool) (net.Conn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := &tlsu.Config{
		ServerName:         serverName,
		MinVersion:         tlsu.VersionTLS12,
		RootCAs:            roots,
		ClientSessionCache: claudeSessionCache,
		OmitEmptyPsk:       true,
	}
	uc := tlsu.UClient(raw, cfg, tlsu.HelloCustom)
	if err := uc.ApplyPreset(claudeHelloSpecPtr()); err != nil {
		raw.Close()
		return nil, err
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return uc, nil
}

func claudeHelloSpecPtr() *tlsu.ClientHelloSpec {
	s := claudeHelloSpec()
	return &s
}

// claudePSKExtension is the resumption PSK (ext 41). OmitEmptyPsk keeps
// it off the wire on fresh handshakes; on reuse it carries the ticket
// binder exactly like real's second-connection hello.
func claudePSKExtension() tlsu.TLSExtension {
	e := &tlsu.UtlsPreSharedKeyExtension{}
	e.SetOmitEmptyPsk(true)
	return e
}

// shared across dials so server-issued tickets resume — real reuses them.
var claudeSessionCache = tlsu.NewLRUClientSessionCache(32)

// claudeTLSHandshake wraps an existing conn (e.g. a proxy tunnel) in utls.
func claudeTLSHandshake(ctx context.Context, conn net.Conn, serverName string, roots *x509.CertPool) (net.Conn, error) {
	return claudeTLSHandshakeBufio(ctx, conn, nil, serverName, roots)
}

func claudeTLSHandshakeBufio(ctx context.Context, conn net.Conn, br *bufio.Reader, serverName string, roots *x509.CertPool) (net.Conn, error) {
	cfg := &tlsu.Config{
		ServerName:         serverName,
		MinVersion:         tlsu.VersionTLS12,
		RootCAs:            roots,
		ClientSessionCache: claudeSessionCache,
		OmitEmptyPsk:       true,
	}
	uc := tlsu.UClient(conn, cfg, tlsu.HelloCustom)
	if err := uc.ApplyPreset(claudeHelloSpecPtr()); err != nil {
		return nil, err
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return uc, nil
}
