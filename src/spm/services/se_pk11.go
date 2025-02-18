// Copyright lowRISC contributors (OpenTitan project).
// Licensed under the Apache License, Version 2.0, see LICENSE for details.
// SPDX-License-Identifier: Apache-2.0

// Secure element implementation using an HSM.
package se

import (
	"crypto"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"reflect"

	"golang.org/x/crypto/sha3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lowRISC/opentitan-provisioning/src/cert/signer"
	"github.com/lowRISC/opentitan-provisioning/src/pk11"
)

// sessionQueue implements a thread-safe HSM session queue. See `insert` and
// `getHandle` functions for more details.
type sessionQueue struct {
	// numSessions is the number of sessions managed by the queue.
	numSessions int

	// s is an HSM session channel.
	s chan *pk11.Session
}

// newSessionQueue creates a session queue with a channel of depth `num`.
func newSessionQueue(num int) *sessionQueue {
	return &sessionQueue{
		numSessions: num,
		s:           make(chan *pk11.Session, num),
	}
}

// insert adds a new session `s` to the session queue.
func (q *sessionQueue) insert(s *pk11.Session) error {
	// TODO: Consider adding a timeout context to avoid deadlocks if the caller
	// forgets to call the release function returned by the `getHandle`
	// function.
	if len(q.s) >= q.numSessions {
		return errors.New("Reached maximum session queue capacity.")
	}
	q.s <- s
	return nil
}

// getHandle returns a session from the queue and a release function to
// get the session back into the queue. Recommended use:
//
//	session, release := s.getHandle()
//	defer release()
//
// Note: failing to call the release function can result into deadlocks
// if the queue remains empty after calling the `insert` function.
func (q *sessionQueue) getHandle() (*pk11.Session, func()) {
	s := <-q.s
	release := func() {
		q.insert(s)
	}
	return s, release
}

// HSMConfig contains parameters used to configure a new HSM instance with the
// `NewHSM` function.
type HSMConfig struct {
	// soPath is the path to the PKCS#11 library used to connect to the HSM.
	SOPath string

	// slotID is the HSM slot ID.
	SlotID int

	// HSMPassword is the Crypto User HSM password.
	HSMPassword string

	// NumSessions configures the number of sessions to open in `SlotID`.
	NumSessions int

	// SymmetricKeys contains the list of symmetric key labels to use for
	// retrieving long-lived symmetric keys on the HSM.
	SymmetricKeys []string

	// PrivateKeys contains the list of private key labels to use for
	// retrieving long-lived private keys on the HSM.
	PrivateKeys []string

	// hsmType contains the type of the HSM (SoftHSM or NetworkHSM)
	HSMType pk11.HSMType
}

// HSM is a wrapper over a pk11 session that conforms to the SPM interface.
type HSM struct {
	// UIDs of key objects to use for retrieving long-lived symmetric keys on
	// the HSM.
	SymmetricKeys map[string][]byte

	// UIDs of key objects to use for retrieving long-lived private keys on
	// the HSM.
	PrivateKeys map[string][]byte

	// The PKCS#11 session we're working with.
	sessions *sessionQueue
}

// openSessions opens `numSessions` sessions on the HSM `tokSlot` slot number.
// Logs in as crypto user with `hsmPW` password. Connects via PKCS#11 shared
// library in `soPath`.
func openSessions(hsmType pk11.HSMType, soPath, hsmPW string, tokSlot, numSessions int) (*sessionQueue, error) {
	mod, err := pk11.Load(hsmType, soPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fail to load pk11: %v", err)
	}
	toks, err := mod.Tokens()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to open tokens: %v", err)
	}
	if tokSlot >= len(toks) {
		return nil, status.Errorf(codes.Internal, "fail to find slot number: %v", err)
	}

	sessions := newSessionQueue(numSessions)
	for i := 0; i < numSessions; i++ {
		s, err := toks[tokSlot].OpenSession()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fail to open session to HSM: %v", err)
		}

		err = s.Login(pk11.NormalUser, hsmPW)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fail to login into the HSM: %v", err)
		}

		err = sessions.insert(s)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to enqueue session: %v", err)
		}
	}
	return sessions, nil
}

// getKeyIDByLabel returns the object ID from a given label
func getKeyIDByLabel(session *pk11.Session, classKeyType pk11.ClassAttribute, label string) ([]byte, error) {
	keyObj, err := session.FindKeyByLabel(classKeyType, label)
	if err != nil {
		return nil, err
	}

	id, err := keyObj.UID()
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, status.Errorf(codes.Internal, "fail to find ID attribute")
	}
	return id, nil
}

// NewHSM creates a new instance of HSM, with dedicated session and keys.
func NewHSM(cfg HSMConfig) (*HSM, error) {
	sq, err := openSessions(cfg.HSMType, cfg.SOPath, cfg.HSMPassword, cfg.SlotID, cfg.NumSessions)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fail to get session: %v", err)
	}

	hsm := &HSM{
		sessions: sq,
	}

	session, release := hsm.sessions.getHandle()
	defer release()

	hsm.SymmetricKeys = make(map[string][]byte)
	for _, key := range cfg.SymmetricKeys {
		id, err := getKeyIDByLabel(session, pk11.ClassSecretKey, key)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fail to find symmetric key ID: %q, error: %v", key, err)
		}
		hsm.SymmetricKeys[key] = id
	}

	hsm.PrivateKeys = make(map[string][]byte)
	for _, key := range cfg.PrivateKeys {
		id, err := getKeyIDByLabel(session, pk11.ClassPrivateKey, key)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fail to find private key ID: %q, error: %v", key, err)
		}
		hsm.PrivateKeys[key] = id
	}

	return hsm, nil
}

type CmdFunc func(*pk11.Session) error

// ExecuteCmd executes a command with a session handle in a thread safe way.
func (h *HSM) ExecuteCmd(cmd CmdFunc) error {
	session, release := h.sessions.getHandle()
	defer release()
	return cmd(session)
}

// The label used for expanding the transport secret.
var transportKeyLabel = []byte("transport key")

// deriveTransportSecret derives the transport secret for the device with the
// given ID, and returns a handle to it.
func (h *HSM) deriveTransportSecret(session *pk11.Session, deviceId []byte) (pk11.SecretKey, error) {
	kt, ok := h.SymmetricKeys["KT"]
	if !ok {
		return pk11.SecretKey{}, status.Errorf(codes.Internal, "failed to find KT key UID")
	}
	transportStatic, err := session.FindSecretKey(kt)
	if err != nil {
		return pk11.SecretKey{}, err
	}
	return transportStatic.HKDFDeriveAES(crypto.SHA256, deviceId, transportKeyLabel, 128, &pk11.KeyOptions{Extractable: true})
}

// DeriveAndWrapTransportSecret generates a fresh secret for the device with the
// given ID, wrapping it with the global secret.
//
// See SPM.
func (h *HSM) DeriveAndWrapTransportSecret(deviceId []byte) ([]byte, error) {
	session, release := h.sessions.getHandle()
	defer release()

	kg, ok := h.SymmetricKeys["KG"]
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to find KG key UID")
	}

	global, err := session.FindSecretKey(kg)
	if err != nil {
		return nil, err
	}

	transport, err := h.deriveTransportSecret(session, deviceId)
	if err != nil {
		return nil, err
	}

	ciphertext, _, err := global.WrapAES(transport)
	return ciphertext, err
}

// VerifySession verifies that a session to the HSM for a given SKU is active
func (h *HSM) VerifySession() error {
	session, release := h.sessions.getHandle()
	defer release()

	kca, ok := h.PrivateKeys["KCAPriv"]
	if !ok {
		return status.Errorf(codes.Internal, "failed to find KCAPriv key UID")
	}

	_, err := session.FindPrivateKey(kca)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to verify session: %v", err)
	}
	return nil
}

// GenerateRandom returns random data extracted from the HSM.
func (h *HSM) GenerateRandom(length int) ([]byte, error) {
	session, release := h.sessions.getHandle()
	defer release()
	return session.GenerateRandom(length)
}

// GenerateKeyPairAndCert generates certificate and the associated key pair;
// must be one of RSAParams or elliptic.Curve.
func (h *HSM) GenerateKeyPairAndCert(caCert *x509.Certificate, params []SigningParams) ([]CertInfo, error) {
	session, release := h.sessions.getHandle()
	defer release()

	kca, ok := h.PrivateKeys["KCAPriv"]
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to find KCAPriv key UID")
	}

	caObj, err := session.FindPrivateKey(kca)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to find Kca key object: %v", err)
	}

	ca, err := caObj.Signer()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get Kca signer: %v", err)
	}

	kg, ok := h.SymmetricKeys["KG"]
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to find KG key UID")
	}

	wi, err := session.FindSecretKey(kg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get KG key object: %v", err)
	}

	certs := []CertInfo{}
	for _, p := range params {
		var kp pk11.KeyPair
		switch k := p.KeyParams.(type) {
		case RSAParams:
			kp, err = session.GenerateRSA(uint(k.ModBits), uint(k.Exp), &pk11.KeyOptions{Extractable: true})
			if err != nil {
				return nil, fmt.Errorf("failed GenerateRSA: %v", err)
			}
		case elliptic.Curve:
			kp, err = session.GenerateECDSA(k, &pk11.KeyOptions{Extractable: true})
			if err != nil {
				return nil, fmt.Errorf("failed GenerateECDSA: %v", err)
			}
		default:
			panic(fmt.Sprintf("unknown key param type: %s", reflect.TypeOf(p)))
		}

		var public any
		public, err = kp.PublicKey.ExportKey()
		if err != nil {
			return nil, fmt.Errorf("failed to export kp public key: %v", err)
		}

		var cert CertInfo
		cert.WrappedKey, cert.Iv, err = wi.WrapAES(kp.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to wrap kp with wi: %v", err)
		}
		cert.Cert, err = signer.CreateCertificate(p.Template, caCert, public, ca)
		if err != nil {
			return nil, fmt.Errorf("failed to create certificate: %v", err)
		}
		certs = append(certs, cert)

		// Delete the keys after they are used once
		session.DestroyKeyPairObject(kp)
	}

	return certs, nil
}

// GenerateSymmetricKeys generates a symmetric key.
func (h *HSM) GenerateSymmetricKeys(params []*SymmetricKeygenParams) ([][]byte, error) {
	session, release := h.sessions.getHandle()
	defer release()
	var symmetricKeys [][]byte

	for _, p := range params {
		// Select the seed asset to use (High or Low security seed).
		var seed pk11.SecretKey
		var err error
		if p.UseHighSecuritySeed {
			khs, ok := h.SymmetricKeys["HighSecKdfSeed"]
			if !ok {
				return nil, status.Errorf(codes.Internal, "failed to find HighSecKdfSeed key UID")
			}
			seed, err = session.FindSecretKey(khs)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to get KHsks key object: %v", err)
			}
		} else {
			kls, ok := h.SymmetricKeys["LowSecKdfSeed"]
			if !ok {
				return nil, status.Errorf(codes.Internal, "failed to find LowSecKdfSeed key UID")
			}
			seed, err = session.FindSecretKey(kls)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to get KLsks key object: %v", err)
			}
		}

		// Generate key from seed and extract.
		seKey, err := seed.HKDFDeriveAES(crypto.SHA256, []byte(p.Sku),
			[]byte(p.Diversifier), p.SizeInBits, &pk11.KeyOptions{Extractable: true})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed HKDFDeriveAES: %v", err)
		}

		// Extract the key from the SE.
		exportedKey, err := seKey.ExportKey()
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to extract symmetric key: %v", err)
		}

		// Parse and format the key bytes.
		aesKey, ok := exportedKey.(pk11.AESKey)
		if !ok {
			return nil, status.Errorf(codes.Internal,
				"failed to parse extracted symmetric key: %v", ok)
		}
		keyBytes := []byte(aesKey)
		if p.KeyType == SymmetricKeyTypeHashedOtLcToken {
			// OpenTitan lifecycle tokens are stored in OTP in hashed form using the
			// cSHAKE128 algorithm with the "LC_CTRL" customization string.
			hasher := sha3.NewCShake128([]byte(""), []byte("LC_CTRL"))
			hasher.Write(keyBytes)
			hasher.Read(keyBytes)
		}

		symmetricKeys = append(symmetricKeys, keyBytes)
	}

	return symmetricKeys, nil
}

// OIDs for ECDSA signature algorithms corresponding to SHA-256, SHA-384 and
// SHA-512.
//
// See https://datatracker.ietf.org/doc/html/rfc5758#section-3.1. The following
// text is copied from the spec for reference:
//
// ecdsa-with-SHA256 OBJECT IDENTIFIER ::= { iso(1) member-body(2)
//   us(840) ansi-X9-62(10045) signatures(4) ecdsa-with-SHA2(3) 2 }

// ecdsa-with-SHA384 OBJECT IDENTIFIER ::= { iso(1) member-body(2)
//   us(840) ansi-X9-62(10045) signatures(4) ecdsa-with-SHA2(3) 3 }

// ecdsa-with-SHA512 OBJECT IDENTIFIER ::= { iso(1) member-body(2)
//
//	us(840) ansi-X9-62(10045) signatures(4) ecdsa-with-SHA2(3) 4 }
var (
	oidECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSAWithSHA384 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSAWithSHA512 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
)

// oidFromSignatureAlgorithm returns the ASN.1 object identifier for the given
// signature algorithm.
func oidFromSignatureAlgorithm(alg x509.SignatureAlgorithm) (asn1.ObjectIdentifier, error) {
	switch alg {
	case x509.ECDSAWithSHA256:
		return oidECDSAWithSHA256, nil
	case x509.ECDSAWithSHA384:
		return oidECDSAWithSHA384, nil
	case x509.ECDSAWithSHA512:
		return oidECDSAWithSHA512, nil
	default:
		return nil, fmt.Errorf("unsupported signature algorithm: %v", alg)
	}
}

// hashFromSignatureAlgorithm returns the crypto.Hash for the given signature
// algorithm.
func hashFromSignatureAlgorithm(alg x509.SignatureAlgorithm) (crypto.Hash, error) {
	switch alg {
	case x509.ECDSAWithSHA256:
		return crypto.SHA256, nil
	case x509.ECDSAWithSHA384:
		return crypto.SHA384, nil
	case x509.ECDSAWithSHA512:
		return crypto.SHA512, nil
	default:
		return 0, fmt.Errorf("unsupported signature algorithm: %v", alg)
	}
}

func (h *HSM) EndorseCert(tbs []byte, params EndorseCertParams) ([]byte, error) {
	session, release := h.sessions.getHandle()
	defer release()

	keyID, err := getKeyIDByLabel(session, pk11.ClassPrivateKey, params.KeyLabel)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fail to find key with label: %q, error: %v", params.KeyLabel, err)
	}

	key, err := session.FindPrivateKey(keyID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to find key object %q: %v", keyID, err)
	}

	hash, err := hashFromSignatureAlgorithm(params.SignatureAlgorithm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get hash from signature algorithm: %v", err)
	}

	rb, sb, err := key.SignECDSA(hash, tbs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to sign: %v", err)
	}

	// Encode the signature as ASN.1 DER.
	var sig struct{ R, S *big.Int }
	sig.R, sig.S = new(big.Int), new(big.Int)
	sig.R.SetBytes(rb)
	sig.S.SetBytes(sb)
	s, err := asn1.Marshal(sig)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal signature: %v", err)
	}

	sigType, err := oidFromSignatureAlgorithm(params.SignatureAlgorithm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get signature algorithm OID: %v", err)
	}

	certRaw := struct {
		TBSCertificate     asn1.RawValue
		SignatureAlgorithm pkix.AlgorithmIdentifier
		SignatureValue     asn1.BitString
	}{
		TBSCertificate:     asn1.RawValue{FullBytes: tbs},
		SignatureAlgorithm: pkix.AlgorithmIdentifier{Algorithm: sigType},
		SignatureValue:     asn1.BitString{Bytes: s, BitLength: len(s) * 8},
	}
	cert, err := asn1.Marshal(certRaw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal certificate: %v", err)
	}
	return cert, nil
}
