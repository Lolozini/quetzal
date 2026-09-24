package main

// Minecraft Java Edition's first packets, enough to tell a player from a port
// scanner.
//
// Waking on any TCP connection let every scanner on the internet wake a sleeping
// server, and each wake bought it a full idle window before it could sleep
// again: a public server spent its nights cycling awake for nobody. A client's
// first packet says what it wants. A server-list ping (next state 1) only reads
// the MOTD, and is answered here with the server shown as asleep; a login (2),
// or a transfer from another server (3), is a player joining, and is the only
// thing that wakes it. Anything that is not a Minecraft handshake at all -- an
// empty connection, a banner grab, an HTTP probe -- wakes nothing.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	mcStateStatus   = 1
	mcStateLogin    = 2
	mcStateTransfer = 3

	// mcMaxHandshake bounds the handshake packet: an id, a protocol version, an
	// address of at most 255 characters (Forge and BungeeCord append to it, so
	// allow room), a port and a state.
	mcMaxHandshake = 2048
	// mcMaxPacket bounds the few other packets read here (status request, ping,
	// login start), which are all small.
	mcMaxPacket = 512

	// What a player sees while the server sleeps: in the server list, and on the
	// disconnect screen after joining wakes it.
	mcAsleepMOTD   = "Asleep: join to wake it up"
	mcWakingReason = "The server is waking up. Reconnect in a minute."
)

var errNotMinecraft = errors.New("not a Minecraft handshake")

// mcHandshake is a client's first packet.
type mcHandshake struct {
	raw      []byte // the packet as received, length included, to replay to the server
	protocol int32
	address  string
	port     uint16
	next     int32
}

// joining reports whether the client is a player joining (login or transfer),
// as opposed to reading the server list.
func (h *mcHandshake) joining() bool {
	return h.next == mcStateLogin || h.next == mcStateTransfer
}

// byteReader reads one byte at a time from r, so nothing past the packets
// parsed here is consumed: in proxy mode the rest belongs to the server.
type byteReader struct{ r io.Reader }

func (b byteReader) ReadByte() (byte, error) {
	var p [1]byte
	_, err := io.ReadFull(b.r, p[:])
	return p[0], err
}

// readVarInt reads a VarInt and returns it with the bytes it was made of.
func readVarInt(r io.ByteReader) (int32, []byte, error) {
	var v uint32
	var raw []byte
	for i := 0; i < 5; i++ {
		c, err := r.ReadByte()
		if err != nil {
			return 0, raw, err
		}
		raw = append(raw, c)
		v |= uint32(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return int32(v), raw, nil
		}
	}
	return 0, raw, errNotMinecraft
}

func appendVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for u >= 0x80 {
		b = append(b, byte(u)|0x80)
		u >>= 7
	}
	return append(b, byte(u))
}

// readPacket reads one length-prefixed packet, returning its id, its body and
// every byte read. The ids read here are all below 0x80, one byte each, which
// is read before the rest: an HTTP probe or a legacy ping announces a length
// it never sends, and is turned away on that byte instead of being waited for.
func readPacket(r io.Reader, max int32) (id int32, body, raw []byte, err error) {
	n, lraw, err := readVarInt(byteReader{r})
	if err != nil {
		return 0, nil, nil, err
	}
	if n < 1 || n > max {
		return 0, nil, nil, errNotMinecraft
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload[:1]); err != nil {
		return 0, nil, nil, err
	}
	if payload[0] >= 0x80 {
		return 0, nil, nil, errNotMinecraft
	}
	if _, err := io.ReadFull(r, payload[1:]); err != nil {
		return 0, nil, nil, err
	}
	return int32(payload[0]), payload[1:], append(lraw, payload...), nil
}

// readHandshakePacket is readPacket for the first packet, which must be a
// handshake (id 0): anything else is refused on its first byte.
func readHandshakePacket(r io.Reader) (body, raw []byte, err error) {
	n, lraw, err := readVarInt(byteReader{r})
	if err != nil {
		return nil, nil, err
	}
	if n < 1 || n > mcMaxHandshake {
		return nil, nil, errNotMinecraft
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload[:1]); err != nil {
		return nil, nil, err
	}
	if payload[0] != 0 {
		return nil, nil, errNotMinecraft
	}
	if _, err := io.ReadFull(r, payload[1:]); err != nil {
		return nil, nil, err
	}
	return payload[1:], append(lraw, payload...), nil
}

// readMCHandshake reads and checks a client's first packet. Anything that is not
// exactly a handshake is errNotMinecraft (or the read error).
func readMCHandshake(r io.Reader) (*mcHandshake, error) {
	body, raw, err := readHandshakePacket(r)
	if err != nil {
		return nil, err
	}
	br := bytes.NewReader(body)
	h := &mcHandshake{raw: raw}
	if h.protocol, _, err = readVarInt(br); err != nil {
		return nil, errNotMinecraft
	}
	if h.address, err = readString(br, mcMaxHandshake); err != nil {
		return nil, errNotMinecraft
	}
	var port [2]byte
	if _, err := io.ReadFull(br, port[:]); err != nil {
		return nil, errNotMinecraft
	}
	h.port = binary.BigEndian.Uint16(port[:])
	if h.next, _, err = readVarInt(br); err != nil {
		return nil, errNotMinecraft
	}
	if br.Len() != 0 || h.next < mcStateStatus || h.next > mcStateTransfer {
		return nil, errNotMinecraft
	}
	return h, nil
}

func readString(r *bytes.Reader, max int) (string, error) {
	n, _, err := readVarInt(r)
	if err != nil || n < 0 || int(n) > max || int(n) > r.Len() {
		return "", errNotMinecraft
	}
	b := make([]byte, n)
	_, _ = io.ReadFull(r, b)
	if !utf8.Valid(b) {
		return "", errNotMinecraft
	}
	return string(b), nil
}

func appendString(b []byte, s string) []byte {
	return append(appendVarInt(b, int32(len(s))), s...)
}

// writePacket writes a length-prefixed packet.
func writePacket(w io.Writer, id int32, body []byte) error {
	data := append(appendVarInt(nil, id), body...)
	_, err := w.Write(append(appendVarInt(nil, int32(len(data))), data...))
	return err
}

// serveMCStatus answers a server-list ping: the status request with the server
// shown as asleep, then the ping with its pong. The client's own protocol
// version is echoed so the list does not flag the server as incompatible.
func serveMCStatus(rw io.ReadWriter, h *mcHandshake) error {
	id, _, _, err := readPacket(rw, mcMaxPacket)
	if err != nil {
		return err
	}
	if id != 0 {
		return errNotMinecraft
	}
	status, _ := json.Marshal(map[string]any{
		"version":     map[string]any{"name": "Quetzal", "protocol": h.protocol},
		"players":     map[string]any{"max": 0, "online": 0},
		"description": map[string]any{"text": mcAsleepMOTD, "color": "gray"},
	})
	if err := writePacket(rw, 0, appendString(nil, string(status))); err != nil {
		return err
	}
	// The ping is optional: some clients close after the status.
	id, body, _, err := readPacket(rw, mcMaxPacket)
	if err != nil || id != 1 || len(body) != 8 {
		return nil
	}
	return writePacket(rw, 1, body)
}

// readMCLoginName reads the login start packet for the player's name, best
// effort: it is only for the log.
func readMCLoginName(r io.Reader) string {
	id, body, _, err := readPacket(r, mcMaxPacket)
	if err != nil || id != 0 {
		return "?"
	}
	name, err := readString(bytes.NewReader(body), 16*4)
	if err != nil || name == "" {
		return "?"
	}
	return name
}

// mcDisconnect ends a login with a message on the player's screen. In the login
// state the reason is still a JSON text component, in every version.
func mcDisconnect(w io.Writer, reason string) error {
	msg, _ := json.Marshal(map[string]string{"text": reason})
	return writePacket(w, 0, appendString(nil, string(msg)))
}

// String names a handshake for the log.
func (h *mcHandshake) String() string {
	return fmt.Sprintf("protocol %d, %s:%d, state %d", h.protocol, h.address, h.port, h.next)
}
