// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package p2p

import (
	"container/heap"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand"
	"net"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/helper/delayheap"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/netutil"
)

const (
	// This is the amount of time spent waiting in between redialing a certain node. The
	// limit is a bit higher than inboundThrottleTime to prevent failing dials in small
	// private networks.
	dialHistoryExpiration = inboundThrottleTime + 5*time.Second

	// Config for the "Looking for peers" message.
	dialStatsLogInterval = 10 * time.Second // printed at most this often
	dialStatsPeerLimit   = 3                // but not if more than this many dialed peers

	// Endpoint resolution is throttled with bounded backoff.
	initialResolveDelay = 60 * time.Second
	maxResolveDelay     = time.Hour
)

// NodeDialer is used to connect to nodes in the network, typically by using
// an underlying net.Dialer but also using net.Pipe in tests.
type NodeDialer interface {
	Dial(context.Context, *enode.Node) (net.Conn, error)
}

type nodeResolver interface {
	Resolve(*enode.Node) *enode.Node
}

// tcpDialer implements NodeDialer using real TCP connections.
type tcpDialer struct {
	d *net.Dialer
}

func (t tcpDialer) Dial(ctx context.Context, dest *enode.Node) (net.Conn, error) {
	return t.d.DialContext(ctx, "tcp", nodeAddr(dest).String())
}

func nodeAddr(n *enode.Node) net.Addr {
	return &net.TCPAddr{IP: n.IP(), Port: n.TCP()}
}

// checkDial errors:
var (
	errSelf             = errors.New("is self")
	errAlreadyDialing   = errors.New("already dialing")
	errAlreadyConnected = errors.New("already connected")
	errRecentlyDialed   = errors.New("recently dialed")
	errNetRestrict      = errors.New("not contained in netrestrict list")
	errNoPort           = errors.New("node does not provide TCP port")
)

// dialer creates outbound connections and submits them into Server.
// Two types of peer connections can be created:
//
//  - static dials are pre-configured connections. The dialer attempts
//    keep these nodes connected at all times.
//
//  - dynamic dials are created from node discovery results. The dialer
//    continuously reads candidate nodes from its input iterator and attempts
//    to create peer connections to nodes arriving through the iterator.
//
type dialScheduler struct {
	config    *dialConfig
	setupFunc dialSetupFunc
	// wg        sync.WaitGroup
	//cancel context.CancelFunc
	//ctx    context.Context
	// nodesIn     chan *enode.Node
	doneCh      chan *dialTask
	addStaticCh chan *enode.Node
	remStaticCh chan *enode.Node
	addPeerCh   chan *conn
	remPeerCh   chan *conn

	closeCh chan struct{}

	// Everything below here belongs to loop and
	// should only be accessed by code on the loop goroutine.

	// active dialing tasks
	dialing map[enode.ID]struct{}

	// list of connected peers
	peers map[enode.ID]struct{}

	// list of static peers
	static map[enode.ID]struct{}

	dialPeers int // current number of dialed peers

	// The static map tracks all static dial tasks. The subset of usable static dial tasks
	// (i.e. those passing checkDial) is kept in staticPool. The scheduler prefers
	// launching random static tasks from the pool over launching dynamic dials from the
	// iterator.

	// staticPool []*dialTask
	staticPool *dialQueue

	// The dial history keeps recently dialed nodes. Members of history are not dialed.
	history *delayheap.PeriodicDispatcher
	//historyTimer     mclock.Timer
	//historyTimerTime mclock.AbsTime

	// for logStats
	lastStatsLog     mclock.AbsTime
	doneSinceLastLog int
}

type dialSetupFunc func(net.Conn, connFlag, *enode.Node) error

type dialConfig struct {
	self           enode.ID         // our own ID
	maxDialPeers   int              // maximum number of dialed peers
	maxActiveDials int              // maximum number of active dials
	netRestrict    *netutil.Netlist // IP netrestrict list, disabled if nil
	resolver       nodeResolver
	dialer         NodeDialer
	log            log.Logger
	clock          mclock.Clock
	rand           *mrand.Rand
}

func (cfg dialConfig) withDefaults() *dialConfig {
	if cfg.maxActiveDials == 0 {
		cfg.maxActiveDials = defaultMaxPendingPeers
	}
	if cfg.log == nil {
		cfg.log = log.Root()
	}
	if cfg.clock == nil {
		cfg.clock = mclock.System{}
	}
	if cfg.rand == nil {
		seedb := make([]byte, 8)
		crand.Read(seedb)
		seed := int64(binary.BigEndian.Uint64(seedb))
		cfg.rand = mrand.New(mrand.NewSource(seed))
	}
	return &cfg
}

func newDialScheduler(config dialConfig, it enode.Iterator, setupFunc dialSetupFunc) *dialScheduler {
	d := &dialScheduler{
		config:    config.withDefaults(),
		setupFunc: setupFunc,
		dialing:   make(map[enode.ID]struct{}),
		static:    make(map[enode.ID]struct{}),
		peers:     make(map[enode.ID]struct{}),
		doneCh:    make(chan *dialTask),
		// nodesIn:     make(chan *enode.Node),
		addStaticCh: make(chan *enode.Node),
		remStaticCh: make(chan *enode.Node),
		addPeerCh:   make(chan *conn),
		remPeerCh:   make(chan *conn),
		staticPool:  newDialQueue(),
		closeCh:     make(chan struct{}),
	}

	d.history = delayheap.NewPeriodicDispatcher(d)
	d.history.Run()

	d.lastStatsLog = d.config.clock.Now()
	//d.ctx, d.cancel = context.WithCancel(context.Background())
	//d.wg.Add(2)
	//go d.readNodes(it)
	go d.loop(it)
	return d
}

func (d *dialScheduler) Enqueue(h delayheap.HeapNode) {
	panic("x")
}

// stop shuts down the dialer, canceling all current dial tasks.
func (d *dialScheduler) stop() {
	// d.cancel()
	close(d.closeCh)
	//d.wg.Wait()
}

// addStatic adds a static dial candidate.
func (d *dialScheduler) addStatic(n *enode.Node) {
	select {
	case d.addStaticCh <- n:
	case <-d.closeCh:
	}
}

// removeStatic removes a static dial candidate.
func (d *dialScheduler) removeStatic(n *enode.Node) {
	select {
	case d.remStaticCh <- n:
	case <-d.closeCh:
	}
}

// peerAdded updates the peer set.
func (d *dialScheduler) peerAdded(c *conn) {
	select {
	case d.addPeerCh <- c:
	case <-d.closeCh:
	}
}

// peerRemoved updates the peer set.
func (d *dialScheduler) peerRemoved(c *conn) {
	select {
	case d.remPeerCh <- c:
	case <-d.closeCh:
	}
}

// loop is the main loop of the dialer.
func (d *dialScheduler) loop(it enode.Iterator) {
	var (
	//nodesCh    chan *enode.Node
	//historyExp = make(chan struct{}, 1)
	)

	notify := make(chan struct{})

	go func() {
		for {
			for it.Next() {
				node := it.Node()
				if node == nil {
					return
				}

				d.staticPool.add(node, false, normalPriority)

				select {
				case notify <- struct{}{}:
				default:
				}
			}
		}
	}()

loop:
	for {
		// Launch new dials if slots are available.

		_, tasks := d.startStaticDials()
		for _, task := range tasks {
			d.startDial(task)
		}

		//if slots > 0 {
		//	nodesCh = d.nodesIn
		//} else {
		//	nodesCh = nil
		//}
		//d.rearmHistoryTimer(historyExp)
		d.logStats()

		select {
		case <-notify:
		/*
			case node := <-nodesCh:
				if err := d.checkDial(node); err != nil {
					d.config.log.Trace("Discarding dial candidate", "id", node.ID(), "ip", node.IP(), "reason", err)
				} else {
					d.startDial(newDialTask(node, dynDialedConn))
				}
		*/

		case task := <-d.doneCh:
			id := task.dest.ID()
			delete(d.dialing, id)
			d.updateStaticPool(task.dest)
			d.doneSinceLastLog++

		case c := <-d.addPeerCh:
			if c.is(dynDialedConn) || c.is(staticDialedConn) {
				d.dialPeers++
			}
			id := c.node.ID()
			d.peers[id] = struct{}{}
			// Remove from static pool because the node is now connected.
			//task := d.static[id]
			//if task != nil && task.staticPoolIndex >= 0 {
			//d.removeFromStaticPool(task.staticPoolIndex)
			//}
			// TODO: cancel dials to connected peers

		case c := <-d.remPeerCh:
			if c.is(dynDialedConn) || c.is(staticDialedConn) {
				d.dialPeers--
			}
			delete(d.peers, c.node.ID())
			d.updateStaticPool(c.node)

		case node := <-d.addStaticCh:
			id := node.ID()
			_, exists := d.static[id]
			d.config.log.Trace("Adding static node", "id", id, "ip", node.IP(), "added", !exists)
			if exists {
				continue loop
			}
			// task := newDialTask(node, staticDialedConn)
			d.static[id] = struct{}{}
			if d.checkDial(node) == nil {
				d.addToStaticPool(node)
			}

		case node := <-d.remStaticCh:
			id := node.ID()
			_, exists := d.static[id]
			d.config.log.Trace("Removing static node", "id", id, "ok", exists)
			if exists {
				delete(d.static, id)
				//if task.staticPoolIndex >= 0 {
				//d.removeFromStaticPool(task.staticPoolIndex)
				//}
			}

		//case <-historyExp:
		//d.expireHistory()

		case <-d.closeCh:
			it.Close()
			break loop
		}
	}

	//d.stopHistoryTimer(historyExp)
	for range d.dialing {
		<-d.doneCh
	}
	// d.wg.Done()
}

/*
// readNodes runs in its own goroutine and delivers nodes from
// the input iterator to the nodesIn channel.
func (d *dialScheduler) readNodes(it enode.Iterator) {
	defer d.wg.Done()

		for it.Next() {
			select {
			case d.nodesIn <- it.Node():
			case <-d.ctx.Done():
			}
		}
}
*/

// logStats prints dialer statistics to the log. The message is suppressed when enough
// peers are connected because users should only see it while their client is starting up
// or comes back online.
func (d *dialScheduler) logStats() {
	now := d.config.clock.Now()
	if d.lastStatsLog.Add(dialStatsLogInterval) > now {
		return
	}
	if d.dialPeers < dialStatsPeerLimit && d.dialPeers < d.config.maxDialPeers {
		d.config.log.Info("Looking for peers", "peercount", len(d.peers), "tried", d.doneSinceLastLog, "static", len(d.static))
	}
	d.doneSinceLastLog = 0
	d.lastStatsLog = now
}

/*
// rearmHistoryTimer configures d.historyTimer to fire when the
// next item in d.history expires.
func (d *dialScheduler) rearmHistoryTimer(ch chan struct{}) {
	if len(d.history) == 0 || d.historyTimerTime == d.history.nextExpiry() {
		return
	}
	d.stopHistoryTimer(ch)
	d.historyTimerTime = d.history.nextExpiry()
	timeout := time.Duration(d.historyTimerTime - d.config.clock.Now())
	d.historyTimer = d.config.clock.AfterFunc(timeout, func() { ch <- struct{}{} })
}

// stopHistoryTimer stops the timer and drains the channel it sends on.
func (d *dialScheduler) stopHistoryTimer(ch chan struct{}) {
	if d.historyTimer != nil && !d.historyTimer.Stop() {
		<-ch
	}
}

// expireHistory removes expired items from d.history.
func (d *dialScheduler) expireHistory() {
	d.historyTimer.Stop()
	d.historyTimer = nil
	d.historyTimerTime = 0
	d.history.expire(d.config.clock.Now(), func(hkey string) {
		var id enode.ID
		copy(id[:], hkey)
		d.updateStaticPool(id)
	})
}
*/

// freeDialSlots returns the number of free dial slots. The result can be negative
// when peers are connected while their task is still running.
func (d *dialScheduler) freeDialSlots() int {
	slots := (d.config.maxDialPeers - d.dialPeers) * 2
	if slots > d.config.maxActiveDials {
		slots = d.config.maxActiveDials
	}
	free := slots - len(d.dialing)
	return free
}

// checkDial returns an error if node n should not be dialed.
func (d *dialScheduler) checkDial(n *enode.Node) error {
	if n.ID() == d.config.self {
		return errSelf
	}
	if n.IP() != nil && n.TCP() == 0 {
		// This check can trigger if a non-TCP node is found
		// by discovery. If there is no IP, the node is a static
		// node and the actual endpoint will be resolved later in dialTask.
		return errNoPort
	}
	if _, ok := d.dialing[n.ID()]; ok {
		return errAlreadyDialing
	}
	if _, ok := d.peers[n.ID()]; ok {
		return errAlreadyConnected
	}
	if d.config.netRestrict != nil && !d.config.netRestrict.Contains(n.IP()) {
		return errNetRestrict
	}
	if d.history.ContainsID(n.ID().String()) {
		return errRecentlyDialed
	}
	return nil
}

// startStaticDials starts n static dial tasks.
func (d *dialScheduler) startStaticDials() (started int, res []*dialTask) {
	n := d.freeDialSlots()

	res = []*dialTask{}

	for started = 0; started < n && d.staticPool.Len() > 0; {
		task := d.staticPool.popImpl()

		if task.isStatic {
			// make sure it is static, otherwise it is not connected here
			if _, ok := d.static[task.addr.ID()]; !ok {
				continue
			}
		}

		// make sure we are not connected to the instance
		if _, ok := d.peers[task.addr.ID()]; ok {
			continue
		}

		if err := d.checkDial(task.addr); err != nil {
			d.config.log.Trace("Discarding dial candidate", "id", task.addr.ID(), "ip", task.addr.IP(), "reason", err)
			continue
		}

		var ttt *dialTask
		if task.isStatic {
			ttt = newDialTask(task.addr, staticDialedConn)
		} else {
			ttt = newDialTask(task.addr, dynDialedConn)
		}

		//idx := d.rand.Intn(len(d.staticPool))
		//task := d.staticPool[idx]
		res = append(res, ttt)
		// d.removeFromStaticPool(idx)

		started++
	}
	return started, res
}

// updateStaticPool attempts to move the given static dial back into staticPool.
func (d *dialScheduler) updateStaticPool(node *enode.Node) {
	_, ok := d.static[node.ID()]
	if ok && d.checkDial(node) == nil {
		d.addToStaticPool(node)
	}
}

func (d *dialScheduler) addToStaticPool(node *enode.Node) {
	d.staticPool.add(node, true, staticPriority)

	/*
		d.staticPool = append(d.staticPool, task)
		task.staticPoolIndex = len(d.staticPool) - 1
	*/
}

/*
// removeFromStaticPool removes the task at idx from staticPool. It does that by moving the
// current last element of the pool to idx and then shortening the pool by one.
func (d *dialScheduler) removeFromStaticPool(idx int) {
	task := d.staticPool[idx]
	end := len(d.staticPool) - 1
	d.staticPool[idx] = d.staticPool[end]
	d.staticPool[idx].staticPoolIndex = idx
	d.staticPool[end] = nil
	d.staticPool = d.staticPool[:end]
	task.staticPoolIndex = -1
}
*/

type enodeWrapper struct {
	enode *enode.Node
}

func (e *enodeWrapper) ID() string {
	return e.enode.ID().String()
}

// startDial runs the given dial task in a separate goroutine.
func (d *dialScheduler) startDial(task *dialTask) {
	d.config.log.Trace("Starting p2p dial", "id", task.dest.ID(), "ip", task.dest.IP(), "flag", task.flags)
	// hkey := string(task.dest.ID().Bytes())

	d.history.Add(&enodeWrapper{enode: task.dest}, time.Now().Add(dialHistoryExpiration))

	//d.history.add(hkey, d.config.clock.Now().Add(dialHistoryExpiration))
	d.dialing[task.dest.ID()] = struct{}{}

	if task.needResolve() && !d.resolve(task) {
		// log it
		return
	}

	dial := func() {
		err := d.dial(task)
		if err != nil {
			// For static nodes, resolve one more time if dialing fails.
			if _, ok := err.(*dialError); ok && task.flags&staticDialedConn != 0 {
				if d.resolve(task) {
					d.dial(task)
				}
			}
		}
	}

	go func() {
		dial()
		d.doneCh <- task
	}()
}

// A dialTask generated for each node that is dialed.
type dialTask struct {
	// staticPoolIndex int
	flags connFlag
	// These fields are private to the task and should not be
	// accessed by dialScheduler while the task is running.
	dest         *enode.Node
	lastResolved mclock.AbsTime
	resolveDelay time.Duration
}

func (d *dialTask) ID() string {
	return d.dest.ID().String()
}

func newDialTask(dest *enode.Node, flags connFlag) *dialTask {
	return &dialTask{dest: dest, flags: flags /*, staticPoolIndex: -1*/}
}

type dialError struct {
	error
}

/*
func (t *dialTask) run(d *dialScheduler) {
	if t.needResolve() && !t.resolve(d) {
		return
	}

	err := t.dial(d, t.dest)
	if err != nil {
		// For static nodes, resolve one more time if dialing fails.
		if _, ok := err.(*dialError); ok && t.flags&staticDialedConn != 0 {
			if t.resolve(d) {
				t.dial(d, t.dest)
			}
		}
	}
}
*/

func (t *dialTask) needResolve() bool {
	return t.flags&staticDialedConn != 0 && t.dest.IP() == nil
}

// resolve attempts to find the current endpoint for the destination
// using discovery.
//
// Resolve operations are throttled with backoff to avoid flooding the
// discovery network with useless queries for nodes that don't exist.
// The backoff delay resets when the node is found.
func (d *dialScheduler) resolve(t *dialTask) bool {
	if d.config.resolver == nil {
		return false
	}
	if t.resolveDelay == 0 {
		t.resolveDelay = initialResolveDelay
	}
	if t.lastResolved > 0 && time.Duration(d.config.clock.Now()-t.lastResolved) < t.resolveDelay {
		return false
	}
	resolved := d.config.resolver.Resolve(t.dest)
	t.lastResolved = d.config.clock.Now()
	if resolved == nil {
		t.resolveDelay *= 2
		if t.resolveDelay > maxResolveDelay {
			t.resolveDelay = maxResolveDelay
		}
		d.config.log.Debug("Resolving node failed", "id", t.dest.ID(), "newdelay", t.resolveDelay)
		return false
	}
	// The node was found.
	t.resolveDelay = initialResolveDelay
	t.dest = resolved
	d.config.log.Debug("Resolved node", "id", t.dest.ID(), "addr", &net.TCPAddr{IP: t.dest.IP(), Port: t.dest.TCP()})
	return true
}

// dial performs the actual connection attempt.
func (d *dialScheduler) dial(t *dialTask) error {
	dest := t.dest

	fd, err := d.config.dialer.Dial(context.Background(), t.dest)
	if err != nil {
		d.config.log.Trace("Dial error", "id", t.dest.ID(), "addr", nodeAddr(t.dest), "conn", t.flags, "err", cleanupDialErr(err))
		return &dialError{err}
	}
	mfd := newMeteredConn(fd, false, &net.TCPAddr{IP: dest.IP(), Port: dest.TCP()})
	return d.setupFunc(mfd, t.flags, dest)
}

func (t *dialTask) String() string {
	id := t.dest.ID()
	return fmt.Sprintf("%v %x %v:%d", t.flags, id[:8], t.dest.IP(), t.dest.TCP())
}

func cleanupDialErr(err error) error {
	if netErr, ok := err.(*net.OpError); ok && netErr.Op == "dial" {
		return netErr.Err
	}
	return err
}

// ----

// dialQueue is a queue where we store all the possible peer targets that
// we can connect to.
type dialQueue struct {
	heap  dialQueueImpl
	lock  sync.Mutex
	items map[*enode.Node]*dialTask2
	//updateCh chan struct{}
	//closeCh  chan struct{}
}

// newDialQueue creates a new DialQueue
func newDialQueue() *dialQueue {
	return &dialQueue{
		heap:  dialQueueImpl{},
		items: map[*enode.Node]*dialTask2{},
		//updateCh: make(chan struct{}),
		//closeCh:  make(chan struct{}),
	}
}

func (d *dialQueue) Close() {
	//close(d.closeCh)
}

/*
// pop is a loop that handles update and close events
func (d *dialQueue) pop() *dialTask2 {
	for {
		tt := d.popImpl() // Blocking pop
		if tt != nil {
			return tt
		}

		select {
		case <-d.updateCh:
		case <-d.closeCh:
			return nil
		}
	}
}
*/

func (d *dialQueue) popImpl() *dialTask2 {
	d.lock.Lock()

	if len(d.heap) != 0 {
		// pop the first value and remove it from the heap
		tt := heap.Pop(&d.heap).(*dialTask2)
		delete(d.items, tt.addr)
		d.lock.Unlock()
		return tt
	}

	d.lock.Unlock()
	return nil
}

/*
func (d *dialQueue) del(peer peer.ID) {
	d.lock.Lock()
	defer d.lock.Unlock()

	item, ok := d.items[peer]
	if ok {
		heap.Remove(&d.heap, item.index)
		delete(d.items, peer)
	}
}
*/

func (d *dialQueue) Len() int {
	d.lock.Lock()
	defer d.lock.Unlock()

	return len(d.items)
}

const (
	staticPriority = 5
	normalPriority = 10
)

func (d *dialQueue) add(addr *enode.Node, isStatic bool, priority uint64) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if _, ok := d.items[addr]; ok {
		return
	}
	task := &dialTask2{
		addr:       addr,
		priority:   priority,
		insertTime: time.Now(),
		isStatic:   isStatic,
	}
	d.items[addr] = task
	heap.Push(&d.heap, task)

	/*
		select {
		case d.updateCh <- struct{}{}:
		default:
		}
	*/
}

type dialTask2 struct {
	index int

	// info of the task
	addr *enode.Node

	// time when the item was inserted
	insertTime time.Time

	isStatic bool

	// priority of the task (the higher the better)
	priority uint64
}

// The DialQueue is implemented as priority queue which utilizes a heap (standard Go implementation)
// https://golang.org/src/container/heap/heap.go
type dialQueueImpl []*dialTask2

// GO HEAP EXTENSION //

// Len returns the length of the queue
func (t dialQueueImpl) Len() int { return len(t) }

// Less compares the priorities of two items at the passed in indexes (A < B)
func (t dialQueueImpl) Less(i, j int) bool {
	if t[i].priority == t[j].priority {
		return t[i].insertTime.Before(t[j].insertTime)
	}
	return t[i].priority < t[j].priority
}

// Swap swaps the places of the items at the passed-in indexes
func (t dialQueueImpl) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
	t[i].index = i
	t[j].index = j
}

// Push adds a new item to the queue
func (t *dialQueueImpl) Push(x interface{}) {
	n := len(*t)
	item := x.(*dialTask2)
	item.index = n
	*t = append(*t, item)
}

// Pop removes an item from the queue
func (t *dialQueueImpl) Pop() interface{} {
	old := *t
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*t = old[0 : n-1]

	return item
}
