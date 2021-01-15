// example server program to rotate a cell of a ZIMA Slim vending machine
// you may need more tests if you want to use this in production
// author: caiguanhao@gmail.com
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"net/rpc/jsonrpc"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	// basic function code of ZIMA vending machine:
	FUNC_ROTATE = 0x05
	// other unused code:
	// FUNC_STATUS = 0x04
	// FUNC_CHECK  = 0x07
	// FUNC_LOOKUP = 0x08
	// FUNC_UNLOCK = 0x09

	KEY_ROTATE = "rotate"
)

var (
	// linble format
	DefaultRegisterRegexp = regexp.MustCompile(`^Client\s*"(.+)\[[0-9+]\]"`)

	// default heartbeat format: ###%device_mac&%wan_ipaddr
	// heartbeat query format: ###csq=%csq_str&mac=%device_mac&ip=%wan_ipaddr
	DefaultHeartbeatPrefix = []byte{'#', '#', '#'}

	ErrTimeout      = errors.New("timeout")
	ErrProcessing   = errors.New("already processing")
	ErrNoContent    = errors.New("no content")
	ErrNoSuchClient = errors.New("no such client")
)

type (
	// Server holds all clients
	Server struct {
		clients sync.Map

		jsonrpcListen, comServerListen string
	}

	// Client holds TCP connection
	Client struct {
		conn     *net.TCPConn
		channels sync.Map

		Id          string    `json:"id"`
		ConnectedAt time.Time `json:"connected_at"`

		CSQ        int    `json:"csq"`
		MacAddress string `json:"mac_address"`
		IpAddress  string `json:"ip_address"`
	}
)

func main() {
	jsonrpc := flag.String("jsonrpc", "127.0.0.1:12345", "jsonrpc server address")
	com := flag.String("com", "0.0.0.0:33333", "com server address")
	flag.Parse()
	server := &Server{
		jsonrpcListen:   *jsonrpc,
		comServerListen: *com,
	}
	go server.startJSONRPCServer()
	server.startComServer()
}

// List shows all connected clients
func (s *Server) List(_ *int, clients *[]*Client) error {
	s.clients.Range(func(_, value interface{}) bool {
		*clients = append(*clients, value.(*Client))
		return true
	})
	return nil
}

// Rotate rotates a cell at position ("row", "column") for client with "client_id" in "timeout" milliseconds (ms), defaults to 10000 ms.
func (s *Server) Rotate(args *struct {
	ClientId string `json:"client_id"`
	Row      int    `json:"row"`
	Column   int    `json:"column"`
	Timeout  int    `json:"timeout"`
}, ret *struct {
	Return   string `json:"return"`
	Frame    int    `json:"frame"`
	Row      int    `json:"row"`
	Column   int    `json:"column"`
	Duration int    `json:"duration"`
	Success  bool   `json:"success"`
}) error {
	bytes, frame := bytesForData(FUNC_ROTATE, []byte{byte(args.Row), byte(args.Column)})
	key := fmt.Sprintf("%s-%d-%d-%d", KEY_ROTATE, int(frame), args.Row, args.Column)
	output, err := s.write(args.ClientId, bytes, key, args.Timeout)
	if err != nil {
		return err
	}
	ret.Return = fmt.Sprintf("% X", output)
	ret.Frame = int(output[3])
	ret.Row = int(output[4])
	ret.Column = int(output[5])
	ret.Duration = int(output[6])
	ret.Success = output[7] == 1
	return nil
}

// write sends data to client, wait for data from channel and return
// timed-out error will be returned if no replies in timeout milliseconds
func (s *Server) write(clientId string, data []byte, channelKey string, timeout int) (output []byte, err error) {
	if len(data) == 0 {
		err = ErrNoContent
		return
	}
	_client, ok := s.clients.Load(clientId)
	if !ok {
		err = ErrNoSuchClient
		return
	}
	client, ok := _client.(*Client)
	if !ok {
		err = ErrNoSuchClient
		return
	}
	channel, hasChannel := client.channels.LoadOrStore(channelKey, make(chan []byte))
	if hasChannel {
		err = ErrProcessing
		return
	} else {
		defer client.channels.Delete(channelKey)
	}
	var n int
	n, err = client.conn.Write(data)
	log.Printf("%s %d bytes written: % X", clientId, n, data)
	if err != nil {
		log.Println("error writting", data, err)
	}
	if timeout == 0 {
		timeout = 10000
	} else if timeout < 1000 {
		timeout = 1000
	}
	timeoutChan := time.After(time.Duration(timeout) * time.Millisecond)
	for {
		// return either timeout error or channel data from Client.handle()
		select {
		case output = <-channel.(chan []byte):
			return
		case <-timeoutChan:
			err = ErrTimeout
			return
		}
	}
}

// read write closer for jsonrpc
type rwc struct {
	reader io.Reader
	writer http.ResponseWriter
}

func (r *rwc) Read(p []byte) (int, error)  { return r.reader.Read(p) }
func (r *rwc) Write(p []byte) (int, error) { return r.writer.Write(p) }
func (r *rwc) Close() error                { return nil }

// start a jsonrpc over http server
func (s *Server) startJSONRPCServer() {
	err := rpc.RegisterName("Server", s)
	if err != nil {
		panic(err)
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if r.Body == nil {
			http.NotFound(w, r)
			return
		}
		defer r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		rpc.ServeRequest(jsonrpc.NewServerCodec(&rwc{r.Body, w}))
		return
	})
	log.Println("listening", "http://"+s.jsonrpcListen)
	panic(http.ListenAndServe(s.jsonrpcListen, nil))
}

// start a server to handle data from router
func (s *Server) startComServer() {
	addr, err := net.ResolveTCPAddr("tcp", s.comServerListen)
	if err != nil {
		panic(err)
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	log.Println("listening", listener.Addr().String())

	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			log.Println(err)
			continue
		}
		go func() {
			buf := make([]byte, 100)
			n, err := conn.Read(buf)
			if n == 0 || err != nil {
				return
			}
			buffer := buf[:n]
			if !DefaultRegisterRegexp.Match(buffer) {
				return
			}
			// registration
			matches := DefaultRegisterRegexp.FindSubmatch(buf)
			id := string(matches[1])
			if id == "" {
				return
			}
			log.Println("registering new client", id)
			client := &Client{
				conn:        conn,
				Id:          id,
				ConnectedAt: time.Now(),
			}
			s.clients.Store(id, client)
			go client.handle()
		}()
	}
}

func (client *Client) handle() {
	buf := make([]byte, 100)
	for {
		// this is just an example, it is possible that Read() only reads
		// a part of the data, you should handle this situation by yourself
		n, err := client.conn.Read(buf)
		if err != nil {
			log.Println("error reading bytes", err.Error())
			return
		}
		if n == 0 {
			log.Println("EOF")
			return
		}

		buffer := buf[:n]

		// already registered, ignore
		if DefaultRegisterRegexp.Match(buffer) {
			continue
		}

		// parsing linble heartbeat
		if bytes.HasPrefix(buffer, DefaultHeartbeatPrefix) {
			content := string(buffer[len(DefaultHeartbeatPrefix):])
			v, err := url.ParseQuery(content)
			if err == nil {
				client.MacAddress = v.Get("mac_address")
				if client.MacAddress == "" {
					client.MacAddress = v.Get("mac")
				}
				client.IpAddress = v.Get("ip_address")
				if client.IpAddress == "" {
					client.IpAddress = v.Get("ip")
				}
				client.CSQ, _ = strconv.Atoi(v.Get("csq"))
			}
			if client.MacAddress == "" {
				client.MacAddress = regexp.MustCompile("[0-9A-F]{12}").FindString(content)
			}
			if client.IpAddress == "" {
				client.IpAddress = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+`).FindString(content)
			}
			continue
		}

		log.Printf("%s received: % X", client.Id, buffer)

		// in this example, only rotation result is handled:
		// once we received rotation result, use channel to send it back to Server.Rotate
		if buffer[2] == FUNC_ROTATE && len(buffer) == 10 {
			frame, row, column := int(buffer[3]), int(buffer[4]), int(buffer[5])
			key := fmt.Sprintf("%s-%d-%d-%d", KEY_ROTATE, frame, row, column)
			if channel, ok := client.channels.LoadAndDelete(key); ok {
				channel.(chan []byte) <- buffer
			}
		}
	}
}

func bytesForData(function byte, data []byte) (out []byte, frame byte) {
	_, min, sec := time.Now().Clock()
	frame = byte((min*60 + sec) % 250)
	size := 4 + 2 + len(data)
	out = append([]byte{0xa8, byte(size), function, frame}, data...)
	var sum byte
	for i := 0; i < len(out); i++ {
		sum += out[i]
	}
	out = append(out, sum&0xff, 0xfe)
	return
}
