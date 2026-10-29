package checks

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// pingSeq unterscheidet gleichzeitige Pings desselben Prozesses.
var pingSeq atomic.Uint32

// probePing schickt ein ICMP-Echo. Bevorzugt wird ein unprivilegierter
// ICMP-Socket ("udp4"); den erlaubt Linux, wenn die Gruppe des Prozesses in
// net.ipv4.ping_group_range liegt (Standard in Containern unter containerd).
// Sonst wird ein Raw-Socket versucht, der root bzw. CAP_NET_RAW braucht.
func probePing(ctx context.Context, host string) store.CheckResult {
	var r store.CheckResult
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		r.Error = "host nicht gefunden"
		return r
	}
	ip := ips[0].IP
	for _, a := range ips { // IPv4 bevorzugen
		if a.IP.To4() != nil {
			ip = a.IP
			break
		}
	}
	v4 := ip.To4() != nil

	conn, raw, err := listenICMP(v4)
	if err != nil {
		r.Error = "ping nicht möglich: " + shortError(err)
		return r
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	seq := int(pingSeq.Add(1) & 0xffff)
	id := os.Getpid() & 0xffff
	var typ icmp.Type = ipv4.ICMPTypeEcho
	proto := 1 // ICMP
	if !v4 {
		typ, proto = ipv6.ICMPTypeEchoRequest, 58
	}
	payload := []byte("mat-check")
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload}}
	b, err := msg.Marshal(nil)
	if err != nil {
		r.Error = "ping vorbereiten: " + err.Error()
		return r
	}
	var dst net.Addr = &net.UDPAddr{IP: ip}
	if raw {
		dst = &net.IPAddr{IP: ip}
	}
	start := time.Now()
	if _, err := conn.WriteTo(b, dst); err != nil {
		r.Error = shortError(err)
		return r
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			r.Error = shortError(err)
			return r
		}
		reply, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		echo, ok := reply.Body.(*icmp.Echo)
		// Beim unprivilegierten Socket setzt der Kernel die ID selbst; deshalb
		// zählen Sequenznummer, Inhalt und Absender.
		if !ok || (reply.Type != ipv4.ICMPTypeEchoReply && reply.Type != ipv6.ICMPTypeEchoReply) ||
			echo.Seq != seq || string(echo.Data) != string(payload) || !sameIP(from, ip) {
			continue
		}
		latency := float64(time.Since(start).Microseconds()) / 1000
		r.OK, r.LatencyMS = true, &latency
		return r
	}
}

func listenICMP(v4 bool) (conn *icmp.PacketConn, raw bool, err error) {
	udp, rawNet, addr := "udp4", "ip4:icmp", "0.0.0.0"
	if !v4 {
		udp, rawNet, addr = "udp6", "ip6:ipv6-icmp", "::"
	}
	if conn, err = icmp.ListenPacket(udp, addr); err == nil {
		return conn, false, nil
	}
	if conn, rerr := icmp.ListenPacket(rawNet, addr); rerr == nil {
		return conn, true, nil
	}
	return nil, false, fmt.Errorf("kein icmp-socket (%w)", err)
}

func sameIP(a net.Addr, ip net.IP) bool {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.IP.Equal(ip)
	case *net.IPAddr:
		return v.IP.Equal(ip)
	}
	return false
}
