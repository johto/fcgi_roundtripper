package fcgirt

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
)

type Dialer interface {
	Dial() (net.Conn, error)
}

type RoundTripper struct {
	http.RoundTripper

	dialer Dialer

	mu sync.Mutex
	numOpenConns int
	maxOpenConns int
	cond *sync.Cond
	freeConns []*FastCGIConn
}

func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	conn, err := rt.freeConn()
	if err != nil {
		return nil, err
	}

	// TODO
	defer rt.releaseConn(conn)

	params := []FastCGIParam{
		FastCGIParam{"QUERY_STRING", []byte(req.URL.RawQuery)},
		FastCGIParam{"REQUEST_METHOD", []byte(req.Method)},
		FastCGIParam{"REQUEST_URI", []byte(req.URL.Path)},
		FastCGIParam{"REMOTE_ADDR", []byte("127.0.0.1")},
		FastCGIParam{"SCRIPT_NAME", []byte("fastcgi")},
	}
	fcgires, err := conn.Do(req.Body, NewParamStream(params))
	if err != nil {
		return nil, err
	}
	return rt.parseResponse(req, fcgires)
}

func (rt *RoundTripper) parseResponse(req *http.Request, fr *FastCGIResponse) (*http.Response, error) {
	res := &http.Response{
		Status: "200 OK",
		StatusCode: 200,

		Proto: "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		ContentLength: -1,
		Close: false,
		Request: req,
	}

	bufr := bufio.NewReader(fr.Stdout)
	tp := textproto.NewReader(bufr)
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	status, ok := header["Status"]
	if ok {
		delete(header, "Status")
		if len(status) != 1 {
			return nil, fmt.Errorf("unexpected number of Status headers (%d)", len(status))
		}
		parts := strings.SplitN(status[0], " ", 2)
		if len(parts) < 2 {
			return nil, fmt.Errorf("malformed Status line")
		}
		res.StatusCode, err = strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("malformed Status line")
		}
		res.Status = status[0]
	}
	res.Header = http.Header(header)
	res.Body = ioutil.NopCloser(bufr)
	return res, nil
}

func (rt *RoundTripper) dial() (*FastCGIConn, error) {
	nc, err := rt.dialer.Dial()
	if err != nil {
		return nil, err
	}
	return NewFastCGIConn(nc), nil
}

func (rt *RoundTripper) freeConn() (*FastCGIConn, error) {
	rt.mu.Lock()
	for len(rt.freeConns) == 0 {
		if rt.maxOpenConns < 0 || rt.numOpenConns < rt.maxOpenConns {
			rt.numOpenConns++
			rt.mu.Unlock()
			conn, err := rt.dial()
			if err != nil {
				rt.mu.Lock()
				rt.numOpenConns--
				rt.cond.Signal()
				rt.mu.Unlock()
			}
			return conn, err
		}
		rt.cond.Wait()
	}
	conn := rt.freeConns[len(rt.freeConns)-1]
	rt.freeConns = rt.freeConns[:len(rt.freeConns)-1]
	rt.mu.Unlock()
	return conn, nil
}

func (rt *RoundTripper) releaseConn(c *FastCGIConn) {
	rt.mu.Lock()
	rt.freeConns = append(rt.freeConns, c)
	rt.cond.Signal()
	rt.mu.Unlock()
}

func (rt *RoundTripper) SetMaxOpenConns(maxOpenConns int) {
	rt.mu.Lock()
	rt.maxOpenConns = maxOpenConns
	rt.mu.Unlock()
}

func NewRoundTripper(dialer Dialer) *RoundTripper {
	rt := &RoundTripper{
		dialer: dialer,
		maxOpenConns: -1,
	}
	rt.cond = sync.NewCond(&rt.mu)
	return rt
}
