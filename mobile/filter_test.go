package mobile

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/AdguardTeam/urlfilter/filterlist"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/AdguardTeam/urlfilter"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
)

const filesJSON = `
	[
		{"id": 1111, "path": "test_filters/network_filter.txt"},
		{"id": 11111, "path": "test_filters/hosts_filter.txt"}
	]`

// testCNAMEs is a simple map of names and CNAMEs necessary for the testUpstream work
var testCNAMEs = map[string]string{
	"mail.google.com.":   "googlemail.l.google.com.",
	"groups.google.com.": "groups.l.google.com.",
	"picasa.google.com.": "www2.l.google.com.",
}

// testIPv4 is a simple map of names and IPv4s necessary for the testUpstream work
var testIPv4 = map[string][]net.IP{
	"dns.adguard.com.":   {{176, 103, 130, 130}},
	"adguard.com.":       {{104, 20, 30, 130}},
	"mail.google.com.":   {{1, 2, 3, 4}},
	"groups.google.com.": {{5, 6, 7, 8}},
	"picasa.google.com.": {{9, 10, 11, 12}},
}

// testIPv6 is a simple map of names and IPv6s necessary for the testUpstream work
var testIPv6 = map[string][]net.IP{
	"dns.adguard.com.":   {net.ParseIP("2a00:5a60::ad1:ff")},
	"adguard.com.":       {net.ParseIP("2606:4700:10::6814:1e82")},
	"mail.google.com.":   {net.ParseIP("2106:4700:10::6814:1e82")},
	"groups.google.com.": {net.ParseIP("2206:4700:10::6814:1e82")},
	"picasa.google.com.": {net.ParseIP("2306:4700:10::6814:1e82")},
}

// TestDecodeFilterRuleLists tests filtering configuration decode for String and File RuleLists
func TestDecodeFilterRuleLists(t *testing.T) {
	const rulesJSON = `
	[
		{"id": 1, "contents": "||google.com^\n||google.ru^\n||google.ua^\n"},
		{"id": 11, "contents": "0.0.0.0 yandex.ru\n0.0.0.0 yandex.ua\n0.0.0.0 bing.com"},
		{"id": 111, "contents": "2000:: bing.com\n::1 yahoo.com"}
	]`

	filters := []filterlist.RuleList{}

	// Let's add string rules and check lists count
	err := addStringRuleLists(rulesJSON, &filters)
	assert.Nil(t, err)
	assert.Equal(t, len(filters), 3)

	// Let's add file rules and check lists count
	err = addFileRuleLists(filesJSON, &filters)
	assert.Nil(t, err)
	assert.Equal(t, len(filters), 5)

	// Init rules storage and check rules count
	rs, err := filterlist.NewRuleStorage(filters)
	assert.Nil(t, err)
	engine := urlfilter.NewDNSEngine(rs)
	assert.Equal(t, engine.RulesCount, 16)

	// Examine filtering engine
	rules, ok := engine.Match("bing.com")
	assert.True(t, ok)
	assert.Equal(t, len(rules), 2)

	rules, ok = engine.Match("yandex.ru")
	assert.True(t, ok)
	assert.Equal(t, len(rules), 1)
	assert.Equal(t, rules[0].GetFilterListID(), 11)

	rules, ok = engine.Match("google.ru")
	assert.True(t, ok)
	assert.Equal(t, len(rules), 1)
	assert.Equal(t, rules[0].GetFilterListID(), 1)

	_, ok = engine.Match("example.org")
	assert.False(t, ok)

	rules, ok = engine.Match("a24help.ru")
	assert.True(t, ok)
	assert.Equal(t, len(rules), 1)
	assert.Equal(t, rules[0].GetFilterListID(), 1111)

	rules, ok = engine.Match("events.appsflyer.com")
	assert.True(t, ok)
	assert.Equal(t, len(rules), 1)
	assert.Equal(t, rules[0].GetFilterListID(), 11111)

	// Close rules storage
	err = rs.Close()
	assert.Nil(t, err)
}

// TestFilteringProxy test all kinds of DNS filtering rules:
// blockType equals BlockTypeRule
// Network filtering rules should block request for any kind of request
// IPv4 Host filtering rules should block only A requests
// IPv6 Host filtering rules should block only AAAA requests
func TestFilteringProxyRuleBlock(t *testing.T) {
	mobileDNSProxy := createTestFilteringProxy(BlockTypeRule)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	// Start listening
	err := mobileDNSProxy.Start()
	assert.Nil(t, err)

	// Create a DNS-over-TCP client connection
	addr := mobileDNSProxy.dnsProxy.Addr(proxy.ProtoUDP)
	conn, err := dns.Dial("udp", addr.String())
	assert.Nil(t, err)

	// Create, send and assert regular test message
	sendAndAssertTestMessage(t, conn)

	// There are Network filtering rules, which matched this hosts
	testFilteringRuleNXDomainBlock(t, conn, "example.com")
	testFilteringRuleNXDomainBlock(t, conn, "example.org")
	testFilteringRuleNXDomainBlock(t, conn, "a24help.ru")
	testFilteringRuleNXDomainBlock(t, conn, "a.sdska.ru")

	// There are IPv4 Host filtering rules matched this hosts
	testFilteringRuleARequestIPBlock(t, conn, "events.appsflyer.com", net.IPv4(0, 0, 0, 0))
	testFilteringRuleARequestIPBlock(t, conn, "google.com", net.IPv4(0, 0, 0, 0))
	testFilteringRuleARequestIPBlock(t, conn, "google.ru", net.IPv4(127, 0, 0, 1))
	//
	//// There are IPv6 Host filtering rules, which matched this hosts
	testFilteringRuleAAAARequestIPBlock(t, conn, "yandex.ru", net.ParseIP("2000::"))
	testFilteringRuleAAAARequestIPBlock(t, conn, "yandex.ua", net.ParseIP("::1"))
	testAAAAHostFilteringRuleARequest(t, conn, "yandex.ru")
	testAAAAHostFilteringRuleARequest(t, conn, "yandex.ua")

	assertListenerEventsCount(t, listener, 16)

	// unregister listener
	ConfigureDNSRequestProcessedListener(nil)
	// Stop the proxy
	err = mobileDNSProxy.Stop()
	assert.Nil(t, err)
}

// TestFilteringProxyNXDomainBlock tests all kinds of DNS filtering rules:
// blockType equals BlockTypeNXDomain
// Network filtering rules should block both A and AAAA requests
// IPv4 Host filtering rules should block A and AAAA requests if it's IPv4Zero rule
// IPv6 Host filtering rules should block only AAAA requests
// All requests should be blocked with NXDomain
func TestFilteringProxyNXDomainBlock(t *testing.T) {
	dnsProxy := createTestFilteringProxy(BlockTypeNXDomain)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	// Start listening
	err := dnsProxy.Start()
	assert.Nil(t, err)

	// Create a DNS-over-UDP client connection
	addr := dnsProxy.dnsProxy.Addr(proxy.ProtoUDP)
	conn, err := dns.Dial("udp", addr.String())
	assert.Nil(t, err)

	// Create, send and assert regular test message
	sendAndAssertTestMessage(t, conn)

	// There are Network filtering rules, which matched this hosts
	testFilteringRuleNXDomainBlock(t, conn, "example.com")
	testFilteringRuleNXDomainBlock(t, conn, "example.org")
	testFilteringRuleNXDomainBlock(t, conn, "a24help.ru")
	testFilteringRuleNXDomainBlock(t, conn, "a.sdska.ru")

	// Let's ensure that hosts which matched to Host filtering rules are blocked with NXDomain too
	testFilteringRuleNXDomainBlock(t, conn, "google.com")
	testFilteringRuleNXDomainBlock(t, conn, "events.appsflyer.com")

	assertListenerEventsCount(t, listener, 13)

	// unregister listener
	ConfigureDNSRequestProcessedListener(nil)

	// Stop the proxy
	err = dnsProxy.Stop()
	assert.Nil(t, err)
}

// TestFilteringProxyIPBlock tests all kinds of DNS filtering rules:
// blockType equals BlockTypeUnspecifiedIP
// Network filtering rules should block both A and AAAA requests
// IPv4 Host filtering rules should block A and AAAA requests if it's IPv4Zero rule
// IPv6 Host filtering rules should block only AAAA requests
// All requests should be blocked with IP
func TestFilteringProxyIPBlock(t *testing.T) {
	mobileDNSProxy := createTestFilteringProxy(BlockTypeUnspecifiedIP)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	// Start listening
	err := mobileDNSProxy.Start()
	assert.Nil(t, err)

	// Create a DNS-over-UDP client connection
	addr := mobileDNSProxy.dnsProxy.Addr(proxy.ProtoUDP)
	conn, err := dns.Dial("udp", addr.String())
	assert.Nil(t, err)

	// Create, send and assert regular test message which shouldn't be blocked
	sendAndAssertTestMessage(t, conn)

	// There are IPv4 Host filtering rules which matched this hosts
	testFilteringRuleARequestIPBlock(t, conn, "events.appsflyer.com", net.IPv4(0, 0, 0, 0))
	testFilteringRuleARequestIPBlock(t, conn, "google.com", net.IPv4(0, 0, 0, 0))
	testFilteringRuleARequestIPBlock(t, conn, "google.ru", net.IPv4(127, 0, 0, 1))

	// There are Network filtering rules which matched this hosts
	testFilteringRuleARequestIPBlock(t, conn, "example.com", net.IPv4(0, 0, 0, 0))
	testFilteringRuleARequestIPBlock(t, conn, "example.org", net.IPv4(0, 0, 0, 0))

	// There are IPv6 Host filtering rules, which matched this hosts
	testFilteringRuleAAAARequestIPBlock(t, conn, "yandex.ru", net.ParseIP("2000::"))
	testFilteringRuleAAAARequestIPBlock(t, conn, "yandex.ua", net.ParseIP("::1"))
	testAAAAHostFilteringRuleARequest(t, conn, "yandex.ru")
	testAAAAHostFilteringRuleARequest(t, conn, "yandex.ua")

	// Let's ensure that AAAA requests for hosts matched Network filtering rules are blocked with net.IPv6zero
	testFilteringRuleAAAARequestIPBlock(t, conn, "example.com", net.IPv6zero)
	testFilteringRuleAAAARequestIPBlock(t, conn, "example.org", net.IPv6zero)

	assertListenerEventsCount(t, listener, 12)

	// unregister listener
	ConfigureDNSRequestProcessedListener(nil)
	// Stop the proxy
	err = mobileDNSProxy.Stop()
	assert.Nil(t, err)
}

// TestFilteringProxyRaceNXDomainBlock sends multiple parallel DNS requests, which should be blocked with NXDomain
func TestFilteringProxyRaceNXDomainBlock(t *testing.T) {
	dnsProxy := createTestFilteringProxy(BlockTypeNXDomain)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	// Start listening
	err := dnsProxy.Start()
	assert.Nil(t, err)

	// Create a DNS-over-UDP client connection
	addr := dnsProxy.Addr()
	conn, err := dns.Dial("udp", addr)
	assert.Nil(t, err)

	// Network filtering rules
	testFilteringRulesNXDomainBlockAsync(t, conn, "example.com")
	testFilteringRulesNXDomainBlockAsync(t, conn, "example.org")
	testFilteringRulesNXDomainBlockAsync(t, conn, "a24help.ru")
	testFilteringRulesNXDomainBlockAsync(t, conn, "a.ruporn.me")

	// IPv4Zero Host filtering rules
	testFilteringRulesNXDomainBlockAsync(t, conn, "google.com")
	testFilteringRulesNXDomainBlockAsync(t, conn, "events.appsflyer.com")
	testFilteringRulesNXDomainBlockAsync(t, conn, "datacollect.vmall.com")

	assertListenerEventsCount(t, listener, 140)

	// unregister listener
	ConfigureDNSRequestProcessedListener(nil)
	// Stop the proxy
	err = dnsProxy.Stop()
	assert.Nil(t, err)
}

// TestFilteringProxyRace sends multiple parallel DNS requests, which should be blocked with IP
func TestFilteringProxyRaceIPBlock(t *testing.T) {
	dnsProxy := createTestFilteringProxy(BlockTypeUnspecifiedIP)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	// Start listening
	err := dnsProxy.Start()
	assert.Nil(t, err)

	// Create a DNS-over-UDP client connection
	addr := dnsProxy.Addr()
	conn, err := dns.Dial("udp", addr)
	assert.Nil(t, err)

	// Requests which shouldn't be blocked
	sendTestMessagesAsync(t, conn)

	// Network filtering rules
	testFilteringRulesIPBlockAsync(t, conn, "example.com", dns.TypeA, net.IPv4(0, 0, 0, 0))
	testFilteringRulesIPBlockAsync(t, conn, "example.org", dns.TypeA, net.IPv4(0, 0, 0, 0))
	testFilteringRulesIPBlockAsync(t, conn, "a24help.ru", dns.TypeA, net.IPv4(0, 0, 0, 0))
	testFilteringRulesIPBlockAsync(t, conn, "a.ruporn.me", dns.TypeA, net.IPv4(0, 0, 0, 0))

	testFilteringRulesIPBlockAsync(t, conn, "a.ruporn.me", dns.TypeAAAA, net.IPv6zero)
	testFilteringRulesIPBlockAsync(t, conn, "a24help.ru", dns.TypeAAAA, net.IPv6zero)

	// IPv4 rules and A requests
	testFilteringRulesIPBlockAsync(t, conn, "events.appsflyer.com", dns.TypeA, net.IPv4(0, 0, 0, 0))
	testFilteringRulesIPBlockAsync(t, conn, "google.com", dns.TypeA, net.IPv4(0, 0, 0, 0))
	testFilteringRulesIPBlockAsync(t, conn, "google.ru", dns.TypeA, net.IPv4(127, 0, 0, 1))

	// IPv6 rules and AAAA requests
	testFilteringRulesIPBlockAsync(t, conn, "yandex.ru", dns.TypeAAAA, net.ParseIP("2000::"))
	testFilteringRulesIPBlockAsync(t, conn, "yandex.ua", dns.TypeAAAA, net.ParseIP("::1"))

	// Zero IPv4 rule should also block AAAA requests with IPv6Zero answer
	testFilteringRulesIPBlockAsync(t, conn, "google.com", dns.TypeAAAA, net.IPv6zero)

	assertListenerEventsCount(t, listener, 130)

	// unregister listener
	ConfigureDNSRequestProcessedListener(nil)
	// Stop the proxy
	err = dnsProxy.Stop()
	assert.Nil(t, err)
}

// TestDNSRequestProcessedEventsIPBlock tests DNSRequest processed events produced with filtering engine
func TestDNSRequestProcessedEventsIPBlock(t *testing.T) {
	dnsProxy := createTestFilteringProxy(BlockTypeUnspecifiedIP)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	// Start listening
	err := dnsProxy.Start()
	assert.Nil(t, err)

	// Create a DNS-over-UDP client connection
	addr := dnsProxy.Addr()
	conn, err := dns.Dial("udp", addr)
	assert.Nil(t, err)

	// Create, send and assert regular test message. Filtering proxy contains whitelist rule for google-public-dns-a.google.com
	eventsCount := 1
	sendAndAssertTestMessage(t, conn)
	assertDNSRequestProcessedEventWithListener(t, listener, "google-public-dns-a.google.com", "A", "@@||google-public-dns-a.google.com^", 4, eventsCount, true)

	// testFilteringRuleARequestIPBlock and testFilteringRuleAAAARequestIPBlock produce one event
	// Let's check events one-by-one

	// A request which blocked with IPv4 HostRule
	eventsCount++
	testFilteringRuleARequestIPBlock(t, conn, "google.ru", net.IPv4(127, 0, 0, 1))
	assertDNSRequestProcessedEventWithListener(t, listener, "google.ru", "A", "127.0.0.1 google.ru", 2, eventsCount, false)

	// A request which blocked with zero IPv4 HostRule
	eventsCount++
	testFilteringRuleARequestIPBlock(t, conn, "google.com", net.IPv4(0, 0, 0, 0))
	assertDNSRequestProcessedEventWithListener(t, listener, "google.com", "A", "0.0.0.0 google.com", 2, eventsCount, false)

	// AAAA request which blocked with zero IPv4 HostRule
	eventsCount++
	testFilteringRuleAAAARequestIPBlock(t, conn, "google.com", net.IPv6zero)
	assertDNSRequestProcessedEventWithListener(t, listener, "google.com", "AAAA", "0.0.0.0 google.com", 2, eventsCount, false)

	// AAAA request which blocked with IPv6 HostRule
	eventsCount++
	testFilteringRuleAAAARequestIPBlock(t, conn, "yandex.ru", net.ParseIP("2000::"))
	assertDNSRequestProcessedEventWithListener(t, listener, "yandex.ru", "AAAA", "2000:: yandex.ru", 3, eventsCount, false)

	// AAAA request which blocked with IPv6 HostRule
	eventsCount++
	testFilteringRuleAAAARequestIPBlock(t, conn, "yandex.ua", net.ParseIP("::1"))
	assertDNSRequestProcessedEventWithListener(t, listener, "yandex.ua", "AAAA", "::1 yandex.ua", 3, eventsCount, false)

	// IPv4 rule for google.ru is not 0.0.0.0. It means that AAAA request for google.ru shouldn't be blocked
	eventsCount++
	res := sendAAAATestMessage(t, conn, "google.ru")
	assert.NotNil(t, res.Answer[0])
	assert.Equal(t, res.Answer[0].Header().Rrtype, dns.TypeAAAA)
	assertDNSRequestProcessedEventWithListener(t, listener, "google.ru", "AAAA", "", 0, eventsCount, false)

	// IPv6 rule for yandex.ru shouldn't block A request
	eventsCount++
	res = sendATestMessage(t, conn, "yandex.ru")
	assert.NotNil(t, res.Answer[0])
	assert.Equal(t, res.Answer[0].Header().Rrtype, dns.TypeA)
	assertDNSRequestProcessedEventWithListener(t, listener, "yandex.ru", "A", "", 0, eventsCount, false)

	// AAAA request which blocked with Network filtering rule
	eventsCount++
	testFilteringRuleAAAARequestIPBlock(t, conn, "a.ruporn.me", net.IPv6zero)
	assertDNSRequestProcessedEventWithListener(t, listener, "a.ruporn.me", "AAAA", "||a.ruporn.me^", 1111, eventsCount, false)

	// unregister listener
	ConfigureDNSRequestProcessedListener(nil)
	// Stop the proxy
	err = dnsProxy.Stop()
	assert.Nil(t, err)
}

// TestDNSRequestProcessedEventsNXDomainBlock tests DNSRequest processed events produced with filtering engine
func TestDNSRequestProcessedEventsNXDomainBlock(t *testing.T) {
	dnsProxy := createTestFilteringProxy(BlockTypeNXDomain)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	// Start listening
	err := dnsProxy.Start()
	assert.Nil(t, err)

	// Create a DNS-over-UDP client connection
	addr := dnsProxy.Addr()
	conn, err := dns.Dial("udp", addr)
	assert.Nil(t, err)

	// There are two events produced with testFilteringRuleNXDomainBlock: A and AAAA requests
	// We should check count of registered events and each event
	// example.com and example.org should be blocked with Network filtering rules
	eventsCount := 2
	testFilteringRuleNXDomainBlock(t, conn, "example.com")
	assertListenerEventsCount(t, listener, eventsCount)
	assertDNSRequestProcessedEvent(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-2), "example.com", "A", "||example.com^", 1, false)
	assertDNSRequestProcessedEvent(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-1), "example.com", "AAAA", "||example.com^", 1, false)

	eventsCount += 2
	testFilteringRuleNXDomainBlock(t, conn, "example.org")
	assertListenerEventsCount(t, listener, eventsCount)
	assertDNSRequestProcessedEvent(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-2), "example.org", "A", "||example.org^", 1, false)
	assertDNSRequestProcessedEvent(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-1), "example.org", "AAAA", "||example.org^", 1, false)

	// events.appsflyer.com should be blocked with Host filtering rule
	eventsCount += 2
	testFilteringRuleNXDomainBlock(t, conn, "events.appsflyer.com")
	assertListenerEventsCount(t, listener, eventsCount)
	assertDNSRequestProcessedEvent(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-2), "events.appsflyer.com", "A", "0.0.0.0 events.appsflyer.com", 11111, false)
	assertDNSRequestProcessedEvent(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-1), "events.appsflyer.com", "AAAA", "0.0.0.0 events.appsflyer.com", 11111, false)

	// unregister listener
	ConfigureDNSRequestProcessedListener(nil)
	// Stop the proxy
	err = dnsProxy.Stop()
	assert.Nil(t, err)
}

// TestFilteringProxyFilterResponse tests response filtering logic
func TestFilteringProxyFilterResponse(t *testing.T) {
	dnsProxy := createTestFilteringProxy(BlockTypeNXDomain)

	listener := &testDNSRequestProcessedListener{}
	ConfigureDNSRequestProcessedListener(listener)

	testUpstream := &testUpstream{
		cn:   testCNAMEs,
		ipv4: testIPv4,
		ipv6: testIPv6,
	}

	// Start listening with mocked upstream
	err := dnsProxy.startWithMock(testUpstream)
	assert.Nil(t, err)

	// Create a DNS-over-UDP client connection
	addr := dnsProxy.Addr()
	conn, err := dns.Dial("udp", addr)
	assert.Nil(t, err)

	// There are two events produced with testFilteringRuleNXDomainBlock: A and AAAA requests
	// We should check count of registered events and each event
	// mail.google.com should be blocked by CNAME with Network filtering rules
	eventsCount := 2
	testFilteringRuleNXDomainBlock(t, conn, "mail.google.com")
	assertListenerEventsCount(t, listener, eventsCount)
	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-2), "mail.google.com",
		"A", "||googlemail.l.google.com^", "A, 1.2.3.4\nCNAME, googlemail.l.google.com.\n", 5, false)
	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-1), "mail.google.com",
		"AAAA", "||googlemail.l.google.com^", "AAAA, 2106:4700:10::6814:1e82\nCNAME, googlemail.l.google.com.\n", 5, false)

	// Let's check that whitelist rule `@@||host` has a higher priority than `||cname^`
	// It means that request which has been whitelisted with `@@||host` rule should not be blocked by cname (see filter with filterId 7)
	eventsCount++
	assertADNSResponseWhitelistedByResponse(t, listener, conn, "groups.google.com", "groups.l.google.com", "@@||groups.google.com^", 6, eventsCount)

	// Let's test the following rule: `0.0.0.0 cname`
	eventsCount += 2
	testFilteringRuleNXDomainBlock(t, conn, "picasa.google.com")
	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-2), "picasa.google.com",
		"A", "0.0.0.0 www2.l.google.com", "A, 9.10.11.12\nCNAME, www2.l.google.com.\n", 7, false)

	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-1), "picasa.google.com",
		"AAAA", "0.0.0.0 www2.l.google.com", "AAAA, 2306:4700:10::6814:1e82\nCNAME, www2.l.google.com.\n", 7, false)

	// Let's check the following rule: `0.0.0.0 ip`
	eventsCount += 2
	testFilteringRuleNXDomainBlock(t, conn, "dns.adguard.com")
	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-2), "dns.adguard.com",
		"A", "0.0.0.0 176.103.130.130", "A, 176.103.130.130\n", 8, false)
	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-1), "dns.adguard.com",
		"AAAA", "0.0.0.0 2a00:5a60::ad1:ff", "AAAA, 2a00:5a60::ad1:ff\n", 8, false)

	// Let's check the following rule: `||ip`
	eventsCount += 2
	testFilteringRuleNXDomainBlock(t, conn, "adguard.com")
	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-2), "adguard.com",
		"A", "||104.20.30.130", "A, 104.20.30.130\n", 9, false)
	assertDNSRequestProcessedEventWithOriginalAnswer(t, getDNSRequestProcessedEventByIdx(listener, eventsCount-1), "adguard.com",
		"AAAA", "||2606:4700:10::6814:1e82", "AAAA, 2606:4700:10::6814:1e82\n", 9, false)

	assertListenerEventsCount(t, listener, 9)

	// Unregister listener
	ConfigureDNSRequestProcessedListener(nil)
	// Stop dnsproxy
	assert.Nil(t, dnsProxy.Stop())
}

// startWithMock starts the DNSProxy with given upstream
// This method call is similar to call d.Start but it replaces the upstreams with given one before start
// Use it if you want to have your own responses and not rely on a remote server
func (d *DNSProxy) startWithMock(u *testUpstream) error {
	d.Lock()
	defer d.Unlock()

	if d.dnsProxy != nil {
		return errors.New("DNS proxy is already started")
	}

	// Create filtering engine
	err := d.createFilteringEngine(d.FilteringConfig)
	if err != nil {
		return fmt.Errorf("cannot start the DNS proxy: %s", err)
	}

	c, err := createConfig(d.Config)
	if err != nil {
		return fmt.Errorf("cannot start the DNS proxy: %s", err)
	}

	c.RequestHandler = d.handleDNSRequest
	d.dnsProxy = &proxy.Proxy{Config: *c}
	d.dnsProxy.Upstreams = []upstream.Upstream{u}

	// Start the proxy
	err = d.dnsProxy.Start()
	if err == nil && d.Config.DetectDNS64Prefix {
		go calculateNAT64Prefix(d.dnsProxy, d.Config.SystemResolvers)
	}

	return err
}

// assertADNSResponseWhitelistedByResponse checks the following logic:
// Response for A request with given host should contains given CNAME (if any) and whitelisted with given rule from filter with given filterId
func assertADNSResponseWhitelistedByResponse(t *testing.T, listener *testDNSRequestProcessedListener, conn *dns.Conn, host string, cname string, rule string, filterId int, eventsCount int) {
	// Create test message and send it
	req := createHostTestMessage(host)
	err := conn.WriteMsg(req)
	assert.Nil(t, err)

	// Receive an answer and check that everything is ok
	res, err := conn.ReadMsg()
	assert.Nil(t, err)
	assert.NotNil(t, res.Answer)

	// Let's check if response contains CNAME record
	hasCNAME := false
	for _, ans := range res.Answer {
		if v, ok := ans.(*dns.CNAME); ok {
			hasCNAME = true
			assert.Equal(t, cname, strings.TrimSuffix(v.Target, "."))
		}
	}
	assert.True(t, hasCNAME)

	// Check the last event in the listener
	assertDNSRequestProcessedEventWithListener(t, listener, host, "A", rule, filterId, eventsCount, true)
}

// assertDNSRequestProcessedEventWithListener asserts count of events in listener and the last event
func assertDNSRequestProcessedEventWithListener(t *testing.T, listener *testDNSRequestProcessedListener, domain, reqType, filteringRule string, filterListID, count int, whitelist bool) {
	assertListenerEventsCount(t, listener, count)
	event := getDNSRequestProcessedEventByIdx(listener, count-1)
	assertDNSRequestProcessedEvent(t, event, domain, reqType, filteringRule, filterListID, whitelist)
}

// assertDNSRequestProcessedEvent examine event
func assertDNSRequestProcessedEvent(t *testing.T, event DNSRequestProcessedEvent, domain, reqType, filteringRule string, filterListID int, whitelist bool) {
	assert.Equal(t, event.Domain, domain)
	assert.Equal(t, event.Type, reqType)
	assert.Equal(t, event.FilteringRule, filteringRule)
	assert.Equal(t, event.FilterListID, filterListID)
	assert.Equal(t, event.Whitelist, whitelist)
}

// assertDNSRequestProcessedEventWithOriginalAnswer examine event with OriginalAnswer check
func assertDNSRequestProcessedEventWithOriginalAnswer(t *testing.T, event DNSRequestProcessedEvent, domain, reqType, filteringRule, originalAnswer string, filterListID int, whitelist bool) {
	assertDNSRequestProcessedEvent(t, event, domain, reqType, filteringRule, filterListID, whitelist)
	assert.Equal(t, event.OriginalAnswer, originalAnswer)
}

// getDNSRequestProcessedEventByIdx returns DNSRequestProcessedEvent from listener
func getDNSRequestProcessedEventByIdx(listener *testDNSRequestProcessedListener, idx int) DNSRequestProcessedEvent {
	dnsRequestProcessedListenerGuard.Lock()
	defer dnsRequestProcessedListenerGuard.Unlock()
	return listener.e[idx]
}

// createTestFilteringProxy returns configured DNSProxy with given blockType
func createTestFilteringProxy(blockType int) *DNSProxy {
	const rulesJSON = `
	[
		{"id": 1, "contents": "||example.com^\n||example.org^"},
		{"id": 2, "contents": "0.0.0.0 google.com\n127.0.0.1 google.ru"},
		{"id": 3, "contents": "2000:: yandex.ru\n::1 yandex.ua"},
		{"id": 4, "contents": "@@||google-public-dns-a.google.com^"},
		{"id": 5, "contents": "||googlemail.l.google.com^"},
		{"id": 6, "contents": "||groups.l.google.com^\n||wide-groups.l.google.com\n@@||groups.google.com^"},
		{"id": 7, "contents": "0.0.0.0 www2.l.google.com"},
		{"id": 8, "contents": "0.0.0.0 176.103.130.130\n0.0.0.0 2a00:5a60::ad1:ff"},
		{"id": 9, "contents": "||104.20.30.130\n||2606:4700:10::6814:1e82"}
	]`

	config := createDefaultConfig()
	filteringConfig := &FilteringConfig{
		FilteringRulesFilesJSON:   filesJSON,
		FilteringRulesStringsJSON: rulesJSON,
		BlockType:                 blockType,
	}

	mobileDNSProxy := DNSProxy{Config: config, FilteringConfig: filteringConfig}
	return &mobileDNSProxy
}

// testUpstream is a mock of real upstream.
// specify fields with necessary values to simulate real upstream behaviour
type testUpstream struct {
	cn   map[string]string   // Map of [name]canonical_name
	ipv4 map[string][]net.IP // Map of [name]IPv4
	ipv6 map[string][]net.IP // Map of [name]IPv6
}

func (u *testUpstream) Exchange(m *dns.Msg) (*dns.Msg, error) {
	resp := dns.Msg{}
	resp.SetReply(m)
	hasARecord := false
	hasAAAARecord := false

	reqType := m.Question[0].Qtype
	name := m.Question[0].Name

	// Let's check if we have any CNAME for given name
	if cname, ok := u.cn[name]; ok {
		cn := dns.CNAME{}
		cn.Hdr.Name = name
		cn.Hdr.Rrtype = dns.TypeCNAME
		cn.Target = cname
		resp.Answer = append(resp.Answer, &cn)
	}

	// Let's check if we can add some A records to the answer
	if ipv4addr, ok := u.ipv4[name]; ok && reqType == dns.TypeA {
		hasARecord = true
		for _, ipv4 := range ipv4addr {
			respA := dns.A{}
			respA.Hdr.Rrtype = dns.TypeA
			respA.Hdr.Name = name
			respA.A = ipv4
			resp.Answer = append(resp.Answer, &respA)
		}
	}

	// Let's check if we can add some AAAA records to the answer
	if ipv6addr, ok := u.ipv6[name]; ok && reqType == dns.TypeAAAA {
		hasAAAARecord = true
		for _, ipv6 := range ipv6addr {
			respAAAA := dns.A{}
			respAAAA.Hdr.Rrtype = dns.TypeAAAA
			respAAAA.Hdr.Name = name
			respAAAA.A = ipv6
			resp.Answer = append(resp.Answer, &respAAAA)
		}
	}

	if len(resp.Answer) == 0 {
		if hasARecord || hasAAAARecord {
			// Set No Error RCode if there are some records for given Qname but we didn't apply them
			resp.SetRcode(m, dns.RcodeSuccess)
		} else {
			// Set NXDomain RCode otherwise
			resp.SetRcode(m, dns.RcodeNameError)
		}
	}

	return &resp, nil
}

func (u *testUpstream) Address() string {
	return "test"
}

// testFilteringRulesNXDomainBlockAsync sends requests, which should be blocked with NXDomain, in parallel
func testFilteringRulesNXDomainBlockAsync(t *testing.T, conn *dns.Conn, host string) {
	g := &sync.WaitGroup{}
	g.Add(testMessagesCount)

	for i := 0; i < testMessagesCount; i++ {
		go testFilteringRuleNXDomainBlockAsync(t, conn, host, g)
	}

	g.Wait()
}

func testFilteringRuleNXDomainBlockAsync(t *testing.T, conn *dns.Conn, host string, g *sync.WaitGroup) {
	defer func() {
		g.Done()
	}()

	testFilteringRuleNXDomainBlock(t, conn, host)
}

// testFilteringRulesIPBlockAsync sends requests, which should be blocked with IP, in parallel
func testFilteringRulesIPBlockAsync(t *testing.T, conn *dns.Conn, host string, reqType uint16, ip net.IP) {
	g := &sync.WaitGroup{}
	g.Add(testMessagesCount)

	for i := 0; i < testMessagesCount; i++ {
		go testFilteringRuleIPBlockAsync(t, conn, host, ip, reqType, g)
	}

	g.Wait()
}

func testFilteringRuleIPBlockAsync(t *testing.T, conn *dns.Conn, host string, ip net.IP, reqType uint16, g *sync.WaitGroup) {
	defer func() {
		g.Done()
	}()

	if reqType == dns.TypeA {
		testFilteringRuleARequestIPBlock(t, conn, host, ip)
	} else if reqType == dns.TypeAAAA {
		testFilteringRuleAAAARequestIPBlock(t, conn, host, ip)
	}
}

func sendAndAssertTestMessage(t *testing.T, conn *dns.Conn) {
	req := createTestMessage()
	err := conn.WriteMsg(req)
	assert.Nil(t, err)

	res, err := conn.ReadMsg()
	assert.Nil(t, err)

	assertResponse(t, res)
}

// testFilteringRuleNXDomainBlock is a test for all kind of filtering rules:
// - There is a filtering rule which matched given host in the filteringEngine.
// - Both A and AAAA requests for this host should be filtered with NXDomain RCode
// If you'd like to test mobile event handler, note that this method will create two events
func testFilteringRuleNXDomainBlock(t *testing.T, conn *dns.Conn, host string) {
	req := createHostTestMessage(host)
	err := conn.WriteMsg(req)
	assert.Nil(t, err)

	res, err := conn.ReadMsg()
	assert.Nil(t, err)

	assert.Nil(t, res.Answer)
	assert.Equal(t, len(res.Ns), 1)
	assert.Equal(t, res.Rcode, dns.RcodeNameError)

	req = createAAAATestMessage(host)
	err = conn.WriteMsg(req)
	assert.Nil(t, err)

	res, err = conn.ReadMsg()
	assert.Nil(t, err)

	assert.Nil(t, res.Answer)
	assert.Equal(t, len(res.Ns), 1)
	assert.Equal(t, res.Rcode, dns.RcodeNameError)
}

// testFilteringRuleARequestIPBlock is a test for the following case:
// - There is a filtering rule for the given host in the DNS Filtering filteringEngine
// - A request for this host should be filtered and response must contain the given ip address
func testFilteringRuleARequestIPBlock(t *testing.T, conn *dns.Conn, host string, ip net.IP) {
	res := sendATestMessage(t, conn, host)
	assertAResponse(t, res, ip)
}

func sendATestMessage(t *testing.T, conn *dns.Conn, host string) *dns.Msg {
	req := createHostTestMessage(host)
	err := conn.WriteMsg(req)
	assert.Nil(t, err)
	res, err := conn.ReadMsg()
	assert.Nil(t, err)
	return res
}

// testFilteringRuleARequestIPBlock is a test for the following case:
// - There is a filtering rule for the given host in the DNS Filtering filteringEngine
// - AAAA request for this host should be filtered and response must contain the given ip address
func testFilteringRuleAAAARequestIPBlock(t *testing.T, conn *dns.Conn, host string, ip net.IP) {
	res := sendAAAATestMessage(t, conn, host)
	assertAAAAResponse(t, res, ip)
}

func sendAAAATestMessage(t *testing.T, conn *dns.Conn, host string) *dns.Msg {
	req := createAAAATestMessage(host)
	err := conn.WriteMsg(req)
	assert.Nil(t, err)
	res, err := conn.ReadMsg()
	assert.Nil(t, err)
	return res
}

// testAAAAHostFilteringRuleARequest is a test for the following case:
// - There is IPv6 host filtering rule for the given host in the DNS Filtering filteringEngine
// - A request for this host should not be filtered
func testAAAAHostFilteringRuleARequest(t *testing.T, conn *dns.Conn, host string) {
	req := createHostTestMessage(host)
	err := conn.WriteMsg(req)
	assert.Nil(t, err)

	res, err := conn.ReadMsg()
	assert.Nil(t, err)

	assert.NotNil(t, res.Answer)
	assert.Equal(t, res.Answer[0].Header().Rrtype, dns.TypeA)
}

// assertAResponse asserts known A response with the given IPv4 address
func assertAResponse(t *testing.T, reply *dns.Msg, ipv4 net.IP) {
	if len(reply.Answer) != 1 {
		t.Fatalf("DNS upstream returned reply with wrong number of answers - %d", len(reply.Answer))
	}

	if a, ok := reply.Answer[0].(*dns.A); ok {
		if !ipv4.Equal(a.A) {
			t.Fatalf("DNS upstream returned wrong answer instead of %v: %v", ipv4, a.A)
		}
	} else {
		t.Fatalf("DNS upstream returned wrong answer type instead of A: %v", reply.Answer[0])
	}
}

// assertAAAAResponse asserts known AAAA response with the given IPv6 address
func assertAAAAResponse(t *testing.T, reply *dns.Msg, ipv6 net.IP) {
	if len(reply.Answer) != 1 {
		t.Fatalf("DNS upstream returned reply with wrong number of answers - %d", len(reply.Answer))
	}

	if a, ok := reply.Answer[0].(*dns.AAAA); ok {
		if !ipv6.Equal(a.AAAA) {
			t.Fatalf("DNS upstream returned wrong answer instead of %v: %v", ipv6, a.AAAA)
		}
	} else {
		t.Fatalf("DNS upstream returned wrong answer type instead of A: %v", reply.Answer[0])
	}
}

func createAAAATestMessage(host string) *dns.Msg {
	req := dns.Msg{}
	req.Id = dns.Id()
	req.RecursionDesired = true
	name := host + "."
	req.Question = []dns.Question{
		{Name: name, Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
	}
	return &req
}
