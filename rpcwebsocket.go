// Copyright (c) 2013-2016 The btcsuite developers
// Copyright (c) 2017 BitGo
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"container/list"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/bitgo/prova/blockchain"
	"github.com/bitgo/prova/btcjson"
	"github.com/bitgo/prova/chaincfg/chainhash"
	"github.com/bitgo/prova/database"
	"github.com/bitgo/prova/provautil"
	"github.com/bitgo/prova/txscript"
	"github.com/bitgo/prova/wire"
	"github.com/btcsuite/fastsha256"
	"github.com/btcsuite/golangcrypto/ripemd160"
	"github.com/btcsuite/websocket"
)

const (
	// websocketSendBufferSize is the number of elements the send channel
	// can queue before blocking.  Note that this only applies to requests
	// handled directly in the websocket client input handler or the async
	// handler since notifications have their own queuing mechanism
	// independent of the send channel buffer.
	websocketSendBufferSize = 50
)

type semaphore chan struct{}

func makeSemaphore(n int) semaphore {
	return make(chan struct{}, n)
}

func (s semaphore) acquire() { s <- struct{}{} }
func (s semaphore) release() { <-s }

// timeZeroVal is simply the zero value for a time.Time and is used to avoid
// creating multiple instances.
var timeZeroVal time.Time

// wsCommandHandler describes a callback function used to handle a specific
// command.
type wsCommandHandler func(*wsClient, interface{}) (interface{}, error)

// wsHandlers maps RPC command strings to appropriate websocket handler
// functions.  This is set by init because help references wsHandlers and thus
// causes a dependency loop.
var wsHandlers map[string]wsCommandHandler
var wsHandlersBeforeInit = map[string]wsCommandHandler{
	"help":                      handleWebsocketHelp,
	"notifyblocks":              handleNotifyBlocks,
	"notifynewtransactions":     handleNotifyNewTransactions,
	"notifyreceived":            handleNotifyReceived,
	"notifyspent":               handleNotifySpent,
	"session":                   handleSession,
	"stopnotifyblocks":          handleStopNotifyBlocks,
	"stopnotifynewtransactions": handleStopNotifyNewTransactions,
	"stopnotifyspent":           handleStopNotifySpent,
	"stopnotifyreceived":        handleStopNotifyReceived,
	"rescan":                    handleRescan,
}

// WebsocketHandler handles a new websocket client by creating a new wsClient,
// starting it, and blocking until the connection closes.  Since it blocks, it
// must be run in a separate goroutine.  It should be invoked from the websocket
// server handler which runs each new connection in a new goroutine thereby
// satisfying the requirement.
func (s *rpcServer) WebsocketHandler(conn *websocket.Conn, remoteAddr string,
	authenticated bool, isAdmin bool) {

	// Clear the read deadline that was set before the websocket hijacked
	// the connection.
	conn.SetReadDeadline(timeZeroVal)

	// Limit max number of websocket clients.
	rpcsLog.Infof("New websocket client %s", remoteAddr)
	if s.ntfnMgr.NumClients()+1 > cfg.RPCMaxWebsockets {
		rpcsLog.Infof("Max websocket clients exceeded [%d] - "+
			"disconnecting client %s", cfg.RPCMaxWebsockets,
			remoteAddr)
		conn.Close()
		return
	}

	// Create a new websocket client to handle the new websocket connection
	// and wait for it to shutdown.  Once it has shutdown (and hence
	// disconnected), remove it and any notifications it registered for.
	client, err := newWebsocketClient(s, conn, remoteAddr, authenticated, isAdmin)
	if err != nil {
		rpcsLog.Errorf("Failed to serve client %s: %v", remoteAddr, err)
		conn.Close()
		return
	}
	s.ntfnMgr.AddClient(client)
	client.Start()
	client.WaitForShutdown()
	s.ntfnMgr.RemoveClient(client)
	rpcsLog.Infof("Disconnected websocket client %s", remoteAddr)
}

// wsNotificationManager is a connection and notification manager used for
// websockets.  It allows websocket clients to register for notifications they
// are interested in.  When an event happens elsewhere in the code such as
// transactions being added to the memory pool or block connects/disconnects,
// the notification manager is provided with the relevant details needed to
// figure out which websocket clients need to be notified based on what they
// have registered for and notifies them accordingly.  It is also used to keep
// track of all connected websocket clients.
type wsNotificationManager struct {
	// server is the RPC server the notification manager is associated with.
	server *rpcServer

	// queueNotification queues a notification for handling.
	queueNotification chan interface{}

	// notificationMsgs feeds notificationHandler with notifications
	// and client (un)registeration requests from a queue as well as
	// registeration and unregisteration requests from clients.
	notificationMsgs chan interface{}

	// Access channel for current number of connected clients.
	numClients chan int

	// Shutdown handling
	wg   sync.WaitGroup
	quit chan struct{}
}

// queueHandler manages a queue of empty interfaces, reading from in and
// sending the oldest unsent to out.  This handler stops when either of the
// in or quit channels are closed, and closes out before returning, without
// waiting to send any variables still remaining in the queue.
func queueHandler(in <-chan interface{}, out chan<- interface{}, quit <-chan struct{}) {
	var q []interface{}
	var dequeue chan<- interface{}
	skipQueue := out
	var next interface{}
out:
	for {
		select {
		case n, ok := <-in:
			if !ok {
				// Sender closed input channel.
				break out
			}

			// Either send to out immediately if skipQueue is
			// non-nil (queue is empty) and reader is ready,
			// or append to the queue and send later.
			select {
			case skipQueue <- n:
			default:
				q = append(q, n)
				dequeue = out
				skipQueue = nil
				next = q[0]
			}

		case dequeue <- next:
			copy(q, q[1:])
			q[len(q)-1] = nil // avoid leak
			q = q[:len(q)-1]
			if len(q) == 0 {
				dequeue = nil
				skipQueue = out
			} else {
				next = q[0]
			}

		case <-quit:
			break out
		}
	}
	close(out)
}

// queueHandler maintains a queue of notifications and notification handler
// control messages.
func (m *wsNotificationManager) queueHandler() {
	queueHandler(m.queueNotification, m.notificationMsgs, m.quit)
	m.wg.Done()
}

// NotifyBlockConnected passes a block newly-connected to the best chain
// to the notification manager for block and transaction notification
// processing.
func (m *wsNotificationManager) NotifyBlockConnected(block *provautil.Block) {
	// As NotifyBlockConnected will be called by the block manager
	// and the RPC server may no longer be running, use a select
	// statement to unblock enqueuing the notification once the RPC
	// server has begun shutting down.
	select {
	case m.queueNotification <- (*notificationBlockConnected)(block):
	case <-m.quit:
	}
}

// NotifyBlockDisconnected passes a block disconnected from the best chain
// to the notification manager for block notification processing.
func (m *wsNotificationManager) NotifyBlockDisconnected(block *provautil.Block) {
	// As NotifyBlockDisconnected will be called by the block manager
	// and the RPC server may no longer be running, use a select
	// statement to unblock enqueuing the notification once the RPC
	// server has begun shutting down.
	select {
	case m.queueNotification <- (*notificationBlockDisconnected)(block):
	case <-m.quit:
	}
}

// NotifyMempoolTx passes a transaction accepted by mempool to the
// notification manager for transaction notification processing.  If
// isNew is true, the tx is is a new transaction, rather than one
// added to the mempool during a reorg.
func (m *wsNotificationManager) NotifyMempoolTx(tx *provautil.Tx, isNew bool) {
	n := &notificationTxAcceptedByMempool{
		isNew: isNew,
		tx:    tx,
	}

	// As NotifyMempoolTx will be called by mempool and the RPC server
	// may no longer be running, use a select statement to unblock
	// enqueuing the notification once the RPC server has begun
	// shutting down.
	select {
	case m.queueNotification <- n:
	case <-m.quit:
	}
}

// Notification types
type notificationBlockConnected provautil.Block
type notificationBlockDisconnected provautil.Block
type notificationTxAcceptedByMempool struct {
	isNew bool
	tx    *provautil.Tx
}

// Notification control requests
type notificationRegisterClient wsClient
type notificationUnregisterClient wsClient
type notificationRegisterBlocks wsClient
type notificationUnregisterBlocks wsClient
type notificationRegisterNewMempoolTxs wsClient
type notificationUnregisterNewMempoolTxs wsClient
type notificationRegisterSpent struct {
	wsc *wsClient
	ops []*wire.OutPoint
}
type notificationUnregisterSpent struct {
	wsc *wsClient
	op  *wire.OutPoint
}
type notificationRegisterAddr struct {
	wsc   *wsClient
	addrs []string
}
type notificationUnregisterAddr struct {
	wsc  *wsClient
	addr string
}

// notificationHandler reads notifications and control messages from the queue
// handler and processes one at a time.
func (m *wsNotificationManager) notificationHandler() {
	// clients is a map of all currently connected websocket clients.
	clients := make(map[chan struct{}]*wsClient)

	// Maps used to hold lists of websocket clients to be notified on
	// certain events.  Each websocket client also keeps maps for the events
	// which have multiple triggers to make removal from these lists on
	// connection close less horrendously expensive.
	//
	// Where possible, the quit channel is used as the unique id for a client
	// since it is quite a bit more efficient than using the entire struct.
	blockNotifications := make(map[chan struct{}]*wsClient)
	txNotifications := make(map[chan struct{}]*wsClient)
	watchedOutPoints := make(map[wire.OutPoint]map[chan struct{}]*wsClient)
	watchedAddrs := make(map[string]map[chan struct{}]*wsClient)

out:
	for {
		select {
		case n, ok := <-m.notificationMsgs:
			if !ok {
				// queueHandler quit.
				break out
			}
			switch n := n.(type) {
			case *notificationBlockConnected:
				block := (*provautil.Block)(n)

				// Skip iterating through all txs if no
				// tx notification requests exist.
				if len(watchedOutPoints) != 0 || len(watchedAddrs) != 0 {
					for _, tx := range block.Transactions() {
						m.notifyForTx(watchedOutPoints,
							watchedAddrs, tx, block)
					}
				}

				if len(blockNotifications) != 0 {
					m.notifyBlockConnected(blockNotifications,
						block)
				}

			case *notificationBlockDisconnected:
				m.notifyBlockDisconnected(blockNotifications,
					(*provautil.Block)(n))

			case *notificationTxAcceptedByMempool:
				if n.isNew && len(txNotifications) != 0 {
					m.notifyForNewTx(txNotifications, n.tx)
				}
				m.notifyForTx(watchedOutPoints, watchedAddrs, n.tx, nil)

			case *notificationRegisterBlocks:
				wsc := (*wsClient)(n)
				blockNotifications[wsc.quit] = wsc

			case *notificationUnregisterBlocks:
				wsc := (*wsClient)(n)
				delete(blockNotifications, wsc.quit)

			case *notificationRegisterClient:
				wsc := (*wsClient)(n)
				clients[wsc.quit] = wsc

			case *notificationUnregisterClient:
				wsc := (*wsClient)(n)
				// Remove any requests made by the client as well as
				// the client itself.
				delete(blockNotifications, wsc.quit)
				delete(txNotifications, wsc.quit)
				for k := range wsc.spentRequests {
					op := k
					m.removeSpentRequest(watchedOutPoints, wsc, &op)
				}
				for addr := range wsc.addrRequests {
					m.removeAddrRequest(watchedAddrs, wsc, addr)
				}
				delete(clients, wsc.quit)

			case *notificationRegisterSpent:
				m.addSpentRequests(watchedOutPoints, n.wsc, n.ops)

			case *notificationUnregisterSpent:
				m.removeSpentRequest(watchedOutPoints, n.wsc, n.op)

			case *notificationRegisterAddr:
				m.addAddrRequests(watchedAddrs, n.wsc, n.addrs)

			case *notificationUnregisterAddr:
				m.removeAddrRequest(watchedAddrs, n.wsc, n.addr)

			case *notificationRegisterNewMempoolTxs:
				wsc := (*wsClient)(n)
				txNotifications[wsc.quit] = wsc

			case *notificationUnregisterNewMempoolTxs:
				wsc := (*wsClient)(n)
				delete(txNotifications, wsc.quit)

			default:
				rpcsLog.Warn("Unhandled notification type")
			}

		case m.numClients <- len(clients):

		case <-m.quit:
			// RPC server shutting down.
			break out
		}
	}

	for _, c := range clients {
		c.Disconnect()
	}
	m.wg.Done()
}

// NumClients returns the number of clients actively being served.
func (m *wsNotificationManager) NumClients() (n int) {
	select {
	case n = <-m.numClients:
	case <-m.quit: // Use default n (0) if server has shut down.
	}
	return
}

// RegisterBlockUpdates requests block update notifications to the passed
// websocket client.
func (m *wsNotificationManager) RegisterBlockUpdates(wsc *wsClient) {
	m.queueNotification <- (*notificationRegisterBlocks)(wsc)
}

// UnregisterBlockUpdates removes block update notifications for the passed
// websocket client.
func (m *wsNotificationManager) UnregisterBlockUpdates(wsc *wsClient) {
	m.queueNotification <- (*notificationUnregisterBlocks)(wsc)
}

// notifyBlockConnected notifies websocket clients that have registered for
// block updates when a block is connected to the main chain.
func (*wsNotificationManager) notifyBlockConnected(clients map[chan struct{}]*wsClient,
	block *provautil.Block) {

	// Notify interested websocket clients about the connected block.
	ntfn := btcjson.NewBlockConnectedNtfn(block.Hash().String(),
		int32(block.Height()), block.MsgBlock().Header.Timestamp.Unix())
	marshalledJSON, err := btcjson.MarshalCmd(nil, ntfn)
	if err != nil {
		rpcsLog.Error("Failed to marshal block connected notification: "+
			"%v", err)
		return
	}
	for _, wsc := range clients {
		wsc.QueueNotification(marshalledJSON)
	}
}

// notifyBlockDisconnected notifies websocket clients that have registered for
// block updates when a block is disconnected from the main chain (due to a
// reorganize).
func (*wsNotificationManager) notifyBlockDisconnected(clients map[chan struct{}]*wsClient, block *provautil.Block) {
	// Skip notification creation if no clients have requested block
	// connected/disconnected notifications.
	if len(clients) == 0 {
		return
	}

	// Notify interested websocket clients about the disconnected block.
	ntfn := btcjson.NewBlockDisconnectedNtfn(block.Hash().String(),
		int32(block.Height()), block.MsgBlock().Header.Timestamp.Unix())
	marshalledJSON, err := btcjson.MarshalCmd(nil, ntfn)
	if err != nil {
		rpcsLog.Error("Failed to marshal block disconnected "+
			"notification: %v", err)
		return
	}
	for _, wsc := range clients {
		wsc.QueueNotification(marshalledJSON)
	}
}

// RegisterNewMempoolTxsUpdates requests notifications to the passed websocket
// client when new transactions are added to the memory pool.
func (m *wsNotificationManager) RegisterNewMempoolTxsUpdates(wsc *wsClient) {
	m.queueNotification <- (*notificationRegisterNewMempoolTxs)(wsc)
}

// UnregisterNewMempoolTxsUpdates removes notifications to the passed websocket
// client when new transaction are added to the memory pool.
func (m *wsNotificationManager) UnregisterNewMempoolTxsUpdates(wsc *wsClient) {
	m.queueNotification <- (*notificationUnregisterNewMempoolTxs)(wsc)
}

// notifyForNewTx notifies websocket clients that have registered for updates
// when a new transaction is added to the memory pool.
func (m *wsNotificationManager) notifyForNewTx(clients map[chan struct{}]*wsClient, tx *provautil.Tx) {
	txHashStr := tx.Hash().String()
	mtx := tx.MsgTx()

	var amount int64
	for _, txOut := range mtx.TxOut {
		amount += txOut.Value
	}

	ntfn := btcjson.NewTxAcceptedNtfn(txHashStr, provautil.Amount(amount).ToRMG())
	marshalledJSON, err := btcjson.MarshalCmd(nil, ntfn)
	if err != nil {
		rpcsLog.Errorf("Failed to marshal tx notification: %s", err.Error())
		return
	}

	var verboseNtfn *btcjson.TxAcceptedVerboseNtfn
	var marshalledJSONVerbose []byte
	for _, wsc := range clients {
		if wsc.verboseTxUpdates {
			if marshalledJSONVerbose != nil {
				wsc.QueueNotification(marshalledJSONVerbose)
				continue
			}

			net := m.server.server.chainParams
			rawTx, err := createTxRawResult(net, mtx, txHashStr, nil,
				"", 0, 0)
			if err != nil {
				return
			}

			verboseNtfn = btcjson.NewTxAcceptedVerboseNtfn(*rawTx)
			marshalledJSONVerbose, err = btcjson.MarshalCmd(nil,
				verboseNtfn)
			if err != nil {
				rpcsLog.Errorf("Failed to marshal verbose tx "+
					"notification: %s", err.Error())
				return
			}
			wsc.QueueNotification(marshalledJSONVerbose)
		} else {
			wsc.QueueNotification(marshalledJSON)
		}
	}
}

// RegisterSpentRequests requests a notification when each of the passed
// outpoints is confirmed spent (contained in a block connected to the main
// chain) for the passed websocket client.  The request is automatically
// removed once the notification has been sent.
func (m *wsNotificationManager) RegisterSpentRequests(wsc *wsClient, ops []*wire.OutPoint) {
	m.queueNotification <- &notificationRegisterSpent{
		wsc: wsc,
		ops: ops,
	}
}

// addSpentRequests modifies a map of watched outpoints to sets of websocket
// clients to add a new request watch all of the outpoints in ops and create
// and send a notification when spent to the websocket client wsc.
func (*wsNotificationManager) addSpentRequests(opMap map[wire.OutPoint]map[chan struct{}]*wsClient,
	wsc *wsClient, ops []*wire.OutPoint) {

	for _, op := range ops {
		// Track the request in the client as well so it can be quickly
		// be removed on disconnect.
		wsc.spentRequests[*op] = struct{}{}

		// Add the client to the list to notify when the outpoint is seen.
		// Create the list as needed.
		cmap, ok := opMap[*op]
		if !ok {
			cmap = make(map[chan struct{}]*wsClient)
			opMap[*op] = cmap
		}
		cmap[wsc.quit] = wsc
	}
}

// UnregisterSpentRequest removes a request from the passed websocket client
// to be notified when the passed outpoint is confirmed spent (contained in a
// block connected to the main chain).
func (m *wsNotificationManager) UnregisterSpentRequest(wsc *wsClient, op *wire.OutPoint) {
	m.queueNotification <- &notificationUnregisterSpent{
		wsc: wsc,
		op:  op,
	}
}

// removeSpentRequest modifies a map of watched outpoints to remove the
// websocket client wsc from the set of clients to be notified when a
// watched outpoint is spent.  If wsc is the last client, the outpoint
// key is removed from the map.
func (*wsNotificationManager) removeSpentRequest(ops map[wire.OutPoint]map[chan struct{}]*wsClient,
	wsc *wsClient, op *wire.OutPoint) {

	// Remove the request tracking from the client.
	delete(wsc.spentRequests, *op)

	// Remove the client from the list to notify.
	notifyMap, ok := ops[*op]
	if !ok {
		rpcsLog.Warnf("Attempt to remove nonexistent spent request "+
			"for websocket client %s", wsc.addr)
		return
	}
	delete(notifyMap, wsc.quit)

	// Remove the map entry altogether if there are
	// no more clients interested in it.
	if len(notifyMap) == 0 {
		delete(ops, *op)
	}
}

// txHexString returns the serialized transaction encoded in hexadecimal.
func txHexString(tx *provautil.Tx) string {
	buf := bytes.NewBuffer(make([]byte, 0, tx.MsgTx().SerializeSize()))
	// Ignore Serialize's error, as writing to a bytes.buffer cannot fail.
	tx.MsgTx().Serialize(buf)
	return hex.EncodeToString(buf.Bytes())
}

// blockDetails creates a BlockDetails struct to include in btcws notifications
// from a block and a transaction's block index.
func blockDetails(block *provautil.Block, txIndex int) *btcjson.BlockDetails {
	if block == nil {
		return nil
	}
	return &btcjson.BlockDetails{
		Height: int32(block.Height()),
		Hash:   block.Hash().String(),
		Index:  txIndex,
		Time:   block.MsgBlock().Header.Timestamp.Unix(),
	}
}

// newRedeemingTxNotification returns a new marshalled redeemingtx notification
// with the passed parameters.
func newRedeemingTxNotification(txHex string, index int, block *provautil.Block) ([]byte, error) {
	// Create and marshal the notification.
	ntfn := btcjson.NewRedeemingTxNtfn(txHex, blockDetails(block, index))
	return btcjson.MarshalCmd(nil, ntfn)
}

// notifyForTxOuts examines each transaction output, notifying interested
// websocket clients of the transaction if an output spends to a watched
// address.  A spent notification request is automatically registered for
// the client for each matching output.
func (m *wsNotificationManager) notifyForTxOuts(ops map[wire.OutPoint]map[chan struct{}]*wsClient,
	addrs map[string]map[chan struct{}]*wsClient, tx *provautil.Tx, block *provautil.Block) {

	// Nothing to do if nobody is listening for address notifications.
	if len(addrs) == 0 {
		return
	}

	txHex := ""
	wscNotified := make(map[chan struct{}]struct{})
	for i, txOut := range tx.MsgTx().TxOut {
		_, txAddrs, _, err := txscript.ExtractPkScriptAddrs(
			txOut.PkScript, m.server.server.chainParams)
		if err != nil {
			continue
		}

		for _, txAddr := range txAddrs {
			cmap, ok := addrs[txAddr.EncodeAddress()]
			if !ok {
				continue
			}

			if txHex == "" {
				txHex = txHexString(tx)
			}
			ntfn := btcjson.NewRecvTxNtfn(txHex, blockDetails(block,
				tx.Index()))

			marshalledJSON, err := btcjson.MarshalCmd(nil, ntfn)
			if err != nil {
				rpcsLog.Errorf("Failed to marshal processedtx notification: %v", err)
				continue
			}

			op := []*wire.OutPoint{wire.NewOutPoint(tx.Hash(), uint32(i))}
			for wscQuit, wsc := range cmap {
				m.addSpentRequests(ops, wsc, op)

				if _, ok := wscNotified[wscQuit]; !ok {
					wscNotified[wscQuit] = struct{}{}
					wsc.QueueNotification(marshalledJSON)
				}
			}
		}
	}
}

// notifyForTx examines the inputs and outputs of the passed transaction,
// notifying websocket clients of outputs spending to a watched address
// and inputs spending a watched outpoint.
func (m *wsNotificationManager) notifyForTx(ops map[wire.OutPoint]map[chan struct{}]*wsClient,
	addrs map[string]map[chan struct{}]*wsClient, tx *provautil.Tx, block *provautil.Block) {

	if len(ops) != 0 {
		m.notifyForTxIns(ops, tx, block)
	}
	if len(addrs) != 0 {
		m.notifyForTxOuts(ops, addrs, tx, block)
	}
}

// notifyForTxIns examines the inputs of the passed transaction and sends
// interested websocket clients a redeemingtx notification if any inputs
// spend a watched output.  If block is non-nil, any matching spent
// requests are removed.
func (m *wsNotificationManager) notifyForTxIns(ops map[wire.OutPoint]map[chan struct{}]*wsClient,
	tx *provautil.Tx, block *provautil.Block) {

	// Nothing to do if nobody is watching outpoints.
	if len(ops) == 0 {
		return
	}

	txHex := ""
	wscNotified := make(map[chan struct{}]struct{})
	for _, txIn := range tx.MsgTx().TxIn {
		prevOut := &txIn.PreviousOutPoint
		if cmap, ok := ops[*prevOut]; ok {
			if txHex == "" {
				txHex = txHexString(tx)
			}
			marshalledJSON, err := newRedeemingTxNotification(txHex, tx.Index(), block)
			if err != nil {
				rpcsLog.Warnf("Failed to marshal redeemingtx notification: %v", err)
				continue
			}
			for wscQuit, wsc := range cmap {
				if block != nil {
					m.removeSpentRequest(ops, wsc, prevOut)
				}

				if _, ok := wscNotified[wscQuit]; !ok {
					wscNotified[wscQuit] = struct{}{}
					wsc.QueueNotification(marshalledJSON)
				}
			}
		}
	}
}

// RegisterTxOutAddressRequests requests notifications to the passed websocket
// client when a transaction output spends to the passed address.
func (m *wsNotificationManager) RegisterTxOutAddressRequests(wsc *wsClient, addrs []string) {
	m.queueNotification <- &notificationRegisterAddr{
		wsc:   wsc,
		addrs: addrs,
	}
}

// addAddrRequests adds the websocket client wsc to the address to client set
// addrMap so wsc will be notified for any mempool or block transaction outputs
// spending to any of the addresses in addrs.
func (*wsNotificationManager) addAddrRequests(addrMap map[string]map[chan struct{}]*wsClient,
	wsc *wsClient, addrs []string) {

	for _, addr := range addrs {
		// Track the request in the client as well so it can be quickly be
		// removed on disconnect.
		wsc.addrRequests[addr] = struct{}{}

		// Add the client to the set of clients to notify when the
		// outpoint is seen.  Create map as needed.
		cmap, ok := addrMap[addr]
		if !ok {
			cmap = make(map[chan struct{}]*wsClient)
			addrMap[addr] = cmap
		}
		cmap[wsc.quit] = wsc
	}
}

// UnregisterTxOutAddressRequest removes a request from the passed websocket
// client to be notified when a transaction spends to the passed address.
func (m *wsNotificationManager) UnregisterTxOutAddressRequest(wsc *wsClient, addr string) {
	m.queueNotification <- &notificationUnregisterAddr{
		wsc:  wsc,
		addr: addr,
	}
}

// removeAddrRequest removes the websocket client wsc from the address to
// client set addrs so it will no longer receive notification updates for
// any transaction outputs send to addr.
func (*wsNotificationManager) removeAddrRequest(addrs map[string]map[chan struct{}]*wsClient,
	wsc *wsClient, addr string) {

	// Remove the request tracking from the client.
	delete(wsc.addrRequests, addr)

	// Remove the client from the list to notify.
	cmap, ok := addrs[addr]
	if !ok {
		rpcsLog.Warnf("Attempt to remove nonexistent addr request "+
			"<%s> for websocket client %s", addr, wsc.addr)
		return
	}
	delete(cmap, wsc.quit)

	// Remove the map entry altogether if there are no more clients
	// interested in it.
	if len(cmap) == 0 {
		delete(addrs, addr)
	}
}

// AddClient adds the passed websocket client to the notification manager.
func (m *wsNotificationManager) AddClient(wsc *wsClient) {
	m.queueNotification <- (*notificationRegisterClient)(wsc)
}

// RemoveClient removes the passed websocket client and all notifications
// registered for it.
func (m *wsNotificationManager) RemoveClient(wsc *wsClient) {
	select {
	case m.queueNotification <- (*notificationUnregisterClient)(wsc):
	case <-m.quit:
	}
}

// Start starts the goroutines required for the manager to queue and process
// websocket client notifications.
func (m *wsNotificationManager) Start() {
	m.wg.Add(2)
	go m.queueHandler()
	go m.notificationHandler()
}

// WaitForShutdown blocks until all notification manager goroutines have
// finished.
func (m *wsNotificationManager) WaitForShutdown() {
	m.wg.Wait()
}

// Shutdown shuts down the manager, stopping the notification queue and
// notification handler goroutines.
func (m *wsNotificationManager) Shutdown() {
	close(m.quit)
}

// newWsNotificationManager returns a new notification manager ready for use.
// See wsNotificationManager for more details.
func newWsNotificationManager(server *rpcServer) *wsNotificationManager {
	return &wsNotificationManager{
		server:            server,
		queueNotification: make(chan interface{}),
		notificationMsgs:  make(chan interface{}),
		numClients:        make(chan int),
		quit:              make(chan struct{}),
	}
}

// wsResponse houses a message to send to a connected websocket client as
// well as a channel to reply on when the message is sent.
type wsResponse struct {
	msg      []byte
	doneChan chan bool
}

// wsClient provides an abstraction for handling a websocket client.  The
// overall data flow is split into 3 main goroutines, a possible 4th goroutine
// for long-running operations (only started if request is made), and a
// websocket manager which is used to allow things such as broadcasting
// requested notifications to all connected websocket clients.   Inbound
// messages are read via the inHandler goroutine and generally dispatched to
// their own handler.  However, certain potentially long-running operations such
// as rescans, are sent to the asyncHander goroutine and are limited to one at a
// time.  There are two outbound message types - one for responding to client
// requests and another for async notifications.  Responses to client requests
// use SendMessage which employs a buffered channel thereby limiting the number
// of outstanding requests that can be made.  Notifications are sent via
// QueueNotification which implements a queue via notificationQueueHandler to
// ensure sending notifications from other subsystems can't block.  Ultimately,
// all messages are sent via the outHandler.
type wsClient struct {
	sync.Mutex

	// server is the RPC server that is servicing the client.
	server *rpcServer

	// conn is the underlying websocket connection.
	conn *websocket.Conn

	// disconnected indicated whether or not the websocket client is
	// disconnected.
	disconnected bool

	// addr is the remote address of the client.
	addr string

	// authenticated specifies whether a client has been authenticated
	// and therefore is allowed to communicated over the websocket.
	authenticated bool

	// isAdmin specifies whether a client may change the state of the server;
	// false means its access is only to the limited set of RPC calls.
	isAdmin bool

	// sessionID is a random ID generated for each client when connected.
	// These IDs may be queried by a client using the session RPC.  A change
	// to the session ID indicates that the client reconnected.
	sessionID uint64

	// verboseTxUpdates specifies whether a client has requested verbose
	// information about all new transactions.
	verboseTxUpdates bool

	// addrRequests is a set of addresses the caller has requested to be
	// notified about.  It is maintained here so all requests can be removed
	// when a wallet disconnects.  Owned by the notification manager.
	addrRequests map[string]struct{}

	// spentRequests is a set of unspent Outpoints a wallet has requested
	// notifications for when they are spent by a processed transaction.
	// Owned by the notification manager.
	spentRequests map[wire.OutPoint]struct{}

	// Networking infrastructure.
	serviceRequestSem semaphore
	ntfnChan          chan []byte
	sendChan          chan wsResponse
	quit              chan struct{}
	wg                sync.WaitGroup
}

// inHandler handles all incoming messages for the websocket connection.  It
// must be run as a goroutine.
func (c *wsClient) inHandler() {
out:
	for {
		// Break out of the loop once the quit channel has been closed.
		// Use a non-blocking select here so we fall through otherwise.
		select {
		case <-c.quit:
			break out
		default:
		}

		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			// Log the error if it's not due to disconnecting.
			if err != io.EOF {
				rpcsLog.Errorf("Websocket receive error from "+
					"%s: %v", c.addr, err)
			}
			break out
		}

		var request btcjson.Request
		err = json.Unmarshal(msg, &request)
		if err != nil {
			if !c.authenticated {
				break out
			}

			jsonErr := &btcjson.RPCError{
				Code:    btcjson.ErrRPCParse.Code,
				Message: "Failed to parse request: " + err.Error(),
			}
			reply, err := createMarshalledReply(nil, nil, jsonErr)
			if err != nil {
				rpcsLog.Errorf("Failed to marshal parse failure "+
					"reply: %v", err)
				continue
			}
			c.SendMessage(reply, nil)
			continue
		}

		// The JSON-RPC 1.0 spec defines that notifications must have their "id"
		// set to null and states that notifications do not have a response.
		//
		// A JSON-RPC 2.0 notification is a request with "json-rpc":"2.0", and
		// without an "id" member. The specification states that notifications
		// must not be responded to. JSON-RPC 2.0 permits the null value as a
		// valid request id, therefore such requests are not notifications.
		//
		// Bitcoin Core serves requests with "id":null or even an absent "id",
		// and responds to such requests with "id":null in the response.
		//
		// Btcd does not respond to any request without and "id" or "id":null,
		// regardless the indicated JSON-RPC protocol version unless RPC quirks
		// are enabled. With RPC quirks enabled, such requests will be responded
		// to if the reqeust does not indicate JSON-RPC version.
		//
		// RPC quirks can be enabled by the user to avoid compatibility issues
		// with software relying on Core's behavior.
		if request.ID == nil && !(cfg.RPCQuirks && request.Jsonrpc == "") {
			if !c.authenticated {
				break out
			}
			continue
		}

		cmd := parseCmd(&request)
		if cmd.err != nil {
			if !c.authenticated {
				break out
			}

			reply, err := createMarshalledReply(cmd.id, nil, cmd.err)
			if err != nil {
				rpcsLog.Errorf("Failed to marshal parse failure "+
					"reply: %v", err)
				continue
			}
			c.SendMessage(reply, nil)
			continue
		}
		rpcsLog.Debugf("Received command <%s> from %s", cmd.method, c.addr)

		// Check auth.  The client is immediately disconnected if the
		// first request of an unauthentiated websocket client is not
		// the authenticate request, an authenticate request is received
		// when the client is already authenticated, or incorrect
		// authentication credentials are provided in the request.
		switch authCmd, ok := cmd.cmd.(*btcjson.AuthenticateCmd); {
		case c.authenticated && ok:
			rpcsLog.Warnf("Websocket client %s is already authenticated",
				c.addr)
			break out
		case !c.authenticated && !ok:
			rpcsLog.Warnf("Unauthenticated websocket message " +
				"received")
			break out
		case !c.authenticated:
			// Check credentials.
			login := authCmd.Username + ":" + authCmd.Passphrase
			auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(login))
			authSha := fastsha256.Sum256([]byte(auth))
			cmp := subtle.ConstantTimeCompare(authSha[:], c.server.authsha[:])
			limitcmp := subtle.ConstantTimeCompare(authSha[:], c.server.limitauthsha[:])
			if cmp != 1 && limitcmp != 1 {
				rpcsLog.Warnf("Auth failure.")
				break out
			}
			c.authenticated = true
			c.isAdmin = cmp == 1

			// Marshal and send response.
			reply, err := createMarshalledReply(cmd.id, nil, nil)
			if err != nil {
				rpcsLog.Errorf("Failed to marshal authenticate reply: "+
					"%v", err.Error())
				continue
			}
			c.SendMessage(reply, nil)
			continue
		}

		// Check if the client is using limited RPC credentials and
		// error when not authorized to call this RPC.
		if !c.isAdmin {
			if _, ok := rpcLimited[request.Method]; !ok {
				jsonErr := &btcjson.RPCError{
					Code:    btcjson.ErrRPCInvalidParams.Code,
					Message: "limited user not authorized for this method",
				}
				// Marshal and send response.
				reply, err := createMarshalledReply(request.ID, nil, jsonErr)
				if err != nil {
					rpcsLog.Errorf("Failed to marshal parse failure "+
						"reply: %v", err)
					continue
				}
				c.SendMessage(reply, nil)
				continue
			}
		}

		// Asynchronously handle the request.  A semaphore is used to
		// limit the number of concurrent requests currently being
		// serviced.  If the semaphore can not be acquired, simply wait
		// until a request finished before reading the next RPC request
		// from the websocket client.
		//
		// This could be a little fancier by timing out and erroring
		// when it takes too long to service the request, but if that is
		// done, the read of the next request should not be blocked by
		// this semaphore, otherwise the next request will be read and
		// will probably sit here for another few seconds before timing
		// out as well.  This will cause the total timeout duration for
		// later requests to be much longer than the check here would
		// imply.
		//
		// If a timeout is added, the semaphore acquiring should be
		// moved inside of the new goroutine with a select statement
		// that also reads a time.After channel.  This will unblock the
		// read of the next request from the websocket client and allow
		// many requests to be waited on concurrently.
		c.serviceRequestSem.acquire()
		go func() {
			c.serviceRequest(cmd)
			c.serviceRequestSem.release()
		}()
	}

	// Ensure the connection is closed.
	c.Disconnect()
	c.wg.Done()
	rpcsLog.Tracef("Websocket client input handler done for %s", c.addr)
}

// serviceRequest services a parsed RPC request by looking up and executing the
// appropiate RPC handler.  The response is marshalled and sent to the websocket
// client.
func (c *wsClient) serviceRequest(r *parsedRPCCmd) {
	var (
		result interface{}
		err    error
	)

	// Lookup the websocket extension for the command and if it doesn't
	// exist fallback to handling the command as a standard command.
	wsHandler, ok := wsHandlers[r.method]
	if ok {
		result, err = wsHandler(c, r.cmd)
	} else {
		result, err = c.server.standardCmdResult(r, nil)
	}
	reply, err := createMarshalledReply(r.id, result, err)
	if err != nil {
		rpcsLog.Errorf("Failed to marshal reply for <%s> "+
			"command: %v", r.method, err)
		return
	}
	c.SendMessage(reply, nil)
}

// notificationQueueHandler handles the queuing of outgoing notifications for
// the websocket client.  This runs as a muxer for various sources of input to
// ensure that queuing up notifications to be sent will not block.  Otherwise,
// slow clients could bog down the other systems (such as the mempool or block
// manager) which are queuing the data.  The data is passed on to outHandler to
// actually be written.  It must be run as a goroutine.
func (c *wsClient) notificationQueueHandler() {
	ntfnSentChan := make(chan bool, 1) // nonblocking sync

	// pendingNtfns is used as a queue for notifications that are ready to
	// be sent once there are no outstanding notifications currently being
	// sent.  The waiting flag is used over simply checking for items in the
	// pending list to ensure cleanup knows what has and hasn't been sent
	// to the outHandler.  Currently no special cleanup is needed, however
	// if something like a done channel is added to notifications in the
	// future, not knowing what has and hasn't been sent to the outHandler
	// (and thus who should respond to the done channel) would be
	// problematic without using this approach.
	pendingNtfns := list.New()
	waiting := false
out:
	for {
		select {
		// This channel is notified when a message is being queued to
		// be sent across the network socket.  It will either send the
		// message immediately if a send is not already in progress, or
		// queue the message to be sent once the other pending messages
		// are sent.
		case msg := <-c.ntfnChan:
			if !waiting {
				c.SendMessage(msg, ntfnSentChan)
			} else {
				pendingNtfns.PushBack(msg)
			}
			waiting = true

		// This channel is notified when a notification has been sent
		// across the network socket.
		case <-ntfnSentChan:
			// No longer waiting if there are no more messages in
			// the pending messages queue.
			next := pendingNtfns.Front()
			if next == nil {
				waiting = false
				continue
			}

			// Notify the outHandler about the next item to
			// asynchronously send.
			msg := pendingNtfns.Remove(next).([]byte)
			c.SendMessage(msg, ntfnSentChan)

		case <-c.quit:
			break out
		}
	}

	// Drain any wait channels before exiting so nothing is left waiting
	// around to send.
cleanup:
	for {
		select {
		case <-c.ntfnChan:
		case <-ntfnSentChan:
		default:
			break cleanup
		}
	}
	c.wg.Done()
	rpcsLog.Tracef("Websocket client notification queue handler done "+
		"for %s", c.addr)
}

// outHandler handles all outgoing messages for the websocket connection.  It
// must be run as a goroutine.  It uses a buffered channel to serialize output
// messages while allowing the sender to continue running asynchronously.  It
// must be run as a goroutine.
func (c *wsClient) outHandler() {
out:
	for {
		// Send any messages ready for send until the quit channel is
		// closed.
		select {
		case r := <-c.sendChan:
			err := c.conn.WriteMessage(websocket.TextMessage, r.msg)
			if err != nil {
				c.Disconnect()
				break out
			}
			if r.doneChan != nil {
				r.doneChan <- true
			}

		case <-c.quit:
			break out
		}
	}

	// Drain any wait channels before exiting so nothing is left waiting
	// around to send.
cleanup:
	for {
		select {
		case r := <-c.sendChan:
			if r.doneChan != nil {
				r.doneChan <- false
			}
		default:
			break cleanup
		}
	}
	c.wg.Done()
	rpcsLog.Tracef("Websocket client output handler done for %s", c.addr)
}

// SendMessage sends the passed json to the websocket client.  It is backed
// by a buffered channel, so it will not block until the send channel is full.
// Note however that QueueNotification must be used for sending async
// notifications instead of the this function.  This approach allows a limit to
// the number of outstanding requests a client can make without preventing or
// blocking on async notifications.
func (c *wsClient) SendMessage(marshalledJSON []byte, doneChan chan bool) {
	// Don't send the message if disconnected.
	if c.Disconnected() {
		if doneChan != nil {
			doneChan <- false
		}
		return
	}

	c.sendChan <- wsResponse{msg: marshalledJSON, doneChan: doneChan}
}

// ErrClientQuit describes the error where a client send is not processed due
// to the client having already been disconnected or dropped.
var ErrClientQuit = errors.New("client quit")

// QueueNotification queues the passed notification to be sent to the websocket
// client.  This function, as the name implies, is only intended for
// notifications since it has additional logic to prevent other subsystems, such
// as the memory pool and block manager, from blocking even when the send
// channel is full.
//
// If the client is in the process of shutting down, this function returns
// ErrClientQuit.  This is intended to be checked by long-running notification
// handlers to stop processing if there is no more work needed to be done.
func (c *wsClient) QueueNotification(marshalledJSON []byte) error {
	// Don't queue the message if disconnected.
	if c.Disconnected() {
		return ErrClientQuit
	}

	c.ntfnChan <- marshalledJSON
	return nil
}

// Disconnected returns whether or not the websocket client is disconnected.
func (c *wsClient) Disconnected() bool {
	c.Lock()
	isDisconnected := c.disconnected
	c.Unlock()

	return isDisconnected
}

// Disconnect disconnects the websocket client.
func (c *wsClient) Disconnect() {
	c.Lock()
	defer c.Unlock()

	// Nothing to do if already disconnected.
	if c.disconnected {
		return
	}

	rpcsLog.Tracef("Disconnecting websocket client %s", c.addr)
	close(c.quit)
	c.conn.Close()
	c.disconnected = true
}

// Start begins processing input and output messages.
func (c *wsClient) Start() {
	rpcsLog.Tracef("Starting websocket client %s", c.addr)

	// Start processing input and output.
	c.wg.Add(3)
	go c.inHandler()
	go c.notificationQueueHandler()
	go c.outHandler()
}

// WaitForShutdown blocks until the websocket client goroutines are stopped
// and the connection is closed.
func (c *wsClient) WaitForShutdown() {
	c.wg.Wait()
}

// newWebsocketClient returns a new websocket client given the notification
// manager, websocket connection, remote address, and whether or not the client
// has already been authenticated (via HTTP Basic access authentication).  The
// returned client is ready to start.  Once started, the client will process
// incoming and outgoing messages in separate goroutines complete with queuing
// and asynchrous handling for long-running operations.
func newWebsocketClient(server *rpcServer, conn *websocket.Conn,
	remoteAddr string, authenticated bool, isAdmin bool) (*wsClient, error) {

	sessionID, err := wire.RandomUint64()
	if err != nil {
		return nil, err
	}

	client := &wsClient{
		conn:              conn,
		addr:              remoteAddr,
		authenticated:     authenticated,
		isAdmin:           isAdmin,
		sessionID:         sessionID,
		server:            server,
		addrRequests:      make(map[string]struct{}),
		spentRequests:     make(map[wire.OutPoint]struct{}),
		serviceRequestSem: makeSemaphore(cfg.RPCMaxConcurrentReqs),
		ntfnChan:          make(chan []byte, 1), // nonblocking sync
		sendChan:          make(chan wsResponse, websocketSendBufferSize),
		quit:              make(chan struct{}),
	}
	return client, nil
}

// handleWebsocketHelp implements the help command for websocket connections.
func handleWebsocketHelp(wsc *wsClient, icmd interface{}) (interface{}, error) {
	cmd, ok := icmd.(*btcjson.HelpCmd)
	if !ok {
		return nil, btcjson.ErrRPCInternal
	}

	// Provide a usage overview of all commands when no specific command
	// was specified.
	var command string
	if cmd.Command != nil {
		command = *cmd.Command
	}
	if command == "" {
		usage, err := wsc.server.helpCacher.rpcUsage(true)
		if err != nil {
			context := "Failed to generate RPC usage"
			return nil, internalRPCError(err.Error(), context)
		}
		return usage, nil
	}

	// Check that the command asked for is supported and implemented.
	// Search the list of websocket handlers as well as the main list of
	// handlers since help should only be provided for those cases.
	valid := true
	if _, ok := rpcHandlers[command]; !ok {
		if _, ok := wsHandlers[command]; !ok {
			valid = false
		}
	}
	if !valid {
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCInvalidParameter,
			Message: "Unknown command: " + command,
		}
	}

	// Get the help for the command.
	help, err := wsc.server.helpCacher.rpcMethodHelp(command)
	if err != nil {
		context := "Failed to generate help"
		return nil, internalRPCError(err.Error(), context)
	}
	return help, nil
}

// handleNotifyBlocks implements the notifyblocks command extension for
// websocket connections.
func handleNotifyBlocks(wsc *wsClient, icmd interface{}) (interface{}, error) {
	wsc.server.ntfnMgr.RegisterBlockUpdates(wsc)
	return nil, nil
}

// handleSession implements the session command extension for websocket
// connections.
func handleSession(wsc *wsClient, icmd interface{}) (interface{}, error) {
	return &btcjson.SessionResult{SessionID: wsc.sessionID}, nil
}

// handleStopNotifyBlocks implements the stopnotifyblocks command extension for
// websocket connections.
func handleStopNotifyBlocks(wsc *wsClient, icmd interface{}) (interface{}, error) {
	wsc.server.ntfnMgr.UnregisterBlockUpdates(wsc)
	return nil, nil
}

// handleNotifySpent implements the notifyspent command extension for
// websocket connections.
func handleNotifySpent(wsc *wsClient, icmd interface{}) (interface{}, error) {
	cmd, ok := icmd.(*btcjson.NotifySpentCmd)
	if !ok {
		return nil, btcjson.ErrRPCInternal
	}

	outpoints, err := deserializeOutpoints(cmd.OutPoints)
	if err != nil {
		return nil, err
	}

	wsc.server.ntfnMgr.RegisterSpentRequests(wsc, outpoints)
	return nil, nil
}

// handleNotifyNewTransations implements the notifynewtransactions command
// extension for websocket connections.
func handleNotifyNewTransactions(wsc *wsClient, icmd interface{}) (interface{}, error) {
	cmd, ok := icmd.(*btcjson.NotifyNewTransactionsCmd)
	if !ok {
		return nil, btcjson.ErrRPCInternal
	}

	wsc.verboseTxUpdates = cmd.Verbose != nil && *cmd.Verbose
	wsc.server.ntfnMgr.RegisterNewMempoolTxsUpdates(wsc)
	return nil, nil
}

// handleStopNotifyNewTransations implements the stopnotifynewtransactions
// command extension for websocket connections.
func handleStopNotifyNewTransactions(wsc *wsClient, icmd interface{}) (interface{}, error) {
	wsc.server.ntfnMgr.UnregisterNewMempoolTxsUpdates(wsc)
	return nil, nil
}

// handleNotifyReceived implements the notifyreceived command extension for
// websocket connections.
func handleNotifyReceived(wsc *wsClient, icmd interface{}) (interface{}, error) {
	cmd, ok := icmd.(*btcjson.NotifyReceivedCmd)
	if !ok {
		return nil, btcjson.ErrRPCInternal
	}

	// Decode addresses to validate input, but the strings slice is used
	// directly if these are all ok.
	err := checkAddressValidity(cmd.Addresses)
	if err != nil {
		return nil, err
	}

	wsc.server.ntfnMgr.RegisterTxOutAddressRequests(wsc, cmd.Addresses)
	return nil, nil
}

// handleStopNotifySpent implements the stopnotifyspent command extension for
// websocket connections.
func handleStopNotifySpent(wsc *wsClient, icmd interface{}) (interface{}, error) {
	cmd, ok := icmd.(*btcjson.StopNotifySpentCmd)
	if !ok {
		return nil, btcjson.ErrRPCInternal
	}

	outpoints, err := deserializeOutpoints(cmd.OutPoints)
	if err != nil {
		return nil, err
	}

	for _, outpoint := range outpoints {
		wsc.server.ntfnMgr.UnregisterSpentRequest(wsc, outpoint)
	}

	return nil, nil
}

// handleStopNotifyReceived implements the stopnotifyreceived command extension
// for websocket connections.
func handleStopNotifyReceived(wsc *wsClient, icmd interface{}) (interface{}, error) {
	cmd, ok := icmd.(*btcjson.StopNotifyReceivedCmd)
	if !ok {
		return nil, btcjson.ErrRPCInternal
	}

	// Decode addresses to validate input, but the strings slice is used
	// directly if these are all ok.
	err := checkAddressValidity(cmd.Addresses)
	if err != nil {
		return nil, err
	}

	for _, addr := range cmd.Addresses {
		wsc.server.ntfnMgr.UnregisterTxOutAddressRequest(wsc, addr)
	}

	return nil, nil
}

// checkAddressValidity checks the validity of each address in the passed
// string slice. It does this by attempting to decode each address using the
// current active network parameters. If any single address fails to decode
// properly, the function returns an error. Otherwise, nil is returned.
func checkAddressValidity(addrs []string) error {
	for _, addr := range addrs {
		_, err := provautil.DecodeAddress(addr, activeNetParams.Params)
		if err != nil {
			return &btcjson.RPCError{
				Code: btcjson.ErrRPCInvalidAddressOrKey,
				Message: fmt.Sprintf("Invalid address or key: %v",
					addr),
			}
		}
	}
	return nil
}

// deserializeOutpoints deserializes each serialized outpoint.
func deserializeOutpoints(serializedOuts []btcjson.OutPoint) ([]*wire.OutPoint, error) {
	outpoints := make([]*wire.OutPoint, 0, len(serializedOuts))
	for i := range serializedOuts {
		blockHash, err := chainhash.NewHashFromStr(serializedOuts[i].Hash)
		if err != nil {
			return nil, rpcDecodeHexError(serializedOuts[i].Hash)
		}
		index := serializedOuts[i].Index
		outpoints = append(outpoints, wire.NewOutPoint(blockHash, index))
	}

	return outpoints, nil
}

type rescanKeys struct {
	fallbacks           map[string]struct{}
	pubKeyHashes        map[[ripemd160.Size]byte]struct{}
	scriptHashes        map[[ripemd160.Size]byte]struct{}
	compressedPubKeys   map[[33]byte]struct{}
	uncompressedPubKeys map[[65]byte]struct{}
	unspent             map[wire.OutPoint]struct{}
}

// unspentSlice returns a slice of currently-unspent outpoints for the rescan
// lookup keys.  This is primarily intended to be used to register outpoints
// for continuous notifications after a rescan has completed.
func (r *rescanKeys) unspentSlice() []*wire.OutPoint {
	ops := make([]*wire.OutPoint, 0, len(r.unspent))
	for op := range r.unspent {
		opCopy := op
		ops = append(ops, &opCopy)
	}
	return ops
}

// ErrRescanReorg defines the error that is returned when an unrecoverable
// reorganize is detected during a rescan.
var ErrRescanReorg = btcjson.RPCError{
	Code:    btcjson.ErrRPCDatabase,
	Message: "Reorganize",
}

// rescanBlock rescans all transactions in a single block.  This is a helper
// function for handleRescan.
func rescanBlock(wsc *wsClient, lookups *rescanKeys, blk *provautil.Block) {
	for _, tx := range blk.Transactions() {
		// Hexadecimal representation of this tx.  Only created if
		// needed, and reused for later notifications if already made.
		var txHex string

		// All inputs and outputs must be iterated through to correctly
		// modify the unspent map, however, just a single notification
		// for any matching transaction inputs or outputs should be
		// created and sent.
		spentNotified := false
		recvNotified := false

		for _, txin := range tx.MsgTx().TxIn {
			if _, ok := lookups.unspent[txin.PreviousOutPoint]; ok {
				delete(lookups.unspent, txin.PreviousOutPoint)

				if spentNotified {
					continue
				}

				if txHex == "" {
					txHex = txHexString(tx)
				}
				marshalledJSON, err := newRedeemingTxNotification(txHex, tx.Index(), blk)
				if err != nil {
					rpcsLog.Errorf("Failed to marshal redeemingtx notification: %v", err)
					continue
				}

				err = wsc.QueueNotification(marshalledJSON)
				// Stop the rescan early if the websocket client
				// disconnected.
				if err == ErrClientQuit {
					return
				}
				spentNotified = true
			}
		}

		for txOutIdx, txout := range tx.MsgTx().TxOut {
			_, addrs, _, _ := txscript.ExtractPkScriptAddrs(
				txout.PkScript, wsc.server.server.chainParams)

			for _, addr := range addrs {
				// A new address type must have been added.  Encode as a
				// payment address string and check the fallback map.
				addrStr := addr.EncodeAddress()
				_, ok := lookups.fallbacks[addrStr]
				if !ok {
					continue
				}

				outpoint := wire.OutPoint{
					Hash:  *tx.Hash(),
					Index: uint32(txOutIdx),
				}
				lookups.unspent[outpoint] = struct{}{}

				if recvNotified {
					continue
				}

				if txHex == "" {
					txHex = txHexString(tx)
				}
				ntfn := btcjson.NewRecvTxNtfn(txHex,
					blockDetails(blk, tx.Index()))

				marshalledJSON, err := btcjson.MarshalCmd(nil, ntfn)
				if err != nil {
					rpcsLog.Errorf("Failed to marshal recvtx notification: %v", err)
					return
				}

				err = wsc.QueueNotification(marshalledJSON)
				// Stop the rescan early if the websocket client
				// disconnected.
				if err == ErrClientQuit {
					return
				}
				recvNotified = true
			}
		}
	}
}

// recoverFromReorg attempts to recover from a detected reorganize during a
// rescan.  It fetches a new range of block shas from the database and
// verifies that the new range of blocks is on the same fork as a previous
// range of blocks.  If this condition does not hold true, the JSON-RPC error
// for an unrecoverable reorganize is returned.
func recoverFromReorg(chain *blockchain.BlockChain, minBlock, maxBlock uint32,
	lastBlock *chainhash.Hash) ([]chainhash.Hash, error) {

	hashList, err := chain.HeightRange(minBlock, maxBlock)
	if err != nil {
		rpcsLog.Errorf("Error looking up block range: %v", err)
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCDatabase,
			Message: "Database error: " + err.Error(),
		}
	}
	if lastBlock == nil || len(hashList) == 0 {
		return hashList, nil
	}

	blk, err := chain.BlockByHash(&hashList[0])
	if err != nil {
		rpcsLog.Errorf("Error looking up possibly reorged block: %v",
			err)
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCDatabase,
			Message: "Database error: " + err.Error(),
		}
	}
	jsonErr := descendantBlock(lastBlock, blk)
	if jsonErr != nil {
		return nil, jsonErr
	}
	return hashList, nil
}

// descendantBlock returns the appropriate JSON-RPC error if a current block
// fetched during a reorganize is not a direct child of the parent block hash.
func descendantBlock(prevHash *chainhash.Hash, curBlock *provautil.Block) error {
	curHash := &curBlock.MsgBlock().Header.PrevBlock
	if !prevHash.IsEqual(curHash) {
		rpcsLog.Errorf("Stopping rescan for reorged block %v "+
			"(replaced by block %v)", prevHash, curHash)
		return &ErrRescanReorg
	}
	return nil
}

// handleRescan implements the rescan command extension for websocket
// connections.
//
// NOTE: This does not smartly handle reorgs, and fixing requires database
// changes (for safe, concurrent access to full block ranges, and support
// for other chains than the best chain).  It will, however, detect whether
// a reorg removed a block that was previously processed, and result in the
// handler erroring.  Clients must handle this by finding a block still in
// the chain (perhaps from a rescanprogress notification) to resume their
// rescan.
func handleRescan(wsc *wsClient, icmd interface{}) (interface{}, error) {
	cmd, ok := icmd.(*btcjson.RescanCmd)
	if !ok {
		return nil, btcjson.ErrRPCInternal
	}

	outpoints := make([]*wire.OutPoint, 0, len(cmd.OutPoints))
	for i := range cmd.OutPoints {
		cmdOutpoint := &cmd.OutPoints[i]
		blockHash, err := chainhash.NewHashFromStr(cmdOutpoint.Hash)
		if err != nil {
			return nil, rpcDecodeHexError(cmdOutpoint.Hash)
		}
		outpoint := wire.NewOutPoint(blockHash, cmdOutpoint.Index)
		outpoints = append(outpoints, outpoint)
	}

	numAddrs := len(cmd.Addresses)
	if numAddrs == 1 {
		rpcsLog.Info("Beginning rescan for 1 address")
	} else {
		rpcsLog.Infof("Beginning rescan for %d addresses", numAddrs)
	}

	// Build lookup maps.
	lookups := rescanKeys{
		fallbacks:           map[string]struct{}{},
		pubKeyHashes:        map[[ripemd160.Size]byte]struct{}{},
		scriptHashes:        map[[ripemd160.Size]byte]struct{}{},
		compressedPubKeys:   map[[33]byte]struct{}{},
		uncompressedPubKeys: map[[65]byte]struct{}{},
		unspent:             map[wire.OutPoint]struct{}{},
	}
	for _, addrStr := range cmd.Addresses {
		_, err := provautil.DecodeAddress(addrStr, activeNetParams.Params)
		if err != nil {
			jsonErr := btcjson.RPCError{
				Code: btcjson.ErrRPCInvalidAddressOrKey,
				Message: "Rescan address " + addrStr + ": " +
					err.Error(),
			}
			return nil, &jsonErr
		}
		// A new address type must have been added.  Use encoded
		// payment address string as a fallback until a fast path
		// is added.
		lookups.fallbacks[addrStr] = struct{}{}
	}
	for _, outpoint := range outpoints {
		lookups.unspent[*outpoint] = struct{}{}
	}

	chain := wsc.server.chain

	minBlockHash, err := chainhash.NewHashFromStr(cmd.BeginBlock)
	if err != nil {
		return nil, rpcDecodeHexError(cmd.BeginBlock)
	}
	minBlock, err := chain.BlockHeightByHash(minBlockHash)
	if err != nil {
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCBlockNotFound,
			Message: "Error getting block: " + err.Error(),
		}
	}

	maxBlock := uint32(math.MaxUint32)
	if cmd.EndBlock != nil {
		maxBlockHash, err := chainhash.NewHashFromStr(*cmd.EndBlock)
		if err != nil {
			return nil, rpcDecodeHexError(*cmd.EndBlock)
		}
		maxBlock, err = chain.BlockHeightByHash(maxBlockHash)
		if err != nil {
			return nil, &btcjson.RPCError{
				Code:    btcjson.ErrRPCBlockNotFound,
				Message: "Error getting block: " + err.Error(),
			}
		}
	}

	// lastBlock and lastBlockHash track the previously-rescanned block.
	// They equal nil when no previous blocks have been rescanned.
	var lastBlock *provautil.Block
	var lastBlockHash *chainhash.Hash

	// A ticker is created to wait at least 10 seconds before notifying the
	// websocket client of the current progress completed by the rescan.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Instead of fetching all block shas at once, fetch in smaller chunks
	// to ensure large rescans consume a limited amount of memory.
fetchRange:
	for minBlock < maxBlock {
		// Limit the max number of hashes to fetch at once to the
		// maximum number of items allowed in a single inventory.
		// This value could be higher since it's not creating inventory
		// messages, but this mirrors the limiting logic used in the
		// peer-to-peer protocol.
		maxLoopBlock := maxBlock
		if maxLoopBlock-minBlock > wire.MaxInvPerMsg {
			maxLoopBlock = minBlock + wire.MaxInvPerMsg
		}
		hashList, err := chain.HeightRange(minBlock, maxLoopBlock)
		if err != nil {
			rpcsLog.Errorf("Error looking up block range: %v", err)
			return nil, &btcjson.RPCError{
				Code:    btcjson.ErrRPCDatabase,
				Message: "Database error: " + err.Error(),
			}
		}
		if len(hashList) == 0 {
			// The rescan is finished if no blocks hashes for this
			// range were successfully fetched and a stop block
			// was provided.
			if maxBlock != math.MaxInt32 {
				break
			}

			// If the rescan is through the current block, set up
			// the client to continue to receive notifications
			// regarding all rescanned addresses and the current set
			// of unspent outputs.
			//
			// This is done safely by temporarily grabbing exclusive
			// access of the block manager.  If no more blocks have
			// been attached between this pause and the fetch above,
			// then it is safe to register the websocket client for
			// continuous notifications if necessary.  Otherwise,
			// continue the fetch loop again to rescan the new
			// blocks (or error due to an irrecoverable reorganize).
			blockManager := wsc.server.server.blockManager
			pauseGuard := blockManager.Pause()
			best := blockManager.chain.BestSnapshot()
			curHash := best.Hash
			again := true
			if lastBlockHash == nil || *lastBlockHash == *curHash {
				again = false
				n := wsc.server.ntfnMgr
				n.RegisterSpentRequests(wsc, lookups.unspentSlice())
				n.RegisterTxOutAddressRequests(wsc, cmd.Addresses)
			}
			close(pauseGuard)
			if err != nil {
				rpcsLog.Errorf("Error fetching best block "+
					"hash: %v", err)
				return nil, &btcjson.RPCError{
					Code: btcjson.ErrRPCDatabase,
					Message: "Database error: " +
						err.Error(),
				}
			}
			if again {
				continue
			}
			break
		}

	loopHashList:
		for i := range hashList {
			blk, err := chain.BlockByHash(&hashList[i])
			if err != nil {
				// Only handle reorgs if a block could not be
				// found for the hash.
				if dbErr, ok := err.(database.Error); !ok ||
					dbErr.ErrorCode != database.ErrBlockNotFound {

					rpcsLog.Errorf("Error looking up "+
						"block: %v", err)
					return nil, &btcjson.RPCError{
						Code: btcjson.ErrRPCDatabase,
						Message: "Database error: " +
							err.Error(),
					}
				}

				// If an absolute max block was specified, don't
				// attempt to handle the reorg.
				if maxBlock != math.MaxInt32 {
					rpcsLog.Errorf("Stopping rescan for "+
						"reorged block %v",
						cmd.EndBlock)
					return nil, &ErrRescanReorg
				}

				// If the lookup for the previously valid block
				// hash failed, there may have been a reorg.
				// Fetch a new range of block hashes and verify
				// that the previously processed block (if there
				// was any) still exists in the database.  If it
				// doesn't, we error.
				//
				// A goto is used to branch executation back to
				// before the range was evaluated, as it must be
				// reevaluated for the new hashList.
				minBlock += uint32(i)
				hashList, err = recoverFromReorg(chain,
					minBlock, maxBlock, lastBlockHash)
				if err != nil {
					return nil, err
				}
				if len(hashList) == 0 {
					break fetchRange
				}
				goto loopHashList
			}
			if i == 0 && lastBlockHash != nil {
				// Ensure the new hashList is on the same fork
				// as the last block from the old hashList.
				jsonErr := descendantBlock(lastBlockHash, blk)
				if jsonErr != nil {
					return nil, jsonErr
				}
			}

			// A select statement is used to stop rescans if the
			// client requesting the rescan has disconnected.
			select {
			case <-wsc.quit:
				rpcsLog.Debugf("Stopped rescan at height %v "+
					"for disconnected client", blk.Height())
				return nil, nil
			default:
				rescanBlock(wsc, &lookups, blk)
				lastBlock = blk
				lastBlockHash = blk.Hash()
			}

			// Periodically notify the client of the progress
			// completed.  Continue with next block if no progress
			// notification is needed yet.
			select {
			case <-ticker.C: // fallthrough
			default:
				continue
			}

			n := btcjson.NewRescanProgressNtfn(hashList[i].String(),
				int32(blk.Height()),
				blk.MsgBlock().Header.Timestamp.Unix())
			mn, err := btcjson.MarshalCmd(nil, n)
			if err != nil {
				rpcsLog.Errorf("Failed to marshal rescan "+
					"progress notification: %v", err)
				continue
			}

			if err = wsc.QueueNotification(mn); err == ErrClientQuit {
				// Finished if the client disconnected.
				rpcsLog.Debugf("Stopped rescan at height %v "+
					"for disconnected client", blk.Height())
				return nil, nil
			}
		}

		minBlock += uint32(len(hashList))
	}

	// Notify websocket client of the finished rescan.  Due to how btcd
	// asynchronously queues notifications to not block calling code,
	// there is no guarantee that any of the notifications created during
	// rescan (such as rescanprogress, recvtx and redeemingtx) will be
	// received before the rescan RPC returns.  Therefore, another method
	// is needed to safely inform clients that all rescan notifications have
	// been sent.
	n := btcjson.NewRescanFinishedNtfn(lastBlockHash.String(),
		int32(lastBlock.Height()),
		lastBlock.MsgBlock().Header.Timestamp.Unix())
	if mn, err := btcjson.MarshalCmd(nil, n); err != nil {
		rpcsLog.Errorf("Failed to marshal rescan finished "+
			"notification: %v", err)
	} else {
		// The rescan is finished, so we don't care whether the client
		// has disconnected at this point, so discard error.
		_ = wsc.QueueNotification(mn)
	}

	rpcsLog.Info("Finished rescan")
	return nil, nil
}

func init() {
	wsHandlers = wsHandlersBeforeInit
}
