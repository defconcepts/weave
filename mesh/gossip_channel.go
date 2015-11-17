package mesh

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"sync"
)

type GossipChannel struct {
	sync.Mutex
	name     string
	ourself  *LocalPeer
	routes   *Routes
	gossiper Gossiper
	senders  connectionSenders
}

type connectionSenders map[Connection]*GossipSender

func NewGossipChannel(channelName string, ourself *LocalPeer, routes *Routes, g Gossiper) *GossipChannel {
	return &GossipChannel{
		name:     channelName,
		ourself:  ourself,
		routes:   routes,
		gossiper: g,
		senders:  make(connectionSenders)}
}

func (router *Router) handleGossip(tag ProtocolTag, payload []byte) error {
	decoder := gob.NewDecoder(bytes.NewReader(payload))
	var channelName string
	if err := decoder.Decode(&channelName); err != nil {
		return err
	}
	channel := router.gossipChannel(channelName)
	var srcName PeerName
	if err := decoder.Decode(&srcName); err != nil {
		return err
	}
	switch tag {
	case ProtocolGossipUnicast:
		return channel.deliverUnicast(srcName, payload, decoder)
	case ProtocolGossipBroadcast:
		return channel.deliverBroadcast(srcName, payload, decoder)
	case ProtocolGossip:
		return channel.deliver(srcName, payload, decoder)
	}
	return nil
}

func (c *GossipChannel) deliverUnicast(srcName PeerName, origPayload []byte, dec *gob.Decoder) error {
	var destName PeerName
	if err := dec.Decode(&destName); err != nil {
		return err
	}
	if c.ourself.Name != destName {
		if err := c.relayUnicast(destName, origPayload); err != nil {
			// just log errors from relayUnicast; a problem between us and destination
			// is not enough reason to break the connection from the source
			c.log(err)
		}
		return nil
	}
	var payload []byte
	if err := dec.Decode(&payload); err != nil {
		return err
	}
	return c.gossiper.OnGossipUnicast(srcName, payload)
}

func (c *GossipChannel) deliverBroadcast(srcName PeerName, _ []byte, dec *gob.Decoder) error {
	var payload []byte
	if err := dec.Decode(&payload); err != nil {
		return err
	}
	data, err := c.gossiper.OnGossipBroadcast(srcName, payload)
	if err != nil || data == nil {
		return err
	}
	return c.relayBroadcast(srcName, data)
}

func (c *GossipChannel) deliver(srcName PeerName, _ []byte, dec *gob.Decoder) error {
	var payload []byte
	if err := dec.Decode(&payload); err != nil {
		return err
	}
	if data, err := c.gossiper.OnGossip(payload); err != nil {
		return err
	} else if data != nil {
		c.Send(srcName, data)
	}
	return nil
}

func (c *GossipChannel) GossipUnicast(dstPeerName PeerName, msg []byte) error {
	return c.relayUnicast(dstPeerName, GobEncode(c.name, c.ourself.Name, dstPeerName, msg))
}

func (c *GossipChannel) GossipBroadcast(update GossipData) error {
	return c.relayBroadcast(c.ourself.Name, update)
}

func (c *GossipChannel) relayUnicast(dstPeerName PeerName, buf []byte) (err error) {
	if relayPeerName, found := c.routes.UnicastAll(dstPeerName); !found {
		err = fmt.Errorf("unknown relay destination: %s", dstPeerName)
	} else if conn, found := c.ourself.ConnectionTo(relayPeerName); !found {
		err = fmt.Errorf("unable to find connection to relay peer %s", relayPeerName)
	} else {
		conn.(ProtocolSender).SendProtocolMsg(ProtocolMsg{ProtocolGossipUnicast, buf})
	}
	return err
}

func (c *GossipChannel) relayBroadcast(srcName PeerName, update GossipData) error {
	c.routes.EnsureRecalculated()
	nextHops := c.routes.BroadcastAll(srcName)
	if len(nextHops) == 0 {
		return nil
	}

	blockedConnections := make(ConnectionSet)
	connections := c.ourself.ConnectionsTo(nextHops)
	for _, msg := range update.Encode() {
		protocolMsg := ProtocolMsg{ProtocolGossipBroadcast, GobEncode(c.name, srcName, msg)}
		for _, conn := range connections {
			if !conn.(ProtocolSender).SendOrDropProtocolMsg(protocolMsg) {
				blockedConnections[conn] = void
			}
		}
	}
	// for any blocked connections we send the broadcast as a normal
	// gossip instead, which is better than dropping it completely.
	c.sendDown(blockedConnections, update)

	return nil
}

func (c *GossipChannel) Send(srcName PeerName, data GossipData) {
	// do this outside the lock below so we avoid lock nesting
	c.routes.EnsureRecalculated()
	selectedConnections := make(ConnectionSet)
	for name := range c.routes.RandomNeighbours(srcName) {
		if conn, found := c.ourself.ConnectionTo(name); found {
			selectedConnections[conn] = void
		}
	}
	c.sendDown(selectedConnections, data)
}

func (c *GossipChannel) SendDown(conn Connection, data GossipData) {
	c.sendDown(ConnectionSet{conn: void}, data)
}

func (c *GossipChannel) sendDown(selectedConnections ConnectionSet, data GossipData) {
	if len(selectedConnections) == 0 {
		return
	}
	connections := c.ourself.Connections()
	c.Lock()
	defer c.Unlock()
	// GC - randomly (courtesy of go's map iterator) pick some
	// existing senders and stop&remove them if the associated
	// connection is no longer active.  We stop as soon as we
	// encounter a valid entry; the idea being that when there is
	// little or no garbage then this executes close to O(1)[1],
	// whereas when there is lots of garbage we remove it quickly.
	//
	// [1] TODO Unfortunately, due to the desire to avoid nested
	// locks, instead of simply invoking LocalPeer.ConnectionTo(name),
	// we operate on LocalPeer.Connections(). That is
	// O(n_our_connections) at best.
	for conn, sender := range c.senders {
		if _, found := connections[conn]; !found {
			delete(c.senders, conn)
			sender.Stop()
		} else {
			break
		}
	}
	// start senders, if necessary, and send.
	for conn := range selectedConnections {
		sender, found := c.senders[conn]
		if !found {
			sender = c.makeSender(conn)
			c.senders[conn] = sender
		}
		sender.Send(data)
	}
}

// We have seen a couple of failures which suggest a >128GB slice was encountered.
// 100MB should be enough for anyone.
const maxFeasibleMessageLen = 100 * 1024 * 1024

func (c *GossipChannel) makeSender(conn Connection) *GossipSender {
	return NewGossipSender(func(pending GossipData) {
		for _, msg := range pending.Encode() {
			if len(msg) > maxFeasibleMessageLen {
				panic(fmt.Sprintf("Gossip message too large: len=%d bytes; on channel '%s' from %+v", len(msg), c.name, pending))
			}
			protocolMsg := ProtocolMsg{ProtocolGossip, GobEncode(c.name, c.ourself.Name, msg)}
			conn.(ProtocolSender).SendProtocolMsg(protocolMsg)
		}
	})
}

func (c *GossipChannel) log(args ...interface{}) {
	log.Println(append(append([]interface{}{}, "[gossip "+c.name+"]:"), args...)...)
}

func GobEncode(items ...interface{}) []byte {
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	for _, i := range items {
		checkFatal(enc.Encode(i))
	}
	return buf.Bytes()
}
