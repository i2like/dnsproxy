package proxy

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	glcache "github.com/AdguardTeam/golibs/cache"
	"github.com/AdguardTeam/golibs/log"
	"github.com/miekg/dns"
	ping "github.com/sparrc/go-ping"
)

const (
	cacheTTLSec = 10 * 60 // cache TTL in seconds
	icmpTimeout = 1000
	tcpTimeout  = 1000
)

// FastestAddr - object data
type FastestAddr struct {
	cache     glcache.Cache // cache of the fastest IP addresses
	allowICMP bool
	allowTCP  bool
	tcpPort   uint
}

// Init - initialize module
func (f *FastestAddr) Init() {
	conf := glcache.Config{
		MaxSize:   1 * 1024 * 1024,
		EnableLRU: true,
	}
	f.cache = glcache.New(conf)
	f.allowICMP = true
	f.allowTCP = true
	f.tcpPort = 80
}

type cacheEntry struct {
	status      int //0:ok; 1:timed out
	latencyMsec uint
}

/*
expire [4]byte
status byte
latency_msec [2]byte
*/
func packCacheEntry(ent *cacheEntry) []byte {
	expire := uint32(time.Now().Unix()) + cacheTTLSec
	var d []byte
	d = make([]byte, 4+1+2)
	binary.BigEndian.PutUint32(d, expire)
	i := 4

	d[i] = byte(ent.status)
	i++

	binary.BigEndian.PutUint16(d[i:], uint16(ent.latencyMsec))
	i += 2

	return d
}

func unpackCacheEntry(data []byte) *cacheEntry {
	now := time.Now().Unix()
	expire := binary.BigEndian.Uint32(data[:4])
	if int64(expire) <= now {
		return nil
	}
	ent := cacheEntry{}
	i := 4

	ent.status = int(data[i])
	i++

	ent.latencyMsec = uint(binary.BigEndian.Uint16(data[i:]))
	i += 2

	return &ent
}

// find in cache
func (f *FastestAddr) cacheFind(domain string, ip net.IP) *cacheEntry {
	val := f.cache.Get(ip)
	if val == nil {
		return nil
	}
	ent := unpackCacheEntry(val)
	if ent == nil {
		return nil
	}
	return ent
}

// store in cache
func (f *FastestAddr) cacheAdd(ent *cacheEntry, addr net.IP) {
	ip := addr.To4()
	if ip == nil {
		ip = addr
	}

	val := packCacheEntry(ent)
	f.cache.Set(ip, val)
}

// Search in cache
func (f *FastestAddr) getFromCache(host string, replies []upstream.ExchangeAllResult) (*upstream.ExchangeAllResult, net.IP, int) {
	var fastestIP net.IP
	var fastestRes *upstream.ExchangeAllResult
	var minLatency uint
	minLatency = 0xffff

	n := 0
	for _, r := range replies {
		for _, a := range r.Resp.Answer {
			var ip net.IP
			switch addr := a.(type) {
			case *dns.A:
				ip = addr.A.To4()

			case *dns.AAAA:
				ip = addr.AAAA

			default:
				continue
			}

			ent := f.cacheFind(host, ip)
			if ent != nil {
				n++
			}
			if ent != nil && ent.status == 0 && minLatency > ent.latencyMsec {
				fastestIP = ip
				fastestRes = &r
				minLatency = ent.latencyMsec
			}
		}
	}

	if fastestRes != nil {
		log.Debug("%s: Using %s address as the fastest (from cache)",
			host, fastestIP)
		return fastestRes, fastestIP, n
	}

	return nil, nil, n
}

// Get the number of A and AAAA records
func (f *FastestAddr) totalIPAddrs(replies []upstream.ExchangeAllResult) int {
	n := 0
	for _, r := range replies {
		for _, a := range r.Resp.Answer {
			switch a.(type) {
			case *dns.A:
				//
			case *dns.AAAA:
				//
			default:
				continue
			}
			n++
		}
	}
	return n
}

// Return DNS response containing the fastest IP address
// Algorithm:
// . Send requests to all upstream servers
// . Receive responses
// . Search all IP addresses in cache:
//   . If all addresses have been found: choose the fastest
//   . If several (but not all) addresses have been found: remember the fastest
// . For each response, for each IP address (not found in cache):
//   . send ICMP packet
//   . connect via TCP
// . Receive ICMP packets.  The first received packet makes it the fastest IP address.
// . Receive TCP connection status.  The first connected address - the fastest IP address.
// . Choose the fastest address between this and the one previously found in cache
// . Return DNS packet containing the chosen IP address (remove all other IP addresses from the packet)
func (f *FastestAddr) exchangeFastest(req *dns.Msg, upstreams []upstream.Upstream) (*dns.Msg, upstream.Upstream, error) {
	replies, err := upstream.ExchangeAll(upstreams, req)
	if err != nil || len(replies) == 0 {
		return nil, nil, err
	}
	host := strings.ToLower(req.Question[0].Name)

	total := f.totalIPAddrs(replies)
	if total <= 1 {
		return replies[0].Resp, replies[0].Upstream, nil
	}

	exresCached, addressCached, nCached := f.getFromCache(host, replies)
	if exresCached != nil && nCached == total {
		return prepareReply(exresCached.Resp, addressCached), exresCached.Upstream, nil
	}

	ch := make(chan *pingResult, total)
	total = 0
	for _, r := range replies {
		for _, a := range r.Resp.Answer {
			var ip net.IP
			switch addr := a.(type) {
			case *dns.A:
				ip = addr.A.To4()

			case *dns.AAAA:
				ip = addr.AAAA

			default:
				continue
			}

			if f.cacheFind(host, ip) == nil {
				if f.allowICMP {
					go f.pingDo(ip, &r, ch)
					total++
				}
				if f.allowTCP {
					go f.pingDoTCP(ip, &r, ch)
					total++
				}
			}
		}
	}

	if total == 0 {
		return replies[0].Resp, replies[0].Upstream, nil
	}

	exres, address, err2 := f.pingWait(total, ch)

	//...

	if err2 != nil {
		return replies[0].Resp, replies[0].Upstream, nil
	}

	return prepareReply(exres.Resp, address), exres.Upstream, nil
}

// remove all A/AAAA records, leaving only the fastest one
func prepareReply(resp *dns.Msg, address net.IP) *dns.Msg {
	ans := []dns.RR{}
	for _, a := range resp.Answer {
		switch addr := a.(type) {
		case *dns.A:
			if address.To4().Equal(addr.A.To4()) {
				ans = append(ans, a)
			}

		case *dns.AAAA:
			if address.Equal(addr.AAAA) {
				ans = append(ans, a)
			}

		default:
			ans = append(ans, a)
		}
	}
	resp.Answer = ans
	return resp
}

type pingResult struct {
	addr        net.IP
	exres       *upstream.ExchangeAllResult
	err         error
	isICMP      bool // 1: ICMP; 0: TCP
	latencyMsec uint
}

// Ping an address via ICMP and then send signal to the channel
func (f *FastestAddr) pingDo(addr net.IP, exres *upstream.ExchangeAllResult, ch chan *pingResult) {
	res := &pingResult{}
	res.addr = addr
	res.exres = exres
	res.isICMP = true

	pinger, err := ping.NewPinger(addr.String())
	if err != nil {
		log.Error("ping.NewPinger(): %v", err)
		res.err = err
		ch <- res
		return
	}

	pinger.SetPrivileged(true)
	pinger.Timeout = icmpTimeout * time.Millisecond
	pinger.Count = 1
	reply := false
	pinger.OnRecv = func(pkt *ping.Packet) {
		// log.Tracef("Received ICMP Reply from %v", target)
		reply = true
	}
	log.Debug("%s: Sending ICMP Echo to %s",
		res.exres.Resp.Question[0].Name, addr)
	start := time.Now()
	pinger.Run()

	if !reply {
		res.err = fmt.Errorf("%s: no reply from %s",
			res.exres.Resp.Question[0].Name, addr)
		log.Debug("%s", res.err)
	} else {
		res.latencyMsec = uint(time.Since(start).Milliseconds())
	}
	ch <- res
}

// Connect to a remote address via TCP and then send signal to the channel
func (f *FastestAddr) pingDoTCP(addr net.IP, exres *upstream.ExchangeAllResult, ch chan *pingResult) {
	res := &pingResult{}
	res.addr = addr
	res.exres = exres

	a := net.JoinHostPort(addr.String(), strconv.Itoa(int(f.tcpPort)))
	log.Debug("%s: Connecting to %s via TCP",
		res.exres.Resp.Question[0].Name, a)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", a, tcpTimeout*time.Millisecond)
	if err != nil {
		res.err = fmt.Errorf("%s: no reply from %s",
			res.exres.Resp.Question[0].Name, addr)
		log.Debug("%s", res.err)
		ch <- res
		return
	}
	res.latencyMsec = uint(time.Since(start).Milliseconds())
	conn.Close()
	ch <- res
}

// Wait for the first successful ping result
func (f *FastestAddr) pingWait(total int, ch chan *pingResult) (*upstream.ExchangeAllResult, net.IP, error) {
	n := 0
	for {
		select {
		case res := <-ch:
			n++
			ent := cacheEntry{}

			if res.err != nil {
				ent.status = 1
				f.cacheAdd(&ent, res.addr)
				break
			}

			proto := "icmp"
			if !res.isICMP {
				proto = "tcp"
			}
			log.Debug("%s: Using %s address as the fastest (%s)",
				res.exres.Resp.Question[0].Name, res.addr, proto)

			ent.status = 0
			ent.latencyMsec = res.latencyMsec
			f.cacheAdd(&ent, res.addr)

			return res.exres, res.addr, nil
		}

		if n == total {
			return nil, nil, fmt.Errorf("all ping tasks were timed out")
		}
	}
}
