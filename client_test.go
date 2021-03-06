package quic

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/lucas-clemente/quic-go/internal/handshake"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/internal/wire"
	"github.com/lucas-clemente/quic-go/qerr"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Client", func() {
	var (
		cl         *client
		sess       *mockSession
		packetConn *mockPacketConn
		addr       net.Addr
		connID     protocol.ConnectionID

		originalClientSessConstructor func(conn connection, hostname string, v protocol.VersionNumber, connectionID protocol.ConnectionID, tlsConf *tls.Config, config *Config, initialVersion protocol.VersionNumber, negotiatedVersions []protocol.VersionNumber, logger utils.Logger) (packetHandler, error)
	)

	// generate a packet sent by the server that accepts the QUIC version suggested by the client
	acceptClientVersionPacket := func(connID protocol.ConnectionID) []byte {
		b := &bytes.Buffer{}
		err := (&wire.Header{
			DestConnectionID: connID,
			SrcConnectionID:  connID,
			PacketNumber:     1,
			PacketNumberLen:  1,
		}).Write(b, protocol.PerspectiveServer, protocol.VersionWhatever)
		Expect(err).ToNot(HaveOccurred())
		return b.Bytes()
	}

	BeforeEach(func() {
		connID = protocol.ConnectionID{0, 0, 0, 0, 0, 0, 0x13, 0x37}
		originalClientSessConstructor = newClientSession
		Eventually(areSessionsRunning).Should(BeFalse())
		msess, _ := newMockSession(nil, 0, connID, nil, nil, nil, nil)
		sess = msess.(*mockSession)
		addr = &net.UDPAddr{IP: net.IPv4(192, 168, 100, 200), Port: 1337}
		packetConn = newMockPacketConn()
		packetConn.addr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
		packetConn.dataReadFrom = addr
		cl = &client{
			srcConnID:  connID,
			destConnID: connID,
			session:    sess,
			version:    protocol.SupportedVersions[0],
			conn:       &conn{pconn: packetConn, currentAddr: addr},
			versionNegotiationChan: make(chan struct{}),
			logger:                 utils.DefaultLogger,
		}
	})

	AfterEach(func() {
		newClientSession = originalClientSessConstructor
	})

	AfterEach(func() {
		if s, ok := cl.session.(*session); ok {
			s.Close(nil)
		}
		Eventually(areSessionsRunning).Should(BeFalse())
	})

	Context("Dialing", func() {
		var origGenerateConnectionID func() (protocol.ConnectionID, error)

		BeforeEach(func() {
			origGenerateConnectionID = generateConnectionID
			generateConnectionID = func() (protocol.ConnectionID, error) {
				return connID, nil
			}
		})

		AfterEach(func() {
			generateConnectionID = origGenerateConnectionID
		})

		It("resolves the address", func() {
			if os.Getenv("APPVEYOR") == "True" {
				Skip("This test is flaky on AppVeyor.")
			}
			closeErr := errors.New("peer doesn't reply")
			remoteAddrChan := make(chan string)
			newClientSession = func(
				conn connection,
				_ string,
				_ protocol.VersionNumber,
				_ protocol.ConnectionID,
				_ *tls.Config,
				_ *Config,
				_ protocol.VersionNumber,
				_ []protocol.VersionNumber,
				_ utils.Logger,
			) (packetHandler, error) {
				remoteAddrChan <- conn.RemoteAddr().String()
				return sess, nil
			}
			dialed := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				_, err := DialAddr("localhost:17890", nil, &Config{HandshakeTimeout: time.Millisecond})
				Expect(err).To(MatchError(closeErr))
				close(dialed)
			}()
			Eventually(remoteAddrChan).Should(Receive(Equal("127.0.0.1:17890")))
			sess.Close(closeErr)
			Eventually(dialed).Should(BeClosed())
		})

		It("uses the tls.Config.ServerName as the hostname, if present", func() {
			closeErr := errors.New("peer doesn't reply")
			hostnameChan := make(chan string)
			newClientSession = func(
				_ connection,
				h string,
				_ protocol.VersionNumber,
				_ protocol.ConnectionID,
				_ *tls.Config,
				_ *Config,
				_ protocol.VersionNumber,
				_ []protocol.VersionNumber,
				_ utils.Logger,
			) (packetHandler, error) {
				hostnameChan <- h
				return sess, nil
			}
			dialed := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				_, err := DialAddr("localhost:17890", &tls.Config{ServerName: "foobar"}, nil)
				Expect(err).To(MatchError(closeErr))
				close(dialed)
			}()
			Eventually(hostnameChan).Should(Receive(Equal("foobar")))
			sess.Close(closeErr)
			Eventually(dialed).Should(BeClosed())
		})

		It("errors when receiving an error from the connection", func() {
			testErr := errors.New("connection error")
			packetConn.readErr = testErr
			_, err := Dial(packetConn, addr, "quic.clemente.io:1337", nil, nil)
			Expect(err).To(MatchError(testErr))
		})

		It("returns after the handshake is complete", func() {
			newClientSession = func(
				_ connection,
				_ string,
				_ protocol.VersionNumber,
				_ protocol.ConnectionID,
				_ *tls.Config,
				_ *Config,
				_ protocol.VersionNumber,
				_ []protocol.VersionNumber,
				_ utils.Logger,
			) (packetHandler, error) {
				return sess, nil
			}
			packetConn.dataToRead <- acceptClientVersionPacket(cl.srcConnID)
			dialed := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				s, err := Dial(packetConn, addr, "quic.clemente.io:1337", nil, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(s).ToNot(BeNil())
				close(dialed)
			}()
			close(sess.handshakeChan)
			Eventually(dialed).Should(BeClosed())
		})

		It("returns an error that occurs while waiting for the connection to become secure", func() {
			testErr := errors.New("early handshake error")
			newClientSession = func(
				conn connection,
				_ string,
				_ protocol.VersionNumber,
				_ protocol.ConnectionID,
				_ *tls.Config,
				_ *Config,
				_ protocol.VersionNumber,
				_ []protocol.VersionNumber,
				_ utils.Logger,
			) (packetHandler, error) {
				Expect(conn.Write([]byte("0 fake CHLO"))).To(Succeed())
				return sess, nil
			}
			packetConn.dataToRead <- acceptClientVersionPacket(cl.srcConnID)
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				_, err := Dial(packetConn, addr, "quic.clemente.io:1337", nil, nil)
				Expect(err).To(MatchError(testErr))
				close(done)
			}()
			sess.handshakeChan <- testErr
			Eventually(done).Should(BeClosed())
		})

		Context("quic.Config", func() {
			It("setups with the right values", func() {
				config := &Config{
					HandshakeTimeout:            1337 * time.Minute,
					IdleTimeout:                 42 * time.Hour,
					RequestConnectionIDOmission: true,
					MaxIncomingStreams:          1234,
					MaxIncomingUniStreams:       4321,
				}
				c := populateClientConfig(config)
				Expect(c.HandshakeTimeout).To(Equal(1337 * time.Minute))
				Expect(c.IdleTimeout).To(Equal(42 * time.Hour))
				Expect(c.RequestConnectionIDOmission).To(BeTrue())
				Expect(c.MaxIncomingStreams).To(Equal(1234))
				Expect(c.MaxIncomingUniStreams).To(Equal(4321))
			})

			It("errors when the Config contains an invalid version", func() {
				version := protocol.VersionNumber(0x1234)
				_, err := Dial(nil, nil, "localhost:1234", &tls.Config{}, &Config{Versions: []protocol.VersionNumber{version}})
				Expect(err).To(MatchError("0x1234 is not a valid QUIC version"))
			})

			It("disables bidirectional streams", func() {
				config := &Config{
					MaxIncomingStreams:    -1,
					MaxIncomingUniStreams: 4321,
				}
				c := populateClientConfig(config)
				Expect(c.MaxIncomingStreams).To(BeZero())
				Expect(c.MaxIncomingUniStreams).To(Equal(4321))
			})

			It("disables unidirectional streams", func() {
				config := &Config{
					MaxIncomingStreams:    1234,
					MaxIncomingUniStreams: -1,
				}
				c := populateClientConfig(config)
				Expect(c.MaxIncomingStreams).To(Equal(1234))
				Expect(c.MaxIncomingUniStreams).To(BeZero())
			})

			It("fills in default values if options are not set in the Config", func() {
				c := populateClientConfig(&Config{})
				Expect(c.Versions).To(Equal(protocol.SupportedVersions))
				Expect(c.HandshakeTimeout).To(Equal(protocol.DefaultHandshakeTimeout))
				Expect(c.IdleTimeout).To(Equal(protocol.DefaultIdleTimeout))
				Expect(c.RequestConnectionIDOmission).To(BeFalse())
			})
		})

		Context("gQUIC", func() {
			It("errors if it can't create a session", func() {
				testErr := errors.New("error creating session")
				newClientSession = func(
					_ connection,
					_ string,
					_ protocol.VersionNumber,
					_ protocol.ConnectionID,
					_ *tls.Config,
					_ *Config,
					_ protocol.VersionNumber,
					_ []protocol.VersionNumber,
					_ utils.Logger,
				) (packetHandler, error) {
					return nil, testErr
				}
				_, err := Dial(packetConn, addr, "quic.clemente.io:1337", nil, nil)
				Expect(err).To(MatchError(testErr))
			})
		})

		Context("IETF QUIC", func() {
			It("creates new TLS sessions with the right parameters", func() {
				config := &Config{Versions: []protocol.VersionNumber{protocol.VersionTLS}}
				c := make(chan struct{})
				var cconn connection
				var hostname string
				var version protocol.VersionNumber
				var conf *Config
				newTLSClientSession = func(
					connP connection,
					hostnameP string,
					versionP protocol.VersionNumber,
					_ protocol.ConnectionID,
					_ protocol.ConnectionID,
					configP *Config,
					tls handshake.MintTLS,
					paramsChan <-chan handshake.TransportParameters,
					_ protocol.PacketNumber,
					_ utils.Logger,
				) (packetHandler, error) {
					cconn = connP
					hostname = hostnameP
					version = versionP
					conf = configP
					close(c)
					// TODO: check connection IDs?
					return sess, nil
				}
				dialed := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					Dial(packetConn, addr, "quic.clemente.io:1337", nil, config)
					close(dialed)
				}()
				Eventually(c).Should(BeClosed())
				Expect(cconn.(*conn).pconn).To(Equal(packetConn))
				Expect(hostname).To(Equal("quic.clemente.io"))
				Expect(version).To(Equal(config.Versions[0]))
				Expect(conf.Versions).To(Equal(config.Versions))
				sess.Close(errors.New("peer doesn't reply"))
				Eventually(dialed).Should(BeClosed())
			})
		})

		Context("version negotiation", func() {
			var origSupportedVersions []protocol.VersionNumber

			BeforeEach(func() {
				origSupportedVersions = protocol.SupportedVersions
				protocol.SupportedVersions = append(protocol.SupportedVersions, []protocol.VersionNumber{77, 78}...)
			})

			AfterEach(func() {
				protocol.SupportedVersions = origSupportedVersions
			})

			It("returns an error that occurs during version negotiation", func() {
				newClientSession = func(
					conn connection,
					_ string,
					_ protocol.VersionNumber,
					_ protocol.ConnectionID,
					_ *tls.Config,
					_ *Config,
					_ protocol.VersionNumber,
					_ []protocol.VersionNumber,
					_ utils.Logger,
				) (packetHandler, error) {
					Expect(conn.Write([]byte("0 fake CHLO"))).To(Succeed())
					return sess, nil
				}
				testErr := errors.New("early handshake error")
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					_, err := Dial(packetConn, addr, "quic.clemente.io:1337", nil, nil)
					Expect(err).To(MatchError(testErr))
					close(done)
				}()
				sess.Close(testErr)
				Eventually(done).Should(BeClosed())
			})

			It("recognizes that a packet without VersionFlag means that the server accepted the suggested version", func() {
				ph := wire.Header{
					PacketNumber:     1,
					PacketNumberLen:  protocol.PacketNumberLen2,
					DestConnectionID: connID,
					SrcConnectionID:  connID,
				}
				b := &bytes.Buffer{}
				err := ph.Write(b, protocol.PerspectiveServer, protocol.VersionWhatever)
				Expect(err).ToNot(HaveOccurred())
				err = cl.handlePacket(nil, b.Bytes())
				Expect(err).ToNot(HaveOccurred())
				Expect(cl.versionNegotiated).To(BeTrue())
				Expect(cl.versionNegotiationChan).To(BeClosed())
			})

			It("changes the version after receiving a version negotiation packet", func() {
				var initialVersion protocol.VersionNumber
				var negotiatedVersions []protocol.VersionNumber
				newVersion := protocol.VersionNumber(77)
				Expect(newVersion).ToNot(Equal(cl.version))
				cl.config = &Config{Versions: []protocol.VersionNumber{newVersion}}
				sessionChan := make(chan *mockSession)
				handshakeChan := make(chan error)
				newClientSession = func(
					_ connection,
					_ string,
					_ protocol.VersionNumber,
					connectionID protocol.ConnectionID,
					_ *tls.Config,
					_ *Config,
					initialVersionP protocol.VersionNumber,
					negotiatedVersionsP []protocol.VersionNumber,
					_ utils.Logger,
				) (packetHandler, error) {
					initialVersion = initialVersionP
					negotiatedVersions = negotiatedVersionsP

					sess := &mockSession{
						connectionID:  connectionID,
						stopRunLoop:   make(chan struct{}),
						handshakeChan: handshakeChan,
					}
					sessionChan <- sess
					return sess, nil
				}

				established := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					err := cl.dial()
					Expect(err).ToNot(HaveOccurred())
					close(established)
				}()
				go cl.listen()

				actualInitialVersion := cl.version
				var firstSession, secondSession *mockSession
				Eventually(sessionChan).Should(Receive(&firstSession))
				packetConn.dataToRead <- wire.ComposeGQUICVersionNegotiation(
					connID,
					[]protocol.VersionNumber{newVersion},
				)
				// it didn't pass the version negoation packet to the old session (since it has no payload)
				Eventually(func() bool { return firstSession.closed }).Should(BeTrue())
				Expect(firstSession.closeReason).To(Equal(errCloseSessionForNewVersion))
				Expect(firstSession.handledPackets).To(BeEmpty())
				Eventually(sessionChan).Should(Receive(&secondSession))
				// make the server accept the new version
				packetConn.dataToRead <- acceptClientVersionPacket(secondSession.connectionID)
				Consistently(func() bool { return secondSession.closed }).Should(BeFalse())
				Expect(negotiatedVersions).To(ContainElement(newVersion))
				Expect(initialVersion).To(Equal(actualInitialVersion))

				close(handshakeChan)
				Eventually(established).Should(BeClosed())
			})

			It("only accepts one version negotiation packet", func() {
				sessionCounter := uint32(0)
				newClientSession = func(
					_ connection,
					_ string,
					_ protocol.VersionNumber,
					connectionID protocol.ConnectionID,
					_ *tls.Config,
					_ *Config,
					_ protocol.VersionNumber,
					_ []protocol.VersionNumber,
					_ utils.Logger,
				) (packetHandler, error) {
					atomic.AddUint32(&sessionCounter, 1)
					return &mockSession{
						connectionID: connectionID,
						stopRunLoop:  make(chan struct{}),
					}, nil
				}
				go cl.dial()
				Eventually(func() uint32 { return atomic.LoadUint32(&sessionCounter) }).Should(BeEquivalentTo(1))
				cl.config = &Config{Versions: []protocol.VersionNumber{77, 78}}
				err := cl.handlePacket(nil, wire.ComposeGQUICVersionNegotiation(connID, []protocol.VersionNumber{77}))
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() uint32 { return atomic.LoadUint32(&sessionCounter) }).Should(BeEquivalentTo(2))
				err = cl.handlePacket(nil, wire.ComposeGQUICVersionNegotiation(connID, []protocol.VersionNumber{78}))
				Expect(err).To(MatchError("received a delayed Version Negotiation Packet"))
				Consistently(func() uint32 { return atomic.LoadUint32(&sessionCounter) }).Should(BeEquivalentTo(2))
			})

			It("errors if no matching version is found", func() {
				cl.config = &Config{Versions: protocol.SupportedVersions}
				err := cl.handlePacket(nil, wire.ComposeGQUICVersionNegotiation(connID, []protocol.VersionNumber{1}))
				Expect(err).ToNot(HaveOccurred())
				Expect(cl.session.(*mockSession).closed).To(BeTrue())
				Expect(cl.session.(*mockSession).closeReason).To(MatchError(qerr.InvalidVersion))
			})

			It("errors if the version is supported by quic-go, but disabled by the quic.Config", func() {
				v := protocol.VersionNumber(1234)
				Expect(v).ToNot(Equal(cl.version))
				cl.config = &Config{Versions: protocol.SupportedVersions}
				err := cl.handlePacket(nil, wire.ComposeGQUICVersionNegotiation(connID, []protocol.VersionNumber{v}))
				Expect(err).ToNot(HaveOccurred())
				Expect(cl.session.(*mockSession).closed).To(BeTrue())
				Expect(cl.session.(*mockSession).closeReason).To(MatchError(qerr.InvalidVersion))
			})

			It("changes to the version preferred by the quic.Config", func() {
				config := &Config{Versions: []protocol.VersionNumber{1234, 4321}}
				cl.config = config
				err := cl.handlePacket(nil, wire.ComposeGQUICVersionNegotiation(connID, []protocol.VersionNumber{4321, 1234}))
				Expect(err).ToNot(HaveOccurred())
				Expect(cl.version).To(Equal(protocol.VersionNumber(1234)))
			})

			It("drops version negotiation packets that contain the offered version", func() {
				ver := cl.version
				err := cl.handlePacket(nil, wire.ComposeGQUICVersionNegotiation(connID, []protocol.VersionNumber{ver}))
				Expect(err).ToNot(HaveOccurred())
				Expect(cl.version).To(Equal(ver))
			})
		})
	})

	It("ignores packets with an invalid public header", func() {
		err := cl.handlePacket(addr, []byte("invalid packet"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("error parsing packet from"))
		Expect(sess.handledPackets).To(BeEmpty())
		Expect(sess.closed).To(BeFalse())
	})

	It("errors on packets that are smaller than the Payload Length in the packet header", func() {
		b := &bytes.Buffer{}
		hdr := &wire.Header{
			IsLongHeader:     true,
			Type:             protocol.PacketTypeHandshake,
			PayloadLen:       1000,
			SrcConnectionID:  protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8},
			DestConnectionID: protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8},
			Version:          versionIETFFrames,
		}
		Expect(hdr.Write(b, protocol.PerspectiveClient, versionIETFFrames)).To(Succeed())
		cl.handlePacket(addr, append(b.Bytes(), make([]byte, 456)...))
		Expect(sess.handledPackets).To(BeEmpty())
		Expect(sess.closed).To(BeFalse())
	})

	It("cuts packets at the payload length", func() {
		b := &bytes.Buffer{}
		hdr := &wire.Header{
			IsLongHeader:     true,
			Type:             protocol.PacketTypeHandshake,
			PayloadLen:       123,
			SrcConnectionID:  connID,
			DestConnectionID: connID,
			Version:          versionIETFFrames,
		}
		Expect(hdr.Write(b, protocol.PerspectiveClient, versionIETFFrames)).To(Succeed())
		cl.handlePacket(addr, append(b.Bytes(), make([]byte, 456)...))
		Expect(sess.handledPackets).To(HaveLen(1))
		Expect(sess.handledPackets[0].data).To(HaveLen(123))
	})

	It("ignores packets without connection id, if it didn't request connection id trunctation", func() {
		cl.config = &Config{RequestConnectionIDOmission: false}
		buf := &bytes.Buffer{}
		err := (&wire.Header{
			OmitConnectionID: true,
			SrcConnectionID:  connID,
			DestConnectionID: connID,
			PacketNumber:     1,
			PacketNumberLen:  1,
		}).Write(buf, protocol.PerspectiveServer, versionGQUICFrames)
		Expect(err).ToNot(HaveOccurred())
		err = cl.handlePacket(addr, buf.Bytes())
		Expect(err).To(MatchError("received packet with truncated connection ID, but didn't request truncation"))
		Expect(sess.handledPackets).To(BeEmpty())
		Expect(sess.closed).To(BeFalse())
	})

	It("ignores packets with the wrong destination connection ID", func() {
		buf := &bytes.Buffer{}
		cl.version = versionIETFFrames
		cl.config = &Config{RequestConnectionIDOmission: false}
		connID2 := protocol.ConnectionID{8, 7, 6, 5, 4, 3, 2, 1}
		Expect(connID).ToNot(Equal(connID2))
		err := (&wire.Header{
			DestConnectionID: connID2,
			SrcConnectionID:  connID,
			PacketNumber:     1,
			PacketNumberLen:  1,
		}).Write(buf, protocol.PerspectiveServer, versionIETFFrames)
		Expect(err).ToNot(HaveOccurred())
		err = cl.handlePacket(addr, buf.Bytes())
		Expect(err).To(MatchError(fmt.Sprintf("received a packet with an unexpected connection ID (0x0807060504030201, expected %s)", connID)))
		Expect(sess.handledPackets).To(BeEmpty())
		Expect(sess.closed).To(BeFalse())
	})

	It("creates new GQUIC sessions with the right parameters", func() {
		config := &Config{Versions: protocol.SupportedVersions}
		closeErr := errors.New("peer doesn't reply")
		c := make(chan struct{})
		var cconn connection
		var hostname string
		var version protocol.VersionNumber
		var conf *Config
		newClientSession = func(
			connP connection,
			hostnameP string,
			versionP protocol.VersionNumber,
			_ protocol.ConnectionID,
			_ *tls.Config,
			configP *Config,
			_ protocol.VersionNumber,
			_ []protocol.VersionNumber,
			_ utils.Logger,
		) (packetHandler, error) {
			cconn = connP
			hostname = hostnameP
			version = versionP
			conf = configP
			close(c)
			return sess, nil
		}
		dialed := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			_, err := Dial(packetConn, addr, "quic.clemente.io:1337", nil, config)
			Expect(err).To(MatchError(closeErr))
			close(dialed)
		}()
		Eventually(c).Should(BeClosed())
		Expect(cconn.(*conn).pconn).To(Equal(packetConn))
		Expect(hostname).To(Equal("quic.clemente.io"))
		Expect(version).To(Equal(config.Versions[0]))
		Expect(conf.Versions).To(Equal(config.Versions))
		sess.Close(closeErr)
		Eventually(dialed).Should(BeClosed())
	})

	It("creates a new session when the server performs a retry", func() {
		config := &Config{Versions: []protocol.VersionNumber{protocol.VersionTLS}}
		cl.config = config
		sessionChan := make(chan *mockSession)
		newTLSClientSession = func(
			connP connection,
			hostnameP string,
			versionP protocol.VersionNumber,
			_ protocol.ConnectionID,
			_ protocol.ConnectionID,
			configP *Config,
			tls handshake.MintTLS,
			paramsChan <-chan handshake.TransportParameters,
			_ protocol.PacketNumber,
			_ utils.Logger,
		) (packetHandler, error) {
			sess := &mockSession{
				stopRunLoop: make(chan struct{}),
			}
			sessionChan <- sess
			return sess, nil
		}
		dialed := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			Dial(packetConn, addr, "quic.clemente.io:1337", nil, config)
			close(dialed)
		}()
		var firstSession, secondSession *mockSession
		Eventually(sessionChan).Should(Receive(&firstSession))
		firstSession.Close(handshake.ErrCloseSessionForRetry)
		Eventually(sessionChan).Should(Receive(&secondSession))
		secondSession.Close(errors.New("stop test"))
		Eventually(dialed).Should(BeClosed())
	})

	Context("handling packets", func() {
		It("handles packets", func() {
			ph := wire.Header{
				PacketNumber:     1,
				PacketNumberLen:  protocol.PacketNumberLen2,
				DestConnectionID: connID,
				SrcConnectionID:  connID,
			}
			b := &bytes.Buffer{}
			err := ph.Write(b, protocol.PerspectiveServer, cl.version)
			Expect(err).ToNot(HaveOccurred())
			packetConn.dataToRead <- b.Bytes()

			Expect(sess.handledPackets).To(BeEmpty())
			stoppedListening := make(chan struct{})
			go func() {
				cl.listen()
				// it should continue listening when receiving valid packets
				close(stoppedListening)
			}()

			Eventually(func() []*receivedPacket { return sess.handledPackets }).Should(HaveLen(1))
			Expect(sess.closed).To(BeFalse())
			Consistently(stoppedListening).ShouldNot(BeClosed())
		})

		It("closes the session when encountering an error while reading from the connection", func() {
			testErr := errors.New("test error")
			packetConn.readErr = testErr
			cl.listen()
			Expect(sess.closed).To(BeTrue())
			Expect(sess.closeReason).To(MatchError(testErr))
		})
	})

	Context("Public Reset handling", func() {
		It("closes the session when receiving a Public Reset", func() {
			err := cl.handlePacket(addr, wire.WritePublicReset(cl.destConnID, 1, 0))
			Expect(err).ToNot(HaveOccurred())
			Expect(cl.session.(*mockSession).closed).To(BeTrue())
			Expect(cl.session.(*mockSession).closedRemote).To(BeTrue())
			Expect(cl.session.(*mockSession).closeReason.(*qerr.QuicError).ErrorCode).To(Equal(qerr.PublicReset))
		})

		It("ignores Public Resets from the wrong remote address", func() {
			spoofedAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5678}
			err := cl.handlePacket(spoofedAddr, wire.WritePublicReset(cl.destConnID, 1, 0))
			Expect(err).To(MatchError("Received a spoofed Public Reset"))
			Expect(cl.session.(*mockSession).closed).To(BeFalse())
			Expect(cl.session.(*mockSession).closedRemote).To(BeFalse())
		})

		It("ignores unparseable Public Resets", func() {
			pr := wire.WritePublicReset(cl.destConnID, 1, 0)
			err := cl.handlePacket(addr, pr[:len(pr)-5])
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Received a Public Reset. An error occurred parsing the packet"))
			Expect(cl.session.(*mockSession).closed).To(BeFalse())
			Expect(cl.session.(*mockSession).closedRemote).To(BeFalse())
		})
	})
})
