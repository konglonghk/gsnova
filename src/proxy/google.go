package proxy

import (
	"bufio"
	"common"
	"crypto/tls"
	"errors"
	"event"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"util"
)

const (
	GOOGLE_HTTPS_IP = "GoogleHttpsIP"
	GOOGLE_HTTPS    = "GoogleHttps"
	GOOGLE_HTTP_IP  = "GoogleHttpIP"
	GOOGLE_HTTP     = "GoogleHttp"
)

var useGlobalProxy bool
var preferIP = true
var googleHttpHost = "GoogleHttpIP"
var googleHttpsHost = "GoogleHttps"
var connTimeoutSecs time.Duration

var httpGoogleManager *Google
var httpsGoogleManager *Google
var google_enable bool
var total_google_conn_num int32
var total_google_routine_num int32

type GoogleConnection struct {
	http_client        net.Conn
	http_client_reader *bufio.Reader
	https_client       net.Conn
	forwardChan        chan int
	proxyAddr          string
	proxyURL           *url.URL
	overProxy          bool
	manager            *Google
	simple_url         bool
	use_sys_dns        bool
}

func (conn *GoogleConnection) IsDisconnected() bool {
	if nil == conn.http_client {
		return false
	}
	conn.http_client.SetReadDeadline(time.Now().Add(1 * time.Nanosecond))
	one := make([]byte, 1)
	if _, err := conn.http_client.Read(one); !util.IsTimeoutError(err) {
		conn.Close()
		return true
	} else {
		var zero time.Time
		err := conn.http_client.SetReadDeadline(zero)
		if nil != err {
			conn.Close()
			return true
		}
	}
	return false
}

func (conn *GoogleConnection) Close() error {
	if nil != conn.http_client {
		conn.http_client.Close()
		conn.http_client = nil
	}
	if nil != conn.https_client {
		conn.https_client.Close()
		conn.https_client = nil
	}
	return nil
}

func isLocalGoogleProxy() bool {
	proxyInfo, exist := common.Cfg.GetProperty("LocalProxy", "Proxy")
	if exist {
		if strings.Contains(proxyInfo, GOOGLE_HTTPS_IP) || strings.Contains(proxyInfo, GOOGLE_HTTPS) {
			return true
		}
		if strings.Contains(proxyInfo, GOOGLE_HTTP_IP) || strings.Contains(proxyInfo, GOOGLE_HTTP) {
			return true
		}
	}
	return false
}

func (conn *GoogleConnection) initHttpsClient() {
	if nil != conn.https_client {
		return
	}
	conn.forwardChan = make(chan int)
	proxyInfo, exist := common.Cfg.GetProperty("LocalProxy", "Proxy")

	if useGlobalProxy && exist && !isLocalGoogleProxy() {
		proxy := getLocalUrlMapping(proxyInfo)
		log.Printf("Google use proxy:%s\n", proxy)
		proxyURL, err := url.Parse(proxy)
		conn.https_client, err = net.Dial("tcp", proxyURL.Host)
		if nil != err {
			log.Printf("Failed to dial address:%s for reason:%s\n", proxyURL.Host, err.Error())
			return
		}
		//3rd Proxy only accept domain as target
		addr, _ := getLocalHostMapping(GOOGLE_HTTPS)
		req := &http.Request{
			Method:        "CONNECT",
			URL:           &url.URL{Scheme: "https", Host: addr},
			Host:          addr,
			Header:        make(http.Header),
			ContentLength: 0,
		}
		req.Write(conn.https_client)
		res, err := http.ReadResponse(bufio.NewReader(conn.https_client), req)
		if nil != err {
			log.Printf("Failed to connect address:%s:443 for reason:%s\n", addr, err.Error())
			conn.https_client.Close()
			return
		}
		if res.StatusCode != 200 {
			log.Printf("Failed to connect address:%s:443 for response code:%d\n", addr, res.StatusCode)
			conn.https_client.Close()
			return
		}
		conn.overProxy = true
	} else {
		get_google_hostport := func() string {
			addr, _ := getLocalHostMapping(googleHttpsHost)
			addr = net.JoinHostPort(addr, "443")
			if !preferIP && !conn.use_sys_dns {
				addr, _ = lookupAvailableAddress(addr)
			}
			return addr
		}
		var err error
		addr := get_google_hostport()
		conn.https_client, err = net.DialTimeout("tcp", addr, connTimeoutSecs)
		//try again
		if nil != err {
			addr = get_google_hostport()
			conn.https_client, err = net.DialTimeout("tcp", addr, connTimeoutSecs)
		}
		if nil != err {
			log.Printf("Failed to dial address:%s for reason:%s\n", addr, err.Error())
		}
	}
}

func (conn *GoogleConnection) initHttpClient(proxyAddr string) {
	if nil != conn.http_client && conn.proxyAddr == proxyAddr {
		return
	}
	conn.Close()
	proxyInfo, exist := common.Cfg.GetProperty("LocalProxy", "Proxy")
	if useGlobalProxy && exist && !isLocalGoogleProxy() {
		proxy := getLocalUrlMapping(proxyInfo)
		log.Printf("Google use proxy:%s\n", proxy)
		proxyURL, err := url.Parse(proxy)
		target := proxyURL.Host

		if !strings.Contains(proxyURL.Host, ":") {
			port := 80
			//log.Println(proxyURL.Scheme)
			if strings.EqualFold(proxyURL.Scheme, "https") {
				port = 443
			}
			target = fmt.Sprintf("%s:%d", target, port)
		}
		conn.http_client, err = net.DialTimeout("tcp", target, connTimeoutSecs)
		if nil != err {
			log.Printf("Failed to dial address:%s for reason:%s\n", proxyURL.Host, err.Error())
			conn.Close()
			return
		}
		addr, _ := getLocalHostMapping(GOOGLE_HTTPS)
		req := &http.Request{
			Method:        "CONNECT",
			URL:           &url.URL{Scheme: "https", Host: addr},
			Host:          addr,
			Header:        make(http.Header),
			ContentLength: 0,
		}
		req.Write(conn.http_client)
		res, err := http.ReadResponse(bufio.NewReader(conn.http_client), req)
		if nil != err {
			log.Printf("Failed to connect address:%s:443 for reason:%s\n", addr, err.Error())
			conn.Close()
			return
		}
		if res.StatusCode != 200 {
			log.Printf("Failed to connect address:%s:443 for response code:%d\n", addr, res.StatusCode)
			conn.Close()
			return
		}
		tlcfg := &tls.Config{InsecureSkipVerify: true}
		conn.http_client = tls.Client(conn.http_client, tlcfg)
		conn.overProxy = true
	} else {
		var err error
		conn.overProxy = false
		if conn.manager == httpGoogleManager {
			get_google_hostport := func() string {
				addr, _ := getLocalHostMapping(googleHttpHost)
				addr = net.JoinHostPort(addr, "80")
				if !preferIP && !conn.use_sys_dns  {
					addr, _ = lookupAvailableAddress(addr)
				}
				return addr
			}
			addr := get_google_hostport()
			conn.http_client, err = net.DialTimeout("tcp", addr, connTimeoutSecs)
			if nil != err {
				conn.Close()
				addr = get_google_hostport()
				conn.http_client, err = net.DialTimeout("tcp", addr, connTimeoutSecs)
			}
			if nil != err {
				conn.Close()
				log.Printf("Failed to dial address:%s for reason:%s\n", addr, err.Error())
				return
			}
		} else {
			get_google_hostport := func() string {
				addr, _ := getLocalHostMapping(googleHttpsHost)
				addr = net.JoinHostPort(addr, "443")
				if !preferIP && !conn.use_sys_dns {
					addr, _ = lookupAvailableAddress(addr)
				}
				return addr
			}
			addr := get_google_hostport()
			conn.http_client, err = net.DialTimeout("tcp", addr, connTimeoutSecs)
			if nil != err {
				conn.Close()
				addr = get_google_hostport()
				conn.http_client, err = net.DialTimeout("tcp", addr, connTimeoutSecs)
			}
			if nil != err {
				log.Printf("Failed to dial address:%s for reason:%s\n", addr, err.Error())
				conn.Close()
				return
			}
			tlcfg := &tls.Config{InsecureSkipVerify: true}
			conn.http_client = tls.Client(conn.http_client, tlcfg)
		}
	}
	conn.proxyAddr = proxyAddr
	conn.http_client_reader = bufio.NewReader(conn.http_client)
}

func (conn *GoogleConnection) GetConnectionManager() RemoteConnectionManager {
	return conn.manager
}

func (google *GoogleConnection) writeHttpRequest(req *http.Request) error {
	var err error
	index := 0
	for {
		if useGlobalProxy && google.overProxy {
			err = req.WriteProxy(google.http_client)
		} else {
			//err = req.Write(google.http_client)
			if google.simple_url {
				err = req.Write(google.http_client)
			} else {
				err = req.WriteProxy(google.http_client)
			}
		}
		//err = req.WriteProxy(google.http_client)
		if nil != err {
			log.Printf("Resend request since error:%s occured.\n", err.Error())
			google.Close()
			google.initHttpClient(req.Host)
		} else {
			return nil
		}
		index++
		if index == 2 {
			return err
		}
	}
	return nil
}

func (google *GoogleConnection) Request(conn *SessionConnection, ev event.Event) (err error, res event.Event) {
	//log.Printf("Enter here for request\n")
	f := func(local, remote net.Conn) {
		io.Copy(remote, local)
		google.forwardChan <- 1
		local.Close()
		remote.Close()
	}
	//L:
	switch ev.GetType() {
	case event.HTTP_REQUEST_EVENT_TYPE:
		req := ev.(*event.HTTPRequestEvent)
		if conn.Type == HTTPS_TUNNEL {
			google.initHttpsClient()
			//try again
			if nil == google.https_client {
				google.initHttpsClient()
			}
			log.Printf("Session[%d]Request %s\n", req.GetHash(), util.GetURLString(req.RawReq, true))
			if nil != google.https_client {
				conn.LocalRawConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			} else {
				return errors.New("No google proxy reachable."), nil
			}
			go f(conn.LocalRawConn, google.https_client)
			go f(google.https_client, conn.LocalRawConn)
			atomic.AddInt32(&total_google_routine_num, 2)
			<-google.forwardChan
			<-google.forwardChan
			atomic.AddInt32(&total_google_routine_num, -2)
			google.Close()
			conn.State = STATE_SESSION_CLOSE
		} else {
			google.initHttpClient(req.RawReq.Host)
			//try again
			if nil == google.http_client {
				google.initHttpClient(req.RawReq.Host)
			}
			if nil == google.http_client {
				log.Printf("Failed to connect google http site.\n")
				return errors.New("No google proxy reachable."), nil
			}
			log.Printf("Session[%d]Request %s\n", req.GetHash(), util.GetURLString(req.RawReq, true))
			//if google.manager.GetName() == GOOGLE_HTTP {
			//	google.http_client.Write(CRLFs)
			//}
			err := google.writeHttpRequest(req.RawReq)
			if nil != err {
				google.Close()
				return err, nil
			}
			//google.http_client.SetReadDeadline(time.Now().Add(10 * time.Second))
			resp, err := http.ReadResponse(google.http_client_reader, req.RawReq)
			if err != nil {
				google.Close()
				//				if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				//				    log.Printf("Read Google connection timeout, retry.\n")
				//					goto L
				//				}
				return err, nil
			}
			//google.http_client.SetReadDeadline(time.Time{})
			//			if resp.StatusCode >= 300 {
			//				log.Printf("Session[%d]Request %s receive error response %s\n", req.GetHash(), util.GetURLString(req.RawReq, true), resp.Status)
			//			}
			//			if resp.StatusCode == 502{
			//			   return fmt.Errorf("Invalid 502 response for request."), nil
			//			}
			err = resp.Write(conn.LocalRawConn)
			if nil == err {
				err = resp.Body.Close()
			}
			if nil != err || !util.IsResponseKeepAlive(resp) || !util.IsRequestKeepAlive(req.RawReq) {
				conn.LocalRawConn.Close()
				google.Close()
				conn.State = STATE_SESSION_CLOSE
			} else {
				conn.State = STATE_RECV_HTTP
			}
		}
	default:
	}
	return nil, nil
}

type Google struct {
	name       string
	idle_conns chan RemoteConnection
}

func (manager *Google) GetName() string {
	return manager.name
}

func (manager *Google) RecycleRemoteConnection(conn RemoteConnection) {
	select {
	case manager.idle_conns <- conn:
		// Buffer on free list; nothing more to do.
	default:
		// Free list full, just carry on.
	}
	atomic.AddInt32(&total_google_conn_num, -1)
}

func (manager *Google) GetRemoteConnection(ev event.Event, attrs map[string]string) (RemoteConnection, error) {
	var b RemoteConnection
	// Grab a buffer if available; allocate if not.
	select {
	case b = <-manager.idle_conns:
		// Got one; nothing more to do.
	default:
		// None free, so allocate a new one.
		g := new(GoogleConnection)
		g.manager = manager
		b = g
		//b.auth =
	} // Read next message from the net.
	b.Close()
	if containsAttr(attrs, ATTR_DIRECT) {
		b.(*GoogleConnection).simple_url = true
	} else {
		b.(*GoogleConnection).simple_url = false
	}
	if containsAttr(attrs, ATTR_SYS_DNS) {
		b.(*GoogleConnection).use_sys_dns = true
	} else {
		b.(*GoogleConnection).use_sys_dns = false
	}
	atomic.AddInt32(&total_google_conn_num, 1)
	return b, nil
}

func InitGoogle() error {
	if enable, exist := common.Cfg.GetIntProperty("Google", "Enable"); exist {
		google_enable = (enable != 0)
		if enable == 0 {
			return nil
		}
	}
	log.Println("Init Google.")
	if tmp, exist := common.Cfg.GetIntProperty("Google", "UseGlobalProxy"); exist {
		if tmp == 1 {
			useGlobalProxy = true
		} else if tmp == 2 {
			if !strings.Contains(common.LocalProxy.Host, "Google") {
				useGlobalProxy = true
			}
		} else {
			useGlobalProxy = false
		}
	}
	if prefer, exist := common.Cfg.GetBoolProperty("Google", "PreferIP"); exist {
		preferIP = prefer
	}
	if preferIP {
		googleHttpsHost = GOOGLE_HTTPS_IP
		googleHttpHost = GOOGLE_HTTP_IP
	} else {
		googleHttpsHost = GOOGLE_HTTPS
		googleHttpHost = GOOGLE_HTTP
	}
	connTimeoutSecs = 1500 * time.Millisecond
	if tmp, exist := common.Cfg.GetIntProperty("Google", "ConnectTimeout"); exist {
		connTimeoutSecs = time.Duration(tmp) * time.Millisecond
	}
	httpGoogleManager = newGoogle(GOOGLE_HTTP_NAME)
	httpsGoogleManager = newGoogle(GOOGLE_HTTPS_NAME)
	return nil
}

func newGoogle(name string) *Google {
	manager := new(Google)
	manager.name = name
	manager.idle_conns = make(chan RemoteConnection, 50)
	return manager
}
