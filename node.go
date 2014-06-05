// Gyre is Golang port of Zyre, an open-source framework for proximity-based
// peer-to-peer applications.
// Gyre does local area discovery and clustering. A Gyre node broadcasts
// UDP beacons, and connects to peers that it finds. This class wraps a
// Gyre node with a message-based API.
package gyre

import (
	"github.com/armen/gyre/beacon"
	"github.com/armen/gyre/msg"
	zmq "github.com/vaughan0/go-zmq"

	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const (
	// IANA-assigned port for ZRE discovery protocol
	zreDiscoveryPort = 5670

	beaconVersion = 0x1

	// Port range 0xc000~0xffff is defined by IANA for dynamic or private ports
	// We use this when choosing a port for dynamic binding
	dynPortFrom uint16 = 0xc000
	dynPortTo   uint16 = 0xffff
)

const (
	EventEnter   = "ENTER"
	EventExit    = "EXIT"
	EventWhisper = "WHISPER"
	EventShout   = "SHOUT"
	EventJoin    = "JOIN"
	EventLeave   = "LEAVE"
	EventSet     = "SET"
)

type sig struct {
	Protocol [3]byte
	Version  byte
	Uuid     []byte
	Port     uint16
}

type Event struct {
	Type    string
	Peer    string
	Group   string
	Key     string // Only used for EventSet
	Content []byte
}

type Node struct {
	quit chan struct{}  // quit is used to signal handler about quiting
	wg   sync.WaitGroup // wait group is used to wait until handler() is done

	events     chan *Event
	commands   chan *Event
	Beacon     *beacon.Beacon
	Uuid       []byte            // Our UUID
	Identity   string            // Our UUID as hex string
	inbox      *zmq.Socket       // Our inbox socket (ROUTER)
	Host       string            // Our host IP address
	Port       uint16            // Our inbox port number
	Status     byte              // Our own change counter
	Peers      map[string]*peer  // Hash of known peers, fast lookup
	PeerGroups map[string]*group // Groups that our peers are in
	OwnGroups  map[string]*group // Groups that we are in
	Headers    map[string]string // Our header values
}

// NewNode creates a new node.
func NewNode() (node *Node, err error) {
	node = &Node{
		quit:       make(chan struct{}),
		events:     make(chan *Event),
		commands:   make(chan *Event),
		Peers:      make(map[string]*peer),
		PeerGroups: make(map[string]*group),
		OwnGroups:  make(map[string]*group),
		Headers:    make(map[string]string),
	}
	node.wg.Add(1) // We're going to wait until handler() is done

	node.inbox, err = zmq.NewSocket(zmq.Router)
	if err != nil {
		return nil, err
	}

	for i := dynPortFrom; i <= dynPortTo; i++ {
		rand.Seed(time.Now().UTC().UnixNano())
		port := uint16(rand.Intn(int(dynPortTo-dynPortFrom))) + dynPortFrom
		err = node.inbox.Bind(fmt.Sprintf("tcp://*:%d", port))
		if err == nil {
			node.Port = port
			break
		}
	}

	// Generate random uuid
	node.Uuid = make([]byte, 16)
	io.ReadFull(crand.Reader, node.Uuid)
	node.Identity = fmt.Sprintf("%X", node.Uuid)

	s := &sig{}
	s.Protocol[0] = 'Z'
	s.Protocol[1] = 'R'
	s.Protocol[2] = 'E'
	s.Version = beaconVersion
	s.Uuid = node.Uuid
	s.Port = node.Port

	buffer := new(bytes.Buffer)
	binary.Write(buffer, binary.BigEndian, s.Protocol)
	binary.Write(buffer, binary.BigEndian, s.Version)
	binary.Write(buffer, binary.BigEndian, s.Uuid)
	binary.Write(buffer, binary.BigEndian, s.Port)

	// Create a beacon
	node.Beacon, err = beacon.New(zreDiscoveryPort)
	if err != nil {
		return nil, err
	}
	node.Host = node.Beacon.Addr()
	node.Beacon.NoEcho()
	node.Beacon.Subscribe([]byte("ZRE"))
	node.Beacon.Publish(buffer.Bytes())

	go node.handle()

	return
}

// Sends message to single peer. peer ID is first frame in message.
func (n *Node) Whisper(identity string, content []byte) *Node {
	n.commands <- &Event{
		Type:    EventWhisper,
		Peer:    identity,
		Content: content,
	}
	return n
}

// Sends message to a group of peers.
func (n *Node) Shout(group string, content []byte) *Node {
	n.commands <- &Event{
		Type:    EventShout,
		Group:   group,
		Content: content,
	}
	return n
}

// Joins a group.
func (n *Node) Join(group string) *Node {
	n.commands <- &Event{
		Type:  EventJoin,
		Group: group,
	}
	return n
}

func (n *Node) Leave(group string) *Node {
	n.commands <- &Event{
		Type:  EventLeave,
		Group: group,
	}
	return n
}

func (n *Node) Set(key, value string) *Node {
	n.commands <- &Event{
		Type:    EventSet,
		Key:     key,
		Content: []byte(value),
	}
	return n
}

func (n *Node) Get(key string) (header string) {
	return n.Headers[key]
}

func (n *Node) whisper(identity string, content []byte) {

	// Get peer to send message to
	peer, ok := n.Peers[identity]

	// Send frame on out to peer's mailbox, drop message
	// if peer doesn't exist (may have been destroyed)
	if ok {
		m := msg.NewWhisper()
		m.Content = content
		peer.send(m)
	}
}

func (n *Node) shout(group string, content []byte) {
	// Get group to send message to
	if g, ok := n.PeerGroups[group]; ok {
		m := msg.NewShout()
		m.Group = group
		m.Content = content
		g.send(m)
	}
}

func (n *Node) join(group string) {
	if _, ok := n.OwnGroups[group]; !ok {

		// Only send if we're not already in group
		n.OwnGroups[group] = newGroup(group)
		m := msg.NewJoin()
		m.Group = group

		// Update status before sending command
		n.Status++
		m.Status = n.Status

		for _, peer := range n.Peers {
			cloned := msg.Clone(m)
			peer.send(cloned)
		}
	}
}

func (n *Node) leave(group string) {
	if _, ok := n.OwnGroups[group]; ok {
		// Only send if we are actually in group
		m := msg.NewLeave()
		m.Group = group

		// Update status before sending command
		n.Status++
		m.Status = n.Status

		for _, peer := range n.Peers {
			cloned := msg.Clone(m)
			peer.send(cloned)
		}
		delete(n.OwnGroups, group)
	}
}

func (n *Node) set(key string, value []byte) {
	n.Headers[key] = string(value)
}

// Chan returns events channel
func (n *Node) Chan() chan *Event {
	return n.events
}

func (n *Node) handle() {
	defer func() {
		n.wg.Done()
	}()

	chans := n.inbox.Channels()
	defer chans.Close()

	ping := time.After(reapInterval)
	stype := n.inbox.GetType()

	for {
		select {
		case <-n.quit:
			return

		case e := <-n.commands:
			// Received a command from the caller/API
			switch e.Type {
			case EventWhisper:
				n.whisper(e.Peer, e.Content)
			case EventShout:
				n.shout(e.Group, e.Content)
			case EventJoin:
				n.join(e.Group)
			case EventLeave:
				n.leave(e.Group)
			case EventSet:
				n.set(e.Key, e.Content)
			}

		case frames := <-chans.In():
			transit, err := msg.Unmarshal(stype, frames...)
			if err != nil {
				continue
			}
			n.recvFromPeer(transit)

		case s := <-n.Beacon.Signals():
			n.recvFromBeacon(s)

		case err := <-chans.Errors():
			log.Println(err)

		case <-ping:
			ping = time.After(reapInterval)
			for _, peer := range n.Peers {
				n.pingPeer(peer)
			}
		}
	}
}

// recvFromPeer handles messages coming from other peers
func (n *Node) recvFromPeer(transit msg.Transit) {
	// Router socket tells us the identity of this peer
	// Identity must be [1] followed by 16-byte UUID, ignore the [1]
	identity := string(transit.Address()[1:])

	peer := n.Peers[identity]

	switch m := transit.(type) {
	case *msg.Hello:
		// On HELLO we may create the peer if it's unknown
		// On other commands the peer must already exist
		peer = n.requirePeer(identity, m.Ipaddress, m.Mailbox)
		peer.ready = true
	}

	// Ignore command if peer isn't ready
	if peer == nil || !peer.ready {
		log.Printf("W: [%s] peer %s wasn't ready, ignoring a %s message", n.Identity, identity, transit)
		return
	}

	if !peer.checkMessage(transit) {
		log.Printf("W: [%s] lost messages from %s", n.Identity, identity)
		return
	}

	// Now process each command
	switch m := transit.(type) {
	case *msg.Hello:
		// Store peer headers for future reference
		for key, val := range m.Headers {
			peer.headers[key] = val
		}

		// Join peer to listed groups
		for _, group := range m.Groups {
			n.joinPeerGroup(peer, group)
		}

		// Hello command holds latest status of peer
		peer.status = m.Status

	case *msg.Whisper:
		// Pass up to caller API as WHISPER event
		n.events <- &Event{
			Type:    EventWhisper,
			Peer:    identity,
			Content: m.Content,
		}

	case *msg.Shout:
		// Pass up to caller as SHOUT event
		n.events <- &Event{
			Type:    EventShout,
			Peer:    identity,
			Group:   m.Group,
			Content: m.Content,
		}

	case *msg.Ping:
		ping := msg.NewPingOk()
		peer.send(ping)

	case *msg.Join:
		n.joinPeerGroup(peer, m.Group)
		if m.Status != peer.status {
			log.Printf("W: [%s] message status isn't equal to peer status, %d != %d", n.Identity, m.Status, peer.status)
		}

	case *msg.Leave:
		n.leavePeerGroup(peer, m.Group)
		if m.Status != peer.status {
			log.Printf("W: [%s] message status isn't equal to peer status, %d != %d", n.Identity, m.Status, peer.status)
		}
	}

	// Activity from peer resets peer timers
	peer.refresh()
}

// recvFromBeacon handles a new signal received from beacon
func (n *Node) recvFromBeacon(b *beacon.Signal) {
	// Get IP address and beacon of peer

	parts := strings.SplitN(b.Addr, ":", 2)
	ipaddress := parts[0]

	s := &sig{}
	buffer := bytes.NewBuffer(b.Transmit)
	binary.Read(buffer, binary.BigEndian, &s.Protocol)
	binary.Read(buffer, binary.BigEndian, &s.Version)

	uuid := make([]byte, 16)
	binary.Read(buffer, binary.BigEndian, uuid)
	s.Uuid = append(s.Uuid, uuid...)

	binary.Read(buffer, binary.BigEndian, &s.Port)

	// Ignore anything that isn't a valid beacon
	if s.Version == beaconVersion {
		// Check that the peer, identified by its UUID, exists
		identity := fmt.Sprintf("%X", s.Uuid)
		peer := n.requirePeer(identity, ipaddress, s.Port)
		peer.refresh()
	}
}

// requirePeer finds or creates peer via its UUID string
func (n *Node) requirePeer(identity, address string, port uint16) (peer *peer) {
	peer, ok := n.Peers[identity]
	if !ok {
		// Purge any previous peer on same endpoint
		endpoint := fmt.Sprintf("%s:%d", address, port)
		for _, p := range n.Peers {
			if p.endpoint == endpoint {
				p.disconnect()
			}
		}

		peer = newPeer(identity)
		peer.connect(n.Identity, endpoint)

		// Handshake discovery by sending HELLO as first message
		m := msg.NewHello()
		m.Ipaddress = n.Host
		m.Mailbox = n.Port
		m.Status = n.Status
		for key := range n.OwnGroups {
			m.Groups = append(m.Groups, key)
		}
		for key, header := range n.Headers {
			m.Headers[key] = header
		}
		peer.send(m)
		n.Peers[identity] = peer

		// Now tell the caller about the peer
		n.events <- &Event{
			Type: EventEnter,
			Peer: identity,
		}
	}

	return peer
}

// requirePeerGroup finds or creates group via its name
func (n *Node) requirePeerGroup(name string) (group *group) {
	group, ok := n.PeerGroups[name]
	if !ok {
		group = newGroup(name)
		n.PeerGroups[name] = group
	}

	return
}

// joinPeerGroup joins the pear to a group
func (n *Node) joinPeerGroup(peer *peer, name string) {
	group := n.requirePeerGroup(name)
	group.join(peer)

	// Now tell the caller about the peer joined group
	n.events <- &Event{
		Type:  EventJoin,
		Peer:  peer.identity,
		Group: name,
	}
}

// leavePeerGroup leaves the pear to a group
func (n *Node) leavePeerGroup(peer *peer, name string) {
	group := n.requirePeerGroup(name)
	group.leave(peer)

	// Now tell the caller about the peer left group
	n.events <- &Event{
		Type:  EventLeave,
		Peer:  peer.identity,
		Group: name,
	}
}

// We do this once a second:
// - if peer has gone quiet, send TCP ping
// - if peer has disappeared, expire it
func (n *Node) pingPeer(peer *peer) {
	if time.Now().Unix() >= peer.expiredAt.Unix() {
		// If peer has really vanished, expire it
		n.events <- &Event{
			Type: EventExit,
			Peer: peer.identity,
		}
		for _, group := range n.PeerGroups {
			group.leave(peer)
		}
		// It's really important to disconnect from the peer before
		// deleting it, unless we'd end up difficulties to reconnect
		// to the same endpoint
		peer.disconnect()
		delete(n.Peers, peer.identity)
	} else if time.Now().Unix() >= peer.evasiveAt.Unix() {
		//  If peer is being evasive, force a TCP ping.
		//  TODO: do this only once for a peer in this state;
		//  it would be nicer to use a proper state machine
		//  for peer management.
		m := msg.NewPing()
		peer.send(m)
	}
}

// Disconnect leaves all the groups and the closes all the connections to the peers
func (n *Node) Disconnect() {
	close(n.quit)
	n.wg.Wait()

	// Close sockets on a signal
	for group := range n.OwnGroups {
		// Note that n.leave is used not n.Leave because we're already in select
		// and Leave sends communicate to events channel which obviously blocks
		n.leave(group)
	}
	// Disconnect from all peers
	for peerId, peer := range n.Peers {
		// It's really important to disconnect from the peer before
		// deleting it, unless we'd end up difficulties to reconnect
		// to the same endpoint
		peer.disconnect()
		delete(n.Peers, peerId)
	}
	// Now it's safe to close the socket
	n.inbox.Unbind(fmt.Sprintf("tcp://*:%d", n.Port))
	n.inbox.Close()
}
