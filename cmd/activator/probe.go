package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Outside Minecraft Java, whose handshake says whether a client is joining,
// anything woke a sleeping server: a port scanner's bare connection, a server
// list refreshing, a tracker's query. On the internet that meant always. What
// follows tells those apart from a player without knowing the game: it only
// sets aside what is certainly not one, so no game's player is ever kept out.

// scanWindow is how long a TCP connection to a sleeping server has to show it
// is not a port scan. A scanner connects and hangs up at once, without a byte;
// a client speaks first, as nearly every game's does, or waits for the server
// to, and either way is still there. Atomic because tests shorten it while
// connection handlers of an earlier test may still read it.
var scanWindow atomic.Int64

func init() { scanWindow.Store(int64(3 * time.Second)) }

// firstMove waits up to scanWindow for a client's first bytes and reports
// whether the connection is a client rather than a port scan. What it read is
// returned, for a proxy to pass on.
func firstMove(conn net.Conn) (bool, []byte) {
	_ = conn.SetReadDeadline(time.Now().Add(time.Duration(scanWindow.Load())))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	_ = conn.SetReadDeadline(time.Time{})
	if n > 0 {
		return true, buf[:n]
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true, nil // waiting for the server to speak first
	}
	return false, nil // hung up without a word
}

// dropAny closes a connection to a game whose protocol the activator does not
// speak, and wakes the server unless it was a port scan.
func dropAny(conn net.Conn, w *waker) {
	client, _ := firstMove(conn)
	_ = conn.Close()
	if client {
		w.trigger()
	}
}

// packetKind is what a UDP packet is to the wake gate.
type packetKind int

const (
	// packetOther is anything the gate cannot place: a player, unless the
	// game's own protocol says otherwise.
	packetOther packetKind = iota
	// packetQuery asks about the server without joining it: a server list, a
	// tracker, a scanner. It never wakes a server nor keeps it awake.
	packetQuery
	// packetPing is RakNet's unconnected ping, how Minecraft Bedrock's server
	// list (and Geyser's, in front of a Java server) sees whether a server is
	// there. A query, which the activator may also answer (bedrockPong).
	packetPing
	// packetJoin opens a RakNet connection: a player joining.
	packetJoin
)

// raknetMagic marks RakNet's offline messages.
var raknetMagic = []byte{0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe, 0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78}

const (
	raknetPing          = 0x01 // unconnected ping
	raknetPingOpen      = 0x02 // unconnected ping, open connections only
	raknetOpenRequest1  = 0x05 // open connection request 1
	raknetOpenRequest2  = 0x07 // open connection request 2
	raknetPong          = 0x1c // unconnected pong
	raknetPingLen       = 1 + 8 + 16 + 8
	raknetPongHeaderLen = 1 + 8 + 8 + 16 + 2
)

// queryPrefixes start packets that only ever ask about a server: Source's A2S
// queries (Steam's server browser, and the trackers that watch it) and A2A
// ping, the GameSpy query protocol (Minecraft's own query among others), and
// the SSDP and SIP probes of UDP scanners.
var queryPrefixes = [][]byte{
	{0xff, 0xff, 0xff, 0xff, 'T'}, // A2S_INFO
	{0xff, 0xff, 0xff, 0xff, 'U'}, // A2S_PLAYER
	{0xff, 0xff, 0xff, 0xff, 'V'}, // A2S_RULES
	{0xff, 0xff, 0xff, 0xff, 'W'}, // A2S_SERVERQUERY_GETCHALLENGE
	{0xff, 0xff, 0xff, 0xff, 'i'}, // A2A_PING
	{0xfe, 0xfd},                  // GameSpy query
	[]byte("M-SEARCH "),
	[]byte("OPTIONS sip:"),
	[]byte("REGISTER sip:"),
}

func classifyUDP(b []byte) packetKind {
	switch {
	case len(b) >= raknetPingLen && (b[0] == raknetPing || b[0] == raknetPingOpen) && bytes.Equal(b[9:25], raknetMagic):
		return packetPing
	case len(b) >= 17 && (b[0] == raknetOpenRequest1 || b[0] == raknetOpenRequest2) && bytes.Equal(b[1:17], raknetMagic):
		return packetJoin
	}
	for _, p := range queryPrefixes {
		if bytes.HasPrefix(b, p) {
			return packetQuery
		}
	}
	return packetOther
}

// playerPacket reports whether a UDP packet is a player's: whether it wakes the
// server and counts as activity.
func (g wakeGate) playerPacket(k packetKind) bool {
	switch k {
	case packetQuery, packetPing:
		return false
	case packetJoin:
		return true // on a Minecraft Java server, a Bedrock player through Geyser
	}
	return !g.minecraft() // a Minecraft Java server's other UDP is its query port
}

// bedrockPong remembers the last line a Minecraft Bedrock server (or Geyser)
// gave the server list, so that while it sleeps the activator can answer its
// pings in its place, with the server shown asleep. It never makes a line up: one
// with a version the client does not expect would mark the server incompatible,
// so until the server has answered once, a sleeping one is not answered at all.
type bedrockPong struct {
	mu   sync.Mutex
	guid []byte
	line string
}

// remember keeps a pong the server sent.
func (b *bedrockPong) remember(pkt []byte) {
	if len(pkt) < raknetPongHeaderLen || pkt[0] != raknetPong || !bytes.Equal(pkt[17:33], raknetMagic) {
		return
	}
	n := int(binary.BigEndian.Uint16(pkt[33:35]))
	if len(pkt) < raknetPongHeaderLen+n {
		return
	}
	line := string(pkt[raknetPongHeaderLen : raknetPongHeaderLen+n])
	if !strings.HasPrefix(line, "MCPE;") && !strings.HasPrefix(line, "MCEE;") {
		return
	}
	b.mu.Lock()
	b.guid = append([]byte(nil), pkt[9:17]...)
	b.line = line
	b.mu.Unlock()
}

// asleep answers ping with the server's own line, showing it asleep and empty,
// or returns nil when the server never answered one.
func (b *bedrockPong) asleep(ping []byte) []byte {
	b.mu.Lock()
	guid, line := b.guid, b.line
	b.mu.Unlock()
	if line == "" || len(ping) < raknetPingLen {
		return nil
	}
	// MCPE;<motd>;<protocol>;<version>;<players>;<max players>;<server id>;...
	f := strings.Split(line, ";")
	if len(f) > 1 {
		f[1] = mcAsleepMOTD
	}
	if len(f) > 4 {
		f[4] = "0"
	}
	line = strings.Join(f, ";")
	out := make([]byte, 0, raknetPongHeaderLen+len(line))
	out = append(out, raknetPong)
	out = append(out, ping[1:9]...) // the ping's time, echoed
	out = append(out, guid...)
	out = append(out, raknetMagic...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(line)))
	return append(out, line...)
}
