package dnsforward

import (
	"net/netip"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/filtering"
	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/golibs/log"
	"github.com/AdguardTeam/urlfilter/rules"
	"github.com/miekg/dns"
	"golang.org/x/exp/slices"
)

// makeResponse creates a DNS response by req and sets necessary flags.  It also
// guarantees that req.Question will be not empty.
func (s *Server) makeResponse(req *dns.Msg) (resp *dns.Msg) {
	resp = &dns.Msg{
		MsgHdr: dns.MsgHdr{
			RecursionAvailable: true,
		},
		Compress: true,
	}

	resp.SetReply(req)

	return resp
}

// ipsFromRules extracts unique non-IP addresses from the filtering result
// rules.
func ipsFromRules(resRules []*filtering.ResultRule) (ips []netip.Addr) {
	for _, r := range resRules {
		// len(resRules) and len(ips) are actually small enough for O(n^2) to do
		// not raise performance questions.
		if ip := r.IP; ip != (netip.Addr{}) && !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}

	return ips
}

// genDNSFilterMessage generates a filtered response to req for the filtering
// result res.
func (s *Server) genDNSFilterMessage(
	dctx *proxy.DNSContext,
	res *filtering.Result,
) (resp *dns.Msg) {
	req := dctx.Req
	qt := req.Question[0].Qtype
	if qt != dns.TypeA && qt != dns.TypeAAAA {
		if s.conf.BlockingMode == BlockingModeNullIP {
			return s.makeResponse(req)
		}

		return s.newMsgNODATA(req)
	}

	switch res.Reason {
	case filtering.FilteredSafeBrowsing:
		return s.genBlockedHost(req, s.conf.SafeBrowsingBlockHost, dctx)
	case filtering.FilteredParental:
		return s.genBlockedHost(req, s.conf.ParentalBlockHost, dctx)
	case filtering.FilteredSafeSearch:
		// If Safe Search generated the necessary IP addresses, use them.
		// Otherwise, if there were no errors, there are no addresses for the
		// requested IP version, so produce a NODATA response.
		return s.genResponseWithIPs(req, ipsFromRules(res.Rules))
	default:
		return s.genForBlockingMode(req, ipsFromRules(res.Rules))
	}
}

// genForBlockingMode generates a filtered response to req based on the server's
// blocking mode.
func (s *Server) genForBlockingMode(req *dns.Msg, ips []netip.Addr) (resp *dns.Msg) {
	qt := req.Question[0].Qtype
	switch m := s.conf.BlockingMode; m {
	case BlockingModeCustomIP:
		switch qt {
		case dns.TypeA:
			return s.genARecord(req, s.conf.BlockingIPv4)
		case dns.TypeAAAA:
			return s.genAAAARecord(req, s.conf.BlockingIPv6)
		default:
			// Generally shouldn't happen, since the types are checked in
			// genDNSFilterMessage.
			log.Error("dns: invalid msg type %s for blocking mode %s", dns.Type(qt), m)

			return s.makeResponse(req)
		}
	case BlockingModeDefault:
		if len(ips) > 0 {
			return s.genResponseWithIPs(req, ips)
		}

		return s.makeResponseNullIP(req)
	case BlockingModeNullIP:
		return s.makeResponseNullIP(req)
	case BlockingModeNXDOMAIN:
		return s.genNXDomain(req)
	case BlockingModeREFUSED:
		return s.makeResponseREFUSED(req)
	default:
		log.Error("dns: invalid blocking mode %q", s.conf.BlockingMode)

		return s.makeResponse(req)
	}
}

func (s *Server) genServerFailure(request *dns.Msg) *dns.Msg {
	resp := dns.Msg{}
	resp.SetRcode(request, dns.RcodeServerFailure)
	resp.RecursionAvailable = true
	return &resp
}

func (s *Server) genARecord(request *dns.Msg, ip netip.Addr) *dns.Msg {
	resp := s.makeResponse(request)
	resp.Answer = append(resp.Answer, s.genAnswerA(request, ip))
	return resp
}

func (s *Server) genAAAARecord(request *dns.Msg, ip netip.Addr) *dns.Msg {
	resp := s.makeResponse(request)
	resp.Answer = append(resp.Answer, s.genAnswerAAAA(request, ip))
	return resp
}

func (s *Server) hdr(req *dns.Msg, rrType rules.RRType) (h dns.RR_Header) {
	return dns.RR_Header{
		Name:   req.Question[0].Name,
		Rrtype: rrType,
		Ttl:    s.conf.BlockedResponseTTL,
		Class:  dns.ClassINET,
	}
}

func (s *Server) genAnswerA(req *dns.Msg, ip netip.Addr) (ans *dns.A) {
	return &dns.A{
		Hdr: s.hdr(req, dns.TypeA),
		A:   ip.AsSlice(),
	}
}

func (s *Server) genAnswerAAAA(req *dns.Msg, ip netip.Addr) (ans *dns.AAAA) {
	return &dns.AAAA{
		Hdr:  s.hdr(req, dns.TypeAAAA),
		AAAA: ip.AsSlice(),
	}
}

func (s *Server) genAnswerCNAME(req *dns.Msg, cname string) (ans *dns.CNAME) {
	return &dns.CNAME{
		Hdr:    s.hdr(req, dns.TypeCNAME),
		Target: dns.Fqdn(cname),
	}
}

func (s *Server) genAnswerMX(req *dns.Msg, mx *rules.DNSMX) (ans *dns.MX) {
	return &dns.MX{
		Hdr:        s.hdr(req, dns.TypeMX),
		Preference: mx.Preference,
		Mx:         dns.Fqdn(mx.Exchange),
	}
}

func (s *Server) genAnswerPTR(req *dns.Msg, ptr string) (ans *dns.PTR) {
	return &dns.PTR{
		Hdr: s.hdr(req, dns.TypePTR),
		Ptr: dns.Fqdn(ptr),
	}
}

func (s *Server) genAnswerSRV(req *dns.Msg, srv *rules.DNSSRV) (ans *dns.SRV) {
	return &dns.SRV{
		Hdr:      s.hdr(req, dns.TypeSRV),
		Priority: srv.Priority,
		Weight:   srv.Weight,
		Port:     srv.Port,
		Target:   dns.Fqdn(srv.Target),
	}
}

func (s *Server) genAnswerTXT(req *dns.Msg, strs []string) (ans *dns.TXT) {
	return &dns.TXT{
		Hdr: s.hdr(req, dns.TypeTXT),
		Txt: strs,
	}
}

// genResponseWithIPs generates a DNS response message with the provided IP
// addresses and an appropriate resource record type.  If any of the IPs cannot
// be converted to the correct protocol, genResponseWithIPs returns an empty
// response.
func (s *Server) genResponseWithIPs(req *dns.Msg, ips []netip.Addr) (resp *dns.Msg) {
	var ans []dns.RR
	switch req.Question[0].Qtype {
	case dns.TypeA:
		for _, ip := range ips {
			if ip.Is4() {
				ans = append(ans, s.genAnswerA(req, ip))
			} else {
				ans = nil

				break
			}
		}
	case dns.TypeAAAA:
		for _, ip := range ips {
			if ip.Is6() {
				ans = append(ans, s.genAnswerAAAA(req, ip))
			}
		}
	default:
		// Go on and return an empty response.
	}

	resp = s.makeResponse(req)
	resp.Answer = ans

	return resp
}

// makeResponseNullIP creates a response with 0.0.0.0 for A requests, :: for
// AAAA requests, and an empty response for other types.
func (s *Server) makeResponseNullIP(req *dns.Msg) (resp *dns.Msg) {
	// Respond with the corresponding zero IP type as opposed to simply
	// using one or the other in both cases, because the IPv4 zero IP is
	// converted to a IPV6-mapped IPv4 address, while the IPv6 zero IP is
	// converted into an empty slice instead of the zero IPv4.
	switch req.Question[0].Qtype {
	case dns.TypeA:
		resp = s.genResponseWithIPs(req, []netip.Addr{netip.IPv4Unspecified()})
	case dns.TypeAAAA:
		resp = s.genResponseWithIPs(req, []netip.Addr{netip.IPv6Unspecified()})
	default:
		resp = s.makeResponse(req)
	}

	return resp
}

func (s *Server) genBlockedHost(request *dns.Msg, newAddr string, d *proxy.DNSContext) *dns.Msg {
	ip, err := netip.ParseAddr(newAddr)
	if err == nil {
		return s.genResponseWithIPs(request, []netip.Addr{ip})
	}

	// look up the hostname, TODO: cache
	replReq := dns.Msg{}
	replReq.SetQuestion(dns.Fqdn(newAddr), request.Question[0].Qtype)
	replReq.RecursionDesired = true

	newContext := &proxy.DNSContext{
		Proto:     d.Proto,
		Addr:      d.Addr,
		StartTime: time.Now(),
		Req:       &replReq,
	}

	prx := s.proxy()
	if prx == nil {
		log.Debug("dns: %s", srvClosedErr)

		return s.genServerFailure(request)
	}

	err = prx.Resolve(newContext)
	if err != nil {
		log.Printf("couldn't look up replacement host %q: %s", newAddr, err)

		return s.genServerFailure(request)
	}

	resp := s.makeResponse(request)
	if newContext.Res != nil {
		for _, answer := range newContext.Res.Answer {
			answer.Header().Name = request.Question[0].Name
			resp.Answer = append(resp.Answer, answer)
		}
	}

	return resp
}

// preBlockedResponse returns a protocol-appropriate response for a request that
// was blocked by access settings.
func (s *Server) preBlockedResponse(pctx *proxy.DNSContext) (reply bool, err error) {
	if pctx.Proto == proxy.ProtoUDP || pctx.Proto == proxy.ProtoDNSCrypt {
		// Return nil so that dnsproxy drops the connection and thus
		// prevent DNS amplification attacks.
		return false, nil
	}

	pctx.Res = s.makeResponseREFUSED(pctx.Req)

	return true, nil
}

// Create REFUSED DNS response
func (s *Server) makeResponseREFUSED(request *dns.Msg) *dns.Msg {
	resp := dns.Msg{}
	resp.SetRcode(request, dns.RcodeRefused)
	resp.RecursionAvailable = true
	return &resp
}

// newMsgNODATA returns a properly initialized NODATA response.
//
// See https://www.rfc-editor.org/rfc/rfc2308#section-2.2.
func (s *Server) newMsgNODATA(req *dns.Msg) (resp *dns.Msg) {
	resp = (&dns.Msg{}).SetRcode(req, dns.RcodeSuccess)
	resp.RecursionAvailable = true
	resp.Ns = s.genSOA(req)

	return resp
}

func (s *Server) genNXDomain(request *dns.Msg) *dns.Msg {
	resp := dns.Msg{}
	resp.SetRcode(request, dns.RcodeNameError)
	resp.RecursionAvailable = true
	resp.Ns = s.genSOA(request)
	return &resp
}

func (s *Server) genSOA(request *dns.Msg) []dns.RR {
	zone := ""
	if len(request.Question) > 0 {
		zone = request.Question[0].Name
	}

	soa := dns.SOA{
		// values copied from verisign's nonexistent .com domain
		// their exact values are not important in our use case because they are used for domain transfers between primary/secondary DNS servers
		Refresh: 1800,
		Retry:   900,
		Expire:  604800,
		Minttl:  86400,
		// copied from AdGuard DNS
		Ns:     "fake-for-negative-caching.adguard.com.",
		Serial: 100500,
		// rest is request-specific
		Hdr: dns.RR_Header{
			Name:   zone,
			Rrtype: dns.TypeSOA,
			Ttl:    s.conf.BlockedResponseTTL,
			Class:  dns.ClassINET,
		},
		Mbox: "hostmaster.", // zone will be appended later if it's not empty or "."
	}
	if soa.Hdr.Ttl == 0 {
		soa.Hdr.Ttl = defaultValues.BlockedResponseTTL
	}
	if len(zone) > 0 && zone[0] != '.' {
		soa.Mbox += zone
	}
	return []dns.RR{&soa}
}
