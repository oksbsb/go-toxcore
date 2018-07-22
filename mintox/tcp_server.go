package mintox

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"gopp"
	"io"
	"log"
	"math/rand"
	"net"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/djherbis/buffer"
	"github.com/pkg/errors"
	deadlock "github.com/sasha-s/go-deadlock"
)

const MAX_INCOMING_CONNECTIONS = 256

const TCP_MAX_BACKLOG = MAX_INCOMING_CONNECTIONS

const MAX_PACKET_SIZE = 2048

const TCP_HANDSHAKE_PLAIN_SIZE = (PUBLIC_KEY_SIZE + NONCE_SIZE)
const TCP_SERVER_HANDSHAKE_SIZE = (NONCE_SIZE + TCP_HANDSHAKE_PLAIN_SIZE + MAC_SIZE)
const TCP_CLIENT_HANDSHAKE_SIZE = (PUBLIC_KEY_SIZE + TCP_SERVER_HANDSHAKE_SIZE)
const TCP_MAX_OOB_DATA_LENGTH = 1024

const NUM_RESERVED_PORTS = 16
const NUM_CLIENT_CONNECTIONS = (256 - NUM_RESERVED_PORTS)

const TCP_PACKET_ROUTING_REQUEST = 0
const TCP_PACKET_ROUTING_RESPONSE = 1
const TCP_PACKET_CONNECTION_NOTIFICATION = 2
const TCP_PACKET_DISCONNECT_NOTIFICATION = 3
const TCP_PACKET_PING = 4
const TCP_PACKET_PONG = 5
const TCP_PACKET_OOB_SEND = 6
const TCP_PACKET_OOB_RECV = 7
const TCP_PACKET_ONION_REQUEST = 8
const TCP_PACKET_ONION_RESPONSE = 9

const ARRAY_ENTRY_SIZE = 6

/* frequency to ping connected nodes and timeout in seconds */
const TCP_PING_FREQUENCY = 30
const TCP_PING_TIMEOUT = 10

const (
	TCP_STATUS_NO_STATUS = iota
	TCP_STATUS_CONNECTED
	TCP_STATUS_UNCONFIRMED
	TCP_STATUS_CONFIRMED
)

//////////

var tcppktnames = map[byte]string{
	TCP_PACKET_ROUTING_REQUEST:         "ROUTING_REQUEST",
	TCP_PACKET_ROUTING_RESPONSE:        "ROUTING_RESPONSE",
	TCP_PACKET_CONNECTION_NOTIFICATION: "CONNECTION_NOTIFICATION",
	TCP_PACKET_DISCONNECT_NOTIFICATION: "DISCONNECT_NOTIFICATION",
	TCP_PACKET_PING:                    "PING",
	TCP_PACKET_PONG:                    "PONG",
	TCP_PACKET_OOB_SEND:                "OOB_SEND",
	TCP_PACKET_OOB_RECV:                "OOB_RECV",
	TCP_PACKET_ONION_REQUEST:           "ONION_REQUEST",
	TCP_PACKET_ONION_RESPONSE:          "ONION_RESPONSE",
}

func tcppktname(ptype byte) string {
	name := "TCP_PACKET_INVALID"
	if ptype > TCP_PACKET_ONION_RESPONSE && ptype < NUM_RESERVED_PORTS {
	} else if ptype >= NUM_RESERVED_PORTS {
		name = fmt.Sprintf("DATA_FOR_CONNID_%d", ptype)
	} else {
		name = tcppktnames[ptype]
	}
	return name
}

/////////
type PeerConnInfo struct {
	Pubkey  *CryptoKey
	Index   uint32
	Status  uint8
	Otherid uint8
}
type TCPSecureConn struct {
	Sock      net.Conn
	Pubkey    *CryptoKey // client's
	Seckey    *CryptoKey // self
	Shrkey    *CryptoKey
	RecvNonce *CBNonce
	SentNonce *CBNonce

	connmu    deadlock.RWMutex
	ConnInfos map[string]*PeerConnInfo // binpk => *PeerConnInfo
	Status    uint8

	crbuf      buffer.Buffer // conn read ring buffer
	cwctrlq    chan []byte   // ctrl packets like pong []byte
	cwctrldlen int32         // data length of cwctrlq
	cwdataq    chan []byte
	cwdatadlen int32 // data length of cwdataq

	Identifier uint64

	LastPinged time.Time
	Pingid     uint64

	OnNetRecv   func(int)
	OnClosed    func(Object)
	OnConfirmed func()
	OnNetSent   func(int)
}

type TCPServer struct {
	Oniono Object // TODO
	lsners []net.Listener

	Pubkey *CryptoKey
	Seckey *CryptoKey

	// c's flow: accept->incomingq -> unconfirmedq -> acceptedq
	connmu   deadlock.RWMutex
	Conns    map[string]*TCPSecureConn // binpk =>
	hsconnmu deadlock.RWMutex
	HSConns  map[net.Conn]*TCPSecureConn
}

/////
func NewTCPSecureConn(c net.Conn) *TCPSecureConn {
	this := &TCPSecureConn{}
	this.Sock = c
	c.(*net.TCPConn).SetWriteBuffer(128 * 1024)

	this.ConnInfos = map[string]*PeerConnInfo{}
	this.crbuf = buffer.NewRing(buffer.New(1024 * 1024))
	this.cwctrlq = make(chan []byte, 64)
	this.cwdataq = make(chan []byte, 128)

	return this
}
func (this *TCPSecureConn) Start() {
	go this.runReadLoop()
	go this.runWriteLoop()
}
func (this *TCPSecureConn) runReadLoop() {
	lastLogTime := time.Now().Add(-3 * time.Second)
	spdc := NewSpeedCalc()
	var nxtpktlen uint16
	stop := false
	for !stop {
		c := this.Sock
		if int(time.Since(lastLogTime).Seconds()) >= 1 {
			lastLogTime = time.Now()
			log.Printf("------- async reading... ----- spd: %d, %s ------\n", spdc.Avgspd, c.RemoteAddr())
		}
		rdbuf := make([]byte, 3000)
		rn, err := c.Read(rdbuf)
		gopp.ErrPrint(err, rn, c.RemoteAddr())
		if err == io.EOF {
			this.Status = TCP_STATUS_NO_STATUS
		}
		if err != nil {
			break
		}
		rdbuf = rdbuf[:rn]
		if rn < 1 {
			log.Println("Invalid packet:", rn, c.RemoteAddr())
			break
		}

		if this.OnNetRecv != nil {
			this.OnNetRecv(rn)
		}
		spdc.Data(rn)
		gopp.Assert(this.crbuf.Len()+int64(rn) <= this.crbuf.Cap(), "ring buffer full",
			this.crbuf.Len()+int64(rn), this.crbuf.Cap())
		wn, err := this.crbuf.Write(rdbuf)
		gopp.ErrPrint(err)
		gopp.Assert(wn == rn, "write ring buffer failed", rn, wn)
		this.doReadPacket(&nxtpktlen)
	}
	log.Println("done.", this.Sock.RemoteAddr(), tcpstname(this.Status))
	if this.OnClosed != nil {
		this.OnClosed(this)
	}
}
func (this *TCPSecureConn) doReadPacket(nxtpktlen *uint16) {
	stop := false
	for !stop {
		var rdbuf []byte
		switch {
		case this.Status == TCP_STATUS_NO_STATUS:
			// handshake request packet
			*nxtpktlen = (PUBLIC_KEY_SIZE+NONCE_SIZE)*2 + MAC_SIZE
			rdbuf = make([]byte, *nxtpktlen)
			rn, err := this.crbuf.Read(rdbuf)
			gopp.ErrPrint(err)
			gopp.Assert(rn == cap(rdbuf), "not read enough data", rn, cap(rdbuf))
		case this.Status == TCP_STATUS_UNCONFIRMED || this.Status == TCP_STATUS_CONFIRMED:
			// length+payload
			if *nxtpktlen == 0 && this.crbuf.Len() < int64(unsafe.Sizeof(uint16(0))) {
				return
			}
			if *nxtpktlen == 0 && this.crbuf.Len() >= int64(unsafe.Sizeof(uint16(0))) {
				pktlenbuf := make([]byte, 2)
				rn, err := this.crbuf.Read(pktlenbuf)
				gopp.ErrPrint(err, rn)
				err = binary.Read(bytes.NewBuffer(pktlenbuf), binary.BigEndian, nxtpktlen)
				gopp.ErrPrint(err)
			}
			if this.crbuf.Len() < int64(*nxtpktlen) {
				return
			}
			rdbuf = make([]byte, 2+*nxtpktlen)
			err := binary.Write(gopp.NewBufferBuf(rdbuf).WBufAt(0), binary.BigEndian, *nxtpktlen)
			gopp.ErrPrint(err)
			rn, err := this.crbuf.Read(rdbuf[2:])
			gopp.ErrPrint(err)
			gopp.Assert(rn+2 == cap(rdbuf), "not read enough data", rn+2, cap(rdbuf))
		}

		switch {
		case this.Status == TCP_STATUS_NO_STATUS:
			this.HandleHandshake(rdbuf)
			this.Status = TCP_STATUS_UNCONFIRMED
		case this.Status == TCP_STATUS_UNCONFIRMED:
			datlen, plnpkt, err := this.Unpacket(rdbuf)
			gopp.ErrPrint(err, len(rdbuf), *nxtpktlen, "//")
			ptype := plnpkt[0]
			log.Println("read data pkt:", len(rdbuf), datlen, ptype, tcppktname(ptype))
			this.HandlePingRequest(plnpkt)
			this.Status = TCP_STATUS_CONFIRMED
			if this.OnConfirmed != nil {
				this.OnConfirmed()
			}
		case this.Status == TCP_STATUS_CONFIRMED:
			// TODO read ringbuffer
			datlen, plnpkt, err := this.Unpacket(rdbuf)
			gopp.ErrPrint(err)
			ptype := plnpkt[0]
			if ptype < NUM_RESERVED_PORTS {
				log.Printf("read data pkt: rdlen:%d, datlen:%d, pktype: %d, pktname: %s\n",
					len(rdbuf), datlen, ptype, tcppktname(ptype))
			}
			switch {
			case ptype == TCP_PACKET_PING:
				// this.HandlePingRequest(plnpkt)
			case ptype == TCP_PACKET_PONG:
				// this.HandlePingResponse(plnpkt)
			case ptype == TCP_PACKET_ROUTING_RESPONSE:
				// this.HandleRoutingResponse(plnpkt)
			case ptype == TCP_PACKET_CONNECTION_NOTIFICATION:
				// this.HandleConnectionNotification(plnpkt)
			case ptype == TCP_PACKET_DISCONNECT_NOTIFICATION:
				// this.HandleDisconnectNotification(plnpkt)
			case ptype == TCP_PACKET_OOB_RECV: // TODO
			case ptype == TCP_PACKET_ONION_RESPONSE: // TODO
			case ptype >= NUM_RESERVED_PORTS:
				// this.HandleRoutingData(plnpkt)
			case ptype > TCP_PACKET_ONION_RESPONSE && ptype < NUM_RESERVED_PORTS:
				// this.HandleReservedData(plnpkt)
			default:
				log.Fatalln("wtf", ptype, tcppktname(ptype))
			}
		default:
			log.Fatalln("wtf", tcpstname(this.Status))
		}
		*nxtpktlen = 0
	}
}

func (this *TCPSecureConn) runWriteLoop() {
	spdc := NewSpeedCalc()

	flushCtrl := func() error {
		for len(this.cwctrlq) > 0 {
			data := <-this.cwctrlq
			atomic.AddInt32(&this.cwctrldlen, -int32(len(data)))
			var datai = []interface{}{data}
			wn, err := this.WritePacket(datai[0].([]byte))
			gopp.ErrPrint(err, wn, this.Sock.RemoteAddr())
			if err != nil {
				return err
			}
			spdc.Data(wn)
			if this.OnNetSent != nil {
				this.OnNetSent(wn)
			}
			// gopp.Assert(wn == len(datai[0].([]byte)), "write lost", wn, len(datai[0].([]byte)), this.ServAddr)
		}
		return nil
	}

	lastLogTime := time.Now().Add(-3 * time.Second)
	stop := false
	for !stop {
		data, ctrlq := []byte(nil), false
		select {
		case data = <-this.cwctrlq:
			atomic.AddInt32(&this.cwctrldlen, -int32(len(data)))
			ctrlq = true
		case data = <-this.cwdataq:
			atomic.AddInt32(&this.cwdatadlen, -int32(len(data)))
		}

		var datai = []interface{}{data}
		wn, err := this.WritePacket(datai[0].([]byte))
		gopp.ErrPrint(err, wn, this.Sock.RemoteAddr())
		if err != nil {
			goto endloop
		}
		spdc.Data(wn)
		if this.OnNetSent != nil {
			this.OnNetSent(wn)
		}
		// gopp.Assert(wn == len(datai[0].([]byte)), "write lost", wn, len(datai[0].([]byte)), this.ServAddr)
		if !ctrlq {
			err = flushCtrl()
			gopp.ErrPrint(err)
			if err != nil {
				goto endloop
			}
		}

		if int(time.Since(lastLogTime).Seconds()) >= 1 {
			lastLogTime = time.Now()
			log.Printf("------- async wrote ----- spd: %d, %s, pq:%d, cq:%d------\n",
				spdc.Avgspd, this.Sock.RemoteAddr(), len(this.cwctrlq), len(this.cwdataq))
		}
	}
endloop:
	log.Println("write routine done:", this.Sock.RemoteAddr())
}
func (this *TCPSecureConn) SetHandshakeInfo() {

}

func (this *TCPSecureConn) HandleHandshake(rdbuf []byte) {
	cliPubkey := NewCryptoKey(rdbuf[:PUBLIC_KEY_SIZE])
	cliTmpNonce := NewCBNonce(rdbuf[PUBLIC_KEY_SIZE : PUBLIC_KEY_SIZE+NONCE_SIZE])
	shrkey, err := CBBeforeNm(cliPubkey, this.Seckey)
	gopp.ErrPrint(err)
	this.Pubkey = cliPubkey

	cliplnpkt, err := DecryptDataSymmetric(shrkey, cliTmpNonce, rdbuf[PUBLIC_KEY_SIZE+NONCE_SIZE:])
	gopp.ErrPrint(err, len(rdbuf), len(cliplnpkt))
	hstmppk := NewCryptoKey(cliplnpkt[:PUBLIC_KEY_SIZE])
	log.Println("hs request from:", this.Sock.RemoteAddr(), hstmppk.ToHex()[:20], cliPubkey.ToHex()[:20])
	// gopp.Assert(hstmppk.Equal(this.SelfPubkey), info string, args ...interface{})
	this.RecvNonce = NewCBNonce(cliplnpkt[PUBLIC_KEY_SIZE : PUBLIC_KEY_SIZE+NONCE_SIZE])

	this.SentNonce = CBRandomNonce()
	srvTmpNonce := CBRandomNonce()

	tmpPubkey, tmpSeckey, _ := NewCBKeyPair()
	this.Shrkey, _ = CBBeforeNm(hstmppk, tmpSeckey)
	srvplnpkt := gopp.NewBufferZero()
	srvplnpkt.Write(tmpPubkey.Bytes())
	srvplnpkt.Write(this.SentNonce.Bytes())

	encpkt, err := EncryptDataSymmetric(shrkey, srvTmpNonce, srvplnpkt.Bytes())
	gopp.ErrPrint(err)

	wrbuf := gopp.NewBufferZero()
	wrbuf.Write(srvTmpNonce.Bytes())
	wrbuf.Write(encpkt)
	wn, err := this.Sock.Write(wrbuf.Bytes())
	gopp.ErrPrint(err, wn, wrbuf.Len())
}

func (this *TCPSecureConn) HandlePingRequest(rpkt []byte) {
	plnpkt := gopp.NewBufferZero()
	plnpkt.WriteByte(byte(TCP_PACKET_PONG))
	plnpkt.Write(rpkt[1:]) // pingid

	this.SendCtrlPacket(plnpkt.Bytes())
	// encpkt, err := this.CreatePacket(plnpkt.Bytes())
	// gopp.ErrPrint(err)
	// wn, err := this.conn.Write(encpkt)
	// gopp.ErrPrint(err, wn)
}

func (this *TCPSecureConn) WritePacket(data []byte) (int, error) {
	encpkt, err := this.CreatePacket(data)
	gopp.ErrPrint(err)
	wn, err := this.Sock.Write(encpkt)
	gopp.ErrPrint(err)
	if err == nil {
		this.SentNonce.Incr()
	}
	return wn, err
}

func (this *TCPSecureConn) SendCtrlPacket(data []byte) (encpkt []byte, err error) {
	if len(data) > 2048 {
		return nil, errors.Errorf("Data too long: %d, want: %d", len(data), 2048)
	}
	if len(this.cwctrlq) >= cap(this.cwctrlq) {
		log.Println("Ctrl queue is full, drop pkt...", len(data), this.cwctrldlen)
		return nil, errors.New("Ctrl queue is full")
	}
	btime := time.Now()
	select {
	case this.cwctrlq <- data:
		atomic.AddInt32(&this.cwctrldlen, int32(len(data)))
	default:
		log.Println("Ctrl queue is full, drop pkt...", len(data), this.cwctrldlen)
		return nil, errors.New("Ctrl queue is full")
	}
	// encpkt, err = this.CreatePacket(buf.Bytes())
	// this.WritePacket(encpkt)
	dtime := time.Since(btime)
	if dtime > 5*time.Millisecond {
		log.Fatalln("send use too long", len(data), dtime)
	} else if dtime > 2*time.Millisecond {
		log.Println("send use too long", len(data), dtime)
	}
	return
}

func (this *TCPSecureConn) MakePingPacket() []byte {
	/// first ping
	ping_plain := gopp.NewBufferZero()
	ping_plain.WriteByte(byte(TCP_PACKET_PING))
	pingid := rand.Uint64()
	pingid = gopp.IfElse(pingid == 0, uint64(1), pingid).(uint64)
	this.Pingid = pingid
	binary.Write(ping_plain, binary.BigEndian, pingid)
	log.Println("ping plnpkt len:", ping_plain.Len())

	encpkt, err := this.CreatePacket(ping_plain.Bytes())
	gopp.ErrPrint(err)

	if false {
		ping_encrypted, err := EncryptDataSymmetric(this.Shrkey, this.SentNonce, ping_plain.Bytes())
		gopp.ErrPrint(err)

		ping_pkt := gopp.NewBufferZero()
		binary.Write(ping_pkt, binary.BigEndian, uint16(len(ping_encrypted)))
		ping_pkt.Write(ping_encrypted)
		log.Println(ping_pkt.Len(), len(ping_encrypted))
		return ping_pkt.Bytes()
	}

	return encpkt
}

// tcp data packet, not include handshake packet
func (this *TCPSecureConn) CreatePacket(plain []byte) (encpkt []byte, err error) {
	// log.Println(len(plain), this.Shrkey.ToHex()[:20], this.SentNonce.ToHex())
	encdat, err := EncryptDataSymmetric(this.Shrkey, this.SentNonce, plain)
	gopp.ErrPrint(err)

	pktbuf := gopp.NewBufferZero()
	binary.Write(pktbuf, binary.BigEndian, uint16(len(encdat)))
	pktbuf.Write(encdat)
	encpkt = pktbuf.Bytes()
	// log.Println("create pkg:", tcppktname(plain[0]), len(encpkt), len(plain))
	// this.SentNonce.Incr()
	return
}
func (this *TCPSecureConn) Unpacket(encpkt []byte) (datlen uint16, plnpkt []byte, err error) {
	err = binary.Read(bytes.NewReader(encpkt), binary.BigEndian, &datlen)
	gopp.ErrPrint(err)
	plnpkt, err = DecryptDataSymmetric(this.Shrkey, this.RecvNonce, encpkt[2:])
	this.RecvNonce.Incr()
	return
}

/////
func NewTCPServer(ports []uint16, seckey *CryptoKey, oniono Object) *TCPServer {
	this := &TCPServer{}
	this.Seckey = seckey
	this.Pubkey = CBDerivePubkey(seckey)
	this.Conns = map[string]*TCPSecureConn{}
	this.HSConns = map[net.Conn]*TCPSecureConn{}

	for i, port := range ports {
		lsner, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		gopp.ErrPrint(err, port)
		if err != nil {
			return nil
		}
		log.Println("listened on:", i, lsner.Addr().String())
		this.lsners = append(this.lsners, lsner)
	}

	return this
}

func (this *TCPServer) Start() {
	for _, lsner := range this.lsners {
		go this.runAcceptProc(lsner)
	}
}

// should block
func (this *TCPServer) runAcceptProc(lsner net.Listener) {
	stop := false
	for !stop {
		c, err := lsner.Accept()
		gopp.ErrPrint(err, lsner.Addr())
		if err != nil {
			break
		}
		this.startHandshake(c)
	}
	log.Println("done", lsner.Addr())
}

func (this *TCPServer) startHandshake(c net.Conn) {
	this.hsconnmu.Lock()
	defer this.hsconnmu.Unlock()
	secon := NewTCPSecureConn(c)
	secon.Seckey = this.Seckey
	this.HSConns[c] = secon
	secon.Start()
}
