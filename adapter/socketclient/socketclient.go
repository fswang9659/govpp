// Copyright (c) 2019 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package socketclient

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lunixbochs/struc"
	logger "github.com/sirupsen/logrus"

	"git.fd.io/govpp.git/adapter"
	"git.fd.io/govpp.git/codec"
)

const (
	// DefaultSocketName is default VPP API socket file path.
	DefaultSocketName = adapter.DefaultBinapiSocket
	legacySocketName  = "/run/vpp-api.sock"
)

var (
	// DefaultConnectTimeout is default timeout for connecting
	DefaultConnectTimeout = time.Second * 3
	// DefaultDisconnectTimeout is default timeout for discconnecting
	DefaultDisconnectTimeout = time.Millisecond * 100
	// MaxWaitReady defines maximum duration before waiting for socket file
	// times out
	MaxWaitReady = time.Second * 10
	// ClientName is used for identifying client in socket registration
	ClientName = "govppsock"
)

var (
	// Debug is global variable that determines debug mode
	Debug = os.Getenv("DEBUG_GOVPP_SOCK") != ""
	// DebugMsgIds is global variable that determines debug mode for msg ids
	DebugMsgIds = os.Getenv("DEBUG_GOVPP_SOCKMSG") != ""

	// Log is global logger
	Log = logger.New()
)

// init initializes global logger, which logs debug level messages to stdout.
func init() {
	Log.Out = os.Stdout
	if Debug {
		Log.Level = logger.DebugLevel
		Log.Debug("govpp/socketclient: enabled debug mode")
	}
}

const socketMissing = `
------------------------------------------------------------
 No socket file found at: %s
 VPP binary API socket file is missing!

  - is VPP running with socket for binapi enabled?
  - is the correct socket name configured?

 To enable it add following section to your VPP config:
   socksvr {
     default
   }
------------------------------------------------------------
`

var warnOnce sync.Once

func (c *vppClient) printMissingSocketMsg() {
	fmt.Fprintf(os.Stderr, socketMissing, c.sockAddr)
}

type vppClient struct {
	sockAddr string

	conn   *net.UnixConn
	reader *bufio.Reader
	writer *bufio.Writer

	connectTimeout    time.Duration
	disconnectTimeout time.Duration

	cb           adapter.MsgCallback
	clientIndex  uint32
	msgTable     map[string]uint16
	sockDelMsgId uint16
	writeMu      sync.Mutex

	quit chan struct{}
	wg   sync.WaitGroup
}

func NewVppClient(sockAddr string) *vppClient {
	if sockAddr == "" {
		sockAddr = DefaultSocketName
	}
	return &vppClient{
		sockAddr:          sockAddr,
		connectTimeout:    DefaultConnectTimeout,
		disconnectTimeout: DefaultDisconnectTimeout,
		cb: func(msgID uint16, data []byte) {
			Log.Warnf("no callback set, dropping message: ID=%v len=%d", msgID, len(data))
		},
	}
}

// SetConnectTimeout sets timeout used during connecting.
func (c *vppClient) SetConnectTimeout(t time.Duration) {
	c.connectTimeout = t
}

// SetDisconnectTimeout sets timeout used during disconnecting.
func (c *vppClient) SetDisconnectTimeout(t time.Duration) {
	c.disconnectTimeout = t
}

func (c *vppClient) SetMsgCallback(cb adapter.MsgCallback) {
	Log.Debug("SetMsgCallback")
	c.cb = cb
}

func (c *vppClient) checkLegacySocket() bool {
	if c.sockAddr == legacySocketName {
		return false
	}
	Log.Debugf("checking legacy socket: %s", legacySocketName)
	// check if socket exists
	if _, err := os.Stat(c.sockAddr); err == nil {
		return false // socket exists
	} else if !os.IsNotExist(err) {
		return false // some other error occurred
	}
	// check if legacy socket exists
	if _, err := os.Stat(legacySocketName); err == nil {
		// legacy socket exists, update sockAddr
		c.sockAddr = legacySocketName
		return true
	}
	// no socket socket found
	return false
}

// WaitReady checks socket file existence and waits for it if necessary
func (c *vppClient) WaitReady() error {
	// check if socket already exists
	if _, err := os.Stat(c.sockAddr); err == nil {
		return nil // socket exists, we are ready
	} else if !os.IsNotExist(err) {
		return err // some other error occurred
	}

	if c.checkLegacySocket() {
		return nil
	}

	// socket does not exist, watch for it
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			Log.Warnf("failed to close file watcher: %v", err)
		}
	}()

	// start directory watcher
	if err := watcher.Add(filepath.Dir(c.sockAddr)); err != nil {
		return err
	}

	timeout := time.NewTimer(MaxWaitReady)
	for {
		select {
		case <-timeout.C:
			if c.checkLegacySocket() {
				return nil
			}
			return fmt.Errorf("timeout waiting (%s) for socket file: %s", MaxWaitReady, c.sockAddr)

		case e := <-watcher.Errors:
			return e

		case ev := <-watcher.Events:
			Log.Debugf("watcher event: %+v", ev)
			if ev.Name == c.sockAddr && (ev.Op&fsnotify.Create) == fsnotify.Create {
				// socket created, we are ready
				return nil
			}
		}
	}
}

func (c *vppClient) Connect() error {
	c.checkLegacySocket()

	// check if socket exists
	if _, err := os.Stat(c.sockAddr); os.IsNotExist(err) {
		warnOnce.Do(c.printMissingSocketMsg)
		return fmt.Errorf("VPP API socket file %s does not exist", c.sockAddr)
	} else if err != nil {
		return fmt.Errorf("VPP API socket error: %v", err)
	}

	if err := c.connect(c.sockAddr); err != nil {
		return err
	}

	if err := c.open(); err != nil {
		c.disconnect()
		return err
	}

	c.quit = make(chan struct{})
	c.wg.Add(1)
	go c.readerLoop()

	return nil
}

func (c *vppClient) Disconnect() error {
	if c.conn == nil {
		return nil
	}
	Log.Debugf("Disconnecting..")

	close(c.quit)

	if err := c.conn.CloseRead(); err != nil {
		Log.Debugf("closing read failed: %v", err)
	}

	// wait for readerLoop to return
	c.wg.Wait()

	if err := c.close(); err != nil {
		Log.Debugf("closing failed: %v", err)
	}

	if err := c.disconnect(); err != nil {
		return err
	}

	return nil
}

func (c *vppClient) connect(sockAddr string) error {
	addr := &net.UnixAddr{Name: sockAddr, Net: "unix"}

	Log.Debugf("Connecting to: %v", c.sockAddr)

	conn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		// we try different type of socket for backwards compatbility with VPP<=19.04
		if strings.Contains(err.Error(), "wrong type for socket") {
			addr.Net = "unixpacket"
			Log.Debugf("%s, retrying connect with type unixpacket", err)
			conn, err = net.DialUnix("unixpacket", nil, addr)
		}
		if err != nil {
			Log.Debugf("Connecting to socket %s failed: %s", addr, err)
			return err
		}
	}

	c.conn = conn
	Log.Debugf("Connected to socket (local addr: %v)", c.conn.LocalAddr().(*net.UnixAddr))

	c.reader = bufio.NewReader(c.conn)
	c.writer = bufio.NewWriter(c.conn)

	return nil
}

func (c *vppClient) disconnect() error {
	Log.Debugf("Closing socket")
	if err := c.conn.Close(); err != nil {
		Log.Debugln("Closing socket failed:", err)
		return err
	}
	return nil
}

const (
	sockCreateMsgId  = 15 // hard-coded sockclnt_create message ID
	createMsgContext = byte(123)
	deleteMsgContext = byte(124)
)

func (c *vppClient) open() error {
	msgCodec := new(codec.MsgCodec)

	req := &SockclntCreate{Name: ClientName}
	msg, err := msgCodec.EncodeMsg(req, sockCreateMsgId)
	if err != nil {
		Log.Debugln("Encode error:", err)
		return err
	}
	// set non-0 context
	msg[5] = createMsgContext

	if err := c.write(msg); err != nil {
		Log.Debugln("Write error: ", err)
		return err
	}

	readDeadline := time.Now().Add(c.connectTimeout)
	if err := c.conn.SetReadDeadline(readDeadline); err != nil {
		return err
	}
	msgReply, err := c.read()
	if err != nil {
		Log.Println("Read error:", err)
		return err
	}
	// reset read deadline
	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}

	reply := new(SockclntCreateReply)
	if err := msgCodec.DecodeMsg(msgReply, reply); err != nil {
		Log.Println("Decode error:", err)
		return err
	}

	Log.Debugf("SockclntCreateReply: Response=%v Index=%v Count=%v",
		reply.Response, reply.Index, reply.Count)

	c.clientIndex = reply.Index
	c.msgTable = make(map[string]uint16, reply.Count)
	for _, x := range reply.MessageTable {
		msgName := strings.Split(x.Name, "\x00")[0]
		name := strings.TrimSuffix(msgName, "\x13")
		c.msgTable[name] = x.Index
		if strings.HasPrefix(name, "sockclnt_delete_") {
			c.sockDelMsgId = x.Index
		}
		if DebugMsgIds {
			Log.Debugf(" - %4d: %q", x.Index, name)
		}
	}

	return nil
}

func (c *vppClient) close() error {
	msgCodec := new(codec.MsgCodec)

	req := &SockclntDelete{
		Index: c.clientIndex,
	}
	msg, err := msgCodec.EncodeMsg(req, c.sockDelMsgId)
	if err != nil {
		Log.Debugln("Encode error:", err)
		return err
	}
	// set non-0 context
	msg[5] = deleteMsgContext

	Log.Debugf("sending socklntDel (%d byes): % 0X", len(msg), msg)
	if err := c.write(msg); err != nil {
		Log.Debugln("Write error: ", err)
		return err
	}

	readDeadline := time.Now().Add(c.disconnectTimeout)
	if err := c.conn.SetReadDeadline(readDeadline); err != nil {
		return err
	}
	msgReply, err := c.read()
	if err != nil {
		Log.Debugln("Read error:", err)
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			// we accept timeout for reply
			return nil
		}
		return err
	}
	// reset read deadline
	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}

	reply := new(SockclntDeleteReply)
	if err := msgCodec.DecodeMsg(msgReply, reply); err != nil {
		Log.Debugln("Decode error:", err)
		return err
	}

	Log.Debugf("SockclntDeleteReply: Response=%v", reply.Response)

	return nil
}

func (c *vppClient) GetMsgID(msgName string, msgCrc string) (uint16, error) {
	msg := msgName + "_" + msgCrc
	msgID, ok := c.msgTable[msg]
	if !ok {
		return 0, &adapter.UnknownMsgError{msgName, msgCrc}
	}
	return msgID, nil
}

type reqHeader struct {
	// MsgID uint16
	ClientIndex uint32
	Context     uint32
}

func (c *vppClient) SendMsg(context uint32, data []byte) error {
	h := &reqHeader{
		ClientIndex: c.clientIndex,
		Context:     context,
	}
	buf := new(bytes.Buffer)
	if err := struc.Pack(buf, h); err != nil {
		return err
	}
	copy(data[2:], buf.Bytes())

	Log.Debugf("sendMsg (%d) context=%v client=%d: data: % 02X", len(data), context, c.clientIndex, data)

	if err := c.write(data); err != nil {
		Log.Debugln("write error: ", err)
		return err
	}

	return nil
}

func (c *vppClient) write(msg []byte) error {
	h := &msgheader{
		DataLen: uint32(len(msg)),
	}
	buf := new(bytes.Buffer)
	if err := struc.Pack(buf, h); err != nil {
		return err
	}
	header := buf.Bytes()

	// we lock to prevent mixing multiple message sends
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if n, err := c.writer.Write(header); err != nil {
		return err
	} else {
		Log.Debugf(" - header sent (%d/%d): % 0X", n, len(header), header)
	}

	writerSize := c.writer.Size()
	for i := 0; i <= len(msg)/writerSize; i++ {
		x := i*writerSize + writerSize
		if x > len(msg) {
			x = len(msg)
		}
		Log.Debugf(" - x=%v i=%v len=%v mod=%v", x, i, len(msg), len(msg)/writerSize)
		if n, err := c.writer.Write(msg[i*writerSize : x]); err != nil {
			return err
		} else {
			Log.Debugf(" - msg sent x=%d (%d/%d): % 0X", x, n, len(msg), msg)
		}
	}
	if err := c.writer.Flush(); err != nil {
		return err
	}

	Log.Debugf(" -- write done")

	return nil
}

type msgHeader struct {
	MsgID   uint16
	Context uint32
}

func (c *vppClient) readerLoop() {
	defer c.wg.Done()
	defer Log.Debugf("reader quit")

	for {
		select {
		case <-c.quit:
			return
		default:
		}

		msg, err := c.read()
		if err != nil {
			if isClosedError(err) {
				return
			}
			Log.Debugf("read failed: %v", err)
			continue
		}

		h := new(msgHeader)
		if err := struc.Unpack(bytes.NewReader(msg), h); err != nil {
			Log.Debugf("unpacking header failed: %v", err)
			continue
		}

		Log.Debugf("recvMsg (%d) msgID=%d context=%v", len(msg), h.MsgID, h.Context)
		c.cb(h.MsgID, msg)
	}
}

type msgheader struct {
	Q               int    `struc:"uint64"`
	DataLen         uint32 `struc:"uint32"`
	GcMarkTimestamp uint32 `struc:"uint32"`
}

func (c *vppClient) read() ([]byte, error) {
	Log.Debug(" reading next msg..")

	header := make([]byte, 16)

	n, err := io.ReadAtLeast(c.reader, header, 16)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		Log.Debugln("zero bytes header")
		return nil, nil
	} else if n != 16 {
		Log.Debugf("invalid header data (%d): % 0X", n, header[:n])
		return nil, fmt.Errorf("invalid header (expected 16 bytes, got %d)", n)
	}
	Log.Debugf(" read header %d bytes: % 0X", n, header)

	h := &msgheader{}
	if err := struc.Unpack(bytes.NewReader(header[:]), h); err != nil {
		return nil, err
	}
	Log.Debugf(" - decoded header: %+v", h)

	msgLen := int(h.DataLen)
	msg := make([]byte, msgLen)

	n, err = c.reader.Read(msg)
	if err != nil {
		return nil, err
	}
	Log.Debugf(" - read msg %d bytes (%d buffered) % 0X", n, c.reader.Buffered(), msg[:n])

	if msgLen > n {
		remain := msgLen - n
		Log.Debugf("continue read for another %d bytes", remain)
		view := msg[n:]

		for remain > 0 {
			nbytes, err := c.reader.Read(view)
			if err != nil {
				return nil, err
			} else if nbytes == 0 {
				return nil, fmt.Errorf("zero nbytes")
			}

			remain -= nbytes
			Log.Debugf("another data received: %d bytes (remain: %d)", nbytes, remain)

			view = view[nbytes:]
		}
	}

	Log.Debugf(" -- read done (buffered: %d)", c.reader.Buffered())

	return msg, nil
}

func isClosedError(err error) bool {
	if err == io.EOF {
		return true
	}
	return strings.HasSuffix(err.Error(), "use of closed network connection")
}
