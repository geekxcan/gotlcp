// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tlcp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	x509 "github.com/emmansun/gmsm/smx509"
	"hash"
	"sync/atomic"
	"time"
)

type clientHandshakeState struct {
	c            *Conn
	ctx          context.Context
	serverHello  *serverHelloMsg
	hello        *clientHelloMsg
	suite        *cipherSuite
	finishedHash finishedHash
	masterSecret []byte
	session      *SessionState
}

func (c *Conn) makeClientHello() (*clientHelloMsg, error) {
	config := c.config
	if len(config.ServerName) == 0 && !config.InsecureSkipVerify {
		return nil, errors.New("tlcp: either ServerName or InsecureSkipVerify must be specified in the tls.Config")
	}

	supportVers := config.supportedVersions(roleClient)
	if len(supportVers) == 0 {
		return nil, errors.New("tlcp: no supported versions satisfy MinVersion and MaxVersion")
	}

	clientHelloVersion := config.maxSupportedVersion(roleClient)

	hello := &clientHelloMsg{
		vers:               clientHelloVersion,
		compressionMethods: []uint8{compressionNone},
		random:             make([]byte, 32),
	}

	preferenceOrder := cipherSuitesPreferenceOrder
	configCipherSuites := config.cipherSuites()
	hello.cipherSuites = make([]uint16, 0, len(configCipherSuites))
	// 选择匹配的密码套件
	for _, suiteId := range preferenceOrder {
		suite := mutualCipherSuite(configCipherSuites, suiteId)
		if suite == nil {
			continue
		}
		hello.cipherSuites = append(hello.cipherSuites, suiteId)
	}

	// 生成客户端随机数
	var err error
	hello.random, err = c.tlcpRand()
	if err != nil {
		return nil, errors.New("tlcp: short read from Rand: " + err.Error())
	}

	return hello, nil
}

func (c *Conn) clientHandshake(ctx context.Context) (err error) {
	if c.config == nil {
		c.config = defaultConfig()
	}

	// This may be a renegotiation handshake, in which case some fields
	// need to be reset.
	c.didResume = false

	hello, err := c.makeClientHello()
	if err != nil {
		return err
	}
	c.serverName = c.config.ServerName

	// 加载会话，如果存在
	cacheKey, session := c.loadSession(hello)
	if cacheKey != "" && session != nil {
		defer func() {
			// 按照 GB/T 38636-2020 6.4.5.2.1 Client Hello 消息 c) session_id 要求
			// 会话标识生成后应一直保持到超时删除 或 这个会话相关的连接遇到致命错误被关闭。
			if err != nil {
				// 删除会话
				c.config.SessionCache.Put(cacheKey, nil)
			}
		}()
	}

	if _, err := c.writeRecord(recordTypeHandshake, hello.marshal()); err != nil {
		return err
	}

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}

	serverHello, ok := msg.(*serverHelloMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(serverHello, msg)
	}

	if err := c.pickProtocolVersion(serverHello); err != nil {
		return err
	}

	hs := &clientHandshakeState{
		c:           c,
		ctx:         ctx,
		serverHello: serverHello,
		hello:       hello,
		session:     session,
	}

	if err := hs.handshake(); err != nil {
		return err
	}

	return nil
}

// 加载会话，如果存在
func (c *Conn) loadSession(hello *clientHelloMsg) (cacheKey string, session *SessionState) {
	if c.config.SessionCache == nil {
		return
	}
	var ok = false
	// 获取最近一个会话
	session, ok = c.config.SessionCache.Get("")
	if !ok || session == nil {
		return cacheKey, nil
	}
	// 设置客户端Hello 会话ID
	hello.sessionId = session.sessionId
	cacheKey = hex.EncodeToString(session.sessionId)

	return cacheKey, session
}

// 根据服务端消息选择客户端协议版本
func (c *Conn) pickProtocolVersion(serverHello *serverHelloMsg) error {
	peerVersion := serverHello.vers

	vers, ok := c.config.mutualVersion(roleClient, []uint16{peerVersion})
	if !ok {
		c.sendAlert(alertProtocolVersion)
		return fmt.Errorf("tlcp: server selected unsupported protocol version %x", peerVersion)
	}

	c.vers = vers
	c.haveVers = true
	c.in.version = vers
	c.out.version = vers

	return nil
}

// Does the handshake, either a full one or resumes old session. Requires hs.c,
// hs.hello, hs.serverHello, and, optionally, hs.session to be set.
func (hs *clientHandshakeState) handshake() error {
	c := hs.c

	isResume, err := hs.processServerHello()
	if err != nil {
		return err
	}

	hs.finishedHash = newFinishedHash(c.vers, hs.suite)

	// - 握手重用不需要计算客户端验证消息
	// - 完整的握手流程中如果客户端采用单向身份认证（没有证书和密钥对）那么也不需要计算客户端验证消息的签名值
	if isResume || (len(c.config.Certificates) == 0 && c.config.GetClientCertificate == nil) {
		hs.finishedHash.discardHandshakeBuffer()
	}

	hs.finishedHash.Write(hs.hello.marshal())
	hs.finishedHash.Write(hs.serverHello.marshal())

	c.buffering = true
	c.didResume = isResume
	if isResume {
		if err := hs.establishKeys(); err != nil {
			return err
		}
		if err := hs.readFinished(c.serverFinished[:]); err != nil {
			return err
		}
		// 握手重用时可以通过连接验证再次验证连接相关的信息
		if c.config.VerifyConnection != nil {
			if err := c.config.VerifyConnection(c.connectionStateLocked()); err != nil {
				c.sendAlert(alertBadCertificate)
				return err
			}
		}
		if err := hs.sendFinished(c.clientFinished[:]); err != nil {
			return err
		}
		if _, err := c.flush(); err != nil {
			return err
		}
	} else {
		if err := hs.doFullHandshake(); err != nil {
			return err
		}
		if err := hs.establishKeys(); err != nil {
			return err
		}
		if err := hs.sendFinished(c.clientFinished[:]); err != nil {
			return err
		}
		if _, err := c.flush(); err != nil {
			return err
		}
		if err := hs.createNewSession(); err != nil {
			return err
		}
		if err := hs.readFinished(c.serverFinished[:]); err != nil {
			return err
		}
	}

	atomic.StoreUint32(&c.handshakeStatus, 1)

	return nil
}

func (hs *clientHandshakeState) pickCipherSuite() error {
	if hs.suite = mutualCipherSuite(hs.hello.cipherSuites, hs.serverHello.cipherSuite); hs.suite == nil {
		hs.c.sendAlert(alertHandshakeFailure)
		return errors.New("tlcp: server chose an unconfigured cipher suite")
	}

	hs.c.cipherSuite = hs.suite.id
	return nil
}

func (hs *clientHandshakeState) doFullHandshake() error {
	c := hs.c

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}
	certMsg, ok := msg.(*certificateMsg)
	if !ok || len(certMsg.certificates) == 0 {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(certMsg, msg)
	}
	hs.finishedHash.Write(certMsg.marshal())

	msg, err = c.readHandshake()
	if err != nil {
		return err
	}

	if c.handshakes == 0 {
		// If this is the first handshake on a connection, process and
		// (optionally) verify the server's certificates.
		if err := c.verifyServerCertificate(certMsg.certificates); err != nil {
			return err
		}
	} else {
		// This is a renegotiation handshake. We require that the
		// server's identity (i.e. leaf certificate) is unchanged and
		// thus any previous trust decision is still valid.
		//
		// See https://mitls.org/pages/attacks/3SHAKE for the
		// motivation behind this requirement.
		if !bytes.Equal(c.peerCertificates[0].Raw, certMsg.certificates[0]) {
			c.sendAlert(alertBadCertificate)
			return errors.New("tlcp: server's identity changed during renegotiation")
		}
	}

	keyAgreement := hs.suite.ka(c.vers)

	skx, ok := msg.(*serverKeyExchangeMsg)
	if ok {
		hs.finishedHash.Write(skx.marshal())
		err = keyAgreement.processServerKeyExchange(c.config, hs.hello, hs.serverHello, c.peerCertificates, skx)
		if err != nil {
			c.sendAlert(alertUnexpectedMessage)
			return err
		}

		msg, err = c.readHandshake()
		if err != nil {
			return err
		}
	}

	var chainToSend *Certificate
	var certRequested bool
	certReq, ok := msg.(*certificateRequestMsg)
	if ok {
		certRequested = true
		hs.finishedHash.Write(certReq.marshal())

		cri := &CertificateRequestInfo{AcceptableCAs: certReq.certificateAuthorities, Version: c.vers, ctx: hs.ctx}
		if chainToSend, err = c.getClientCertificate(cri); err != nil {
			c.sendAlert(alertInternalError)
			return err
		}

		msg, err = c.readHandshake()
		if err != nil {
			return err
		}
	}

	shd, ok := msg.(*serverHelloDoneMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(shd, msg)
	}
	hs.finishedHash.Write(shd.marshal())

	// 如果服务端发送了证书请求消息，那么我们必须发送证书消息（客户端）
	// 即便客户端没有证书，也需要发一条空证书的证书消息到服务端。
	if certRequested {
		certMsg = new(certificateMsg)
		certMsg.certificates = chainToSend.Certificate
		hs.finishedHash.Write(certMsg.marshal())
		if _, err := c.writeRecord(recordTypeHandshake, certMsg.marshal()); err != nil {
			return err
		}
	}

	preMasterSecret, ckx, err := keyAgreement.generateClientKeyExchange(c.config, hs.hello, c.peerCertificates)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	if ckx != nil {
		hs.finishedHash.Write(ckx.marshal())
		if _, err := c.writeRecord(recordTypeHandshake, ckx.marshal()); err != nil {
			return err
		}
	}

	// 准备 客户端证书验证消息
	if chainToSend != nil && len(chainToSend.Certificate) > 0 {
		certVerify := &certificateVerifyMsg{}

		// 根据算法套件获取签名算法类型
		sigType, newHash, err := typeAndHashFrom(hs.suite.id)
		if !ok {
			c.sendAlert(alertInternalError)
			return fmt.Errorf("tlcp: client certificate private key of type %T does not implement crypto.Signer", chainToSend.PrivateKey)
		}
		// 计算从Hello开始至今的握手消息Hash
		signed := hs.finishedHash.Sum()
		// 根据算法套件使用密钥签名
		certVerify.signature, err = signHandshake(c, sigType, chainToSend.PrivateKey, newHash, signed)
		if err != nil {
			c.sendAlert(alertInternalError)
			return err
		}

		hs.finishedHash.Write(certVerify.marshal())
		if _, err := c.writeRecord(recordTypeHandshake, certVerify.marshal()); err != nil {
			return err
		}
	}
	hs.masterSecret = masterFromPreMasterSecret(c.vers, hs.suite, preMasterSecret, hs.hello.random, hs.serverHello.random)
	hs.finishedHash.discardHandshakeBuffer()

	return nil
}

func (hs *clientHandshakeState) establishKeys() error {
	c := hs.c

	clientMAC, serverMAC, clientKey, serverKey, clientIV, serverIV :=
		keysFromMasterSecret(c.vers, hs.suite, hs.masterSecret, hs.hello.random, hs.serverHello.random, hs.suite.macLen, hs.suite.keyLen, hs.suite.ivLen)
	var clientCipher, serverCipher interface{}
	var clientHash, serverHash hash.Hash
	if hs.suite.cipher != nil {
		clientCipher = hs.suite.cipher(clientKey, clientIV, false /* not for reading */)
		clientHash = hs.suite.mac(clientMAC)
		serverCipher = hs.suite.cipher(serverKey, serverIV, true /* for reading */)
		serverHash = hs.suite.mac(serverMAC)
	} else {
		clientCipher = hs.suite.aead(clientKey, clientIV)
		serverCipher = hs.suite.aead(serverKey, serverIV)
	}

	c.in.prepareCipherSpec(c.vers, serverCipher, serverHash)
	c.out.prepareCipherSpec(c.vers, clientCipher, clientHash)
	return nil
}

func (hs *clientHandshakeState) serverResumedSession() bool {
	// If the server responded with the same sessionId then it means the
	// sessionTicket is being used to resume a TLS session.
	return hs.session != nil && hs.hello.sessionId != nil &&
		bytes.Equal(hs.serverHello.sessionId, hs.hello.sessionId)
}

// 处理服务端握手消息
// return: 握手重用（true-启用；false-不启用）;
func (hs *clientHandshakeState) processServerHello() (bool, error) {
	c := hs.c

	// 选择匹配的密码套件
	if err := hs.pickCipherSuite(); err != nil {
		return false, err
	}

	if hs.serverHello.compressionMethod != compressionNone {
		c.sendAlert(alertUnexpectedMessage)
		return false, errors.New("tlcp: server selected unsupported compression format")
	}
	if !hs.serverResumedSession() {
		return false, nil
	}

	if hs.session.vers != c.vers {
		c.sendAlert(alertHandshakeFailure)
		return false, errors.New("tlcp: server resumed a session with a different version")
	}

	if hs.session.cipherSuite != hs.suite.id {
		c.sendAlert(alertHandshakeFailure)
		return false, errors.New("tlcp: server resumed a session with a different cipher suite")
	}

	// 根据会话恢复 会话密钥 以及 证书
	hs.masterSecret = hs.session.masterSecret
	c.peerCertificates = hs.session.peerCertificates
	return true, nil
}

func (hs *clientHandshakeState) readFinished(out []byte) error {
	c := hs.c

	if err := c.readChangeCipherSpec(); err != nil {
		return err
	}

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}
	serverFinished, ok := msg.(*finishedMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(serverFinished, msg)
	}

	verify := hs.finishedHash.serverSum(hs.masterSecret)
	if len(verify) != len(serverFinished.verifyData) ||
		subtle.ConstantTimeCompare(verify, serverFinished.verifyData) != 1 {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("tlcp: server's Finished message was incorrect")
	}
	hs.finishedHash.Write(serverFinished.marshal())
	copy(out, verify)
	return nil
}

// 生成Session会话信息，用于握手重用
func (hs *clientHandshakeState) createNewSession() error {
	if hs.c.config.SessionCache == nil {
		return nil
	}

	sessionKey := hex.EncodeToString(hs.serverHello.sessionId)
	cs := &SessionState{
		sessionId:    hs.serverHello.sessionId,
		vers:         hs.serverHello.vers,
		cipherSuite:  hs.serverHello.cipherSuite,
		masterSecret: hs.masterSecret,
		createdAt:    time.Now(),
	}
	hs.c.config.SessionCache.Put(sessionKey, cs)
	return nil
}

func (hs *clientHandshakeState) sendFinished(out []byte) error {
	c := hs.c

	if _, err := c.writeRecord(recordTypeChangeCipherSpec, []byte{1}); err != nil {
		return err
	}

	finished := new(finishedMsg)
	finished.verifyData = hs.finishedHash.clientSum(hs.masterSecret)
	hs.finishedHash.Write(finished.marshal())
	if _, err := c.writeRecord(recordTypeHandshake, finished.marshal()); err != nil {
		return err
	}
	copy(out, finished.verifyData)
	return nil
}

// verifyServerCertificate 解析并验证服务端证书（签名,加密）
// c.verifiedChains and c.peerCertificates or sending the appropriate alert.
func (c *Conn) verifyServerCertificate(certificates [][]byte) error {
	certs := make([]*x509.Certificate, len(certificates))
	for i, asn1Data := range certificates {
		cert, err := x509.ParseCertificate(asn1Data)
		if err != nil {
			c.sendAlert(alertBadCertificate)
			return errors.New("tlcp: failed to parse certificate from server: " + err.Error())
		}
		certs[i] = cert
	}

	if len(certs) < 2 {
		c.sendAlert(alertBadCertificate)
		return errors.New("tlcp: need two of certificate one for sign one for encrypt")
	}

	if !c.config.InsecureSkipVerify {
		opts := x509.VerifyOptions{
			Roots:       c.config.RootCAs,
			CurrentTime: c.config.time(),
			DNSName:     c.config.ServerName,
		}

		var err error

		// 验证签名证书
		c.verifiedChains, err = certs[0].Verify(opts)
		if err != nil {
			c.sendAlert(alertBadCertificate)
			return err
		}
		// 验证加密证书
		_, err = certs[1].Verify(opts)
		if err != nil {
			c.sendAlert(alertBadCertificate)
			return err
		}
	}

	switch certs[0].PublicKey.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey:
		break
	default:
		c.sendAlert(alertUnsupportedCertificate)
		return fmt.Errorf("tlcp: server's certificate contains an unsupported type of public key: %T", certs[0].PublicKey)
	}

	c.peerCertificates = certs

	if c.config.VerifyPeerCertificate != nil {
		if err := c.config.VerifyPeerCertificate(certificates, c.verifiedChains); err != nil {
			c.sendAlert(alertBadCertificate)
			return err
		}
	}

	if c.config.VerifyConnection != nil {
		if err := c.config.VerifyConnection(c.connectionStateLocked()); err != nil {
			c.sendAlert(alertBadCertificate)
			return err
		}
	}

	return nil
}

// 通过证书请求信息的Subject获取匹配的数字证书
func (c *Conn) getClientCertificate(cri *CertificateRequestInfo) (*Certificate, error) {
	if c.config.GetClientCertificate != nil {
		return c.config.GetClientCertificate(cri)
	}

	for _, chain := range c.config.Certificates {
		if err := cri.SupportsCertificate(&chain); err != nil {
			continue
		}
		return &chain, nil
	}

	// No acceptable certificate found. Don't send a certificate.
	return new(Certificate), nil
}

//// hostnameInSNI converts name into an appropriate hostname for SNI.
//// Literal IP addresses and absolute FQDNs are not permitted as SNI values.
//// See RFC 6066, Section 3.
//func hostnameInSNI(name string) string {
//	host := name
//	if len(host) > 0 && host[0] == '[' && host[len(host)-1] == ']' {
//		host = host[1 : len(host)-1]
//	}
//	if i := strings.LastIndex(host, "%"); i > 0 {
//		host = host[:i]
//	}
//	if net.ParseIP(host) != nil {
//		return ""
//	}
//	for len(name) > 0 && name[len(name)-1] == '.' {
//		name = name[:len(name)-1]
//	}
//	return name
//}
