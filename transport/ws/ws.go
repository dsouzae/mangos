// Copyright 2015 The Mangos Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ws implements an simple websocket transport for mangos.
// This transport is considered EXPERIMENTAL.
package ws

import (
	"golang.org/x/net/websocket"
	"net"
	"net/http"
	"net/url"

	"github.com/gdamore/mangos"
	"sync"
)

type options map[string]interface{}

// GetOption retrieves an option value.
func (o options) get(name string) (interface{}, error) {
	if o == nil {
		return nil, mangos.ErrBadOption
	}
	if v, ok := o[name]; !ok {
		return nil, mangos.ErrBadOption
	} else {
		return v, nil
	}
}

// SetOption sets an option.  We have none, so just ErrBadOption.
func (o options) set(name string, val interface{}) error {
	switch name {
	case mangos.OptionNoDelay:
	}
	return mangos.ErrBadOption
}

// wsPipe implements the Pipe interface on a websocket
type wsPipe struct {
	ws    *websocket.Conn
	rlock sync.Mutex
	wlock sync.Mutex
	proto mangos.Protocol
	addr  string
	open  bool
	wg    sync.WaitGroup
	props map[string]interface{}
}

type wsTran int

func (w *wsPipe) Recv() (*mangos.Message, error) {

	var buf []byte

	// prevent interleaved reads
	w.rlock.Lock()
	defer w.rlock.Unlock()

	if err := websocket.Message.Receive(w.ws, &buf); err != nil {
		return nil, err
	}
	msg := mangos.NewMessage(len(buf))

	// This is kind of suboptimal copying...
	msg.Body = append(msg.Body, buf...)
	return msg, nil
}

func (w *wsPipe) Send(m *mangos.Message) error {

	var buf []byte

	w.wlock.Lock()
	defer w.wlock.Unlock()

	if len(m.Header) > 0 {
		buf = make([]byte, 0, len(m.Header)+len(m.Body))
		buf = append(buf, m.Header...)
		buf = append(buf, m.Body...)
	} else {
		buf = m.Body
	}
	if err := websocket.Message.Send(w.ws, buf); err != nil {
		return err
	}
	m.Free()
	return nil
}

func (w *wsPipe) LocalProtocol() uint16 {
	return w.proto.Number()
}

func (w *wsPipe) RemoteProtocol() uint16 {
	return w.proto.PeerNumber()
}

func (w *wsPipe) Close() error {
	w.open = false
	w.ws.Close()
	w.wg.Done()
	return nil
}

func (w *wsPipe) IsOpen() bool {
	return w.open
}

func (w *wsPipe) GetProp(name string) (interface{}, error) {
	if v, ok := w.props[name]; ok {
		return v, nil
	}
	return nil, mangos.ErrBadProperty
}

type dialer struct {
	addr   string // url
	proto  mangos.Protocol
	origin string
	opts   options
}

func (d *dialer) Dial() (mangos.Pipe, error) {
	pname := d.proto.PeerName() + ".sp.nanomsg.org"
	// We have to supply an origin because Go's websocket
	// implementation seems to require it.  We fake a garbage one.
	// Perhaps we should allow applications to fake this out.
	d.origin = "x://"
	ws, err := websocket.Dial(d.addr, pname, d.origin)
	if err != nil {
		return nil, err
	}
	w := &wsPipe{ws: ws, proto: d.proto, addr: d.addr, open: true, props: make(map[string]interface{})}
	w.wg.Add(1)
	return w, nil
}

func (d *dialer) SetOption(n string, v interface{}) error {
	return d.opts.set(n, v)
}

func (d *dialer) GetOption(n string) (interface{}, error) {
	return d.opts.get(n)
}

type listener struct {
	pending  []*wsPipe
	lock     sync.Mutex
	cv       sync.Cond
	running  bool
	addr     string
	wssvr    websocket.Server
	htsvr    *http.Server
	url_     *url.URL
	listener *net.TCPListener
	proto    mangos.Protocol
	opts     options
}

func (l *listener) SetOption(n string, v interface{}) error {
	return l.opts.set(n, v)
}

func (l *listener) GetOption(n string) (interface{}, error) {
	return l.opts.get(n)
}

func (l *listener) Listen() error {
	var taddr *net.TCPAddr
	var err error

	// We listen separately, that way we can catch and deal with the
	// case of a port already in use.  This also lets us configure
	// properties of the underlying TCP connection.

	if taddr, err = net.ResolveTCPAddr("tcp", l.url_.Host); err != nil {
		return err
	}

	if l.listener, err = net.ListenTCP("tcp", taddr); err != nil {
		return err
	}
	l.pending = nil
	l.running = true

	l.wssvr.Handler = l.handler
	l.wssvr.Handshake = l.handshake
	l.htsvr = &http.Server{Addr: l.url_.Host, Handler: l.wssvr}

	go l.htsvr.Serve(l.listener)

	return nil
}

func (l *listener) Accept() (mangos.Pipe, error) {
	var w *wsPipe

	l.lock.Lock()
	defer l.lock.Unlock()

	for {
		if !l.running {
			return nil, mangos.ErrClosed
		}
		if len(l.pending) == 0 {
			l.cv.Wait()
			continue
		}
		w = l.pending[len(l.pending)-1]
		l.pending = l.pending[:len(l.pending)-1]
		break
	}

	return w, nil
}

func (l *listener) handler(ws *websocket.Conn) {
	l.lock.Lock()

	if !l.running {
		ws.Close()
		l.lock.Unlock()
		return
	}

	w := &wsPipe{ws: ws, addr: l.addr, proto: l.proto, open: true, props: make(map[string]interface{})}

	w.wg.Add(1)
	l.pending = append(l.pending, w)
	l.cv.Broadcast()
	l.lock.Unlock()

	// We must not return before the socket is closed, because
	// our caller will close the websocket on our return.
	w.wg.Wait()
}

func (l *listener) handshake(c *websocket.Config, _ *http.Request) error {
	pname := l.proto.Name() + ".sp.nanomsg.org"
	for _, p := range c.Protocol {
		if p == pname {
			c.Protocol = append([]string{}, p)
			return nil
		}
	}
	return websocket.ErrBadWebSocketProtocol
}

func (l *listener) Close() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if !l.running {
		return mangos.ErrClosed
	}
	l.listener.Close()
	l.running = false
	l.cv.Broadcast()
	for _, ws := range l.pending {
		ws.Close()
	}
	l.pending = nil
	return nil
}

func (l *listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.wssvr.ServeHTTP(w, r)
}

func (wsTran) Scheme() string {
	return "ws"
}

func (wsTran) NewDialer(addr string, proto mangos.Protocol) (mangos.PipeDialer, error) {
	return &dialer{addr: addr, proto: proto, opts: make(map[string]interface{})}, nil
}

func (wsTran) NewListener(addr string, proto mangos.Protocol) (mangos.PipeListener, error) {
	var err error
	l := &listener{proto: proto, opts: make(map[string]interface{})}
	l.cv.L = &l.lock

	l.url_, err = url.ParseRequestURI(addr)
	if err != nil {
		return nil, err
	}

	return l, nil
}

func (wsTran) SetOption(string, interface{}) error {
	return mangos.ErrBadOption
}

func (wsTran) GetOption(string) (interface{}, error) {
	return nil, mangos.ErrBadOption
}

// NewTransport allocates a new inproc:// transport.
func NewTransport() mangos.Transport {
	return wsTran(0)
}
