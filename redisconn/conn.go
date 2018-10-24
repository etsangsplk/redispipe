package redisconn

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joomcode/redispipe/redis"
)

const (
	DoAsking      = 1
	DoTransaction = 2

	connDisconnected = 0
	connConnecting   = 1
	connConnected    = 2
	connClosed       = 3

	defaultIOTimeout  = 1 * time.Second
	defaultWritePause = 10 * time.Microsecond
)

type Opts struct {
	// DB - database number
	DB int
	// Password for AUTH
	Password string
	// IOTimeout - timeout on read/write to socket.
	// If IOTimeout == 0, then it is set to 1 second
	// If IOTimeout < 0, then timeout is disabled
	IOTimeout time.Duration
	// DialTimeout is timeout for net.Dialer
	// If it is <= 0 or >= IOTimeout, then IOTimeout
	// If IOTimeout is disabled, then 5 seconds used (but without affect on ReconnectPause)
	DialTimeout time.Duration
	// ReconnectPause is a pause after failed connection attempt before next one.
	// If ReconnectPause < 0, then no reconnection will be performed.
	// If ReconnectPause == 0, then DialTimeout * 2 is used
	ReconnectPause time.Duration
	// TCPKeepAlive - KeepAlive parameter for net.Dialer
	// default is IOTimeout / 3
	TCPKeepAlive time.Duration
	// Handle is returned with Connection.Handle()
	Handle interface{}
	// Concurrency - number for shards. Default is runtime.GOMAXPROCS(-1)*4
	Concurrency uint32
	// WritePause - write loop pauses for this time to collect more requests.
	// Default is 10microseconds. Set < 0 to disable.
	// It is not wise to set it larger than 100 microseconds.
	WritePause time.Duration
	// Logger
	Logger Logger
	// Async - do not establish connection immediately
	Async bool
}

// Connection represents single connection to single redis instance.
// Underlying net.Conn is re-established as necessary.
type Connection struct {
	ctx      context.Context
	cancel   context.CancelFunc
	state    uint32
	closeErr error

	addr  string
	c     net.Conn
	mutex sync.Mutex

	shardid    uint32
	shard      []connShard
	dirtyShard chan uint32

	firstConn chan struct{}
	opts      Opts
}

type oneconn struct {
	c       net.Conn
	futures chan []future
	control chan struct{}
	err     error
	erronce sync.Once
	futpool chan []future
}

type connShard struct {
	sync.Mutex
	futures []future
	_pad    [16]uint64
}

func Connect(ctx context.Context, addr string, opts Opts) (conn *Connection, err error) {
	if ctx == nil {
		return nil, redis.NewErr(redis.ErrKindOpts, redis.ErrContextIsNil)
	}
	if addr == "" {
		return nil, redis.NewErr(redis.ErrKindOpts, redis.ErrNoAddressProvided)
	}
	conn = &Connection{
		addr: addr,
		opts: opts,
	}
	conn.ctx, conn.cancel = context.WithCancel(ctx)

	maxprocs := uint32(runtime.GOMAXPROCS(-1))
	if opts.Concurrency == 0 || opts.Concurrency > maxprocs*128 {
		conn.opts.Concurrency = maxprocs
	}

	conn.shard = make([]connShard, conn.opts.Concurrency)
	conn.dirtyShard = make(chan uint32, conn.opts.Concurrency*2)

	if conn.opts.IOTimeout == 0 {
		conn.opts.IOTimeout = defaultIOTimeout
	} else if conn.opts.IOTimeout < 0 {
		conn.opts.IOTimeout = 0
	}

	if conn.opts.DialTimeout <= 0 || conn.opts.DialTimeout > conn.opts.IOTimeout {
		conn.opts.DialTimeout = conn.opts.IOTimeout
	}

	if conn.opts.ReconnectPause == 0 {
		conn.opts.ReconnectPause = conn.opts.DialTimeout * 2
	}

	if conn.opts.TCPKeepAlive == 0 {
		conn.opts.TCPKeepAlive = conn.opts.IOTimeout / 3
	}
	if conn.opts.TCPKeepAlive < 0 {
		conn.opts.TCPKeepAlive = 0
	}

	if conn.opts.WritePause == 0 {
		conn.opts.WritePause = defaultWritePause
	}

	if conn.opts.Logger == nil {
		conn.opts.Logger = defaultLogger{}
	}

	if !conn.opts.Async {
		if err = conn.createConnection(false, nil); err != nil {
			if opts.ReconnectPause < 0 {
				return nil, err
			}
			if cer, ok := err.(*redis.Error); ok && cer.Code == redis.ErrAuth {
				return nil, err
			}
		}
	}

	if conn.opts.Async || err != nil {
		var ch chan struct{}
		if conn.opts.Async {
			ch = make(chan struct{})
		}
		go func() {
			conn.mutex.Lock()
			defer conn.mutex.Unlock()
			conn.createConnection(true, ch)
		}()
		// in async mode we are still waiting for state to set to connConnecting
		// so that Send will put requests into queue
		if conn.opts.Async {
			<-ch
		}
	}

	go conn.control()

	return conn, nil
}

// Ctx returns context of this connection
func (conn *Connection) Ctx() context.Context {
	return conn.ctx
}

// ConnectedNow answers if connection is certainly connected at the moment
func (conn *Connection) ConnectedNow() bool {
	return atomic.LoadUint32(&conn.state) == connConnected
}

// MayBeConnected answers if connection either connected or connecting at the moment.
// Ie it returns false if connection is disconnected at the moment, and reconnection is not started yet.
func (conn *Connection) MayBeConnected() bool {
	s := atomic.LoadUint32(&conn.state)
	return s == connConnected || s == connConnecting
}

// Close closes connection forever
func (conn *Connection) Close() {
	conn.cancel()
}

// RemoteAddr is address of Redis socket
// Attention: do not call this method from Logger.Report, because it could lead to deadlock!
func (conn *Connection) RemoteAddr() string {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	if conn.c == nil {
		return ""
	}
	return conn.c.RemoteAddr().String()
}

// LocalAddr is outgoing socket addr
// Attention: do not call this method from Logger.Report, because it could lead to deadlock!
func (conn *Connection) LocalAddr() string {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	if conn.c == nil {
		return ""
	}
	return conn.c.LocalAddr().String()
}

// Addr retuns configurred address
func (conn *Connection) Addr() string {
	return conn.addr
}

// Handle returns user specified handle from Opts
func (conn *Connection) Handle() interface{} {
	return conn.opts.Handle
}

// Ping sends ping request synchronously
func (conn *Connection) Ping() error {
	res := redis.Sync{conn}.Do("PING")
	if err := redis.AsError(res); err != nil {
		return err
	}
	if str, ok := res.(string); !ok || str != "PONG" {
		return conn.err(redis.ErrKindResponse, redis.ErrPing).With("response", res)
	}
	return nil
}

// choose next "shard" to send query to
func (conn *Connection) getShard() (uint32, *connShard) {
	shardn := atomic.AddUint32(&conn.shardid, 1) % conn.opts.Concurrency
	return shardn, &conn.shard[shardn]
}

// dumb redis.Future implementation
type dumbcb struct{}

func (d dumbcb) Cancelled() bool             { return false }
func (d dumbcb) Resolve(interface{}, uint64) {}

var dumb dumbcb

// Send implements redis.Sender.Send
// It sends request asynchronously. At some moment in a future it will call cb.Resolve(result, n)
// But if cb is cancelled, then cb.Resolve will be called immediately.
func (conn *Connection) Send(req Request, cb Future, n uint64) {
	conn.SendAsk(req, cb, n, false)
}

// SendAsk is a helper method for redis-cluster client implementation.
// It will send request with ASKING request sent before.
func (conn *Connection) SendAsk(req Request, cb Future, n uint64, asking bool) {
	if cb == nil {
		cb = dumb
	}
	if err := conn.doSend(req, cb, n, asking); err != nil {
		cb.Resolve(err.With("connection", conn), n)
	}
}

func (conn *Connection) doSend(req Request, cb Future, n uint64, asking bool) *redis.Error {
	if cb.Cancelled() {
		return conn.err(redis.ErrKindRequest, redis.ErrRequestCancelled)
	}

	// Since we do not pack request here, we need to be sure it could be packed
	if err := redis.CheckArgs(req); err != nil {
		return err
	}

	shardn, shard := conn.getShard()
	shard.Lock()
	defer shard.Unlock()

	// we need to check conn.state first
	// since we do not lock connection itself, we need to use atomics.
	// Note: we do not check for connConnecting, ie we will try to send request after connection established.
	switch atomic.LoadUint32(&conn.state) {
	case connClosed:
		return redis.NewErrWrap(redis.ErrKindContext, redis.ErrContextClosed, conn.ctx.Err())
	case connDisconnected:
		return redis.NewErr(redis.ErrKindConnection, redis.ErrNotConnected)
	}
	futures := shard.futures
	if asking {
		// send ASKING request before actual
		futures = append(futures, future{dumb, 0, 0, Request{"ASKING", nil}})
	}
	futures = append(futures, future{cb, n, nownano(), req})

	// should notify writer about this shard having queries.
	// Since we are under shard lock, it is safe to send notification before assigning futures.
	if len(shard.futures) == 0 {
		conn.dirtyShard <- shardn
	}
	shard.futures = futures
	return nil
}

// SendMany implements redis.Sender.SendMany
// Sends several requests asynchronously. Fills with cb.Resolve(res, n), cb.Resolve(res, n+1), ... etc.
// Note: it could resolve requests in arbitrary order.
func (conn *Connection) SendMany(requests []Request, cb Future, start uint64) {
	// split requests by chunks of 16 to not block shards for a long time.
	// Also it could help a bit to save pipeline with writer loop.
	for i := 0; i < len(requests); i += 16 {
		j := i + 16
		if j > len(requests) {
			j = len(requests)
		}
		conn.SendBatch(requests[i:j], cb, start+uint64(i))
	}
}

// SendBatch sends several requests in preserved order.
// They will be serialized to network in the order passed.
func (conn *Connection) SendBatch(requests []Request, cb Future, start uint64) {
	conn.SendBatchFlags(requests, cb, start, 0)
}

// SendBatchFlags sends several requests in preserved order with addition ASKING, MULTI+EXEC commands.
// If flag&DoAsking != 0 , then "ASKING" command is prepended.
// If flag&DoTransaction != 0, then "MULTI" command is prepended, and "EXEC" command appended.
// Note: cb.Resolve will be also called with start+len(requests) index with result of EXEC command.
// It is mostly helper method for SendTransaction for single connect and cluster implementations.
//
// Note: since it is used for transaction, single wrong argument in single request
// will result in error for all commands in a batch.
func (conn *Connection) SendBatchFlags(requests []Request, cb Future, start uint64, flags int) {
	var err *redis.Error
	var commonerr *redis.Error
	errpos := -1
	// check arguments of all commands. If single request is malformed, then all requests will be aborted.
	for i, req := range requests {
		if err = redis.CheckArgs(req); err != nil {
			err = err.With("connection", conn).With("request", requests[i])
			commonerr = conn.err(redis.ErrKindRequest, redis.ErrBatchFormat).
				Wrap(err).
				With("requests", requests).
				With("request", requests[i])
			errpos = i
			break
		}
	}
	if commonerr == nil {
		commonerr = conn.doSendBatch(requests, cb, start, flags)
	}
	if commonerr != nil {
		for i := 0; i < len(requests); i++ {
			if i != errpos {
				cb.Resolve(commonerr, start+uint64(i))
			} else {
				cb.Resolve(err, start+uint64(i))
			}
		}
		if flags&DoTransaction != 0 {
			// resolve EXEC request as well
			cb.Resolve(commonerr, start+uint64(len(requests)))
		}
	}
}

func (conn *Connection) doSendBatch(requests []Request, cb Future, start uint64, flags int) *redis.Error {
	if len(requests) == 0 {
		if flags&DoTransaction != 0 {
			cb.Resolve([]interface{}{}, start)
		}
		return nil
	}

	if cb.Cancelled() {
		return conn.err(redis.ErrKindRequest, redis.ErrRequestCancelled)
	}

	shardn, shard := conn.getShard()
	shard.Lock()
	defer shard.Unlock()

	// we need to check conn.state first
	// since we do not lock connection itself, we need to use atomics.
	// Note: we do not check for connConnecting, ie we will try to send request after connection established.
	switch atomic.LoadUint32(&conn.state) {
	case connClosed:
		return conn.err(redis.ErrKindContext, redis.ErrContextClosed).Wrap(conn.ctx.Err())
	case connDisconnected:
		return conn.err(redis.ErrKindConnection, redis.ErrNotConnected)
	}

	futures := shard.futures
	if flags&DoAsking != 0 {
		// send ASKING request before actual
		futures = append(futures, future{dumb, 0, 0, Request{"ASKING", nil}})
	}
	if flags&DoTransaction != 0 {
		// send MULTI request for transaction start
		futures = append(futures, future{dumb, 0, 0, Request{"MULTI", nil}})
	}

	now := nownano()

	for i, req := range requests {
		futures = append(futures, future{cb, start + uint64(i), now, req})
	}

	if flags&DoTransaction != 0 {
		// send EXEC request for transaction end
		futures = append(futures, future{cb, start + uint64(len(requests)), now, Request{"EXEC", nil}})
	}

	// should notify writer about this shard having queries
	// Since we are under shard lock, it is safe to send notification before assigning futures.
	if len(shard.futures) == 0 {
		conn.dirtyShard <- shardn
	}
	shard.futures = futures
	return nil
}

// wrapped preserves Cancelled method of wrapped future, but redefines Resolve
type transactionFuture struct {
	Future
	l   int
	off uint64
}

func (cw transactionFuture) Resolve(res interface{}, n uint64) {
	if n == uint64(cw.l) {
		cw.Future.Resolve(res, cw.off)
	}
}

// SendTransaction implements redis.Sender.SendTransaction
func (conn *Connection) SendTransaction(reqs []Request, cb Future, off uint64) {
	if cb.Cancelled() {
		cb.Resolve(conn.err(redis.ErrKindRequest, redis.ErrRequestCancelled), off)
		return
	}
	conn.SendBatchFlags(reqs, transactionFuture{cb, len(reqs), off}, 0, DoTransaction)
}

// String implements fmt.Stringer
func (conn *Connection) String() string {
	return fmt.Sprintf("*redisconn.Connection{addr: %s}", conn.addr)
}

/********** private api **************/

// lock all shards to prevent creation of new requests.
// Under this lock, all already sent requiests are revoked.
func (conn *Connection) lockShards() {
	for i := range conn.shard {
		conn.shard[i].Lock()
	}
}

func (conn *Connection) unlockShards() {
	for i := range conn.shard {
		conn.shard[i].Unlock()
	}
}

func (conn *Connection) dial() error {
	var connection net.Conn
	var err error
	network := "tcp"
	address := conn.addr
	timeout := conn.opts.DialTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	if address[0] == '.' || address[0] == '/' {
		network = "unix"
	} else if address[0:7] == "unix://" {
		network = "unix"
		address = address[7:]
	} else if address[0:6] == "tcp://" {
		network = "tcp"
		address = address[6:]
	}
	dialer := net.Dialer{
		Timeout:       timeout,
		DualStack:     true,
		FallbackDelay: timeout / 2,
		KeepAlive:     conn.opts.TCPKeepAlive,
	}
	connection, err = dialer.DialContext(conn.ctx, network, address)
	if err != nil {
		return redis.NewErrWrap(redis.ErrKindConnection, redis.ErrDial, err)
	}
	dc := newDeadlineIO(connection, conn.opts.IOTimeout)
	r := bufio.NewReaderSize(dc, 128*1024)

	var req []byte
	if conn.opts.Password != "" {
		req = append(req, authReq...)
	}
	req = append(req, pingReq...)
	if conn.opts.DB != 0 {
		req, _ = redis.AppendRequest(req, Request{"SELECT", []interface{}{conn.opts.DB}})
	}
	if conn.opts.IOTimeout > 0 {
		connection.SetWriteDeadline(time.Now().Add(conn.opts.IOTimeout))
	}
	if _, err = dc.Write(req); err != nil {
		connection.Close()
		return redis.NewErrWrap(redis.ErrKindConnection, redis.ErrConnSetup, err)
	}
	connection.SetWriteDeadline(time.Time{})
	var res interface{}
	// Password response
	if conn.opts.Password != "" {
		res = redis.ReadResponse(r)
		if err := redis.AsRedisError(res); err != nil {
			connection.Close()
			if strings.Contains(err.Error(), "password") {
				return conn.err(redis.ErrKindConnection, redis.ErrAuth).Wrap(err)
			}
			return conn.err(redis.ErrKindConnection, redis.ErrConnSetup).Wrap(err)
		}
	}
	// PING Response
	res = redis.ReadResponse(r)
	if err = redis.AsError(res); err != nil {
		connection.Close()
		return redis.NewErrWrap(redis.ErrKindConnection, redis.ErrConnSetup, err)
	}
	if str, ok := res.(string); !ok || str != "PONG" {
		connection.Close()
		return conn.err(redis.ErrKindConnection, redis.ErrConnSetup).
			WithMsg("ping response mismatch").
			With("response", res)
	}
	// SELECT DB Response
	if conn.opts.DB != 0 {
		res = redis.ReadResponse(r)
		if err = redis.AsError(res); err != nil {
			connection.Close()
			return conn.err(redis.ErrKindConnection, redis.ErrConnSetup).Wrap(err)
		}
		if str, ok := res.(string); !ok || str != "OK" {
			connection.Close()
			return conn.err(redis.ErrKindConnection, redis.ErrConnSetup).
				WithMsg("SELECT db response mismatch").
				With("db", conn.opts.DB).With("response", res)
		}
	}

	conn.lockShards()
	conn.c = connection
	conn.unlockShards()

	one := &oneconn{
		c:       connection,
		futures: make(chan []future, conn.opts.Concurrency/2+1),
		control: make(chan struct{}),
		futpool: make(chan []future, conn.opts.Concurrency/2+1),
	}

	go conn.writer(one)
	go conn.reader(r, one)

	return nil
}

func (conn *Connection) createConnection(reconnect bool, ch chan struct{}) error {
	var err error
	for conn.c == nil && atomic.LoadUint32(&conn.state) == connDisconnected {
		conn.report(LogConnecting)
		now := time.Now()
		// start accepting requests
		atomic.StoreUint32(&conn.state, connConnecting)
		if ch != nil {
			close(ch)
			ch = nil
		}
		err = conn.dial()
		if err == nil {
			atomic.StoreUint32(&conn.state, connConnected)
			conn.report(LogConnected,
				conn.c.LocalAddr().String(),
				conn.c.RemoteAddr().String())
			return nil
		}

		conn.report(LogConnectFailed, err)
		atomic.StoreUint32(&conn.state, connDisconnected)
		conn.lockShards()
		conn.dropShardFutures(err)
		conn.unlockShards()

		if !reconnect {
			return err
		}
		conn.mutex.Unlock()
		time.Sleep(now.Add(conn.opts.ReconnectPause).Sub(time.Now()))
		conn.mutex.Lock()
	}
	if ch != nil {
		close(ch)
	}
	if atomic.LoadUint32(&conn.state) == connClosed {
		err = conn.ctx.Err()
	}
	return err
}

func (conn *Connection) dropShardFutures(err error) {
Loop:
	for {
		select {
		case _, ok := <-conn.dirtyShard:
			if !ok {
				break Loop
			}
		default:
			break Loop
		}
	}
	for i := range conn.shard {
		sh := &conn.shard[i]
		for _, fut := range sh.futures {
			conn.call(fut, err)
		}
		sh.futures = nil
	}
}

func (conn *Connection) closeConnection(neterr error, forever bool) error {
	if forever {
		atomic.StoreUint32(&conn.state, connClosed)
		conn.report(LogContextClosed)
	} else {
		atomic.StoreUint32(&conn.state, connDisconnected)
		conn.report(LogDisconnected, neterr)
	}

	var err error

	conn.lockShards()
	defer conn.unlockShards()
	if forever {
		close(conn.dirtyShard)
	}

	if conn.c != nil {
		err = conn.c.Close()
		conn.c = nil
	}

	conn.dropShardFutures(neterr)
	return err
}

func (conn *Connection) control() {
	timeout := conn.opts.IOTimeout / 3
	if timeout <= 0 {
		timeout = time.Second
	}
	t := time.NewTicker(timeout)
	defer t.Stop()
	for {
		select {
		case <-conn.ctx.Done():
			conn.mutex.Lock()
			defer conn.mutex.Unlock()
			conn.closeErr = conn.err(redis.ErrKindContext, redis.ErrContextClosed).
				Wrap(conn.ctx.Err())
			conn.closeConnection(conn.closeErr, true)
			return
		case <-t.C:
		}
		if err := conn.Ping(); err != nil {
			if cer, ok := err.(*redis.Error); ok && cer.Code == redis.ErrPing {
				// that states about serious error in our code
				panic(err)
			}
		}
	}
}

func (one *oneconn) setErr(neterr error, conn *Connection) {
	one.erronce.Do(func() {
		close(one.control)
		rerr, ok := neterr.(*redis.Error)
		if !ok {
			rerr = redis.NewErrWrap(redis.ErrKindIO, redis.ErrIO, neterr)
		}
		one.err = rerr.With("connection", conn)
		go conn.reconnect(one.err, one.c)
	})
}

func (conn *Connection) reconnect(neterr error, c net.Conn) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	if atomic.LoadUint32(&conn.state) == connClosed {
		return
	}
	if conn.opts.ReconnectPause < 0 {
		conn.Close()
		return
	}
	if conn.c == c {
		conn.closeConnection(neterr, false)
		conn.createConnection(true, nil)
	}
}

func (conn *Connection) writer(one *oneconn) {
	var shardn uint32
	var packet []byte
	var futures []future
	var ok bool

	defer func() {
		if len(futures) != 0 {
			one.futures <- futures
		}
		close(one.futures)
	}()

	round := 1023
	write := func() bool {
		if _, err := one.c.Write(packet); err != nil {
			one.setErr(err, conn)
			return false
		}
		if round--; round == 0 {
			round = 1023
			if cap(packet) > 128*1024 {
				packet = nil
			}
		}
		packet = packet[:0]
		return true
	}

BigLoop:
	select {
	case shardn, ok = <-conn.dirtyShard:
		if !ok {
			return
		}
	case <-one.control:
		return
	}

	if conn.opts.WritePause > 0 {
		time.Sleep(conn.opts.WritePause)
	}

	for {
		shard := &conn.shard[shardn]
		shard.Lock()
		futures, shard.futures = shard.futures, futures
		shard.Unlock()

		i := 0
		for j, fut := range futures {
			var err error
			if packet, err = redis.AppendRequest(packet, fut.req); err != nil {
				conn.call(fut, err)
				continue
			}
			if i != j {
				futures[i] = fut
			}
			i++
		}
		futures = futures[:i]

		if len(futures) == 0 {
			goto control
		}

		select {
		case one.futures <- futures:
			if len(packet) > 64*1024 && !write() {
				return
			}
		default:
			if !write() {
				return
			}
			one.futures <- futures
		}

		select {
		case futures = <-one.futpool:
		default:
			futures = make([]future, 0, len(futures)*2)
		}

	control:
		select {
		case <-one.control:
			return
		default:
		}

		select {
		case shardn, ok = <-conn.dirtyShard:
			if !ok {
				return
			}
		default:
			if len(packet) != 0 && !write() {
				return
			}
			goto BigLoop
		}
	}
}

func (conn *Connection) reader(r *bufio.Reader, one *oneconn) {
	var futures []future
	var packetfutures []future
	var res interface{}
	var ok bool

	for {
		res = redis.ReadResponse(r)
		if rerr := redis.AsRedisError(res); rerr != nil {
			if redis.HardError(rerr) {
				one.setErr(rerr, conn)
				break
			} else {
				res = rerr.With("connection", conn)
			}
		}
		if len(futures) == 0 {
			select {
			case one.futpool <- packetfutures[:0]:
			default:
			}
			packetfutures, ok = <-one.futures
			if !ok {
				break
			}
			futures = packetfutures
		}
		fut := futures[0]
		futures[0].Future = nil
		futures = futures[1:]
		conn.call(fut, res)
	}

	for _, fut := range futures {
		conn.call(fut, one.err)
	}
	for futures := range one.futures {
		for _, fut := range futures {
			conn.call(fut, one.err)
		}
	}
}

func (conn *Connection) err(kind redis.ErrorKind, code redis.ErrorCode) *redis.Error {
	return redis.NewErr(kind, code).With("connection", conn)
}
