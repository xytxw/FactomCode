// Copyright (c) 2013-2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"container/list"
	"fmt"
	"io"
	prand "math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

//	"github.com/FactomProject/FactomCode/factomchain/factoid"
	"github.com/FactomProject/FactomCode/factomd/addrmgr"
	//	"github.com/FactomProject/FactomCode/btcdb"
	//	"github.com/FactomProject/FactomCode/btcutil/bloom"
	"github.com/FactomProject/FactomCode/factomwire"
	//	"github.com/FactomProject/FactomCode/go-socks/socks"
	"github.com/FactomProject/FactomCode/fastsha256"
	"github.com/davecgh/go-spew/spew"
)

const (
	// maxProtocolVersion is the max protocol version the peer supports.
	maxProtocolVersion = 70002

	// outputBufferSize is the number of elements the output channels use.
	outputBufferSize = 50

	// invTrickleSize is the maximum amount of inventory to send in a single
	// message when trickling inventory to remote peers.
	maxInvTrickleSize = 1000

	// maxKnownInventory is the maximum number of items to keep in the known
	// inventory cache.
	maxKnownInventory = 1000

	// negotiateTimeoutSeconds is the number of seconds of inactivity before
	// we timeout a peer that hasn't completed the initial version
	// negotiation.
	negotiateTimeoutSeconds = 30

	// idleTimeoutMinutes is the number of minutes of inactivity before
	// we time out a peer.
	idleTimeoutMinutes = 5

	// pingTimeoutMinutes is the number of minutes since we last sent a
	// message requiring a reply before we will ping a host.
	//	pingTimeoutMinutes = 2
	pingTimeoutMinutes = 1
)

var (
	// userAgentName is the user agent name and is used to help identify
	// ourselves to other bitcoin peers.
	userAgentName = "factomd"

	// userAgentVersion is the user agent version and is used to help
	// identify ourselves to other bitcoin peers.
	userAgentVersion = fmt.Sprintf("%d.%d.%d", 0, 0, 1)
)

// zeroHash is the zero value hash (all zeros).  It is defined as a convenience.
var zeroHash factomwire.ShaHash

// minUint32 is a helper function to return the minimum of two uint32s.
// This avoids a math import and the need to cast to floats.
func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

// newNetAddress attempts to extract the IP address and port from the passed
// net.Addr interface and create a bitcoin NetAddress structure using that
// information.
func newNetAddress(addr net.Addr, services factomwire.ServiceFlag) (*factomwire.NetAddress, error) {
	// addr will be a net.TCPAddr when not using a proxy.
	if tcpAddr, ok := addr.(*net.TCPAddr); ok {
		ip := tcpAddr.IP
		port := uint16(tcpAddr.Port)
		na := factomwire.NewNetAddressIPPort(ip, port, services)
		return na, nil
	}

	// addr will be a socks.ProxiedAddr when using a proxy.
	/*	if proxiedAddr, ok := addr.(*socks.ProxiedAddr); ok {
			ip := net.ParseIP(proxiedAddr.Host)
			if ip == nil {
				ip = net.ParseIP("0.0.0.0")
			}
			port := uint16(proxiedAddr.Port)
			na := factomwire.NewNetAddressIPPort(ip, port, services)
			return na, nil
		}
	*/
	// For the most part, addr should be one of the two above cases, but
	// to be safe, fall back to trying to parse the information from the
	// address string as a last resort.
	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, err
	}
	na := factomwire.NewNetAddressIPPort(ip, uint16(port), services)
	return na, nil
}

// outMsg is used to house a message to be sent along with a channel to signal
// when the message has been sent (or won't be sent due to things such as
// shutdown)
type outMsg struct {
	msg      factomwire.Message
	doneChan chan struct{}
}

// peer provides a bitcoin peer for handling bitcoin communications.  The
// overall data flow is split into 3 goroutines and a separate block manager.
// Inbound messages are read via the inHandler goroutine and generally
// dispatched to their own handler.  For inbound data-related messages such as
// blocks, transactions, and inventory, the data is passed on to the block
// manager to handle it.  Outbound messages are queued via QueueMessage or
// QueueInventory.  QueueMessage is intended for all messages, including
// responses to data such as blocks and transactions.  QueueInventory, on the
// other hand, is only intended for relaying inventory as it employs a trickling
// mechanism to batch the inventory together.  The data flow for outbound
// messages uses two goroutines, queueHandler and outHandler.  The first,
// queueHandler, is used as a way for external entities (mainly block manager)
// to queue messages quickly regardless of whether the peer is currently
// sending or not.  It acts as the traffic cop between the external world and
// the actual goroutine which writes to the network socket.  In addition, the
// peer contains several functions which are of the form pushX, that are used
// to push messages to the peer.  Internally they use QueueMessage.
type peer struct {
	server         *server
	btcnet         factomwire.BitcoinNet
	started        int32
	connected      int32
	disconnect     int32 // only to be used atomically
	conn           net.Conn
	addr           string
	na             *factomwire.NetAddress
	inbound        bool
	persistent     bool
	knownAddresses map[string]struct{}
	knownInventory *MruInventoryMap
	knownInvMutex  sync.Mutex
	//	requestedTxns      map[factomwire.ShaHash]struct{} // owned by blockmanager
	//	requestedBlocks    map[factomwire.ShaHash]struct{} // owned by blockmanager
	retryCount int64
	//	prevGetBlocksBegin *factomwire.ShaHash // owned by blockmanager
	//	prevGetBlocksStop  *factomwire.ShaHash // owned by blockmanager
	//	prevGetHdrsBegin   *factomwire.ShaHash // owned by blockmanager
	//	prevGetHdrsStop    *factomwire.ShaHash // owned by blockmanager
	requestQueue []*factomwire.InvVect
	//	filter             *bloom.Filter
	relayMtx sync.Mutex
	//	disableRelayTx     bool
	continueHash  *factomwire.ShaHash
	outputQueue   chan outMsg
	sendQueue     chan outMsg
	sendDoneQueue chan struct{}
	queueWg       sync.WaitGroup // TODO(oga) wg -> single use channel?
	outputInvChan chan *factomwire.InvVect
	//	txProcessed        chan struct{}
	//	blockProcessed     chan struct{}
	quit            chan struct{}
	StatsMtx        sync.Mutex // protects all statistics below here.
	versionKnown    bool
	protocolVersion uint32
	services        factomwire.ServiceFlag
	timeConnected   time.Time
	lastSend        time.Time
	lastRecv        time.Time
	bytesReceived   uint64
	bytesSent       uint64
	userAgent       string
	lastBlock       int32
	lastPingNonce   uint64    // Set to nonce if we have a pending ping.
	lastPingTime    time.Time // Time we sent last ping.
	lastPingMicros  int64     // Time for last ping to return.
	itself          bool
	display         bool
}

// String returns the peer's address and directionality as a human-readable
// string.
func (p *peer) String() string {
	return fmt.Sprintf("%s (%s)", p.addr, directionString(p.inbound))
}

// directionString is a helper function that returns a string that represents
// the direction of a connection (inbound or outbound).
func directionString(inbound bool) string {
	if inbound {
		return "inbound"
	}
	return "outbound"
}

// isKnownInventory returns whether or not the peer is known to have the passed
// inventory.  It is safe for concurrent access.
func (p *peer) isKnownInventory(invVect *factomwire.InvVect) bool {
	p.knownInvMutex.Lock()
	defer p.knownInvMutex.Unlock()

	fmt.Println("invVect =", invVect)

	if p.knownInventory.Exists(invVect) {
		return true
	}
	return false
}

// AddKnownInventory adds the passed inventory to the cache of known inventory
// for the peer.  It is safe for concurrent access.
func (p *peer) AddKnownInventory(invVect *factomwire.InvVect) {
	p.knownInvMutex.Lock()
	defer p.knownInvMutex.Unlock()

	fastsha256.Trace()

	p.knownInventory.Add(invVect)
}

// VersionKnown returns the whether or not the version of a peer is known locally.
// It is safe for concurrent access.
func (p *peer) VersionKnown() bool {
	p.StatsMtx.Lock()
	defer p.StatsMtx.Unlock()

	return p.versionKnown
}

// ProtocolVersion returns the peer protocol version in a manner that is safe
// for concurrent access.
func (p *peer) ProtocolVersion() uint32 {
	p.StatsMtx.Lock()
	defer p.StatsMtx.Unlock()

	return p.protocolVersion
}

/*
// RelayTxDisabled returns whether or not relaying of transactions is disabled.
// It is safe for concurrent access.
func (p *peer) RelayTxDisabled() bool {
	p.relayMtx.Lock()
	defer p.relayMtx.Unlock()

	return p.disableRelayTx
}*/

// pushVersionMsg sends a version message to the connected peer using the
// current state.
func (p *peer) pushVersionMsg() error {
	//	_, blockNum, err := p.server.db.NewestSha()
	//	if err != nil {
	//		return err
	//	}

	theirNa := p.na

	// If we are behind a proxy and the connection comes from the proxy then
	// we return an unroutable address as their address. This is to prevent
	// leaking the tor proxy address.
	/*	if cfg.Proxy != "" {
			proxyaddress, _, err := net.SplitHostPort(cfg.Proxy)
			// invalid proxy means poorly configured, be on the safe side.
			if err != nil || p.na.IP.String() == proxyaddress {
				theirNa = &factomwire.NetAddress{
					Timestamp: time.Now(),
					IP:        net.IP([]byte{0, 0, 0, 0}),
				}
			}
		}
	*/
	// Version message.
	msg := factomwire.NewMsgVersion(
		p.server.addrManager.GetBestLocalAddress(p.na), theirNa,
		p.server.nonce, 0) //int32(blockNum))
	msg.AddUserAgent(userAgentName, userAgentVersion)

	// XXX: bitcoind appears to always enable the full node services flag
	// of the remote peer netaddress field in the version message regardless
	// of whether it knows it supports it or not.  Also, bitcoind sets
	// the services field of the local peer to 0 regardless of support.
	//
	// Realistically, this should be set as follows:
	// - For outgoing connections:
	//    - Set the local netaddress services to what the local peer
	//      actually supports
	//    - Set the remote netaddress services to 0 to indicate no services
	//      as they are still unknown
	// - For incoming connections:
	//    - Set the local netaddress services to what the local peer
	//      actually supports
	//    - Set the remote netaddress services to the what was advertised by
	//      by the remote peer in its version message
	msg.AddrYou.Services = factomwire.SFNodeNetwork

	// Advertise that we're a full node.
	msg.Services = factomwire.SFNodeNetwork

	// Advertise our max supported protocol version.
	msg.ProtocolVersion = maxProtocolVersion

	p.QueueMessage(msg, nil)
	return nil
}

// updateAddresses potentially adds addresses to the address manager and
// requests known addresses from the remote peer depending on whether the peer
// is an inbound or outbound peer and other factors such as address routability
// and the negotiated protocol version.
func (p *peer) updateAddresses(msg *factomwire.MsgVersion) {
	// Outbound connections.
	if !p.inbound {
		// TODO(davec): Only do this if not doing the initial block
		// download and the local address is routable.
		//if !cfg.DisableListen /* && isCurrent? */ {
		// Get address that best matches.
		lna := p.server.addrManager.GetBestLocalAddress(p.na)
		if addrmgr.IsRoutable(lna) {
			addresses := []*factomwire.NetAddress{lna}
			p.pushAddrMsg(addresses)
		}
		//}

		// Request known addresses if the server address manager needs
		// more and the peer has a protocol version new enough to
		// include a timestamp with addresses.
		hasTimestamp := p.ProtocolVersion() >=
			factomwire.NetAddressTimeVersion
		if p.server.addrManager.NeedMoreAddresses() && hasTimestamp {
			p.QueueMessage(factomwire.NewMsgGetAddr(), nil)
		}

		// Mark the address as a known good address.
		p.server.addrManager.Good(p.na)
	} else {
		// A peer might not be advertising the same address that it
		// actually connected from.  One example of why this can happen
		// is with NAT.  Only add the address to the address manager if
		// the addresses agree.
		if addrmgr.NetAddressKey(&msg.AddrMe) == addrmgr.NetAddressKey(p.na) {
			p.server.addrManager.AddAddress(p.na, p.na)
			p.server.addrManager.Good(p.na)
		}
	}
}

// returns true if the message should be relayed, false otherwise
func (p *peer) shallRelay(msg interface{}) bool {
	fastsha256.Trace()

	fmt.Println("shallRelay msg= ", msg)

	hash, _ := factomwire.NewShaHashFromStruct(msg)
	fmt.Println("shallRelay hash= ", hash)

	iv := factomwire.NewInvVect(factomwire.InvTypeFactomData, hash)

	fmt.Println("shallRelay iv= ", iv)

	if !p.isKnownInventory(iv) {
		p.AddKnownInventory(iv)

		return true
	}

	fmt.Println("******************* SHALL NOT RELAY !!!!!!!!!!! ******************")

	return false
}

func (p *peer) FactomRelay(msg factomwire.Message) {
	fastsha256.Trace()

	fmt.Println("FactomRelay msg= ", msg)

	// broadcast/relay only if hadn't been done for this peer
	if p.shallRelay(msg) {
		p.server.BroadcastMessage(msg, p)
	}
}

// handleVersionMsg is invoked when a peer receives a version bitcoin message
// and is used to negotiate the protocol version details as well as kick start
// the communications.
func (p *peer) handleVersionMsg(msg *factomwire.MsgVersion) {
	fastsha256.Trace()
	// Detect self connections.
	if msg.Nonce == p.server.nonce {
		fmt.Printf("Disconnecting peer connected to self %s\n", p)
		p.Disconnect()
		return
	}

	// Updating a bunch of stats.
	p.StatsMtx.Lock()

	// Limit to one version message per peer.
	if p.versionKnown {
		fmt.Println("Only one version message per peer is allowed %s.",
			p)
		p.StatsMtx.Unlock()

		// Send an reject message indicating the version message was
		// incorrectly sent twice and wait for the message to be sent
		// before disconnecting.
		p.PushRejectMsg(msg.Command(), factomwire.RejectDuplicate,
			"duplicate version message", nil, true)

		p.Disconnect()
		return
	}

	// Negotiate the protocol version.
	p.protocolVersion = minUint32(p.protocolVersion, uint32(msg.ProtocolVersion))
	p.versionKnown = true
	fmt.Sprintf("Negotiated protocol version %d for peer %s",
		p.protocolVersion, p)
	p.lastBlock = msg.LastBlock

	// Set the supported services for the peer to what the remote peer
	// advertised.
	p.services = msg.Services

	// Set the remote peer's user agent.
	p.userAgent = msg.UserAgent

	p.StatsMtx.Unlock()

	// Choose whether or not to relay transactions before a filter command
	// is received.
	//	p.relayMtx.Lock()
	//	p.disableRelayTx = msg.DisableRelayTx
	//	p.relayMtx.Unlock()

	// Inbound connections.
	if p.inbound {
		// Set up a NetAddress for the peer to be used with AddrManager.
		// We only do this inbound because outbound set this up
		// at connection time and no point recomputing.
		na, err := newNetAddress(p.conn.RemoteAddr(), p.services)
		if err != nil {
			fmt.Println("Can't get remote address: %v", err)
			p.Disconnect()
			return
		}
		p.na = na

		// Send version.
		err = p.pushVersionMsg()
		if err != nil {
			fmt.Println("Can't send version message to %s: %v",
				p, err)
			p.Disconnect()
			return
		}
	}

	// Send verack.
	p.QueueMessage(factomwire.NewMsgVerAck(), nil)

	// Update the address manager and request known addresses from the
	// remote peer for outbound connections.  This is skipped when running
	// on the simulation test network since it is only intended to connect
	// to specified peers and actively avoids advertising and connecting to
	// discovered peers.
	//if !cfg.SimNet {
	p.updateAddresses(msg)
	//}

	// Add the remote peer time as a sample for creating an offset against
	// the local clock to keep the network time in sync.
	//p.server.timeSource.AddTimeSample(p.addr, msg.Timestamp)

	// Signal the block manager this peer is a new sync candidate.
	//p.server.blockManager.NewPeer(p)

	// TODO: Relay alerts.
}

/*
// pushTxMsg sends a tx message for the provided transaction hash to the
// connected peer.  An error is returned if the transaction hash is not known.
func (p *peer) pushTxMsg(sha *factomwire.ShaHash, doneChan, waitChan chan struct{}) error {
	// Attempt to fetch the requested transaction from the pool.  A
	// call could be made to check for existence first, but simply trying
	// to fetch a missing transaction results in the same behavior.
	tx, err := p.server.txMemPool.FetchTransaction(sha)
	if err != nil {
		fmt.Sprintf("Unable to fetch tx %v from transaction "+
			"pool: %v", sha, err)

		if doneChan != nil {
			doneChan <- struct{}{}
		}
		return err
	}

	// Once we have fetched data wait for any previous operation to finish.
	if waitChan != nil {
		<-waitChan
	}

	p.QueueMessage(tx.MsgTx(), doneChan)

	return nil
}

// pushBlockMsg sends a block message for the provided block hash to the
// connected peer.  An error is returned if the block hash is not known.
func (p *peer) pushBlockMsg(sha *factomwire.ShaHash, doneChan, waitChan chan struct{}) error {
	blk, err := p.server.db.FetchBlockBySha(sha)
	if err != nil {
		fmt.Sprintf("Unable to fetch requested block sha %v: %v",
			sha, err)

		if doneChan != nil {
			doneChan <- struct{}{}
		}
		return err
	}

	// Once we have fetched data wait for any previous operation to finish.
	if waitChan != nil {
		<-waitChan
	}

	// We only send the channel for this message if we aren't sending
	// an inv straight after.
	var dc chan struct{}
	sendInv := p.continueHash != nil && p.continueHash.IsEqual(sha)
	if !sendInv {
		dc = doneChan
	}
	p.QueueMessage(blk.MsgBlock(), dc)

	// When the peer requests the final block that was advertised in
	// response to a getblocks message which requested more blocks than
	// would fit into a single message, send it a new inventory message
	// to trigger it to issue another getblocks message for the next
	// batch of inventory.
	if p.continueHash != nil && p.continueHash.IsEqual(sha) {
		hash, _, err := p.server.db.NewestSha()
		if err == nil {
			invMsg := factomwire.NewMsgInvSizeHint(1)
			iv := factomwire.NewInvVect(factomwire.InvTypeBlock, hash)
			invMsg.AddInvVect(iv)
			p.QueueMessage(invMsg, doneChan)
			p.continueHash = nil
		} else if doneChan != nil {
			doneChan <- struct{}{}
		}
	}
	return nil
}

// pushMerkleBlockMsg sends a merkleblock message for the provided block hash to
// the connected peer.  Since a merkle block requires the peer to have a filter
// loaded, this call will simply be ignored if there is no filter loaded.  An
// error is returned if the block hash is not known.
func (p *peer) pushMerkleBlockMsg(sha *factomwire.ShaHash, doneChan, waitChan chan struct{}) error {
	// Do not send a response if the peer doesn't have a filter loaded.
	if !p.filter.IsLoaded() {
		if doneChan != nil {
			doneChan <- struct{}{}
		}
		return nil
	}

	blk, err := p.server.db.FetchBlockBySha(sha)
	if err != nil {
		fmt.Sprintf("Unable to fetch requested block sha %v: %v",
			sha, err)

		if doneChan != nil {
			doneChan <- struct{}{}
		}
		return err
	}

	// Generate a merkle block by filtering the requested block according
	// to the filter for the peer and fetch any matched transactions from
	// the database.
	merkle, matchedHashes := bloom.NewMerkleBlock(blk, p.filter)
	txList := p.server.db.FetchTxByShaList(matchedHashes)

	// Warn on any missing transactions which should not happen since the
	// matched transactions come from an existing block.  Also, find the
	// final valid transaction index for later.
	finalValidTxIndex := -1
	for i, txR := range txList {
		if txR.Err != nil || txR.Tx == nil {
			warnMsg := fmt.Sprintf("Failed to fetch transaction "+
				"%v which was matched by merkle block %v",
				txR.Sha, sha)
			if txR.Err != nil {
				warnMsg += ": " + err.Error()
			}
			fmt.Sprintf(warnMsg)
			continue
		}
		finalValidTxIndex = i
	}

	// Once we have fetched data wait for any previous operation to finish.
	if waitChan != nil {
		<-waitChan
	}

	// Send the merkleblock.  Only send the done channel with this message
	// if no transactions will be sent afterwards.
	var dc chan struct{}
	if finalValidTxIndex == -1 {
		dc = doneChan
	}
	p.QueueMessage(merkle, dc)

	// Finally, send any matched transactions.
	for i, txR := range txList {
		// Only send the done channel on the final transaction.
		var dc chan struct{}
		if i == finalValidTxIndex {
			dc = doneChan
		}
		if txR.Err == nil && txR.Tx != nil {
			p.QueueMessage(txR.Tx, dc)
		}
	}

	return nil
}

// PushGetBlocksMsg sends a getblocks message for the provided block locator
// and stop hash.  It will ignore back-to-back duplicate requests.
func (p *peer) PushGetBlocksMsg(locator btcchain.BlockLocator, stopHash *factomwire.ShaHash) error {
	// Extract the begin hash from the block locator, if one was specified,
	// to use for filtering duplicate getblocks requests.
	// request.
	var beginHash *factomwire.ShaHash
	if len(locator) > 0 {
		beginHash = locator[0]
	}

	// Filter duplicate getblocks requests.
	if p.prevGetBlocksStop != nil && p.prevGetBlocksBegin != nil &&
		beginHash != nil && stopHash.IsEqual(p.prevGetBlocksStop) &&
		beginHash.IsEqual(p.prevGetBlocksBegin) {

		fmt.Sprintf("Filtering duplicate [getblocks] with begin "+
			"hash %v, stop hash %v", beginHash, stopHash)
		return nil
	}

	// Construct the getblocks request and queue it to be sent.
	msg := factomwire.NewMsgGetBlocks(stopHash)
	for _, hash := range locator {
		err := msg.AddBlockLocatorHash(hash)
		if err != nil {
			return err
		}
	}
	p.QueueMessage(msg, nil)

	// Update the previous getblocks request information for filtering
	// duplicates.
	p.prevGetBlocksBegin = beginHash
	p.prevGetBlocksStop = stopHash
	return nil
}

// PushGetHeadersMsg sends a getblocks message for the provided block locator
// and stop hash.  It will ignore back-to-back duplicate requests.
func (p *peer) PushGetHeadersMsg(locator btcchain.BlockLocator, stopHash *factomwire.ShaHash) error {
	// Extract the begin hash from the block locator, if one was specified,
	// to use for filtering duplicate getheaders requests.
	var beginHash *factomwire.ShaHash
	if len(locator) > 0 {
		beginHash = locator[0]
	}

	// Filter duplicate getheaders requests.
	if p.prevGetHdrsStop != nil && p.prevGetHdrsBegin != nil &&
		beginHash != nil && stopHash.IsEqual(p.prevGetHdrsStop) &&
		beginHash.IsEqual(p.prevGetHdrsBegin) {

		fmt.Sprintf("Filtering duplicate [getheaders] with begin "+
			"hash %v", beginHash)
		return nil
	}

	// Construct the getheaders request and queue it to be sent.
	msg := factomwire.NewMsgGetHeaders()
	msg.HashStop = *stopHash
	for _, hash := range locator {
		err := msg.AddBlockLocatorHash(hash)
		if err != nil {
			return err
		}
	}
	p.QueueMessage(msg, nil)

	// Update the previous getheaders request information for filtering
	// duplicates.
	p.prevGetHdrsBegin = beginHash
	p.prevGetHdrsStop = stopHash
	return nil
}
*/

// PushRejectMsg sends a reject message for the provided command, reject code,
// and reject reason, and hash.  The hash will only be used when the command
// is a tx or block and should be nil in other cases.  The wait parameter will
// cause the function to block until the reject message has actually been sent.
func (p *peer) PushRejectMsg(command string, code factomwire.RejectCode, reason string, hash *factomwire.ShaHash, wait bool) {
	fastsha256.Trace()
	// Don't bother sending the reject message if the protocol version
	// is too low.
	if p.VersionKnown() && p.ProtocolVersion() < factomwire.RejectVersion {
		return
	}
	fastsha256.Trace()

	msg := factomwire.NewMsgReject(command, code, reason)
	if command == factomwire.CmdTx || command == factomwire.CmdBlock {
		if hash == nil {
			fmt.Sprintf("Sending a reject message for command "+
				"type %v which should have specified a hash "+
				"but does not", command)
			hash = &zeroHash
		}
		msg.Hash = *hash
	}

	// Send the message without waiting if the caller has not requested it.
	if !wait {
		p.QueueMessage(msg, nil)
		return
	}

	// Send the message and block until it has been sent before returning.
	doneChan := make(chan struct{}, 1)
	p.QueueMessage(msg, doneChan)
	<-doneChan
}

/*
// handleMemPoolMsg is invoked when a peer receives a mempool bitcoin message.
// It creates and sends an inventory message with the contents of the memory
// pool up to the maximum inventory allowed per message.  When the peer has a
// bloom filter loaded, the contents are filtered accordingly.
func (p *peer) handleMemPoolMsg(msg *factomwire.MsgMemPool) {
	// Generate inventory message with the available transactions in the
	// transaction memory pool.  Limit it to the max allowed inventory
	// per message.  The the NewMsgInvSizeHint function automatically limits
	// the passed hint to the maximum allowed, so it's safe to pass it
	// without double checking it here.
	txDescs := p.server.txMemPool.TxDescs()
	invMsg := factomwire.NewMsgInvSizeHint(uint(len(txDescs)))

	for i, txDesc := range txDescs {
		// Another thread might have removed the transaction from the
		// pool since the initial query.
		hash := txDesc.Tx.Sha()
		if !p.server.txMemPool.IsTransactionInPool(hash) {
			continue
		}

		// Either add all transactions when there is no bloom filter,
		// or only the transactions that match the filter when there is
		// one.
		if !p.filter.IsLoaded() || p.filter.MatchTxAndUpdate(txDesc.Tx) {
			iv := factomwire.NewInvVect(factomwire.InvTypeTx, hash)
			invMsg.AddInvVect(iv)
			if i+1 >= factomwire.MaxInvPerMsg {
				break
			}
		}
	}

	// Send the inventory message if there is anything to send.
	if len(invMsg.InvList) > 0 {
		p.QueueMessage(invMsg, nil)
	}
}
*/

/*
// handleTxMsg is invoked when a peer receives a tx bitcoin message.  It blocks
// until the bitcoin transaction has been fully processed.  Unlock the block
// handler this does not serialize all transactions through a single thread
// transactions don't rely on the previous one in a linear fashion like blocks.
/*func (p *peer) handleTxMsg(msg *factomwire.MsgTx) {
	fastsha256.Trace()
	// Add the transaction to the known inventory for the peer.
	// Convert the raw MsgTx to a btcutil.Tx which provides some convenience
	// methods and things such as hash caching.
	tx := factoid.NewTx(msg)
	iv := factomwire.NewInvVect(factomwire.InvTypeTx, tx.Sha())
	p.AddKnownInventory(iv)

	// Queue the transaction up to be handled by the block manager and
	// intentionally block further receives until the transaction is fully
	// processed and known good or bad.  This helps prevent a malicious peer
	// from queueing up a bunch of bad transactions before disconnecting (or
	// being disconnected) and wasting memory.
	//p.server.blockManager.QueueTx(tx, p)
	///<-p.txProcessed
}
*/
// Handle factom app imcoming msg
func (p *peer) handleBuyCreditMsg(msg *factomwire.MsgBuyCredit) {
	fastsha256.Trace()

	// Add the msg to inbound msg queue
	inMsgQueue <- msg
}

// Handle factom app imcoming msg
func (p *peer) handleCommitChainMsg(msg *factomwire.MsgCommitChain) {
	fastsha256.Trace()

	// Add the msg to inbound msg queue
	inMsgQueue <- msg
}

// Handle factom app imcoming msg
func (p *peer) handleRevealChainMsg(msg *factomwire.MsgRevealChain) {
	fastsha256.Trace()

	// Add the msg to inbound msg queue
	inMsgQueue <- msg
}

// Handle factom app imcoming msg
func (p *peer) handleCommitEntryMsg(msg *factomwire.MsgCommitEntry) {
	fastsha256.Trace()

	// Add the msg to inbound msg queue
	inMsgQueue <- msg
}

// Handle factom app imcoming msg
func (p *peer) handleRevealEntryMsg(msg *factomwire.MsgRevealEntry) {
	fastsha256.Trace()

	// Add the msg to inbound msg queue
	inMsgQueue <- msg
}

/*
// handleBlockMsg is invoked when a peer receives a block bitcoin message.  It
// blocks until the bitcoin block has been fully processed.
func (p *peer) handleBlockMsg(msg *factomwire.MsgBlock, buf []byte) {
	// Convert the raw MsgBlock to a btcutil.Block which provides some
	// convenience methods and things such as hash caching.
	block := btcutil.NewBlockFromBlockAndBytes(msg, buf)

	// Add the block to the known inventory for the peer.
	hash, err := block.Sha()
	if err != nil {
		fmt.Sprintf("Unable to get block hash: %v", err)
		return
	}
	iv := factomwire.NewInvVect(factomwire.InvTypeBlock, hash)
	p.AddKnownInventory(iv)

	// Queue the block up to be handled by the block
	// manager and intentionally block further receives
	// until the bitcoin block is fully processed and known
	// good or bad.  This helps prevent a malicious peer
	// from queueing up a bunch of bad blocks before
	// disconnecting (or being disconnected) and wasting
	// memory.  Additionally, this behavior is depended on
	// by at least the block acceptance test tool as the
	// reference implementation processes blocks in the same
	// thread and therefore blocks further messages until
	// the bitcoin block has been fully processed.
	p.server.blockManager.QueueBlock(block, p)
	<-p.blockProcessed
}

// handleInvMsg is invoked when a peer receives an inv bitcoin message and is
// used to examine the inventory being advertised by the remote peer and react
// accordingly.  We pass the message down to blockmanager which will call
// QueueMessage with any appropriate responses.
func (p *peer) handleInvMsg(msg *factomwire.MsgInv) {
	p.server.blockManager.QueueInv(msg, p)
}

// handleHeadersMsg is invoked when a peer receives a headers bitcoin message.
// The message is passed down to the block manager.
func (p *peer) handleHeadersMsg(msg *factomwire.MsgHeaders) {
	p.server.blockManager.QueueHeaders(msg, p)
}

// handleGetData is invoked when a peer receives a getdata bitcoin message and
// is used to deliver block and transaction information.
func (p *peer) handleGetDataMsg(msg *factomwire.MsgGetData) {
	numAdded := 0
	notFound := factomwire.NewMsgNotFound()

	// We wait on the this wait channel periodically to prevent queueing
	// far more data than we can send in a reasonable time, wasting memory.
	// The waiting occurs after the database fetch for the next one to
	// provide a little pipelining.
	var waitChan chan struct{}
	doneChan := make(chan struct{}, 1)

	for i, iv := range msg.InvList {
		var c chan struct{}
		// If this will be the last message we send.
		if i == len(msg.InvList)-1 && len(notFound.InvList) == 0 {
			c = doneChan
		} else if (i+1)%3 == 0 {
			// Buffered so as to not make the send goroutine block.
			c = make(chan struct{}, 1)
		}
		var err error
		switch iv.Type {
		case factomwire.InvTypeTx:
			err = p.pushTxMsg(&iv.Hash, c, waitChan)
		case factomwire.InvTypeBlock:
			err = p.pushBlockMsg(&iv.Hash, c, waitChan)
		case factomwire.InvTypeFilteredBlock:
			err = p.pushMerkleBlockMsg(&iv.Hash, c, waitChan)
		default:
			fmt.Sprintf("Unknown type in inventory request %d",
				iv.Type)
			continue
		}
		if err != nil {
			notFound.AddInvVect(iv)

			// When there is a failure fetching the final entry
			// and the done channel was sent in due to there
			// being no outstanding not found inventory, consume
			// it here because there is now not found inventory
			// that will use the channel momentarily.
			if i == len(msg.InvList)-1 && c != nil {
				<-c
			}
		}
		numAdded++
		waitChan = c
	}
	if len(notFound.InvList) != 0 {
		p.QueueMessage(notFound, doneChan)
	}

	// Wait for messages to be sent. We can send quite a lot of data at this
	// point and this will keep the peer busy for a decent amount of time.
	// We don't process anything else by them in this time so that we
	// have an idea of when we should hear back from them - else the idle
	// timeout could fire when we were only half done sending the blocks.
	if numAdded > 0 {
		<-doneChan
	}
}

// handleGetBlocksMsg is invoked when a peer receives a getblocks bitcoin message.
func (p *peer) handleGetBlocksMsg(msg *factomwire.MsgGetBlocks) {
	// Return all block hashes to the latest one (up to max per message) if
	// no stop hash was specified.
	// Attempt to find the ending index of the stop hash if specified.
	endIdx := btcdb.AllShas
	if !msg.HashStop.IsEqual(&zeroHash) {
		height, err := p.server.db.FetchBlockHeightBySha(&msg.HashStop)
		if err == nil {
			endIdx = height + 1
		}
	}

	// Find the most recent known block based on the block locator.
	// Use the block after the genesis block if no other blocks in the
	// provided locator are known.  This does mean the client will start
	// over with the genesis block if unknown block locators are provided.
	// This mirrors the behavior in the reference implementation.
	startIdx := int64(1)
	for _, hash := range msg.BlockLocatorHashes {
		height, err := p.server.db.FetchBlockHeightBySha(hash)
		if err == nil {
			// Start with the next hash since we know this one.
			startIdx = height + 1
			break
		}
	}

	// Don't attempt to fetch more than we can put into a single message.
	autoContinue := false
	if endIdx-startIdx > factomwire.MaxBlocksPerMsg {
		endIdx = startIdx + factomwire.MaxBlocksPerMsg
		autoContinue = true
	}

	// Generate inventory message.
	//
	// The FetchBlockBySha call is limited to a maximum number of hashes
	// per invocation.  Since the maximum number of inventory per message
	// might be larger, call it multiple times with the appropriate indices
	// as needed.
	invMsg := factomwire.NewMsgInv()
	for start := startIdx; start < endIdx; {
		// Fetch the inventory from the block database.
		hashList, err := p.server.db.FetchHeightRange(start, endIdx)
		if err != nil {
			fmt.Sprintf("Block lookup failed: %v", err)
			return
		}

		// The database did not return any further hashes.  Break out of
		// the loop now.
		if len(hashList) == 0 {
			break
		}

		// Add block inventory to the message.
		for _, hash := range hashList {
			hashCopy := hash
			iv := factomwire.NewInvVect(factomwire.InvTypeBlock, &hashCopy)
			invMsg.AddInvVect(iv)
		}
		start += int64(len(hashList))
	}

	// Send the inventory message if there is anything to send.
	if len(invMsg.InvList) > 0 {
		invListLen := len(invMsg.InvList)
		if autoContinue && invListLen == factomwire.MaxBlocksPerMsg {
			// Intentionally use a copy of the final hash so there
			// is not a reference into the inventory slice which
			// would prevent the entire slice from being eligible
			// for GC as soon as it's sent.
			continueHash := invMsg.InvList[invListLen-1].Hash
			p.continueHash = &continueHash
		}
		p.QueueMessage(invMsg, nil)
	}
}

// handleGetHeadersMsg is invoked when a peer receives a getheaders bitcoin
// message.
func (p *peer) handleGetHeadersMsg(msg *factomwire.MsgGetHeaders) {
	// Attempt to look up the height of the provided stop hash.
	endIdx := btcdb.AllShas
	height, err := p.server.db.FetchBlockHeightBySha(&msg.HashStop)
	if err == nil {
		endIdx = height + 1
	}

	// There are no block locators so a specific header is being requested
	// as identified by the stop hash.
	if len(msg.BlockLocatorHashes) == 0 {
		// No blocks with the stop hash were found so there is nothing
		// to do.  Just return.  This behavior mirrors the reference
		// implementation.
		if endIdx == btcdb.AllShas {
			return
		}

		// Fetch and send the requested block header.
		header, err := p.server.db.FetchBlockHeaderBySha(&msg.HashStop)
		if err != nil {
			fmt.Sprintf("Lookup of known block hash failed: %v",
				err)
			return
		}

		headersMsg := factomwire.NewMsgHeaders()
		headersMsg.AddBlockHeader(header)
		p.QueueMessage(headersMsg, nil)
		return
	}

	// Find the most recent known block based on the block locator.
	// Use the block after the genesis block if no other blocks in the
	// provided locator are known.  This does mean the client will start
	// over with the genesis block if unknown block locators are provided.
	// This mirrors the behavior in the reference implementation.
	startIdx := int64(1)
	for _, hash := range msg.BlockLocatorHashes {
		height, err := p.server.db.FetchBlockHeightBySha(hash)
		if err == nil {
			// Start with the next hash since we know this one.
			startIdx = height + 1
			break
		}
	}

	// Don't attempt to fetch more than we can put into a single message.
	if endIdx-startIdx > factomwire.MaxBlockHeadersPerMsg {
		endIdx = startIdx + factomwire.MaxBlockHeadersPerMsg
	}

	// Generate headers message and send it.
	//
	// The FetchHeightRange call is limited to a maximum number of hashes
	// per invocation.  Since the maximum number of headers per message
	// might be larger, call it multiple times with the appropriate indices
	// as needed.
	headersMsg := factomwire.NewMsgHeaders()
	for start := startIdx; start < endIdx; {
		// Fetch the inventory from the block database.
		hashList, err := p.server.db.FetchHeightRange(start, endIdx)
		if err != nil {
			fmt.Sprintf("Header lookup failed: %v", err)
			return
		}

		// The database did not return any further hashes.  Break out of
		// the loop now.
		if len(hashList) == 0 {
			break
		}

		// Add headers to the message.
		for _, hash := range hashList {
			header, err := p.server.db.FetchBlockHeaderBySha(&hash)
			if err != nil {
				fmt.Sprintf("Lookup of known block hash "+
					"failed: %v", err)
				continue
			}
			headersMsg.AddBlockHeader(header)
		}

		// Start at the next block header after the latest one on the
		// next loop iteration.
		start += int64(len(hashList))
	}
	p.QueueMessage(headersMsg, nil)
}

// handleFilterAddMsg is invoked when a peer receives a filteradd bitcoin
// message and is used by remote peers to add data to an already loaded bloom
// filter.  The peer will be disconnected if a filter is not loaded when this
// message is received.
func (p *peer) handleFilterAddMsg(msg *factomwire.MsgFilterAdd) {
	if !p.filter.IsLoaded() {
		fmt.Sprintf("%s sent a filteradd request with no filter "+
			"loaded -- disconnecting", p)
		p.Disconnect()
		return
	}

	p.filter.Add(msg.Data)
}

// handleFilterClearMsg is invoked when a peer receives a filterclear bitcoin
// message and is used by remote peers to clear an already loaded bloom filter.
// The peer will be disconnected if a filter is not loaded when this message is
// received.
func (p *peer) handleFilterClearMsg(msg *factomwire.MsgFilterClear) {
	if !p.filter.IsLoaded() {
		fmt.Sprintf("%s sent a filterclear request with no "+
			"filter loaded -- disconnecting", p)
		p.Disconnect()
		return
	}
	p.filter.Unload()
}

// handleFilterLoadMsg is invoked when a peer receives a filterload bitcoin
// message and it used to load a bloom filter that should be used for delivering
// merkle blocks and associated transactions that match the filter.
func (p *peer) handleFilterLoadMsg(msg *factomwire.MsgFilterLoad) {
	// Transaction relay is no longer disabled once a filterload message is
	// received regardless of its original state.
	p.relayMtx.Lock()
	p.disableRelayTx = false
	p.relayMtx.Unlock()

	p.filter.Reload(msg)
}*/

// handleGetAddrMsg is invoked when a peer receives a getaddr bitcoin message
// and is used to provide the peer with known addresses from the address
// manager.
func (p *peer) handleGetAddrMsg(msg *factomwire.MsgGetAddr) {
	fastsha256.Trace()
	// Don't return any addresses when running on the simulation test
	// network.  This helps prevent the network from becoming another
	// public test network since it will not be able to learn about other
	// peers that have not specifically been provided.
	//if cfg.SimNet {
	//	return
	//}

	fastsha256.Trace()

	// Get the current known addresses from the address manager.
	addrCache := p.server.addrManager.AddressCache()

	// Push the addresses.
	err := p.pushAddrMsg(addrCache)
	if err != nil {
		fmt.Println("Can't push address message to %s: %v", p, err)
		p.Disconnect()
		return
	}
	fastsha256.Trace()
}

// pushAddrMsg sends one, or more, addr message(s) to the connected peer using
// the provided addresses.
func (p *peer) pushAddrMsg(addresses []*factomwire.NetAddress) error {
	// Nothing to send.
	if len(addresses) == 0 {
		fmt.Println("pushAddrMsg: nothing to send !!!!!!!!!!!!!!")
		return nil
	}

	r := prand.New(prand.NewSource(time.Now().UnixNano()))
	numAdded := 0
	msg := factomwire.NewMsgAddr()
	for _, na := range addresses {
		// Filter addresses the peer already knows about.
		if _, exists := p.knownAddresses[addrmgr.NetAddressKey(na)]; exists {
			continue
		}

		// If the maxAddrs limit has been reached, randomize the list
		// with the remaining addresses.
		if numAdded == factomwire.MaxAddrPerMsg {
			msg.AddrList[r.Intn(factomwire.MaxAddrPerMsg)] = na
			continue
		}

		// Add the address to the message.
		err := msg.AddAddress(na)
		if err != nil {
			return err
		}
		numAdded++
	}
	if numAdded > 0 {
		for _, na := range msg.AddrList {
			// Add address to known addresses for this peer.
			p.knownAddresses[addrmgr.NetAddressKey(na)] = struct{}{}
		}

		p.QueueMessage(msg, nil)
	}
	return nil
}

// handleAddrMsg is invoked when a peer receives an addr bitcoin message and
// is used to notify the server about advertised addresses.
func (p *peer) handleAddrMsg(msg *factomwire.MsgAddr) {
	fastsha256.Trace()
	// Ignore addresses when running on the simulation test network.  This
	// helps prevent the network from becoming another public test network
	// since it will not be able to learn about other peers that have not
	// specifically been provided.
	//if cfg.SimNet {
	//	return
	//}

	// Ignore old style addresses which don't include a timestamp.
	if p.ProtocolVersion() < factomwire.NetAddressTimeVersion {
		return
	}

	// A message that has no addresses is invalid.
	if len(msg.AddrList) == 0 {
		fmt.Println("Command [%s] from %s does not contain any addresses",
			msg.Command(), p)
		p.Disconnect()
		return
	}

	for _, na := range msg.AddrList {
		// Don't add more address if we're disconnecting.
		if atomic.LoadInt32(&p.disconnect) != 0 {
			return
		}

		// Set the timestamp to 5 days ago if it's more than 24 hours
		// in the future so this address is one of the first to be
		// removed when space is needed.
		now := time.Now()
		if na.Timestamp.After(now.Add(time.Minute * 10)) {
			na.Timestamp = now.Add(-1 * time.Hour * 24 * 5)
		}

		// Add address to known addresses for this peer.
		p.knownAddresses[addrmgr.NetAddressKey(na)] = struct{}{}
	}

	// Add addresses to server address manager.  The address manager handles
	// the details of things such as preventing duplicate addresses, max
	// addresses, and last seen updates.
	// XXX bitcoind gives a 2 hour time penalty here, do we want to do the
	// same?
	p.server.addrManager.AddAddresses(msg.AddrList, p.na)
}

// handlePingMsg is invoked when a peer receives a ping bitcoin message.  For
// recent clients (protocol version > BIP0031Version), it replies with a pong
// message.  For older clients, it does nothing and anything other than failure
// is considered a successful ping.
func (p *peer) handlePingMsg(msg *factomwire.MsgPing) {
	// Only Reply with pong is message comes from a new enough client.
	//if p.ProtocolVersion() > factomwire.BIP0031Version {
	// Include nonce from ping so pong can be identified.
	p.QueueMessage(factomwire.NewMsgPong(msg.Nonce), nil)
	//}
}

// handlePongMsg is invoked when a peer received a pong bitcoin message.
// recent clients (protocol version > BIP0031Version), and if we had send a ping
// previosuly we update our ping time statistics. If the client is too old or
// we had not send a ping we ignore it.
func (p *peer) handlePongMsg(msg *factomwire.MsgPong) {
	p.StatsMtx.Lock()
	defer p.StatsMtx.Unlock()

	// Arguably we could use a buffered channel here sending data
	// in a fifo manner whenever we send a ping, or a list keeping track of
	// the times of each ping. For now we just make a best effort and
	// only record stats if it was for the last ping sent. Any preceding
	// and overlapping pings will be ignored. It is unlikely to occur
	// without large usage of the ping rpc call since we ping
	// infrequently enough that if they overlap we would have timed out
	// the peer.
	//if p.protocolVersion > factomwire.BIP0031Version &&
	if p.lastPingNonce != 0 && msg.Nonce == p.lastPingNonce {
		p.lastPingMicros = time.Now().Sub(p.lastPingTime).Nanoseconds()
		p.lastPingMicros /= 1000 // convert to usec.
		p.lastPingNonce = 0
	}
}

// readMessage reads the next bitcoin message from the peer with logging.
func (p *peer) readMessage() (factomwire.Message, []byte, error) {
	if p.display {
		fmt.Println("<<< readMessage()", p)
		//		fastsha256.Trace()
	}

	n, msg, buf, err := factomwire.ReadMessageN(p.conn, p.ProtocolVersion(),
		p.btcnet)
	p.StatsMtx.Lock()
	p.bytesReceived += uint64(n)
	p.StatsMtx.Unlock()
	p.server.AddBytesReceived(uint64(n))
	if err != nil {
		return nil, nil, err
	}

	// Use closures to log expensive operations so they are only run when
	// the logging level requires it.
	/*	fmt.Sprintf("%v", newLogClosure(func() string {
			// Debug summary of message.
			summary := messageSummary(msg)
			if len(summary) > 0 {
				summary = " (" + summary + ")"
			}
			return fmt.Sprintf("Received %v%s from %s",
				msg.Command(), summary, p)
		}))
		fmt.Sprintf("%v", newLogClosure(func() string {
			return spew.Sdump(msg)
		}))
		fmt.Sprintf("%v", newLogClosure(func() string {
			return spew.Sdump(buf)
		}))
	*/

	if p.display {
		fmt.Printf("spew read msg:\n" + spew.Sdump(msg))
		fmt.Printf("spew read buf:\n" + spew.Sdump(buf))
	}

	return msg, buf, nil
}

// writeMessage sends a bitcoin Message to the peer with logging.
func (p *peer) writeMessage(msg factomwire.Message) {
	if p.display {
		fmt.Println(">>> writeMessage()", p)
	}

	// Don't do anything if we're disconnecting.
	if atomic.LoadInt32(&p.disconnect) != 0 {
		return
	}
	if !p.VersionKnown() {
		switch msg.(type) {
		case *factomwire.MsgVersion:
			// This is OK.
		case *factomwire.MsgReject:
			// This is OK.
		default:
			// Drop all messages other than version and reject if
			// the handshake has not already been done.
			return
		}
	}

	// Use closures to log expensive operations so they are only run when
	// the logging level requires it.
	/*	fmt.Sprintf("%v", newLogClosure(func() string {
			// Debug summary of message.
			summary := messageSummary(msg)
			if len(summary) > 0 {
				summary = " (" + summary + ")"
			}
			return fmt.Sprintf("Sending %v%s to %s", msg.Command(),
				summary, p)
		}))
		fmt.Sprintf("%v", newLogClosure(func() string {
			return spew.Sdump(msg)
		}))
		fmt.Sprintf("%v", newLogClosure(func() string {
			var buf bytes.Buffer
			err := factomwire.WriteMessage(&buf, msg, p.ProtocolVersion(),
				p.btcnet)
			if err != nil {
				return err.Error()
			}
			return spew.Sdump(buf.Bytes())
		}))
	*/

	if p.display {
		fmt.Printf("spew write msg:\n" + spew.Sdump(msg))
		{
			var buf bytes.Buffer
			factomwire.WriteMessage(&buf, msg, p.ProtocolVersion(), p.btcnet)
			fmt.Printf("spew write buf:\n" + spew.Sdump(buf.Bytes()))
		}
	}

	// Write the message to the peer.
	n, err := factomwire.WriteMessageN(p.conn, msg, p.ProtocolVersion(),
		p.btcnet)
	p.StatsMtx.Lock()
	p.bytesSent += uint64(n)
	p.StatsMtx.Unlock()
	p.server.AddBytesSent(uint64(n))
	if err != nil {
		p.Disconnect()
		fmt.Println("Can't send message to %s: %v", p, err)
		return
	}
}

// isAllowedByRegression returns whether or not the passed error is allowed by
// regression tests without disconnecting the peer.  In particular, regression
// tests need to be allowed to send malformed messages without the peer being
// disconnected.
func (p *peer) isAllowedByRegression(err error) bool {
	// Don't allow the error if it's not specifically a malformed message
	// error.
	if _, ok := err.(*factomwire.MessageError); !ok {
		return false
	}

	// Don't allow the error if it's not coming from localhost or the
	// hostname can't be determined for some reason.
	host, _, err := net.SplitHostPort(p.addr)
	if err != nil {
		return false
	}

	if host != "127.0.0.1" && host != "localhost" {
		return false
	}

	// Allowed if all checks passed.
	return true
}

// inHandler handles all incoming messages for the peer.  It must be run as a
// goroutine.
func (p *peer) inHandler() {
	// Peers must complete the initial version negotiation within a shorter
	// timeframe than a general idle timeout.  The timer is then reset below
	// to idleTimeoutMinutes for all future messages.
	idleTimer := time.AfterFunc(negotiateTimeoutSeconds*time.Second, func() {
		if p.VersionKnown() {
			fmt.Sprintf("Peer %s no answer for %d minutes, "+
				"disconnecting", p, idleTimeoutMinutes)
		}
		p.Disconnect()
	})
out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		rmsg, _, err := p.readMessage()
		// Stop the timer now, if we go around again we will reset it.
		idleTimer.Stop()
		if err != nil {
			// In order to allow regression tests with malformed
			// messages, don't disconnect the peer when we're in
			// regression test mode and the error is one of the
			// allowed errors.
			/*	if cfg.RegressionTest && p.isAllowedByRegression(err) {
				fmt.Sprintf("Allowed regression test "+
					"error from %s: %v", p, err)
				idleTimer.Reset(idleTimeoutMinutes * time.Minute)
				continue
			}*/

			// Only log the error and possibly send reject message
			// if we're not forcibly disconnecting.
			if atomic.LoadInt32(&p.disconnect) == 0 {
				errMsg := fmt.Sprintf("Can't read message "+
					"from %s: %v", p, err)
				fmt.Println(errMsg)

				// Only send the reject message if it's not
				// because the remote client disconnected.
				if err != io.EOF {
					// Push a reject message for the
					// malformed message and wait for the
					// message to be sent before
					// disconnecting.
					//
					// NOTE: Ideally this would include the
					// command in the header if at least
					// that much of the message was valid,
					// but that is not currently exposed by
					// factomwire, so just used malformed for
					// the command.
					p.PushRejectMsg("malformed",
						factomwire.RejectMalformed, errMsg,
						nil, true)
				}

			}
			break out
		}
		p.StatsMtx.Lock()
		p.lastRecv = time.Now()
		p.StatsMtx.Unlock()

		// Ensure version message comes first.
		if vmsg, ok := rmsg.(*factomwire.MsgVersion); !ok && !p.VersionKnown() {
			errStr := "A version message must precede all others"
			fmt.Println(errStr)

			// Push a reject message and wait for the message to be
			// sent before disconnecting.
			p.PushRejectMsg(vmsg.Command(), factomwire.RejectMalformed,
				errStr, nil, true)
			break out
		}

		// Handle each supported message type.
		markConnected := false
		switch msg := rmsg.(type) {
		case *factomwire.MsgVersion:
			p.handleVersionMsg(msg)
			markConnected = true

		case *factomwire.MsgVerAck:
			// Do nothing.

		case *factomwire.MsgGetAddr:
			p.handleGetAddrMsg(msg)

		case *factomwire.MsgAddr:
			p.handleAddrMsg(msg)
			markConnected = true

		case *factomwire.MsgPing:
			p.handlePingMsg(msg)
			markConnected = true

		case *factomwire.MsgPong:
			p.handlePongMsg(msg)

		case *factomwire.MsgAlert:
			p.server.BroadcastMessage(msg, p)

		case *factomwire.MsgBuyCredit:
			p.handleBuyCreditMsg(msg)
			p.FactomRelay(msg)

		case *factomwire.MsgCommitChain:
			p.handleCommitChainMsg(msg)
			p.FactomRelay(msg)

		case *factomwire.MsgRevealChain:
			p.handleRevealChainMsg(msg)
			p.FactomRelay(msg)

		case *factomwire.MsgCommitEntry:
			p.handleCommitEntryMsg(msg)
			p.FactomRelay(msg)

		case *factomwire.MsgRevealEntry:
			p.handleRevealEntryMsg(msg)
			p.FactomRelay(msg)

			/*
				case *factomwire.MsgMemPool:
					p.handleMemPoolMsg(msg)
			*/
//		case *factomwire.MsgTx:
//			p.handleTxMsg(msg)
			/*
				case *factomwire.MsgBlock:
					p.handleBlockMsg(msg, buf)

							case *factomwire.MsgInv:
								p.handleInvMsg(msg)
								markConnected = true

							case *factomwire.MsgHeaders:
								p.handleHeadersMsg(msg)

							case *factomwire.MsgNotFound:
								// TODO(davec): Ignore this for now, but ultimately
								// it should probably be used to detect when something
								// we requested needs to be re-requested from another
								// peer.

							case *factomwire.MsgGetData:
								p.handleGetDataMsg(msg)
								markConnected = true

							case *factomwire.MsgGetBlocks:
								p.handleGetBlocksMsg(msg)

							case *factomwire.MsgGetHeaders:
								p.handleGetHeadersMsg(msg)

							case *factomwire.MsgFilterAdd:
								p.handleFilterAddMsg(msg)

							case *factomwire.MsgFilterClear:
								p.handleFilterClearMsg(msg)

							case *factomwire.MsgFilterLoad:
								p.handleFilterLoadMsg(msg)
			*/

		case *factomwire.MsgReject:
			// Nothing to do currently.  Logging of the rejected
			// message is handled already in readMessage.
		default:
			fmt.Sprintf("Received unhandled message of type %v: Fix Me",
				rmsg.Command())
		}

		// Mark the address as currently connected and working as of
		// now if one of the messages that trigger it was processed.
		if markConnected && atomic.LoadInt32(&p.disconnect) == 0 {
			if p.na == nil {
				fmt.Sprintf("we're getting stuff before we " +
					"got a version message. that's bad")
				continue
			}
			p.server.addrManager.Connected(p.na)
		}
		// ok we got a message, reset the timer.
		// timer just calls p.Disconnect() after logging.
		idleTimer.Reset(idleTimeoutMinutes * time.Minute)
		p.retryCount = 0
	}

	idleTimer.Stop()

	// Ensure connection is closed and notify the server that the peer is
	// done.
	p.Disconnect()
	p.server.donePeers <- p

	// Only tell block manager we are gone if we ever told it we existed.
	if p.VersionKnown() {
		//p.server.blockManager.DonePeer(p)
	}

	fmt.Sprintf("Peer input handler done for %s", p)
}

// queueHandler handles the queueing of outgoing data for the peer. This runs
// as a muxer for various sources of input so we can ensure that blockmanager
// and the server goroutine both will not block on us sending a message.
// We then pass the data on to outHandler to be actually written.
func (p *peer) queueHandler() {
	pendingMsgs := list.New()
	invSendQueue := list.New()
	trickleTicker := time.NewTicker(time.Second * 10)
	defer trickleTicker.Stop()

	// We keep the waiting flag so that we know if we have a message queued
	// to the outHandler or not.  We could use the presence of a head of
	// the list for this but then we have rather racy concerns about whether
	// it has gotten it at cleanup time - and thus who sends on the
	// message's done channel.  To avoid such confusion we keep a different
	// flag and pendingMsgs only contains messages that we have not yet
	// passed to outHandler.
	waiting := false

	// To avoid duplication below.
	queuePacket := func(msg outMsg, list *list.List, waiting bool) bool {
		if !waiting {
			fmt.Sprintf("%s: sending to outHandler", p)
			p.sendQueue <- msg
			fmt.Sprintf("%s: sent to outHandler", p)
		} else {
			list.PushBack(msg)
		}
		// we are always waiting now.
		return true
	}
out:
	for {
		select {
		case msg := <-p.outputQueue:
			waiting = queuePacket(msg, pendingMsgs, waiting)

		// This channel is notified when a message has been sent across
		// the network socket.
		case <-p.sendDoneQueue:
			fmt.Sprintf("%s: acked by outhandler", p)

			// No longer waiting if there are no more messages
			// in the pending messages queue.
			next := pendingMsgs.Front()
			if next == nil {
				waiting = false
				continue
			}

			// Notify the outHandler about the next item to
			// asynchronously send.
			val := pendingMsgs.Remove(next)
			fmt.Sprintf("%s: sending to outHandler", p)
			p.sendQueue <- val.(outMsg)
			fmt.Sprintf("%s: sent to outHandler", p)

		case iv := <-p.outputInvChan:
			// No handshake?  They'll find out soon enough.
			if p.VersionKnown() {
				invSendQueue.PushBack(iv)
			}

		case <-trickleTicker.C:
			// Don't send anything if we're disconnecting or there
			// is no queued inventory.
			// version is known if send queue has any entries.
			if atomic.LoadInt32(&p.disconnect) != 0 ||
				invSendQueue.Len() == 0 {
				continue
			}

			// Create and send as many inv messages as needed to
			// drain the inventory send queue.
			invMsg := factomwire.NewMsgInv()
			for e := invSendQueue.Front(); e != nil; e = invSendQueue.Front() {
				iv := invSendQueue.Remove(e).(*factomwire.InvVect)

				// Don't send inventory that became known after
				// the initial check.
				if p.isKnownInventory(iv) {
					continue
				}

				invMsg.AddInvVect(iv)
				if len(invMsg.InvList) >= maxInvTrickleSize {
					waiting = queuePacket(
						outMsg{msg: invMsg},
						pendingMsgs, waiting)
					invMsg = factomwire.NewMsgInv()
				}

				// Add the inventory that is being relayed to
				// the known inventory for the peer.
				p.AddKnownInventory(iv)
			}
			if len(invMsg.InvList) > 0 {
				waiting = queuePacket(outMsg{msg: invMsg},
					pendingMsgs, waiting)
			}

		case <-p.quit:
			break out
		}
	}

	// Drain any wait channels before we go away so we don't leave something
	// waiting for us.
	for e := pendingMsgs.Front(); e != nil; e = pendingMsgs.Front() {
		val := pendingMsgs.Remove(e)
		msg := val.(outMsg)
		if msg.doneChan != nil {
			msg.doneChan <- struct{}{}
		}
	}
cleanup:
	for {
		select {
		case msg := <-p.outputQueue:
			if msg.doneChan != nil {
				msg.doneChan <- struct{}{}
			}
		case <-p.outputInvChan:
			// Just drain channel
		// sendDoneQueue is buffered so doesn't need draining.
		default:
			break cleanup
		}
	}
	p.queueWg.Done()
	fmt.Sprintf("Peer queue handler done for %s", p)
}

// outHandler handles all outgoing messages for the peer.  It must be run as a
// goroutine.  It uses a buffered channel to serialize output messages while
// allowing the sender to continue running asynchronously.
func (p *peer) outHandler() {
	pingTimer := time.AfterFunc(pingTimeoutMinutes*time.Minute, func() {
		nonce, err := factomwire.RandomUint64()
		if err != nil {
			fmt.Println("Not sending ping on timeout to %s: %v",
				p, err)
			return
		}
		p.QueueMessage(factomwire.NewMsgPing(nonce), nil)
	})
out:
	for {
		select {
		case msg := <-p.sendQueue:
			// If the message is one we should get a reply for
			// then reset the timer, we only want to send pings
			// when otherwise we would not receive a reply from
			// the peer. We specifically do not count block or inv
			// messages here since they are not sure of a reply if
			// the inv is of no interest explicitly solicited invs
			// should elicit a reply but we don't track them
			// specially.
			//			fmt.Printf("%s: received from queuehandler\n", p)
			reset := true
			switch m := msg.msg.(type) {
			case *factomwire.MsgVersion:
				// should get an ack
			case *factomwire.MsgGetAddr:
				// should get addresses
			case *factomwire.MsgPing:
				// expects pong
				// Also set up statistics.
				p.StatsMtx.Lock()
				//if p.protocolVersion > factomwire.BIP0031Version {
				p.lastPingNonce = m.Nonce
				p.lastPingTime = time.Now()
				//}
				p.StatsMtx.Unlock()
				/*			case *factomwire.MsgMemPool:
								// Should return an inv.
							case *factomwire.MsgGetData:
								// Should get us block, tx, or not found.
							case *factomwire.MsgGetHeaders:
								// Should get us headers back.
				*/default:
				// Not one of the above, no sure reply.
				// We want to ping if nothing else
				// interesting happens.
				reset = false
			}
			if reset {
				pingTimer.Reset(pingTimeoutMinutes * time.Minute)
			}
			p.writeMessage(msg.msg)
			p.StatsMtx.Lock()
			p.lastSend = time.Now()
			p.StatsMtx.Unlock()
			if msg.doneChan != nil {
				msg.doneChan <- struct{}{}
			}
			//			fmt.Sprintf("%s: acking queuehandler\n", p)
			p.sendDoneQueue <- struct{}{}
			//			fmt.Printf("%s: acked queuehandler\n", p)

		case <-p.quit:
			break out
		}
	}

	pingTimer.Stop()

	p.queueWg.Wait()

	// Drain any wait channels before we go away so we don't leave something
	// waiting for us. We have waited on queueWg and thus we can be sure
	// that we will not miss anything sent on sendQueue.
cleanup:
	for {
		select {
		case msg := <-p.sendQueue:
			if msg.doneChan != nil {
				msg.doneChan <- struct{}{}
			}
			// no need to send on sendDoneQueue since queueHandler
			// has been waited on and already exited.
		default:
			break cleanup
		}
	}
	fmt.Printf("Peer output handler done for %s\n", p)
}

// QueueMessage adds the passed bitcoin message to the peer send queue.  It
// uses a buffered channel to communicate with the output handler goroutine so
// it is automatically rate limited and safe for concurrent access.
func (p *peer) QueueMessage(msg factomwire.Message, doneChan chan struct{}) {
	// Avoid risk of deadlock if goroutine already exited. The goroutine
	// we will be sending to hangs around until it knows for a fact that
	// it is marked as disconnected. *then* it drains the channels.
	if !p.Connected() {
		// avoid deadlock...
		if doneChan != nil {
			go func() {
				doneChan <- struct{}{}
			}()
		}
		return
	}
	//	fmt.Printf("========= %s = %s = connected: %d =======================================================\n", time.Now().String(), p, p.server.ConnectedCount())
	fmt.Printf("========= %s = %s ========================================================\n", time.Now().String(), p)
	//	fastsha256.Trace()
	p.outputQueue <- outMsg{msg: msg, doneChan: doneChan}
}

// QueueInventory adds the passed inventory to the inventory send queue which
// might not be sent right away, rather it is trickled to the peer in batches.
// Inventory that the peer is already known to have is ignored.  It is safe for
// concurrent access.
func (p *peer) QueueInventory(invVect *factomwire.InvVect) {
	// Don't add the inventory to the send queue if the peer is
	// already known to have it.
	if p.isKnownInventory(invVect) {
		return
	}

	// Avoid risk of deadlock if goroutine already exited. The goroutine
	// we will be sending to hangs around until it knows for a fact that
	// it is marked as disconnected. *then* it drains the channels.
	if !p.Connected() {
		return
	}

	p.outputInvChan <- invVect
}

// Connected returns whether or not the peer is currently connected.
func (p *peer) Connected() bool {
	return atomic.LoadInt32(&p.connected) != 0 &&
		atomic.LoadInt32(&p.disconnect) == 0
}

// Disconnect disconnects the peer by closing the connection.  It also sets
// a flag so the impending shutdown can be detected.
func (p *peer) Disconnect() {
	// did we win the race?
	if atomic.AddInt32(&p.disconnect, 1) != 1 {
		return
	}
	fmt.Printf("disconnecting %s\n", p)
	close(p.quit)
	if atomic.LoadInt32(&p.connected) != 0 {
		p.conn.Close()
	}
}

// Start begins processing input and output messages.  It also sends the initial
// version message for outbound connections to start the negotiation process.
func (p *peer) Start() error {
	// Already started?
	if atomic.AddInt32(&p.started, 1) != 1 {
		return nil
	}

	fmt.Printf("Starting peer %s\n", p)

	// Send an initial version message if this is an outbound connection.
	if !p.inbound {
		err := p.pushVersionMsg()
		if err != nil {
			fmt.Printf("Can't send outbound version message %v\n", err)
			p.Disconnect()
			return err
		}
	}

	// Start processing input and output.
	go p.inHandler()
	// queueWg is kept so that outHandler knows when the queue has exited so
	// it can drain correctly.
	p.queueWg.Add(1)
	go p.queueHandler()
	go p.outHandler()

	return nil
}

// Shutdown gracefully shuts down the peer by disconnecting it.
func (p *peer) Shutdown() {
	fmt.Printf("Shutdown peer %s\n", p)
	p.Disconnect()
}

// newPeerBase returns a new base bitcoin peer for the provided server and
// inbound flag.  This is used by the newInboundPeer and newOutboundPeer
// functions to perform base setup needed by both types of peers.
func newPeerBase(s *server, inbound bool) *peer {
	p := peer{
		server:          s,
		protocolVersion: maxProtocolVersion,
		btcnet:          s.netParams.Net,
		services:        factomwire.SFNodeNetwork,
		inbound:         inbound,
		knownAddresses:  make(map[string]struct{}),
		knownInventory:  NewMruInventoryMap(maxKnownInventory),
		//		requestedTxns:   make(map[factomwire.ShaHash]struct{}),
		//		requestedBlocks: make(map[factomwire.ShaHash]struct{}),
		//		filter:          bloom.LoadFilter(nil),
		outputQueue:   make(chan outMsg, outputBufferSize),
		sendQueue:     make(chan outMsg, 1),   // nonblocking sync
		sendDoneQueue: make(chan struct{}, 1), // nonblocking sync
		outputInvChan: make(chan *factomwire.InvVect, outputBufferSize),
		//		txProcessed:     make(chan struct{}, 1),
		//		blockProcessed:  make(chan struct{}, 1),
		quit: make(chan struct{}),
	}
	return &p
}

// newInboundPeer returns a new inbound bitcoin peer for the provided server and
// connection.  Use Start to begin processing incoming and outgoing messages.
func newInboundPeer(s *server, conn net.Conn) *peer {
	p := newPeerBase(s, true)
	p.conn = conn
	p.addr = conn.RemoteAddr().String()
	p.timeConnected = time.Now()
	atomic.AddInt32(&p.connected, 1)
	return p
}

// newOutbountPeer returns a new outbound bitcoin peer for the provided server and
// address and connects to it asynchronously. If the connection is successful
// then the peer will also be started.
func newOutboundPeer(s *server, addr string, persistent bool, retryCount int64) *peer {
	p := newPeerBase(s, false)
	p.addr = addr
	p.persistent = persistent
	p.retryCount = retryCount

	// Setup p.na with a temporary address that we are connecting to with
	// faked up service flags.  We will replace this with the real one after
	// version negotiation is successful.  The only failure case here would
	// be if the string was incomplete for connection so can't be split
	// into address and port, and thus this would be invalid anyway.  In
	// which case we return nil to be handled by the caller.  This must be
	// done before we fork off the goroutine because as soon as this
	// function returns the peer must have a valid netaddress.
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Println("Tried to create a new outbound peer with invalid "+
			"address %s: %v", addr, err)
		return nil
	}

	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		fmt.Println("Tried to create a new outbound peer with invalid "+
			"port %s: %v", portStr, err)
		return nil
	}

	p.na, err = s.addrManager.HostToNetAddress(host, uint16(port), 0)
	if err != nil {
		fmt.Println("Can not turn host %s into netaddress: %v",
			host, err)
		return nil
	}

	go func() {
		if atomic.LoadInt32(&p.disconnect) != 0 {
			return
		}
		if p.retryCount > 0 {
			scaledInterval := connectionRetryInterval.Nanoseconds() * p.retryCount / 2
			scaledDuration := time.Duration(scaledInterval)
			fmt.Sprintf("Retrying connection to %s in %s", addr, scaledDuration)
			time.Sleep(scaledDuration)
		}
		fmt.Sprintf("Attempting to connect to %s", addr)
		conn, err := net.Dial("tcp", addr) //btcdDial("tcp", addr)
		if err != nil {
			fmt.Sprintf("Failed to connect to %s: %v", addr, err)
			p.server.donePeers <- p
			return
		}

		// We may have slept and the server may have scheduled a shutdown.  In that
		// case ditch the peer immediately.
		if atomic.LoadInt32(&p.disconnect) == 0 {
			p.timeConnected = time.Now()
			p.server.addrManager.Attempt(p.na)

			// Connection was successful so log it and start peer.
			fmt.Sprintf("Connected to %s", conn.RemoteAddr())
			p.conn = conn
			atomic.AddInt32(&p.connected, 1)
			p.Start()
		}
	}()
	return p
}

/*
// logError makes sure that we only log errors loudly on user peers.
func (p *peer) logError(fmt string, args ...interface{}) {
	if p.persistent {
		fmt.Printf(fmt, args...)
	} else {
		fmt.Println(fmt, args...)
	}
}
*/